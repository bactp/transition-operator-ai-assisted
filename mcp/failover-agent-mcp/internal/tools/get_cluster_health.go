package tools

import (
	"context"
	"fmt"

	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
	"github.com/vitu1234/transition-operator/reconcilers/cnifault"
	"github.com/vitu1234/transition-operator/reconcilers/controlplane"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	registerTool(GetClusterHealth())
}

type GetClusterHealthParams struct {
	ClusterName string `json:"clusterName" description:"Name of the CAPI workload cluster to check (matches the ClusterPolicy's spec.clusterSelector.name or a target cluster name)."`
}

type GetClusterHealthResult struct {
	ClusterName  string `json:"clusterName" description:"Cluster that was checked."`
	ControlPlane string `json:"controlPlane" description:"Control-plane health: Ready, NotReady, Unreachable, or Error."`
	CNI          string `json:"cni" description:"CNI health: Ready, NotReady, Unreachable, or Error."`
	Error        string `json:"error,omitempty" description:"Set if a check could not be completed."`
}

// GetClusterHealth confirms whether a workload cluster is actually unhealthy by
// re-running the same checks transition-operator's own health monitors use
// (reconcilers/controlplane and reconcilers/cnifault), rather than trusting the
// fault-detection message alone.
func GetClusterHealth() MCPTool[GetClusterHealthParams, GetClusterHealthResult] {
	return MCPTool[GetClusterHealthParams, GetClusterHealthResult]{
		Name:        "get_cluster_health",
		Description: "Check control-plane and CNI health of a workload cluster by name, using the same checks transition-operator's own fault monitors use.",
		Handler: func(ctx context.Context, cc *mcp.ServerSession, params *mcp.CallToolParamsFor[GetClusterHealthParams]) (*mcp.CallToolResultFor[GetClusterHealthResult], error) {
			cl, err := k8sClient()
			if err != nil {
				return nil, fmt.Errorf("build k8s client: %w", err)
			}

			clusterName := params.Arguments.ClusterName
			cpStatus, cniStatus, checkErr := checkClusterHealth(ctx, cl, clusterName)
			result := GetClusterHealthResult{ClusterName: clusterName, ControlPlane: cpStatus, CNI: cniStatus}
			if checkErr != nil {
				result.Error = checkErr.Error()
			}

			return toolResult(result), nil
		},
	}
}

// checkClusterHealth re-runs the same checks transition-operator's own health
// monitors use (reconcilers/controlplane and reconcilers/cnifault) against a
// named workload cluster. Shared by get_cluster_health and
// get_transition_candidates so both agree on what "healthy" means.
func checkClusterHealth(ctx context.Context, cl client.Client, clusterName string) (controlPlane, cni string, err error) {
	capiCluster, err := capictrl.GetCapiClusterFromName(ctx, clusterName, "default", cl)
	if err != nil || capiCluster == nil {
		return "", "", fmt.Errorf("failed to resolve CAPI cluster %q: %w", clusterName, err)
	}

	clusterClient, restConfig, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil || !ready {
		return "", "", fmt.Errorf("workload cluster %q not reachable: %w", clusterName, err)
	}

	cpStatus, cpErr := controlplane.CheckControlPlaneStatus(ctx, clusterClient)
	cniStatus, cniErr := cnifault.CheckCNIStatus(ctx, clusterClient, restConfig)

	if cpErr != nil {
		err = cpErr
	} else if cniErr != nil {
		err = cniErr
	}
	return string(cpStatus), string(cniStatus), err
}

func toolResult[O any](v O) *mcp.CallToolResultFor[O] {
	return &mcp.CallToolResultFor[O]{
		Content:           []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%+v", v)}},
		StructuredContent: v,
	}
}
