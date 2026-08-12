package tools

import (
	"context"
	"fmt"
	"log"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// recordEvent creates a plain corev1.Event on a ClusterPolicy - this is the
// phase-1 "notification" that a human watching `kubectl describe clusterpolicy`
// sees when the agent proposes or rejects a failover. Deliberately a direct
// Create rather than a record.EventRecorder/broadcaster: this process is
// short-lived per tool call, so there is no long-running broadcaster to own.
func recordEvent(ctx context.Context, cl client.Client, cp *transitionv1.ClusterPolicy, eventType, reason, message string) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-", cp.Name, reason),
			Namespace:    cp.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:            "ClusterPolicy",
			APIVersion:      transitionv1.GroupVersion.String(),
			Name:            cp.Name,
			Namespace:       cp.Namespace,
			UID:             cp.UID,
			ResourceVersion: cp.ResourceVersion,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
		Source:         corev1.EventSource{Component: "failover-agent-mcp"},
	}
	if err := cl.Create(ctx, event); err != nil {
		log.Printf("failed to record event %s on ClusterPolicy %s: %v", reason, cp.Name, err)
	}
}
