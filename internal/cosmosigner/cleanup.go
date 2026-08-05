package cosmosigner

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cosmopilotv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

// IsOwnedSignerStatefulSet reports whether sts is a cosmosigner deployment controlled by owner.
// The pod-template identity is preferred; immutable generated PVC-template identity provides a
// recovery path when mutable pod-template labels drift.
func IsOwnedSignerStatefulSet(sts *appsv1.StatefulSet, owner metav1.Object) bool {
	if !metav1.IsControlledBy(sts, owner) {
		return false
	}
	if sts.Spec.Template.Labels[labelAppName] == appNameCosmosigner &&
		sts.Spec.Template.Labels[labelInstance] == sts.GetName() {
		return true
	}
	for i := range sts.Spec.VolumeClaimTemplates {
		claim := &sts.Spec.VolumeClaimTemplates[i]
		if claim.GetName() == dataVolumeName &&
			claim.GetLabels()[labelAppName] == appNameCosmosigner &&
			claim.GetLabels()[labelInstance] == sts.GetName() &&
			claim.GetLabels()[labelOwnerUID] == string(owner.GetUID()) {
			return true
		}
	}
	return false
}

func statefulSetPVCTemplateReadyForCleanup(sts *appsv1.StatefulSet, root resourcecleanup.RootOwner) bool {
	for i := range sts.Spec.VolumeClaimTemplates {
		claim := &sts.Spec.VolumeClaimTemplates[i]
		if claim.GetName() == dataVolumeName {
			return controllerutil.ContainsFinalizer(claim, RetainedStateFinalizer) &&
				resourcecleanup.IsAttributed(claim, root, resourcecleanup.ClassCosmosignerState)
		}
	}
	return false
}

func isAttributedSignerStatefulSet(sts *appsv1.StatefulSet, owner client.Object) bool {
	root := resourcecleanup.RootOwnerFor(owner)
	for i := range sts.Spec.VolumeClaimTemplates {
		claim := &sts.Spec.VolumeClaimTemplates[i]
		if claim.GetName() != dataVolumeName {
			continue
		}
		labelAttributed := claim.GetLabels()[labelAppName] == appNameCosmosigner &&
			claim.GetLabels()[labelInstance] == sts.GetName() &&
			claim.GetLabels()[labelOwnerUID] == string(owner.GetUID())
		stableAttributed := resourcecleanup.IsAttributed(claim, root, resourcecleanup.ClassCosmosignerState)
		if labelAttributed || stableAttributed {
			return true
		}
	}
	return false
}

