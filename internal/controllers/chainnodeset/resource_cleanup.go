package chainnodeset

import (
	"context"
	"fmt"

	k8sappsv1 "k8s.io/api/apps/v1"
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
func (r *Reconciler) finalizeResources(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	signerQuiesced, err := cosmosigner.QuiesceOwner(ctx, r.Client, nodeSet, nodeSet.GetNamespace())
	if err != nil || !signerQuiesced {
		return false, err
	}

	childrenDone, err := r.quiesceAndDeleteChildren(ctx, nodeSet)
	if err != nil || !childrenDone {
		return false, err
	}
	seedDone, err := r.quiesceCosmoseed(ctx, nodeSet)
	if err != nil || !seedDone {
		return false, err
	}

	signerStateDone, err := cosmosigner.FinalizeOwner(
		ctx,
		r.Client,
		nodeSet,
		nodeSet.GetNamespace(),
		nodeSet.Spec.DeletionPolicy.GetCosmosignerState(),
	)
	if err != nil || !signerStateDone {
		return false, err
	}
	if err := r.attributeControlledLegacyKeys(ctx, nodeSet); err != nil {
		return false, err
	}

	root := resourcecleanup.RootOwnerFor(nodeSet)
	dataDone, err := resourcecleanup.FinalizeClass(
		ctx,
		r.Client,
		root,
		resourcecleanup.ClassDataVolumes,
		nodeSet.Spec.DeletionPolicy.GetDataVolumes(),
		nodeSet.GetUID(),
	)
	if err != nil {
		return false, err
	}
	keysDone, err := resourcecleanup.FinalizeClass(
		ctx,
		r.Client,
		root,
		resourcecleanup.ClassGeneratedKeys,
		nodeSet.Spec.DeletionPolicy.GetGeneratedKeys(),
		nodeSet.GetUID(),
	)
	if err != nil || !dataDone || !keysDone {
		return false, err
	}

	changed := false
	for _, finalizer := range []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer} {
		if controllerutil.ContainsFinalizer(nodeSet, finalizer) {
			controllerutil.RemoveFinalizer(nodeSet, finalizer)
			changed = true
		}
	}
	if changed {
		if err := r.Update(ctx, nodeSet); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *Reconciler) quiesceAndDeleteChildren(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	children := &appsv1.ChainNodeList{}
	if err := r.List(ctx, children, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range children.Items {
		child := &children.Items[i]
		if !metav1.IsControlledBy(child, nodeSet) {
			continue
		}
		if child.GetDeletionTimestamp().IsZero() {
			if !controllerutil.ContainsFinalizer(child, resourcecleanup.Finalizer) {
				controllerutil.AddFinalizer(child, resourcecleanup.Finalizer)
				if err := r.Update(ctx, child); err != nil {
					return false, err
				}
			}
			uid := child.GetUID()
			if err := r.Delete(ctx, child, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
			allDone = false
			continue
		}
		podDone, err := r.quiesceChildPod(ctx, child)
		if err != nil {
			return false, err
		}
		if !podDone {
			allDone = false
			continue
		}
		childDone, err := r.finalizeChildDurableResources(ctx, nodeSet, child)
		if err != nil {
			return false, err
		}
		if !childDone {
			allDone = false
			continue
		}
		remaining := &appsv1.ChainNode{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(child), remaining); err == nil {
			allDone = false
		} else if !errors.IsNotFound(err) {
			return false, err
		}
	}
	return allDone, nil
}

func (r *Reconciler) finalizeChildDurableResources(
	ctx context.Context,
	nodeSet *appsv1.ChainNodeSet,
	child *appsv1.ChainNode,
) (bool, error) {
	if err := r.attributeControlledLegacyChildResources(ctx, child); err != nil {
		return false, err
	}
	root := resourcecleanup.RootOwnerFor(child)
	dataDone, err := resourcecleanup.FinalizeClass(
		ctx,
		r.Client,
		root,
		resourcecleanup.ClassDataVolumes,
		nodeSet.Spec.DeletionPolicy.GetDataVolumes(),
		child.GetUID(),
	)
	if err != nil {
		return false, err
	}
	keysDone, err := resourcecleanup.FinalizeClass(
		ctx,
		r.Client,
		root,
		resourcecleanup.ClassGeneratedKeys,
		nodeSet.Spec.DeletionPolicy.GetGeneratedKeys(),
		child.GetUID(),
	)
	if err != nil || !dataDone || !keysDone {
		return false, err
	}
	controllerutil.RemoveFinalizer(child, resourcecleanup.Finalizer)
	if err := r.Update(ctx, child); err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) attributeControlledLegacyChildResources(ctx context.Context, child *appsv1.ChainNode) error {
	root := resourcecleanup.RootOwnerFor(child)
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(child.GetNamespace())); err != nil {
		return err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !metav1.IsControlledBy(pvc, child) || resourcecleanup.IsAttributed(pvc, root, resourcecleanup.ClassDataVolumes) {
			continue
		}
		managed, changed, err := resourcecleanup.PrepareGeneratedResource(pvc, child, r.Scheme, resourcecleanup.ClassDataVolumes, false)
		if err != nil {
			return err
		}
		if managed && changed {
			if err := r.Update(ctx, pvc); err != nil {
				return err
			}
		}
	}
	secretNames := map[string]struct{}{child.GetName(): {}}
	if child.Spec.Validator != nil {
		secretNames[child.Spec.Validator.GetAccountSecretName(child)] = struct{}{}
		secretNames[child.Spec.Validator.GetPrivKeySecretName(child)] = struct{}{}
	}
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(child.GetNamespace())); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		_, known := secretNames[secret.GetName()]
		if !known || !metav1.IsControlledBy(secret, child) || resourcecleanup.IsAttributed(secret, root, resourcecleanup.ClassGeneratedKeys) {
			continue
		}
		managed, changed, err := resourcecleanup.PrepareGeneratedResource(secret, child, r.Scheme, resourcecleanup.ClassGeneratedKeys, false)
		if err != nil {
			return err
		}
		if managed && changed {
			if err := r.Update(ctx, secret); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) attributeControlledLegacyKeys(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	knownNames := map[string]struct{}{fmt.Sprintf("%s-cosmoseed", nodeSet.GetName()): {}}
	for i := range nodeSet.Spec.Nodes {
		group := &nodeSet.Spec.Nodes[i]
		if group.Validator == nil {
			continue
		}
		for _, validator := range groupGenesisValidators(nodeSet, group.Name, group.GetInstances(), group.Validator) {
			knownNames[validator.AccountMnemonicSecret] = struct{}{}
			knownNames[validator.PrivKeySecret] = struct{}{}
		}
	}
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return err
	}
	root := resourcecleanup.RootOwnerFor(nodeSet)
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		_, known := knownNames[secret.GetName()]
		if !known || !metav1.IsControlledBy(secret, nodeSet) || resourcecleanup.IsAttributed(secret, root, resourcecleanup.ClassGeneratedKeys) {
			continue
		}
		managed, changed, err := resourcecleanup.PrepareGeneratedResource(secret, nodeSet, r.Scheme, resourcecleanup.ClassGeneratedKeys, false)
		if err != nil {
			return err
		}
		if managed && changed {
			if err := r.Update(ctx, secret); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) quiesceChildPod(ctx context.Context, child *appsv1.ChainNode) (bool, error) {
	key := client.ObjectKeyFromObject(child)
	mainPod := &corev1.Pod{}
	if err := r.Get(ctx, key, mainPod); err == nil && !metav1.IsControlledBy(mainPod, child) {
		return false, fmt.Errorf("refusing durable cleanup while pod %s/%s is not controlled by ChainNode UID %s", mainPod.GetNamespace(), mainPod.GetName(), child.GetUID())
	} else if err != nil && !errors.IsNotFound(err) {
		return false, err
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(child.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, child) {
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

func (r *Reconciler) quiesceCosmoseed(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	statefulSets := &k8sappsv1.StatefulSetList{}
	if err := r.List(ctx, statefulSets, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if !metav1.IsControlledBy(sts, nodeSet) || sts.GetName() != nodeSet.GetName()+"-seed" {
			continue
		}
		if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
			zero := int32(0)
			sts.Spec.Replicas = &zero
			if err := r.Update(ctx, sts); err != nil {
				return false, err
			}
			allDone = false
			continue
		}
		podsDone, err := r.deleteControlledPods(ctx, sts)
		if err != nil {
			return false, err
		}
		if !podsDone {
			allDone = false
			continue
		}
		if sts.GetDeletionTimestamp().IsZero() {
			uid := sts.GetUID()
			if err := r.Delete(ctx, sts, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
		remaining := &k8sappsv1.StatefulSet{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sts), remaining); err == nil {
			allDone = false
		} else if !errors.IsNotFound(err) {
			return false, err
		}
	}
	return allDone, nil
}

func (r *Reconciler) deleteControlledPods(ctx context.Context, owner client.Object) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(owner.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, owner) {
			continue
		}
		if pod.GetDeletionTimestamp().IsZero() {
			uid := pod.GetUID()
			if err := r.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
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

func (r *Reconciler) finalizeTerminatingNamespace(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	signerQuiesced, err := cosmosigner.QuiesceOwnerForNamespaceTermination(ctx, r.Client, nodeSet, nodeSet.GetNamespace())
	if err != nil || !signerQuiesced {
		return false, err
	}
	childrenDone, err := r.quiesceAndDeleteChildren(ctx, nodeSet)
	if err != nil || !childrenDone {
		return false, err
	}
	seedDone, err := r.quiesceCosmoseed(ctx, nodeSet)
	if err != nil || !seedDone {
		return false, err
	}
	if err := cosmosigner.ReleaseOwnerStateFinalizers(ctx, r.Client, nodeSet, nodeSet.GetNamespace()); err != nil {
		return false, err
	}
	changed := false
	for _, finalizer := range []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer, podDisruptionBudgetFinalizer} {
		if controllerutil.ContainsFinalizer(nodeSet, finalizer) {
			controllerutil.RemoveFinalizer(nodeSet, finalizer)
			changed = true
		}
	}
	if !changed {
		return true, nil
	}
	if err := r.Update(ctx, nodeSet); err != nil {
		return false, err
	}
	return true, nil
}
