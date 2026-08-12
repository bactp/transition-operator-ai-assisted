package tools

import (
	"context"
	"fmt"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func init() {
	registerTool(ProposeFailover())
}

type ProposeFailoverParams struct {
	ClusterPolicyName string `json:"clusterPolicyName" description:"Name of the ClusterPolicy to record the recommendation on."`
	Namespace         string `json:"namespace,omitempty" description:"Namespace of the ClusterPolicy. Defaults to \"default\"."`
	TargetCluster     string `json:"targetCluster" description:"Cluster to fail over to. Must be one of the ClusterPolicy's spec.targetClusterPolicy.preferClusters names."`
	Reason            string `json:"reason" description:"Concise justification for this recommendation, surfaced to the human approver."`
	FaultType         string `json:"faultType" description:"The fault category that triggered this reasoning, e.g. \"control-plane unhealthy\"."`
}

type ProposeFailoverResult struct {
	Phase   string `json:"phase" description:"Resulting ClusterPolicy phase."`
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
}

// ProposeFailover is the only write tool. It never executes a transition - it
// records a recommendation on the ClusterPolicy and moves it to
// AwaitingApproval. A human must separately patch status.phase to Approved
// before transition-operator's ClusterPolicyReconciler is allowed to proceed
// (see internal/controller/clusterpolicy_controller.go's canAutoTransition).
func ProposeFailover() MCPTool[ProposeFailoverParams, ProposeFailoverResult] {
	return MCPTool[ProposeFailoverParams, ProposeFailoverResult]{
		Name: "propose_failover",
		Description: "Record an AI-recommended failover target on a ClusterPolicy and set it to AwaitingApproval. " +
			"Does NOT execute the failover - a human must separately approve it via kubectl before transition-operator acts on it.",
		Handler: func(ctx context.Context, cc *mcp.ServerSession, params *mcp.CallToolParamsFor[ProposeFailoverParams]) (*mcp.CallToolResultFor[ProposeFailoverResult], error) {
			cl, err := k8sClient()
			if err != nil {
				return nil, fmt.Errorf("build k8s client: %w", err)
			}

			ns := params.Arguments.Namespace
			if ns == "" {
				ns = "default"
			}

			var cp transitionv1.ClusterPolicy
			key := types.NamespacedName{Name: params.Arguments.ClusterPolicyName, Namespace: ns}
			if err := cl.Get(ctx, key, &cp); err != nil {
				return nil, fmt.Errorf("get ClusterPolicy %s: %w", key, err)
			}

			// Idempotency: don't clobber a cycle that's already past the
			// recommendation stage.
			switch cp.Status.Phase {
			case transitionv1.PhaseAwaitingApproval, transitionv1.PhaseApproved, transitionv1.PhaseExecuting:
				return toolResult(ProposeFailoverResult{
					Phase:   string(cp.Status.Phase),
					Message: fmt.Sprintf("ClusterPolicy %s is already %s; not overwriting the existing recommendation.", cp.Name, cp.Status.Phase),
				}), nil
			}

			// Validate the target is actually one of the policy's configured
			// candidates - this is the "Rejected" path: an out-of-policy target
			// never reaches AwaitingApproval.
			validTarget := false
			for _, target := range cp.Spec.TargetClusterPolicy.PreferClusters {
				if target.Name == params.Arguments.TargetCluster {
					validTarget = true
					break
				}
			}
			if !validTarget {
				cp.Status.Phase = transitionv1.PhaseRejected
				cp.Status.Recommendation = &transitionv1.FailoverRecommendation{
					RecommendedTargetCluster: params.Arguments.TargetCluster,
					Reason:                   params.Arguments.Reason,
					RecommendedBy:            "kagent:failover-agent",
					FaultType:                params.Arguments.FaultType,
					LastPhaseTransitionTime:  metav1.Now(),
				}
				if err := cl.Status().Update(ctx, &cp); err != nil {
					return nil, fmt.Errorf("update ClusterPolicy %s status: %w", key, err)
				}
				recordEvent(ctx, cl, &cp, corev1.EventTypeWarning, "FailoverRejected",
					fmt.Sprintf("AI agent proposed target %q which is not in spec.targetClusterPolicy.preferClusters; rejected.", params.Arguments.TargetCluster))
				return toolResult(ProposeFailoverResult{
					Phase:   string(cp.Status.Phase),
					Target:  params.Arguments.TargetCluster,
					Message: fmt.Sprintf("Rejected: %q is not a configured preferred cluster for %s.", params.Arguments.TargetCluster, cp.Name),
				}), nil
			}

			cp.Status.Phase = transitionv1.PhaseAwaitingApproval
			cp.Status.Recommendation = &transitionv1.FailoverRecommendation{
				RecommendedTargetCluster: params.Arguments.TargetCluster,
				Reason:                   params.Arguments.Reason,
				RecommendedBy:            "kagent:failover-agent",
				FaultType:                params.Arguments.FaultType,
				LastPhaseTransitionTime:  metav1.Now(),
			}
			if err := cl.Status().Update(ctx, &cp); err != nil {
				return nil, fmt.Errorf("update ClusterPolicy %s status: %w", key, err)
			}

			recordEvent(ctx, cl, &cp, corev1.EventTypeNormal, "FailoverRecommended",
				fmt.Sprintf("AI agent recommends failover to %q: %s", params.Arguments.TargetCluster, params.Arguments.Reason))

			return toolResult(ProposeFailoverResult{
				Phase:  string(cp.Status.Phase),
				Target: params.Arguments.TargetCluster,
				Message: fmt.Sprintf(
					"Recorded. A human must run: kubectl patch clusterpolicy %s -n %s --type=merge --subresource=status -p '{\"status\":{\"phase\":\"Approved\"}}' before transition-operator will act.",
					cp.Name, cp.Namespace),
			}), nil
		},
	}
}