// DeletePVCs deletes the per-pod raft-state PVCs of a signer instance owned by owner. StatefulSet
// PVCs are not garbage-collected with the StatefulSet, so they are cleaned up explicitly on teardown;
// the retained-state finalizer is released only in this controlled path.
// A claim is only deleted when its name matches the exact StatefulSet per-pod claim pattern
// `<dataVolumeName>-<name>-<ordinal>` and its owner-UID label matches owner, so edited selector labels
// cannot hide a name-bound claim and a same-name signer's claim owned by another CR is never deleted.
func DeletePVCs(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range pvcs.Items {
		if !isOwnedStatefulSetDataPVC(&pvcs.Items[i], owner, name) {
			continue
		}
		pvc := &pvcs.Items[i]
		if controllerutil.ContainsFinalizer(pvc, RetainedStateFinalizer) {
			controllerutil.RemoveFinalizer(pvc, RetainedStateFinalizer)
			if err := c.Update(ctx, pvc); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return err
			}
		}
		if !pvc.GetDeletionTimestamp().IsZero() {
			continue
		}
		uid := pvc.GetUID()
		if err := c.Delete(ctx, pvc, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ProtectRetainedStatePVCs ensures verified live claims carry the deletion guard that an immutable
// volumeClaimTemplate cannot add in place. Owned StatefulSets whose template cannot protect new
// claims are retired through the normal retain-and-quiesce path before reconciliation resumes.
// Unknown or unsafe claims remain untouched for strict preflight to reject.
func ProtectRetainedStatePVCs(ctx context.Context, c client.Client, owner client.Object, namespace string) (bool, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	changed := false
	root := resourcecleanup.RootOwnerFor(owner)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		name := pvc.GetLabels()[labelInstance]
		if pvc.GetLabels()[labelAppName] != appNameCosmosigner ||
			pvc.GetLabels()[labelOwnerUID] != string(owner.GetUID()) ||
			!isStatefulSetDataPVC(pvc.GetName(), name) ||
			!pvc.GetDeletionTimestamp().IsZero() {
			continue
		}
		metadataChanged := resourcecleanup.Stamp(pvc, root, resourcecleanup.ClassCosmosignerState)
		_, preparedChanged, err := resourcecleanup.PrepareGeneratedResource(
			pvc, owner, nil, resourcecleanup.ClassCosmosignerState, false,
		)
		if err != nil {
			return changed, err
		}
		metadataChanged = preparedChanged || metadataChanged
		if pvc.Status.Phase == corev1.ClaimBound && pvc.Spec.VolumeName != "" &&
			!controllerutil.ContainsFinalizer(pvc, RetainedStateFinalizer) {
			controllerutil.AddFinalizer(pvc, RetainedStateFinalizer)
			metadataChanged = true
		}
		if metadataChanged {
			if err := c.Update(ctx, pvc); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	statefulSets := &appsv1.StatefulSetList{}
	if err := c.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		return changed, err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if !IsOwnedSignerStatefulSet(sts, owner) || statefulSetPVCTemplateReadyForCleanup(sts, root) {
			continue
		}
		_, err := DeleteStatefulSet(ctx, c, owner, namespace, sts.GetName())
		return true, err
	}
	return changed, nil
}

// HasOwnedSignerState reports whether the root still has a managed signer StatefulSet or an
// attributable raft-state claim, including status-loss and spec-removal recovery cases.
func HasOwnedSignerState(ctx context.Context, c client.Client, owner metav1.Object, namespace string) (bool, error) {
	statefulSets := &appsv1.StatefulSetList{}
	if err := c.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range statefulSets.Items {
		if IsOwnedSignerStatefulSet(&statefulSets.Items[i], owner) {
			return true, nil
		}
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.GetLabels()[labelAppName] == appNameCosmosigner &&
			pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID()) {
			return true, nil
		}
	}
	return false, nil
}

// FinalizeOwner advances root-CR deletion in fail-safe order: attribute legacy claims, quiesce signer
// pods, remove their StatefulSets, then apply the explicit state policy. It reports complete only
// after deleted claims are absent or retained claims have released the signer-only finalizer.
func FinalizeOwner(
	ctx context.Context,
	c client.Client,
	owner client.Object,
	namespace string,
	policy cosmopilotv1.DeletionPolicyType,
) (bool, error) {
	attributed, err := attributeOwnedStatePVCs(ctx, c, owner, namespace)
	if err != nil || attributed {
		return false, err
	}
	quiesced, err := QuiesceOwner(ctx, c, owner, namespace)
	if err != nil || !quiesced {
		return false, err
	}
	return FinalizeState(ctx, c, owner, namespace, policy)
}

// QuiesceOwner removes every managed signer StatefulSet only after its pods have stopped. Raft-state
// PVCs remain protected for the caller to process after all other signing workloads are also absent.
func QuiesceOwner(ctx context.Context, c client.Client, owner client.Object, namespace string) (bool, error) {
	jobPodsDone, err := deleteOwnedKeyJobPods(ctx, c, owner, namespace)
	if err != nil || !jobPodsDone {
		return false, err
	}
	statefulSets := &appsv1.StatefulSetList{}
	if err := c.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if !IsOwnedSignerStatefulSet(sts, owner) {
			if isAttributedSignerStatefulSet(sts, owner) {
				controller := metav1.GetControllerOf(sts)
				ownership := "has no controller"
				if controller != nil {
					ownership = fmt.Sprintf("is controlled by %s %q UID %s", controller.Kind, controller.Name, controller.UID)
				}
				return false, fmt.Errorf("refusing Cosmosigner state cleanup while attributed StatefulSet %s/%s %s; restore its owner UID %s controller reference or quiesce and remove it", sts.GetNamespace(), sts.GetName(), ownership, owner.GetUID())
			}
			continue
		}
		if !sts.GetDeletionTimestamp().IsZero() {
			return false, nil
		}
		deleted, err := DeleteStatefulSet(ctx, c, owner, namespace, sts.GetName())
		if err != nil || !deleted {
			return false, err
		}
	}
	return true, nil
}

