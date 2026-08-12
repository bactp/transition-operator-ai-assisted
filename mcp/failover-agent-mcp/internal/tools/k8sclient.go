package tools

import (
	"sync"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	k8sClientOnce sync.Once
	k8sClientInst client.Client
	k8sClientErr  error
)

// k8sClient lazily builds the in-cluster controller-runtime client shared by
// all tools. It reuses transition-operator's own scheme helper
// (reconcilers/capi.GetDefaultKubeScheme) plus the transition-operator CRD
// types, so it can read ClusterPolicy/Checkpoint/NodeHealth objects and the
// CAPI Cluster/Machine/kubeconfig-Secret objects needed to reach into workload
// clusters - the same scheme shape the operator's own manager uses.
func k8sClient() (client.Client, error) {
	k8sClientOnce.Do(func() {
		scheme := capictrl.GetDefaultKubeScheme()
		if err := transitionv1.AddToScheme(scheme); err != nil {
			k8sClientErr = err
			return
		}
		if err := capiv1beta1.AddToScheme(scheme); err != nil {
			k8sClientErr = err
			return
		}
		cfg, err := ctrl.GetConfig()
		if err != nil {
			k8sClientErr = err
			return
		}
		k8sClientInst, k8sClientErr = client.New(cfg, client.Options{Scheme: scheme})
	})
	return k8sClientInst, k8sClientErr
}
