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
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

// finalizeResources stops every signing workload before applying durable-resource policy. Consensus
// key reservations are deliberately outside this workflow; issue #86 owns their safe release.
func (r *Reconciler) finalizeResources(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	deletionPolicy, err := r.effectiveDeletionPolicy(ctx, chainNode)
	if err != nil {
		return false, err
	}
	if err := r.refuseOrphanedRecordedChild(ctx, chainNode); err != nil {
		return false, err
	}
	quiesced, err := r.quiesceNodePod(ctx, chainNode)
	if err != nil || !quiesced {
		return false, err
	}

	signerDone, err := cosmosigner.FinalizeOwner(
		ctx,
		r.Client,
		chainNode,
		chainNode.GetNamespace(),
		deletionPolicy.GetCosmosignerState(),
	)
	if err != nil || !signerDone {
		return false, err
	}
	if err := r.attributeControlledLegacyDataVolumes(ctx, chainNode); err != nil {
		return false, err
	}
	if err := r.attributeControlledLegacyKeys(ctx, chainNode); err != nil {
		return false, err
	}

	root := resourcecleanup.RootOwnerFor(chainNode)
	dataDone, err := resourcecleanup.FinalizeClass(
		ctx,
		r.Client,
		root,
		resourcecleanup.ClassDataVolumes,
		deletionPolicy.GetDataVolumes(),
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
		deletionPolicy.GetGeneratedKeys(),
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

// refuseOrphanedRecordedChild blocks standalone finalization of a generated child that lost its
// ChainNodeSet controller reference. Such a child resolves to itself as cleanup root and would stamp
// its durable resources under its own UID, where the parent can no longer reach them during
// scale-down or root cleanup. A recorded name and UID proves the parent relationship, so this fails
// closed rather than guessing; restoring the reference lets cleanup proceed under the parent root.
func (r *Reconciler) refuseOrphanedRecordedChild(ctx context.Context, chainNode *appsv1.ChainNode) error {
	if metav1.GetControllerOf(chainNode) != nil {
		return nil
	}
	nodeSets := &appsv1.ChainNodeSetList{}
	if err := r.List(ctx, nodeSets, client.InNamespace(chainNode.GetNamespace())); err != nil {
		return err
	}
	for i := range nodeSets.Items {
		nodeSet := &nodeSets.Items[i]
		if !recordsChildIdentity(nodeSet, chainNode) {
			continue
		}
		return fmt.Errorf(
			"refusing standalone durable cleanup of ChainNode %s/%s UID %s recorded by ChainNodeSet %s UID %s; restore its controller reference so its resources are finalized under the ChainNodeSet root",
			chainNode.GetNamespace(), chainNode.GetName(), chainNode.GetUID(), nodeSet.GetName(), nodeSet.GetUID(),
		)
	}
	return nil
}

func recordsChildIdentity(nodeSet *appsv1.ChainNodeSet, child *appsv1.ChainNode) bool {
	for _, status := range nodeSet.Status.Nodes {
		if status.Name == child.GetName() && status.UID != "" && status.UID == child.GetUID() {
			return true
		}
	}
	for _, status := range nodeSet.Status.Validators {
		if status.Name == child.GetName() && status.UID != "" && status.UID == child.GetUID() {
			return true
		}
	}
	return false
}

func (r *Reconciler) effectiveDeletionPolicy(ctx context.Context, chainNode *appsv1.ChainNode) (*appsv1.DeletionPolicy, error) {
	controller := metav1.GetControllerOf(chainNode)
	if controller == nil || controller.APIVersion != appsv1.GroupVersion.String() || controller.Kind != "ChainNodeSet" {
		return chainNode.Spec.DeletionPolicy, nil
	}
	nodeSet := &appsv1.ChainNodeSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: controller.Name}, nodeSet); err != nil {
		if errors.IsNotFound(err) {
			return chainNode.Spec.DeletionPolicy, nil
		}
		return nil, err
	}
	if nodeSet.GetUID() == controller.UID {
		return nodeSet.Spec.DeletionPolicy, nil
	}
	return chainNode.Spec.DeletionPolicy, nil
}

func (r *Reconciler) attributeControlledLegacyKeys(ctx context.Context, chainNode *appsv1.ChainNode) error {
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(chainNode.GetNamespace())); err != nil {
		return err
	}
	root := resourcecleanup.RootOwnerFor(chainNode)
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if !metav1.IsControlledBy(secret, chainNode) ||
			resourcecleanup.IsAttributed(secret, root, resourcecleanup.ClassGeneratedKeys) ||
			!isLegacyGeneratedKeySecret(chainNode, secret) {
			continue
		}
		managed, changed, err := resourcecleanup.PrepareGeneratedResource(
			secret, chainNode, r.Scheme, resourcecleanup.ClassGeneratedKeys, false,
		)
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

func isLegacyGeneratedKeySecret(chainNode *appsv1.ChainNode, secret *corev1.Secret) bool {
	knownNames := []string{chainNode.GetName()}
	if chainNode.Spec.Validator != nil {
		knownNames = append(knownNames,
			chainNode.Spec.Validator.GetAccountSecretName(chainNode),
			chainNode.Spec.Validator.GetPrivKeySecretName(chainNode),
		)
	}
	return resourcecleanup.IsLegacyGeneratedKeySecret(secret, knownNames...)
}

func (r *Reconciler) attributeControlledLegacyDataVolumes(ctx context.Context, chainNode *appsv1.ChainNode) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(chainNode.GetNamespace())); err != nil {
		return err
	}
	root := resourcecleanup.RootOwnerFor(chainNode)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if resourcecleanup.IsAttributed(pvc, root, resourcecleanup.ClassDataVolumes) ||
			!metav1.IsControlledBy(pvc, chainNode) {
			continue
		}
		managed, changed, err := resourcecleanup.PrepareGeneratedResource(pvc, chainNode, r.Scheme, resourcecleanup.ClassDataVolumes, false)
		if err != nil {
			return err
		}
		if managed && changed {
			if err := r.Update(ctx, pvc); err != nil {
				return err
			}
		}
	}
	return nil
}

// MigrateLegacyDurableResources removes cascading root ownership from verified pre-upgrade durable
// resources before deletion reconciliation is allowed to start.
func (r *Reconciler) MigrateLegacyDurableResources(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if err := r.migrateExistingValidatorSecrets(ctx, chainNode); err != nil {
		return false, err
	}
	if err := r.attributeControlledLegacyDataVolumes(ctx, chainNode); err != nil {
		return false, err
	}
	if err := r.attributeControlledLegacyKeys(ctx, chainNode); err != nil {
		return false, err
	}
	return cosmosigner.ProtectRetainedStatePVCs(ctx, r.Client, chainNode, chainNode.GetNamespace())
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
		controlled := metav1.IsControlledBy(pod, chainNode)
		if !controlled && !controllers.IsDeterministicChainNodePodName(pod.GetName(), chainNode.GetName()) {
			continue
		}
		if !controlled {
			return false, fmt.Errorf("refusing durable cleanup while pod %s/%s is not controlled by ChainNode UID %s", pod.GetNamespace(), pod.GetName(), chainNode.GetUID())
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