// QuiesceOwnerForNamespaceTermination directly stops signer pods before deleting their StatefulSets.
// It does not wait for StatefulSet scale or retention-policy observations, which may no longer
// progress after namespace termination begins; durable-state finalizers remain for the caller.
func QuiesceOwnerForNamespaceTermination(ctx context.Context, c client.Client, owner client.Object, namespace string) (bool, error) {
	jobPodsDone, err := deleteOwnedKeyJobPods(ctx, c, owner, namespace)
	if err != nil || !jobPodsDone {
		return false, err
	}

	ownedNames := map[string]types.UID{}
	statefulSets := &appsv1.StatefulSetList{}
	if err := c.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if IsOwnedSignerStatefulSet(sts, owner) {
			ownedNames[sts.GetName()] = sts.GetUID()
		}
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		name := pvc.GetLabels()[labelInstance]
		if pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID()) && isStatefulSetDataPVC(pvc.GetName(), name) {
			if _, exists := ownedNames[name]; !exists {
				ownedNames[name] = ""
			}
		}
	}

	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	allPodsGone := true
	for i := range pods.Items {
		pod := &pods.Items[i]
		instance := pod.GetLabels()[labelInstance]
		stsUID, owned := ownedNames[instance]
		if !owned {
			for name, uid := range ownedNames {
				if uid == "" {
					continue
				}
				if isStatefulSetReplicaPodName(pod.GetName(), name) {
					instance, stsUID, owned = name, uid, true
					break
				}
			}
			if !owned {
				continue
			}
		}
		if pod.GetLabels()[labelAppName] != appNameCosmosigner && !isStatefulSetReplicaPodName(pod.GetName(), instance) {
			continue
		}
		controller := metav1.GetControllerOf(pod)
		if stsUID == "" || controller == nil || controller.Kind != "StatefulSet" || controller.Name != instance || controller.UID != stsUID {
			allPodsGone = false
			continue
		}
		if pod.GetDeletionTimestamp().IsZero() {
			uid := pod.GetUID()
			if err := c.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
		remaining := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), remaining); err == nil {
			allPodsGone = false
		} else if !errors.IsNotFound(err) {
			return false, err
		}
	}
	if !allPodsGone {
		return false, nil
	}

	allStatefulSetsGone := true
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if !IsOwnedSignerStatefulSet(sts, owner) {
			continue
		}
		if sts.GetDeletionTimestamp().IsZero() {
			uid := sts.GetUID()
			if err := c.Delete(ctx, sts, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
		remaining := &appsv1.StatefulSet{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(sts), remaining); err == nil {
			allStatefulSetsGone = false
		} else if !errors.IsNotFound(err) {
			return false, err
		}
	}
	return allStatefulSetsGone, nil
}

