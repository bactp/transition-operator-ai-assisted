package tools

import (
	"context"
	"fmt"
	"strings"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
	helpers "github.com/vitu1234/transition-operator/reconcilers/helpers"

	"github.com/go-logr/logr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	registerTool(GetTransitionCandidates())
}

type GetTransitionCandidatesParams struct {
	ClusterPolicyName string `json:"clusterPolicyName" description:"Name of the ClusterPolicy to inspect."`
	Namespace         string `json:"namespace,omitempty" description:"Namespace of the ClusterPolicy. Defaults to \"default\"."`
}

type CandidateCluster struct {
	Name     string `json:"name"`
	RepoType string `json:"repoType"`
	Weight   int    `json:"weight"`
	Avoided  bool   `json:"avoided" description:"True if this cluster is in avoidClusters instead of preferClusters."`
	Healthy  bool   `json:"healthy" description:"True if get_cluster_health reported the control plane and CNI as Ready."`
	// ImageAvailable/StaleImages: a Checkpoint's Phase reflects only the MOST
	// RECENT checkpoint attempt and gets overwritten every cron cycle
	// (checkpoint_controller.go reuses one Checkpoint CR per pod/package), but
	// its LastCheckpointImage field is only ever set on success and is left
	// untouched by later failures. So Phase=Failed does NOT mean "no usable
	// checkpoint" - it means "the most recent refresh attempt failed", and a
	// prior image can still be sitting on the target node ready to restore
	// from right now. Checking readiness for failover therefore means asking
	// "is a checkpoint image already present on the target cluster's nodes",
	// not "did the last checkpoint attempt succeed" - so this checks the
	// target's own Node.Status.Images directly (ground truth: the pre-pull
	// step already ran and pulled it there, or it's stale-but-present from an
	// earlier run) rather than trusting the source-side Checkpoint CR's phase.
	ImageAvailable bool     `json:"imageAvailable" description:"True if at least one live-state package's last-known checkpoint image is already present on this cluster's nodes (checked directly via Node.Status.Images), regardless of whether the most recent checkpoint attempt succeeded."`
	StaleImages    []string `json:"staleImages,omitempty" description:"Checkpoint images that ARE present on this cluster's nodes but come from a Checkpoint CR whose most recent attempt shows Phase=Failed - present but not freshly refreshed."`
}

type GetTransitionCandidatesResult struct {
	Candidates     []CandidateCluster `json:"candidates"`
	CurrentTopPick string             `json:"currentTopPick,omitempty" description:"The cluster helpers.DetermineTargetRepo (the operator's own static-weight picker) would choose today."`
	Error          string             `json:"error,omitempty"`
}

