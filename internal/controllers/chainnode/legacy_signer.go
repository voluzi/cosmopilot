package chainnode

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func (r *Reconciler) refuseLegacyTmKMS(ctx context.Context, chainNode *appsv1.ChainNode) error {
	pod, err := r.getChainNodePod(ctx, chainNode)
	if err != nil {
		return err
	}
	legacy := false
	if pod != nil {
		for _, container := range pod.Spec.Containers {
			if container.Name == "tmkms" {
				legacy = true
				break
			}
		}
	}
	config := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.Namespace, Name: chainNode.Name + "-tmkms"}, config); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	} else if metav1.IsControlledBy(config, chainNode) {
		legacy = true
	}
	if !legacy {
		return nil
	}
	err = fmt.Errorf("tmKMS validator %s/%s cannot be reconciled by Cosmopilot 5; migrate to cosmosigner on Cosmopilot 4.x before upgrading", chainNode.Namespace, chainNode.Name)
	if r.recorder != nil {
		r.recorder.Event(chainNode, corev1.EventTypeWarning, appsv1.ReasonInvalid, err.Error())
	}
	return err
}