func deleteOwnedKeyJobPods(ctx context.Context, c client.Client, owner client.Object, namespace string) (bool, error) {
	expectedSignerNames, err := signerNamesAttributedToOwner(ctx, c, owner, namespace)
	if err != nil {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	allDone := true
	for i := range pods.Items {
		pod := &pods.Items[i]
		signerName, keyJob := keyJobSignerName(pod.GetName())
		if !keyJob {
			continue
		}
		controlled := metav1.IsControlledBy(pod, owner)
		if !controlled {
			if _, expected := expectedSignerNames[signerName]; !expected {
				continue
			}
			controller := metav1.GetControllerOf(pod)
			ownership := "has no controller"
			if controller != nil {
				ownership = fmt.Sprintf("is controlled by %s %q UID %s", controller.Kind, controller.Name, controller.UID)
			}
			return false, fmt.Errorf("refusing Cosmosigner cleanup while deterministic key-job pod %s/%s %s; restore its owner UID %s controller reference or quiesce and remove it", pod.GetNamespace(), pod.GetName(), ownership, owner.GetUID())
		}
		if pod.GetDeletionTimestamp().IsZero() {
			uid := pod.GetUID()
			if err := c.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
				return false, err
			}
		}
		remaining := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), remaining); err == nil {
			allDone = false
		} else if !errors.IsNotFound(err) {
			return false, err
		}
	}
	return allDone, nil
}

func signerNamesAttributedToOwner(ctx context.Context, c client.Client, owner client.Object, namespace string) (map[string]struct{}, error) {
	names := map[string]struct{}{}
	statefulSets := &appsv1.StatefulSetList{}
	if err := c.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if IsOwnedSignerStatefulSet(sts, owner) || isAttributedSignerStatefulSet(sts, owner) {
			names[sts.GetName()] = struct{}{}
		}
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	root := resourcecleanup.RootOwnerFor(owner)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		labelOwned := pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID())
		attributed := resourcecleanup.IsAttributed(pvc, root, resourcecleanup.ClassCosmosignerState) &&
			pvc.GetAnnotations()[resourcecleanup.AnnotationResourceOwnerUID] == string(owner.GetUID())
		if !labelOwned && !attributed {
			continue
		}
		if name, ok := statefulSetNameFromDataPVC(pvc.GetName()); ok {
			names[name] = struct{}{}
		}
	}
	return names, nil
}

func isKeyJobPodName(name string) bool {
	_, ok := keyJobSignerName(name)
	return ok
}

func keyJobSignerName(name string) (string, bool) {
	for _, suffix := range []string{"-" + importJobSuffix, "-" + pubkeyJobSuffix} {
		if strings.HasSuffix(name, suffix) {
			signerName := strings.TrimSuffix(name, suffix)
			if signerName != "" {
				return signerName, true
			}
		}
	}
	return "", false
}

// FinalizeState applies the configured policy after every signer workload is absent.
func FinalizeState(
	ctx context.Context,
	c client.Client,
	owner client.Object,
	namespace string,
	policy cosmopilotv1.DeletionPolicyType,
) (bool, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	root := resourcecleanup.RootOwnerFor(owner)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		name := pvc.GetLabels()[labelInstance]
		labelOwned := pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID()) && isStatefulSetDataPVC(pvc.GetName(), name)
		attributed := resourcecleanup.IsAttributed(pvc, root, resourcecleanup.ClassCosmosignerState) &&
			pvc.GetAnnotations()[resourcecleanup.AnnotationResourceOwnerUID] == string(owner.GetUID())
		if !labelOwned && !attributed {
			continue
		}
		if !labelOwned {
			var ok bool
			name, ok = statefulSetNameFromDataPVC(pvc.GetName())
			if !ok {
				return false, fmt.Errorf("refusing Cosmosigner state cleanup for attributed PVC %s/%s with an invalid StatefulSet claim name", pvc.GetNamespace(), pvc.GetName())
			}
		}
		gone, err := SignerPodsGone(ctx, c, namespace, name)
		if err != nil || !gone {
			return false, err
		}
	}

	return resourcecleanup.FinalizeClass(
		ctx,
		c,
		root,
		resourcecleanup.ClassCosmosignerState,
		policy,
		owner.GetUID(),
		RetainedStateFinalizer,
	)
}

