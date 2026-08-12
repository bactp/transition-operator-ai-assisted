package tools

import (
	"context"
	"fmt"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(GetCheckpointStatus())
}

type GetCheckpointStatusParams struct {
	ClusterName string `json:"clusterName,omitempty" description:"Optional: only return Checkpoints whose clusterRef/targetClusterRef name matches this cluster."`
	PackageName string `json:"packageName,omitempty" description:"Optional: only return Checkpoints whose targetClusterRef.packageName matches this package."`
}

type CheckpointSummary struct {
	Name                 string `json:"name"`
	Phase                string `json:"phase"`
	LastCheckpointTime   string `json:"lastCheckpointTime,omitempty"`
	LastCheckpointImage  string `json:"lastCheckpointImage,omitempty"`
	OriginalImage        string `json:"originalImage,omitempty"`
	Message              string `json:"message,omitempty"`
}

type GetCheckpointStatusResult struct {
	Checkpoints []CheckpointSummary `json:"checkpoints"`
	Error       string              `json:"error,omitempty"`
}

// GetCheckpointStatus lists Checkpoint CRs and their phase/last-checkpoint-image,
// so the agent can tell whether a live-state workload actually has a restorable
// checkpoint before recommending a failover for it.
func GetCheckpointStatus() MCPTool[GetCheckpointStatusParams, GetCheckpointStatusResult] {
	return MCPTool[GetCheckpointStatusParams, GetCheckpointStatusResult]{
		Name:        "get_checkpoint_status",
		Description: "List Checkpoint CRs (optionally filtered by cluster/package) and their phase, last checkpoint time, and checkpoint image.",
		Handler: func(ctx context.Context, cc *mcp.ServerSession, params *mcp.CallToolParamsFor[GetCheckpointStatusParams]) (*mcp.CallToolResultFor[GetCheckpointStatusResult], error) {
			cl, err := k8sClient()
			if err != nil {
				return nil, fmt.Errorf("build k8s client: %w", err)
			}

			var list transitionv1.CheckpointList
			if err := cl.List(ctx, &list); err != nil {
				return toolResult(GetCheckpointStatusResult{Error: err.Error()}), nil
			}

			result := GetCheckpointStatusResult{Checkpoints: []CheckpointSummary{}}
			for _, cp := range list.Items {
				if params.Arguments.ClusterName != "" {
					matchesCluster := (cp.Spec.ClusterRef != nil && cp.Spec.ClusterRef.Name == params.Arguments.ClusterName) ||
						(cp.Spec.TargetClusterRef != nil && cp.Spec.TargetClusterRef.Name == params.Arguments.ClusterName)
					if !matchesCluster {
						continue
					}
				}
				if params.Arguments.PackageName != "" {
					matchesPkg := cp.Spec.TargetClusterRef != nil && cp.Spec.TargetClusterRef.PackageName == params.Arguments.PackageName
					if !matchesPkg {
						continue
					}
				}

				summary := CheckpointSummary{
					Name:                cp.Name,
					Phase:               string(cp.Status.Phase),
					LastCheckpointImage: cp.Status.LastCheckpointImage,
					OriginalImage:       cp.Status.OriginalImage,
					Message:             cp.Status.Message,
				}
				if cp.Status.LastCheckpointTime != nil {
					summary.LastCheckpointTime = cp.Status.LastCheckpointTime.Format("2006-01-02T15:04:05Z07:00")
				}
				result.Checkpoints = append(result.Checkpoints, summary)
			}

			return toolResult(result), nil
		},
	}
}
