package chainnodeset

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
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
	if err := r.attributeCosmoseedDataVolumes(ctx, nodeSet); err != nil {
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

func (r *Reconciler) attributeCosmoseedDataVolumes(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	// Deterministic names and StatefulSet managed fields also occur on pre-provisioned claims, so
	// only claims that already prove their root attribution are safe to complete here.
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return err
	}
	root := resourcecleanup.RootOwnerFor(nodeSet)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !isCosmoseedDataVolume(nodeSet, pvc) ||
			!resourcecleanup.IsAttributed(pvc, root, resourcecleanup.ClassDataVolumes) {
			continue
		}
		changed := resourcecleanup.Stamp(pvc, root, resourcecleanup.ClassDataVolumes)
		changed = resourcecleanup.StampResourceOwner(pvc, nodeSet.GetUID()) || changed
		if changed {
			if err := r.Update(ctx, pvc); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) attributeControlledLegacyCosmoseedDataVolumes(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	seed := &k8sappsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: nodeSet.GetName() + "-seed"}
	if err := r.Get(ctx, key, seed); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(seed, nodeSet) {
		return nil
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !isCosmoseedDataVolume(nodeSet, pvc) || !metav1.IsControlledBy(pvc, seed) {
			continue
		}
		root := resourcecleanup.RootOwnerFor(nodeSet)
		changed := resourcecleanup.Stamp(pvc, root, resourcecleanup.ClassDataVolumes)
		changed = resourcecleanup.StampResourceOwner(pvc, nodeSet.GetUID()) || changed
		changed = removeControllerReferenceByUID(pvc, seed.GetUID()) || changed
		if changed {
			if err := r.Update(ctx, pvc); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func removeControllerReferenceByUID(object metav1.Object, uid types.UID) bool {
	references := object.GetOwnerReferences()
	filtered := make([]metav1.OwnerReference, 0, len(references))
	changed := false
	for _, reference := range references {
		if reference.UID == uid && reference.Controller != nil && *reference.Controller {
			changed = true
			continue
		}
		filtered = append(filtered, reference)
	}
	if changed {
		object.SetOwnerReferences(filtered)
	}
	return changed
}

func isCosmoseedDataVolume(nodeSet *appsv1.ChainNodeSet, pvc *corev1.PersistentVolumeClaim) bool {
	return hasCanonicalOrdinal(pvc.GetName(), "data-"+nodeSet.GetName()+"-seed-")
}

func hasCanonicalOrdinal(name, prefix string) bool {
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == name || suffix == "" {
		return false
	}
	ordinal, err := strconv.Atoi(suffix)
	return err == nil && ordinal >= 0 && strconv.Itoa(ordinal) == suffix
}

func (r *Reconciler) quiesceAndDeleteChildren(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	children := &appsv1.ChainNodeList{}
	if err := r.List(ctx, children, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range children.Items {
		child := &children.Items[i]
		controlled := metav1.IsControlledBy(child, nodeSet)
		recordedName, recordedUID := recordedNodeSetChildIdentity(nodeSet, child)
		if !controlled && !recordedUID {
			if recordedName {
				return false, fmt.Errorf("refusing cleanup of ChainNode %s/%s UID %s because it does not match the UID recorded by ChainNodeSet %s", child.GetNamespace(), child.GetName(), child.GetUID(), nodeSet.GetName())
			}
			continue
		}
		if controller := metav1.GetControllerOf(child); controller != nil && !metav1.IsControlledBy(child, nodeSet) {
			return false, fmt.Errorf(
				"refusing cleanup of recorded ChainNode %s/%s UID %s controlled by %s UID %s",
				child.GetNamespace(), child.GetName(), child.GetUID(), controller.Name, controller.UID,
			)
		}
		// A recorded child that lost its controller reference would resolve to itself as cleanup
		// root, stamping its durable resources under the child UID where this parent can no longer
		// reach them. The recorded UID proves the relationship, so restore it before deleting.
		if !controlled {
			if err := controllerutil.SetControllerReference(nodeSet, child, r.Scheme); err != nil {
				return false, err
			}
			if err := r.Update(ctx, child); err != nil {
				return false, err
			}
			return false, nil
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
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(child.GetNamespace())); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		// Unowned Secrets at the generated names stay retained: a user-imported key is
		// indistinguishable from a generated one, so adopting on name and payload shape would place
		// user-supplied key material under .spec.deletionPolicy.generatedKeys: Delete.
		if !metav1.IsControlledBy(secret, child) ||
			resourcecleanup.IsAttributed(secret, root, resourcecleanup.ClassGeneratedKeys) ||
			!isLegacyChildGeneratedKeySecret(child, secret) {
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

func isLegacyChildGeneratedKeySecret(child *appsv1.ChainNode, secret *corev1.Secret) bool {
	knownNames := []string{child.GetName()}
	if child.Spec.Validator != nil {
		knownNames = append(knownNames,
			child.Spec.Validator.GetAccountSecretName(child),
			child.Spec.Validator.GetPrivKeySecretName(child),
		)
	}
	return resourcecleanup.IsLegacyGeneratedKeySecret(secret, knownNames...)
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
	known := make([]string, 0, len(knownNames))
	for name := range knownNames {
		known = append(known, name)
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		annotations := secret.GetAnnotations()
		if attributedUID := annotations[resourcecleanup.AnnotationRootOwnerUID]; attributedUID != "" &&
			attributedUID != string(nodeSet.GetUID()) {
			continue
		}
		if !resourcecleanup.IsLegacyGeneratedKeySecret(secret, known...) ||
			(!metav1.IsControlledBy(secret, nodeSet) && !isLegacyCosmoseedKeySecret(nodeSet, secret)) ||
			resourcecleanup.IsAttributed(secret, root, resourcecleanup.ClassGeneratedKeys) {
			continue
		}
		changed := false
		if metav1.IsControlledBy(secret, nodeSet) {
			managed, prepared, err := resourcecleanup.PrepareGeneratedResource(secret, nodeSet, r.Scheme, resourcecleanup.ClassGeneratedKeys, false)
			if err != nil {
				return err
			}
			changed = managed && prepared
		} else {
			changed = resourcecleanup.Stamp(secret, root, resourcecleanup.ClassGeneratedKeys)
			changed = resourcecleanup.StampResourceOwner(secret, nodeSet.GetUID()) || changed
		}
		if changed {
			if err := r.Update(ctx, secret); err != nil {
				return err
			}
		}
	}
	return nil
}

func isLegacyCosmoseedKeySecret(nodeSet *appsv1.ChainNodeSet, secret *corev1.Secret) bool {
	if secret.GetName() != nodeSet.GetName()+"-cosmoseed" || metav1.GetControllerOf(secret) != nil || len(secret.Data) == 0 {
		return false
	}
	prefix := nodeSet.GetName() + "-seed-"
	for name, key := range secret.Data {
		if !hasCanonicalOrdinal(name, prefix) {
			return false
		}
		if _, err := cometbft.GetNodeID(key); err != nil {
			return false
		}
	}
	return true
}

// MigrateLegacyDurableResources removes cascading ownership from verified root and child durable
// resources before deletion reconciliation is allowed to start.
func (r *Reconciler) MigrateLegacyDurableResources(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	if err := r.attributeControlledLegacyKeys(ctx, nodeSet); err != nil {
		return false, err
	}
	if err := r.attributeControlledLegacyCosmoseedDataVolumes(ctx, nodeSet); err != nil {
		return false, err
	}
	children := &appsv1.ChainNodeList{}
	if err := r.List(ctx, children, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range children.Items {
		child := &children.Items[i]
		_, recordedUID := recordedNodeSetChildIdentity(nodeSet, child)
		if !metav1.IsControlledBy(child, nodeSet) && !recordedUID {
			continue
		}
		if controller := metav1.GetControllerOf(child); controller != nil && !metav1.IsControlledBy(child, nodeSet) {
			continue
		}
		if !metav1.IsControlledBy(child, nodeSet) {
			if err := controllerutil.SetControllerReference(nodeSet, child, r.Scheme); err != nil {
				return false, err
			}
			if err := r.Update(ctx, child); err != nil {
				return false, err
			}
		}
		if err := r.attributeControlledLegacyChildResources(ctx, child); err != nil {
			return false, err
		}
	}
	return cosmosigner.ProtectRetainedStatePVCs(ctx, r.Client, nodeSet, nodeSet.GetNamespace())
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
		controlled := metav1.IsControlledBy(pod, child)
		if !controlled && !controllers.IsDeterministicChainNodePodName(pod.GetName(), child.GetName()) {
			continue
		}
		if !controlled {
			return false, fmt.Errorf("refusing durable cleanup while pod %s/%s is not controlled by ChainNode UID %s", pod.GetNamespace(), pod.GetName(), child.GetUID())
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

func (r *Reconciler) canonicalSeedPodsGone(ctx context.Context, nodeSet *appsv1.ChainNodeSet, seedName string) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if hasCanonicalOrdinal(pods.Items[i].GetName(), seedName+"-") {
			return false, nil
		}
	}
	return true, nil
}

func (r *Reconciler) quiesceCosmoseed(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	sts := &k8sappsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: nodeSet.GetName() + "-seed"}
	if err := r.Get(ctx, key, sts); err != nil {
		if errors.IsNotFound(err) {
			// An orphan-deleted StatefulSet leaves its pods running and still mounting the seed
			// claims, so a missing StatefulSet alone does not mean the seed is quiesced.
			return r.canonicalSeedPodsGone(ctx, nodeSet, key.Name)
		}
		return false, err
	}
	if !metav1.IsControlledBy(sts, nodeSet) {
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(nodeSet.GetNamespace())); err != nil {
			return false, err
		}
		podsPresent := false
		for i := range pods.Items {
			pod := &pods.Items[i]
			if metav1.IsControlledBy(pod, sts) || hasCanonicalOrdinal(pod.GetName(), sts.GetName()+"-") {
				podsPresent = true
				break
			}
		}
		quiesced := !podsPresent && ((sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0) || !sts.GetDeletionTimestamp().IsZero())
		if quiesced {
			return true, nil
		}
		ownership := "has no controller"
		if controller := metav1.GetControllerOf(sts); controller != nil {
			ownership = fmt.Sprintf("is controlled by %s %q UID %s", controller.Kind, controller.Name, controller.UID)
		}
		return false, fmt.Errorf("refusing durable cleanup while Cosmoseed StatefulSet %s/%s %s; restore its ChainNodeSet UID %s controller reference, or scale/delete it and wait for all seed pods to terminate", sts.GetNamespace(), sts.GetName(), ownership, nodeSet.GetUID())
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		zero := int32(0)
		sts.Spec.Replicas = &zero
		if err := r.Update(ctx, sts); err != nil {
			return false, err
		}
		return false, nil
	}
	podsDone, err := r.deleteControlledPods(ctx, sts)
	if err != nil || !podsDone {
		return false, err
	}
	if sts.GetDeletionTimestamp().IsZero() {
		uid := sts.GetUID()
		if err := r.Delete(ctx, sts, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	}
	remaining := &k8sappsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sts), remaining); err == nil {
		return false, nil
	} else if !errors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) deleteControlledPods(ctx context.Context, owner client.Object) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(owner.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range pods.Items {
		pod := &pods.Items[i]
		controlled := metav1.IsControlledBy(pod, owner)
		if !controlled && !hasCanonicalOrdinal(pod.GetName(), owner.GetName()+"-") {
			continue
		}
		if !controlled {
			ownership := "has no controller"
			if controller := metav1.GetControllerOf(pod); controller != nil {
				ownership = fmt.Sprintf("is controlled by %s %q UID %s", controller.Kind, controller.Name, controller.UID)
			}
			return false, fmt.Errorf("refusing to delete StatefulSet %s/%s while canonical pod %s/%s %s instead of StatefulSet UID %s; restore the expected controller reference, or delete the drifted pod and wait for it to terminate", owner.GetNamespace(), owner.GetName(), pod.GetNamespace(), pod.GetName(), ownership, owner.GetUID())
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