// attributeOwnedStatePVCs upgrades claims carrying the immutable signer owner-UID label. Unlike
// deterministic-name recovery, that label distinguishes same-name roots and is sufficient evidence
// to classify pre-deletion-policy Cosmosigner state.
func attributeOwnedStatePVCs(ctx context.Context, c client.Client, owner client.Object, namespace string) (bool, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	root := resourcecleanup.RootOwnerFor(owner)
	changed := false
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.GetLabels()[labelOwnerUID] != string(owner.GetUID()) {
			continue
		}
		if _, ok := statefulSetNameFromDataPVC(pvc.GetName()); !ok {
			continue
		}
		metadataChanged := resourcecleanup.Stamp(pvc, root, resourcecleanup.ClassCosmosignerState)
		metadataChanged = resourcecleanup.StampResourceOwner(pvc, owner.GetUID()) || metadataChanged
		if metadataChanged {
			if err := c.Update(ctx, pvc); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// ReleaseOwnerStateFinalizers removes only the Cosmosigner retained-state guard from claims proven
// by either the signer owner-UID/name labels or stable root/class attribution. Namespace deletion
// already quiesces every managed signer workload, so keeping this operator finalizer would only
// deadlock namespace termination when mutable labels have drifted.
func ReleaseOwnerStateFinalizers(ctx context.Context, c client.Client, owner client.Object, namespace string) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return err
	}
	root := resourcecleanup.RootOwnerFor(owner)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		name := pvc.GetLabels()[labelInstance]
		ownerLabelMatches := pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID())
		labelOwned := ownerLabelMatches && isStatefulSetDataPVC(pvc.GetName(), name)
		if ownerLabelMatches && !labelOwned {
			_, labelOwned = statefulSetNameFromDataPVC(pvc.GetName())
		}
		attributed := resourcecleanup.IsAttributed(pvc, root, resourcecleanup.ClassCosmosignerState) &&
			pvc.GetAnnotations()[resourcecleanup.AnnotationResourceOwnerUID] == string(owner.GetUID())
		if (!labelOwned && !attributed) || !controllerutil.ContainsFinalizer(pvc, RetainedStateFinalizer) {
			continue
		}
		controllerutil.RemoveFinalizer(pvc, RetainedStateFinalizer)
		if err := c.Update(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// OwnedPVCsGone reports whether all raft-state claims attributable to owner are absent. Claims
// still terminating under a finalizer remain present and keep a different-key migration blocked.
func OwnedPVCsGone(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) (bool, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pvcs.Items {
		if isOwnedStatefulSetDataPVC(&pvcs.Items[i], owner, name) {
			return false, nil
		}
	}
	return true, nil
}

// isOwnedStatefulSetDataPVC reports whether pvc is a per-pod raft-state claim of the signer named
// `name` attributable to owner: its name matches the StatefulSet per-pod pattern and its owner-UID
// label equals owner's UID. Matching is STRICT — a claim without a matching label is never deleted,
// regardless of any point-in-time StatefulSet ownership read (all such reads are racy: the
// StatefulSet can be replaced between the check and the delete). Ambiguous unlabeled claims instead
// BLOCK teardown completion (see isAmbiguousLegacyDataPVC), so no signer ever binds them silently.
func isOwnedStatefulSetDataPVC(pvc *corev1.PersistentVolumeClaim, owner metav1.Object, name string) bool {
	if !isStatefulSetDataPVC(pvc.GetName(), name) {
		return false
	}
	return pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID())
}

// isAmbiguousLegacyDataPVC reports whether pvc is a per-pod raft-state claim of the signer named
// `name` that carries NO owner-UID label. Such claims predate the owner label (an existing
// StatefulSet keeps its original volumeClaimTemplates, since Kubernetes forbids updating them) and
// cannot be attributed to any owner without a race, so they are never deleted NOR treated as gone:
// they block IsTornDown until the operator resolves them (delete the claim, or label it with the
// owning CR's UID). This keeps a recreated signer from silently binding stale raft state whose
// membership is unknown. In practice such claims only exist on pre-release deployments of this
// feature, so the block is a safety net rather than an operational path.
func isAmbiguousLegacyDataPVC(pvc *corev1.PersistentVolumeClaim, name string) bool {
	if !isStatefulSetDataPVC(pvc.GetName(), name) {
		return false
	}
	_, labeled := pvc.GetLabels()[labelOwnerUID]
	return !labeled
}