// GetTransitionCandidates returns the ClusterPolicy's configured preferred/avoided
// target clusters, annotated with live health, plus the operator's own static
// top pick (reconcilers/helpers.DetermineTargetRepo) so the agent can agree with
// or override that pick with a reason.
func GetTransitionCandidates() MCPTool[GetTransitionCandidatesParams, GetTransitionCandidatesResult] {
	return MCPTool[GetTransitionCandidatesParams, GetTransitionCandidatesResult]{
		Name: "get_transition_candidates",
		Description: "Return a ClusterPolicy's configured preferred/avoided target clusters, their live health, the operator's own static top pick, and - per candidate - whether a checkpoint image is already present on that cluster's nodes (imageAvailable). Use imageAvailable, not get_checkpoint_status's Phase, to decide if a candidate is ready for failover: Phase reflects only the most recent checkpoint attempt and gets overwritten every cycle, so Phase=Failed does not mean no usable image exists.",
		Handler: func(ctx context.Context, cc *mcp.ServerSession, params *mcp.CallToolParamsFor[GetTransitionCandidatesParams]) (*mcp.CallToolResultFor[GetTransitionCandidatesResult], error) {
			cl, err := k8sClient()
			if err != nil {
				return nil, fmt.Errorf("build k8s client: %w", err)
			}

			ns := params.Arguments.Namespace
			if ns == "" {
				ns = "default"
			}

			var cp transitionv1.ClusterPolicy
			if err := cl.Get(ctx, types.NamespacedName{Name: params.Arguments.ClusterPolicyName, Namespace: ns}, &cp); err != nil {
				return toolResult(GetTransitionCandidatesResult{Error: err.Error()}), nil
			}

			// Known checkpoint images for this ClusterPolicy's SOURCE cluster,
			// independent of whether the most recent checkpoint attempt for
			// each succeeded - see the CandidateCluster.ImageAvailable doc.
			var checkpoints transitionv1.CheckpointList
			knownImages := map[string]bool{} // image -> most-recent-attempt-failed
			if err := cl.List(ctx, &checkpoints); err == nil {
				for _, ckpt := range checkpoints.Items {
					if ckpt.Spec.ClusterRef == nil || ckpt.Spec.ClusterRef.Name != cp.Spec.ClusterSelector.Name {
						continue
					}
					if ckpt.Status.LastCheckpointImage == "" {
						continue
					}
					knownImages[ckpt.Status.LastCheckpointImage] = ckpt.Status.Phase == transitionv1.CheckpointPhaseFailed
				}
			}

			result := GetTransitionCandidatesResult{Candidates: []CandidateCluster{}}
			for _, target := range cp.Spec.TargetClusterPolicy.PreferClusters {
				cpStatus, cniStatus, healthErr := checkClusterHealth(ctx, cl, target.Name)
				healthy := healthErr == nil && cpStatus == "Ready" && cniStatus == "Ready"
				available, stale := checkImagesOnCluster(ctx, cl, target.Name, knownImages)
				result.Candidates = append(result.Candidates, CandidateCluster{
					Name: target.Name, RepoType: target.RepoType, Weight: target.Weight,
					Healthy:        healthy,
					ImageAvailable: available,
					StaleImages:    stale,
				})
			}
			for _, target := range cp.Spec.TargetClusterPolicy.AvoidClusters {
				result.Candidates = append(result.Candidates, CandidateCluster{
					Name: target.Name, RepoType: target.RepoType, Weight: target.Weight, Avoided: true,
				})
			}

			_, topPick, found := helpers.DetermineTargetRepo(&cp, logr.Discard())
			if found {
				result.CurrentTopPick = topPick
			}

			return toolResult(result), nil
		},
	}
}

// checkImagesOnCluster reports whether any of `images` (checkpoint image
// references, keyed to whether their Checkpoint's most recent attempt
// failed) is already present in the target cluster's own Node.Status.Images
// - i.e. genuinely pulled and ready to run right now on that cluster,
// regardless of what the source-side Checkpoint CR's Phase currently says.
func checkImagesOnCluster(ctx context.Context, cl client.Client, clusterName string, images map[string]bool) (available bool, stale []string) {
	capiCluster, err := capictrl.GetCapiClusterFromName(ctx, clusterName, "default", cl)
	if err != nil || capiCluster == nil {
		return false, nil
	}
	clusterClient, _, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil || !ready {
		return false, nil
	}

	var nodes corev1.NodeList
	if err := clusterClient.List(ctx, &nodes); err != nil {
		return false, nil
	}

	// containerd records Node.Status.Images by DIGEST (repo@sha256:...), not
	// by the ":latest" tag checkpoint images are pushed with - each push
	// overwrites the tag but leaves the old digest cached, so an exact-string
	// match against a tag reference would never hit. Compare by repository
	// (registry+path, tag/digest stripped) instead: "some version of this
	// workload's checkpoint is already on this node" is what matters for
	// failover readiness, not which exact digest.
	present := map[string]bool{}
	for _, node := range nodes.Items {
		for _, img := range node.Status.Images {
			for _, name := range img.Names {
				present[imageRepo(name)] = true
			}
		}
	}

	for image, mostRecentAttemptFailed := range images {
		if !present[imageRepo(image)] {
			continue
		}
		available = true
		if mostRecentAttemptFailed {
			stale = append(stale, image)
		}
	}
	return available, stale
}

// imageRepo strips a trailing ":tag" or "@sha256:..." from an image
// reference, leaving just "registry/path" for repository-level comparison.
func imageRepo(ref string) string {
	if i := strings.LastIndex(ref, "@"); i != -1 {
		return ref[:i]
	}
	// Only strip ":tag" if the colon comes after the last "/", so a registry
	// host:port (e.g. localhost:5000/foo) isn't mistaken for a tag.
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon != -1 && colon > slash {
		return ref[:colon]
	}
	return ref
}
