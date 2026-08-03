package chainnode

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

// finalizeResources stops every signing workload before applying durable-resource policy. Consensus
// key reservations are deliberately outside this workflow; issue #86 owns their safe release.
func (r *Reconciler) finalizeResources(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	quiesced, err := r.quiesceNodePod(ctx, chainNode)
	if err != nil || !quiesced {
		return false, err
	}

	signerDone, err := cosmosigner.FinalizeOwner(
		ctx,
		r.Client,
		chainNode,
		chainNode.GetNamespace(),
		chainNode.Spec.DeletionPolicy.GetCosmosignerState(),
	)
	if err != nil || !signerDone {
		return false, err
	}

	root := resourcecleanup.RootOwnerFor(chainNode)
	dataDone, err := resourcecleanup.FinalizeClass(
		ctx,
		r.Client,
		root,
		resourcecleanup.ClassDataVolumes,
		chainNode.Spec.DeletionPolicy.GetDataVolumes(),
		chainNode.GetUID(),
	)
	if err != nil {
		return false, err
	}
	keysDone, err := resourcecleanup.FinalizeClass(
		ctx,
		r.Client,
		root,
		resourcecleanup.ClassGeneratedKeys,
		chainNode.Spec.DeletionPolicy.GetGeneratedKeys(),
		chainNode.GetUID(),
	)
	if err != nil || !dataDone || !keysDone {
		return false, err
	}

	changed := false
	for _, finalizer := range []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer} {
		if controllerutil.ContainsFinalizer(chainNode, finalizer) {
			controllerutil.RemoveFinalizer(chainNode, finalizer)
			changed = true
		}
	}
	if changed {
		if err := r.Update(ctx, chainNode); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *Reconciler) quiesceNodePod(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	key := client.ObjectKeyFromObject(chainNode)
	mainPod := &corev1.Pod{}
	if err := r.Get(ctx, key, mainPod); err == nil && !metav1.IsControlledBy(mainPod, chainNode) {
		return false, fmt.Errorf("refusing durable cleanup while pod %s/%s is not controlled by ChainNode UID %s", mainPod.GetNamespace(), mainPod.GetName(), chainNode.GetUID())
	} else if err != nil && !errors.IsNotFound(err) {
		return false, err
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(chainNode.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, chainNode) {
			continue
		}
		if !pod.GetDeletionTimestamp().IsZero() {
			allDone = false
			continue
		}
		uid := pod.GetUID()
		if err := r.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
		remaining := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(pod), remaining); err == nil {
			allDone = false
		} else if !errors.IsNotFound(err) {
			return false, err
		}
	}
	return allDone, nil
}

func (r *Reconciler) namespaceTerminating(ctx context.Context, namespace string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return !ns.GetDeletionTimestamp().IsZero(), nil
}

func (r *Reconciler) finalizeTerminatingNamespace(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	nodeQuiesced, err := r.quiesceNodePod(ctx, chainNode)
	if err != nil || !nodeQuiesced {
		return false, err
	}
	signerQuiesced, err := cosmosigner.QuiesceOwnerForNamespaceTermination(ctx, r.Client, chainNode, chainNode.GetNamespace())
	if err != nil || !signerQuiesced {
		return false, err
	}
	if err := cosmosigner.ReleaseOwnerStateFinalizers(ctx, r.Client, chainNode, chainNode.GetNamespace()); err != nil {
		return false, err
	}
	changed := false
	for _, finalizer := range []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer} {
		if controllerutil.ContainsFinalizer(chainNode, finalizer) {
			controllerutil.RemoveFinalizer(chainNode, finalizer)
			changed = true
		}
	}
	if !changed {
		return true, nil
	}
	if err := r.Update(ctx, chainNode); err != nil {
		return false, err
	}
	return true, nil
}