// ensureNoForeignDataPVCs fails when any per-pod raft-state claim of the signer named `name` exists
// that is NOT attributable to owner — a FOREIGN claim (different owner-UID label, e.g. left behind by
// a deleted CR recreated under the same name with a new UID) or an ambiguous claim without the label.
// Called before creating or updating a StatefulSet, which would otherwise silently bind those claims
// and inherit raft membership/double-sign-protection state from a different owner.
//
// The list is deliberately NOT label-scoped: the StatefulSet controller binds claims purely by NAME
// (`<dataVolumeName>-<sts>-<ordinal>`), so a claim whose labels were stripped or edited would evade a
// label selector yet still be re-bound. Every name-matching claim in the namespace is checked.
func ensureNoForeignDataPVCs(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !isStatefulSetDataPVC(pvc.GetName(), name) {
			continue
		}
		if pvc.GetLabels()[labelOwnerUID] != string(owner.GetUID()) {
			return fmt.Errorf("%w: refusing to deploy cosmosigner %q: raft-state PVC %q belongs to a different owner (a previous same-name signer) and would be silently re-bound with its stale raft state — delete the claim, or label it with this owner's UID to adopt it", errUnsafeRetainedState, name, pvc.GetName())
		}
	}
	return nil
}

// isStatefulSetDataPVC reports whether pvcName is exactly `<dataVolumeName>-<stsName>-<ordinal>`,
// the name the StatefulSet controller gives the signer's per-pod state claims.
func isStatefulSetDataPVC(pvcName, stsName string) bool {
	_, ok := statefulSetDataPVCOrdinal(pvcName, stsName)
	return ok
}

// statefulSetDataPVCOrdinal parses the pod ordinal from a `<dataVolumeName>-<stsName>-<ordinal>` claim
// name, returning ok=false for any name that is not exactly that shape. It requires a canonical
// non-negative ordinal (no sign, no leading zeros) — exactly what the StatefulSet controller produces —
// so names like "data-<name>--1" or "data-<name>-007" never match.
func statefulSetDataPVCOrdinal(pvcName, stsName string) (int, bool) {
	prefix := dataVolumeName + "-" + stsName + "-"
	if !strings.HasPrefix(pvcName, prefix) {
		return 0, false
	}
	ordinal := strings.TrimPrefix(pvcName, prefix)
	if ordinal == "" {
		return 0, false
	}
	n, err := strconv.Atoi(ordinal)
	if err != nil || n < 0 || strconv.Itoa(n) != ordinal {
		return 0, false
	}
	return n, true
}

func statefulSetNameFromDataPVC(pvcName string) (string, bool) {
	prefix := dataVolumeName + "-"
	if !strings.HasPrefix(pvcName, prefix) {
		return "", false
	}
	nameAndOrdinal := strings.TrimPrefix(pvcName, prefix)
	separator := strings.LastIndex(nameAndOrdinal, "-")
	if separator <= 0 {
		return "", false
	}
	name := nameAndOrdinal[:separator]
	if !isStatefulSetDataPVC(pvcName, name) {
		return "", false
	}
	return name, true
}

func isStatefulSetReplicaPodName(podName, stsName string) bool {
	prefix := stsName + "-"
	if !strings.HasPrefix(podName, prefix) {
		return false
	}
	ordinal := strings.TrimPrefix(podName, prefix)
	n, err := strconv.Atoi(ordinal)
	return err == nil && n >= 0 && strconv.Itoa(n) == ordinal
}
