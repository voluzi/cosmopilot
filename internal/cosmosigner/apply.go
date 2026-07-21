package cosmosigner

import (
	"context"
	stderrors "errors"
	"fmt"
	"reflect"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v2/internal/k8s"
)

const LifecycleDigestAnnotation = "cosmopilot.voluzi.com/cosmosigner-lifecycle-digest"

var errUnsafeRetainedState = stderrors.New("unsafe retained cosmosigner state")

// applyGuard carries deployment-time checks that depend on persisted controller state rather than
// the rendered Kubernetes object alone.
type applyGuard struct {
	RequireRetainedState  bool
	RetainedStateReplicas int32
}

// SetLifecycleDigest stamps the rendered lifecycle fingerprint onto a signer StatefulSet.
func SetLifecycleDigest(sts *appsv1.StatefulSet, digest string) {
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	sts.Annotations[LifecycleDigestAnnotation] = digest
}

// ReadLifecycleDigest returns the lifecycle fingerprint stamped on a live signer StatefulSet.
func ReadLifecycleDigest(ctx context.Context, c client.Client, namespace, name string) (string, bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	digest := sts.Annotations[LifecycleDigestAnnotation]
	return digest, digest != "", nil
}

// PreflightDeployable reports (as an error) whether the signer named `name` can be deployed by owner,
// running the SAME blocking checks its deployment performs — a name-collision refusal on EVERY object
// the deployment creates/updates (each is applied with ApplyOwned, which refuses to overwrite an object
// owned by a different controller; the one-shot import pod refuses the same way), plus the foreign/
// ambiguous and required retained raft-state PVC guards — without creating resources. ApplyOwned
// repeats both PVC checks immediately before a StatefulSet create, update, or no-op apply. Unsafe
// retained state on an existing signer is latched at zero replicas. The ChainNodeSet controller calls
// this BEFORE it retargets child validators to the remote signer, so a signer that a later apply would
// refuse does not leave a validator with neither its local key nor a deployable signer. Objects this
// owner already controls remain deployable only when their immutable shape is compatible with the
// signer resource that will reuse the name.
//
// usesImportPod must be true only when the signer actually runs the one-shot `<name>-import` pod (a
// Vault uploadGenerated signer). Software, GCP KMS and pre-provisioned Vault signers never create it, so
// checking that name for them would let an unrelated foreign pod block an otherwise-deployable signer on
// every reconcile.
// usesPubkeyPod must be true when public-key preflight runs the one-shot `<name>-pubkey` pod. Software
// and uploadGenerated signers resolve that key directly from their source Secret.
// replicas is the desired count for a first rollout and the locked Raft membership for an established
// signer. Every deterministic `<name>-<ordinal>` pod name is reserved before validators are retargeted.
func PreflightDeployable(ctx context.Context, c client.Client, owner client.Object, namespace, name string, replicas int32, usesImportPod, usesPubkeyPod, requireRetainedState bool) error {
	// Objects that need only the same-owner check. Services are handled separately because their
	// headless shape is immutable and must also match before deployment.
	named := []struct {
		kind string
		obj  client.Object
	}{
		{"ConfigMap", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}},
		{"NetworkPolicy", networkPolicyObject(namespace, name)},
	}
	if usesPubkeyPod {
		named = append(named, struct {
			kind string
			obj  client.Object
		}{"pubkey pod", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name + "-" + pubkeyJobSuffix}}})
	}
	if usesImportPod {
		named = append(named, struct {
			kind string
			obj  client.Object
		}{"import pod", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name + "-" + importJobSuffix}}})
	}
	for _, n := range named {
		if err := ensureNoForeignObject(ctx, c, owner, n.kind, n.obj); err != nil {
			return err
		}
	}
	if err := ensureHeadlessServiceDeployable(ctx, c, owner, namespace, "raft Service", name); err != nil {
		return err
	}
	if err := ensureHeadlessServiceDeployable(ctx, c, owner, namespace, "discovery Service", name+discoveryServiceSuffix); err != nil {
		return err
	}

	sts := &appsv1.StatefulSet{}
	hasStatefulSet := false
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); {
	case err == nil:
		if !metav1.IsControlledBy(sts, owner) {
			return foreignObjectErr("StatefulSet", name)
		}
		hasStatefulSet = true
		if sts.GetAnnotations()[retainedStateLostAnnotation] == "true" {
			// A latched signer stays stopped by the apply path, but preflight must continue far enough
			// to let a different-key migration persist its reset phases.
			requireRetainedState = false
		} else if statefulSetEverRolledOut(sts) {
			requireRetainedState = true
		}
		if err := ensureReplicaPodNamesAvailable(ctx, c, namespace, name, replicas, sts); err != nil {
			return err
		}
	case errors.IsNotFound(err):
		// A fresh StatefulSet cannot create a replica while any pod already holds its deterministic
		// <name>-<ordinal> name, regardless of that pod's owner.
		if err := ensureReplicaPodNamesAvailable(ctx, c, namespace, name, replicas, nil); err != nil {
			return err
		}
	default:
		return err
	}
	// ApplyOwned re-runs both PVC guards immediately before every StatefulSet apply path.
	if err := ensureNoForeignDataPVCs(ctx, c, owner, namespace, name); err != nil {
		if hasStatefulSet {
			return quiesceUnsafeRetainedState(ctx, c, sts, err)
		}
		return err
	}
	if requireRetainedState {
		err := ensureRetainedDataPVCs(ctx, c, owner, namespace, name, replicas)
		if err != nil && hasStatefulSet {
			return quiesceUnsafeRetainedState(ctx, c, sts, err)
		}
		return err
	}
	return nil
}

func ensureRetainedDataPVCs(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string, replicas int32) error {
	for ordinal := int32(0); ordinal < replicas; ordinal++ {
		claimName := fmt.Sprintf("%s-%s-%d", dataVolumeName, name, ordinal)
		pvc := &corev1.PersistentVolumeClaim{}
		switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: claimName}, pvc); {
		case errors.IsNotFound(err):
			return fmt.Errorf("%w: refusing to deploy established cosmosigner %q: retained raft-state PVC %q is missing, so slash-protection history may have been lost; restore the claim from the original volume or complete a persisted different-key state reset", errUnsafeRetainedState, name, claimName)
		case err != nil:
			return err
		case pvc.GetLabels()[labelOwnerUID] != string(owner.GetUID()):
			return fmt.Errorf("%w: refusing to deploy established cosmosigner %q: retained raft-state PVC %q is not attributed to the current owner", errUnsafeRetainedState, name, claimName)
		case !pvc.GetDeletionTimestamp().IsZero():
			return fmt.Errorf("%w: refusing to deploy established cosmosigner %q: retained raft-state PVC %q is terminating, so recreating the StatefulSet could replace slash-protection history with an empty volume", errUnsafeRetainedState, name, claimName)
		case pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "":
			return fmt.Errorf("%w: refusing to deploy established cosmosigner %q: retained raft-state PVC %q is not bound to a persistent volume, so its slash-protection history cannot be trusted", errUnsafeRetainedState, name, claimName)
		case !controllerutil.ContainsFinalizer(pvc, RetainedStateFinalizer):
			return fmt.Errorf("%w: refusing to deploy established cosmosigner %q: retained raft-state PVC %q is not protected against deletion", errUnsafeRetainedState, name, claimName)
		}
	}
	return nil
}

func ensureHeadlessServiceDeployable(ctx context.Context, c client.Client, owner client.Object, namespace, kind, name string) error {
	svc := &corev1.Service{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, svc); {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !metav1.IsControlledBy(svc, owner):
		return foreignObjectErr(kind, name)
	case svc.Spec.ClusterIP != corev1.ClusterIPNone:
		return fmt.Errorf("cosmosigner %s %q is not headless and cannot be converted in place; delete the stale owned Service before deploying the signer", kind, name)
	default:
		return nil
	}
}

func ensureReplicaPodNamesAvailable(ctx context.Context, c client.Client, namespace, name string, replicas int32, sts *appsv1.StatefulSet) error {
	for ordinal := int32(0); ordinal < replicas; ordinal++ {
		podName := fmt.Sprintf("%s-%d", name, ordinal)
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, pod); errors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if sts == nil || !metav1.IsControlledBy(pod, sts) {
			return fmt.Errorf("cosmosigner replica pod %q already exists; refusing to create or scale a StatefulSet that cannot start all replicas", podName)
		}
	}
	return nil
}

// ensureNoForeignObject errors when obj exists and is controlled by a different owner (a same-name
// collision an apply would refuse). A missing object is fine.
func ensureNoForeignObject(ctx context.Context, c client.Client, owner client.Object, kind string, obj client.Object) error {
	switch err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !metav1.IsControlledBy(obj, owner):
		return foreignObjectErr(kind, obj.GetName())
	default:
		return nil
	}
}

func foreignObjectErr(kind, name string) error {
	return fmt.Errorf("cosmosigner %s %q is managed by another owner; refusing to deploy over it — rename the ChainNode/ChainNodeSet to avoid the name collision", kind, name)
}

// ApplyOwned creates or updates a cosmosigner-managed object owned by owner. It refuses to
// overwrite an object owned by a different controller (a same-name CR collision), preserves
// StatefulSet fields Kubernetes forbids updating, rechecks requested PVC safety invariants at apply
// time, and skips the write entirely when nothing changed (patch-equality), so steady-state
// reconciles do not churn resourceVersions.
func ApplyOwned(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, obj client.Object, guards ...applyGuard) error {
	if len(guards) > 1 {
		return fmt.Errorf("at most one cosmosigner apply guard may be supplied")
	}
	if _, isStatefulSet := obj.(*appsv1.StatefulSet); isStatefulSet && len(guards) != 1 {
		return fmt.Errorf("a cosmosigner StatefulSet requires an explicit apply guard")
	}
	guard := applyGuard{}
	if len(guards) == 1 {
		guard = guards[0]
	}
	if policy, ok := obj.(*networkingv1.NetworkPolicy); ok {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(policy)
		if err != nil {
			return fmt.Errorf("converting NetworkPolicy %q: %w", policy.GetName(), err)
		}
		converted := &unstructured.Unstructured{Object: raw}
		converted.SetGroupVersionKind(schema.GroupVersionKind{Group: networkingv1.GroupName, Version: "v1", Kind: "NetworkPolicy"})
		obj = converted
	}
	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return err
	}
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object is not a client.Object")
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if errors.IsNotFound(err) {
		if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(obj); err != nil {
			return err
		}
		if sts, isSts := obj.(*appsv1.StatefulSet); isSts {
			if err := ensureStatefulSetPVCsForApply(ctx, c, owner, sts, nil, guard); err != nil {
				return err
			}
		}
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(existing, owner) {
		return fmt.Errorf("cosmosigner resource %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", obj.GetName())
	}
	k8s.PreserveImmutableStatefulSetFields(obj, existing)
	if sts, isSts := obj.(*appsv1.StatefulSet); isSts {
		live, ok := existing.(*appsv1.StatefulSet)
		if !ok {
			return fmt.Errorf("existing cosmosigner StatefulSet is not an apps/v1 StatefulSet")
		}
		if err := ensureStatefulSetPVCsForApply(ctx, c, owner, sts, live, guard); err != nil {
			return quiesceUnsafeRetainedState(ctx, c, live, err)
		}
		if live.GetAnnotations()[EverRolledOutAnnotation] == "true" {
			if sts.Annotations == nil {
				sts.Annotations = map[string]string{}
			}
			sts.Annotations[EverRolledOutAnnotation] = "true"
		}
	}

	// Skip the write when nothing changed, so steady-state reconciles do not bump
	// resourceVersions (which would re-trigger the owner watch every cycle). The live object is
	// copied back into obj either way, so callers can read current status (e.g. ReadyReplicas).
	patchResult, err := patch.DefaultPatchMaker.Calculate(existing, obj, patch.IgnoreStatusFields())
	if err != nil {
		return err
	}
	if patchResult.IsEmpty() && reflect.DeepEqual(existing.GetLabels(), obj.GetLabels()) {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(existing).Elem())
		return nil
	}

	if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(obj); err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

func ensureStatefulSetPVCsForApply(ctx context.Context, c client.Client, owner client.Object, sts, live *appsv1.StatefulSet, guard applyGuard) error {
	if live != nil && live.GetAnnotations()[retainedStateLostAnnotation] == "true" {
		return fmt.Errorf("%w: refusing to deploy cosmosigner %q because retained slash-protection state was previously lost or became unsafe; complete a persisted different-key state reset", errUnsafeRetainedState, sts.GetName())
	}
	if live != nil && ptr.Deref(sts.Spec.Replicas, 1) > ptr.Deref(live.Spec.Replicas, 1) &&
		!statefulSetPVCTemplateRetainsState(live) {
		return fmt.Errorf("%w: refusing to scale cosmosigner %q because its live volume claim template does not protect newly created raft-state PVCs; complete the break-before-make StatefulSet migration first", errUnsafeRetainedState, sts.GetName())
	}
	if err := ensureNoForeignDataPVCs(ctx, c, owner, sts.GetNamespace(), sts.GetName()); err != nil {
		return err
	}
	if live != nil && statefulSetEverRolledOut(live) {
		guard.RequireRetainedState = true
		if guard.RetainedStateReplicas <= 0 {
			guard.RetainedStateReplicas = ptr.Deref(live.Spec.Replicas, 1)
		}
	}
	if !guard.RequireRetainedState {
		return nil
	}
	if guard.RetainedStateReplicas <= 0 {
		return fmt.Errorf("%w: refusing to deploy established cosmosigner %q: retained raft-state replica lock is missing or invalid", errUnsafeRetainedState, sts.GetName())
	}
	return ensureRetainedDataPVCs(ctx, c, owner, sts.GetNamespace(), sts.GetName(), guard.RetainedStateReplicas)
}

func statefulSetPVCTemplateRetainsState(sts *appsv1.StatefulSet) bool {
	for i := range sts.Spec.VolumeClaimTemplates {
		claim := &sts.Spec.VolumeClaimTemplates[i]
		if claim.GetName() == dataVolumeName {
			return controllerutil.ContainsFinalizer(claim, RetainedStateFinalizer)
		}
	}
	return false
}

func quiesceUnsafeRetainedState(ctx context.Context, c client.Client, sts *appsv1.StatefulSet, cause error) error {
	if !stderrors.Is(cause, errUnsafeRetainedState) {
		return cause
	}
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	alreadyLatched := sts.Annotations[retainedStateLostAnnotation] == "true"
	alreadyStopped := sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0
	if alreadyLatched && alreadyStopped {
		return cause
	}
	sts.Annotations[retainedStateLostAnnotation] = "true"
	sts.Spec.Replicas = new(int32)
	if err := c.Update(ctx, sts); err != nil {
		return fmt.Errorf("%w; additionally failed to quiesce unsafe StatefulSet %q: %v", cause, sts.GetName(), err)
	}
	return fmt.Errorf("%w; StatefulSet %q was latched at zero replicas", cause, sts.GetName())
}

// ScaleDown scales an existing signer StatefulSet owned by owner to zero replicas and reports
// whether the signer is fully quiesced (no pods left). Used while a key re-import is pending: an
// already-running signer must not keep signing with the previously imported key while the target
// is being re-keyed. The scale-down is asynchronous, so callers must treat quiesced=false as
// "retry later" and NOT proceed with the import (nor re-apply the StatefulSet at full replicas,
// which would cancel the scale-down). A missing or foreign-owned StatefulSet counts as quiesced.
func ScaleDown(ctx context.Context, c client.Client, owner client.Object, namespace, name string) (quiesced bool, err error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return SignerPodsGone(ctx, c, namespace, name)
		}
		return false, err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return true, nil
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		zero := int32(0)
		sts.Spec.Replicas = &zero
		if err := c.Update(ctx, sts); err != nil {
			return false, err
		}
		// Just requested; pods are still terminating.
		return false, nil
	}
	// Already requested zero: first require the StatefulSet controller to have observed the
	// scale-down generation and report no replicas. That status is necessary but not sufficient:
	// terminating pods may still exist after the count reaches zero, and starting another signer in
	// that window can double-sign. The direct pod list below includes terminating pods.
	if sts.Status.ObservedGeneration < sts.Generation || sts.Status.Replicas != 0 {
		return false, nil
	}
	return SignerPodsGone(ctx, c, namespace, name)
}

// SignerPodsGone directly lists pods and reports whether no signer replica remains. It deliberately
// checks both immutable signer labels and deterministic StatefulSet replica names, so a terminating
// pod still blocks even if its labels were edited. Pods from any owner block reuse of the signer
// name; ownership does not make concurrent signing or a pod-name collision safe.
func SignerPodsGone(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.GetLabels()[labelAppName] == appNameCosmosigner && pod.GetLabels()[labelInstance] == name {
			return false, nil
		}
		if isStatefulSetReplicaPodName(pod.GetName(), name) {
			return false, nil
		}
	}
	return true, nil
}

// retainStatefulSetPVCs enables and observes PVC retention before a migration can scale or delete the
// signer StatefulSet. A missing StatefulSet is ready; pod absence is checked by the calling phase.
func retainStatefulSetPVCs(ctx context.Context, c client.Client, owner client.Object, namespace, name string) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return false, foreignObjectErr("StatefulSet", name)
	}

	retention := sts.Spec.PersistentVolumeClaimRetentionPolicy
	if retention == nil ||
		retention.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType ||
		retention.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		}
		if err := c.Update(ctx, sts); err != nil {
			return false, err
		}
		return false, nil
	}
	return sts.Status.ObservedGeneration >= sts.Generation, nil
}

// DeleteStatefulSet removes only the signer StatefulSet, retaining its PVCs for a same-key
// migration. It first enables and observes PVC retention for both scale-down and deletion, then
// drives the StatefulSet to zero and directly confirms that every signer pod is gone. Completion is
// reported only after the StatefulSet is absent and a second pod listing is empty, so callers cannot
// recreate the signer in the asynchronous deletion window.
func DeleteStatefulSet(ctx context.Context, c client.Client, owner client.Object, namespace, name string) (deleted bool, err error) {
	retained, err := retainStatefulSetPVCs(ctx, c, owner, namespace, name)
	if err != nil || !retained {
		return false, err
	}

	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return SignerPodsGone(ctx, c, namespace, name)
		}
		return false, err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return false, foreignObjectErr("StatefulSet", name)
	}

	quiesced, err := ScaleDown(ctx, c, owner, namespace, name)
	if err != nil || !quiesced {
		return false, err
	}
	if err := c.Delete(ctx, sts); err != nil && !errors.IsNotFound(err) {
		return false, err
	}

	remaining := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, remaining); err == nil {
		return false, nil
	} else if !errors.IsNotFound(err) {
		return false, err
	}
	return SignerPodsGone(ctx, c, namespace, name)
}

// DeleteDiscoveryService removes the owned target-discovery Service and confirms it is absent. A
// ChainNodeSet migration uses this while the signer is down so stale endpoints cannot reconnect the
// recreated signer to the previous target group.
func DeleteDiscoveryService(ctx context.Context, c client.Client, owner client.Object, namespace, name string) (bool, error) {
	service := &corev1.Service{}
	key := client.ObjectKey{Namespace: namespace, Name: name + discoveryServiceSuffix}
	if err := c.Get(ctx, key, service); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !metav1.IsControlledBy(service, owner) {
		return false, foreignObjectErr("discovery Service", service.Name)
	}
	if err := c.Delete(ctx, service); err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	remaining := &corev1.Service{}
	if err := c.Get(ctx, key, remaining); err == nil {
		return false, nil
	} else if !errors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// DiscoveryEndpointsGone reports whether the deleted discovery Service has no legacy Endpoints or
// EndpointSlices left. Waiting for both prevents a recreated same-name Service from briefly exposing
// stale target IPs to the new signer.
func DiscoveryEndpointsGone(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	serviceName := name + discoveryServiceSuffix
	endpoints := &corev1.Endpoints{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: serviceName}, endpoints); err == nil {
		return false, nil
	} else if !errors.IsNotFound(err) {
		return false, err
	}
	slices := &discoveryv1.EndpointSliceList{}
	if err := c.List(ctx, slices, client.InNamespace(namespace), client.MatchingLabels{
		discoveryv1.LabelServiceName: serviceName,
	}); err != nil {
		return false, err
	}
	return len(slices.Items) == 0, nil
}

// IsRolledOut reports whether the signer StatefulSet's CURRENT generation is fully deployed: the
// controller has observed it, and every desired replica is both updated to the current revision and
// ready. Gating on this (rather than bare ReadyReplicas) prevents treating readiness left over from
// a previous revision as success for a pending change.
func IsRolledOut(ctx context.Context, c client.Client, namespace, name string, desiredReplicas int32) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return statefulSetRolledOut(sts, desiredReplicas), nil
}

func statefulSetRolledOut(sts *appsv1.StatefulSet, desiredReplicas int32) bool {
	return desiredReplicas > 0 && sts.Status.ObservedGeneration == sts.Generation &&
		sts.Status.UpdatedReplicas == desiredReplicas && sts.Status.ReadyReplicas == desiredReplicas
}

func statefulSetEverRolledOut(sts *appsv1.StatefulSet) bool {
	return sts.GetAnnotations()[EverRolledOutAnnotation] == "true" ||
		statefulSetRolledOut(sts, ptr.Deref(sts.Spec.Replicas, 1))
}

// MarkEverRolledOut persists monotonic live evidence before root status records the applied
// lifecycle. A later status restore therefore cannot reinterpret an established signer as fresh.
func MarkEverRolledOut(ctx context.Context, c client.Client, namespace, name string, desiredReplicas int32) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !statefulSetRolledOut(sts, desiredReplicas) {
		return false, nil
	}
	if sts.GetAnnotations()[EverRolledOutAnnotation] == "true" {
		return true, nil
	}
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	sts.Annotations[EverRolledOutAnnotation] = "true"
	if err := c.Update(ctx, sts); err != nil {
		return false, err
	}
	return true, nil
}

// Undeploy removes the managed signer resources for the given base name, deleting only objects the
// owner controls. Each named resource is deleted only when this owner controls it, so a same-name
// resource owned by a different CR (a "<name>-signer" collision) is skipped rather than
// short-circuiting the whole teardown. Owner-scoped PVC cleanup always runs — even when a foreign
// StatefulSet holds the name — so this owner's lingering raft-state claims are never stranded (which
// would deadlock the IsTornDown gate waiting on them).
func Undeploy(ctx context.Context, c client.Client, owner client.Object, namespace, name string) error {
	sts := &appsv1.StatefulSet{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); {
	case err == nil && metav1.IsControlledBy(sts, owner):
		deleted, err := DeleteStatefulSet(ctx, c, owner, namespace, name)
		if err != nil || !deleted {
			return err
		}
	case err == nil:
		gone, err := SignerPodsGone(ctx, c, namespace, name)
		if err != nil || !gone {
			return err
		}
	case errors.IsNotFound(err):
		gone, err := SignerPodsGone(ctx, c, namespace, name)
		if err != nil || !gone {
			return err
		}
	default:
		return err
	}

	objects := []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + importJobSuffix, Namespace: namespace}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + pubkeyJobSuffix, Namespace: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + discoveryServiceSuffix, Namespace: namespace}},
		networkPolicyObject(namespace, name),
	}
	for _, obj := range objects {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, owner) {
			continue
		}
		if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	// StatefulSet PVCs are not garbage-collected with the StatefulSet. DeletePVCs filters on the
	// owner-UID label, so only this owner's claims are removed even when a foreign same-name signer
	// exists.
	return DeletePVCs(ctx, c, owner, namespace, name)
}

func networkPolicyObject(namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: networkingv1.GroupName, Version: "v1", Kind: "NetworkPolicy"})
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

// IsTornDown reports whether the signer resources owned by owner are fully gone. Deletion is
// asynchronous — Undeploy only requests removal — so callers that must not act on a half-deleted
// cluster (e.g. clearing the recorded raft membership before allowing a re-add) gate on this.
//
// Only resources owned by owner count. A StatefulSet with the same name owned by ANOTHER CR (a name
// collision) is not ours to wait on — Undeploy skips it too — so it does not block. The per-pod PVCs
// are matched by the owner-UID label, so OUR lingering raft-state claims still gate the clear (even
// when a foreign same-name StatefulSet exists), while the foreign CR's identically-named claims do
// not. Any deterministic same-name signer pod blocks regardless of owner, because concurrent signing
// and pod-name reuse are unsafe. A claim already marked for deletion but held by a finalizer still
// counts as present, since a fresh StatefulSet could bind it and inherit stale raft state.
func IsTornDown(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) (bool, error) {
	for _, suffix := range []string{importJobSuffix, pubkeyJobSuffix} {
		jobPod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-" + suffix}, jobPod); err == nil {
			if metav1.IsControlledBy(jobPod, owner) {
				return false, nil
			}
		} else if !errors.IsNotFound(err) {
			return false, err
		}
	}
	policy := networkPolicyObject(namespace, name)
	if err := c.Get(ctx, client.ObjectKeyFromObject(policy), policy); err == nil {
		if metav1.IsControlledBy(policy, owner) {
			return false, nil
		}
	} else if !errors.IsNotFound(err) {
		return false, err
	}

	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err == nil {
		// A same-name StatefulSet exists. Only OUR StatefulSet blocks teardown completion; a foreign
		// one falls through to the owner-scoped PVC check below.
		if metav1.IsControlledBy(sts, owner) {
			return false, nil
		}
	} else if !errors.IsNotFound(err) {
		return false, err
	}
	podsGone, err := SignerPodsGone(ctx, c, namespace, name)
	if err != nil || !podsGone {
		return false, err
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pvcs.Items {
		// Our own claims block until deleted. AMBIGUOUS legacy claims (no owner-UID label — cannot be
		// attributed to any owner without a race) also block: treating them as gone would let a
		// recreated signer bind stale raft state with unknown membership. They are never deleted
		// automatically; the operator resolves them by deleting or labeling the claim.
		if isOwnedStatefulSetDataPVC(&pvcs.Items[i], owner, name) || isAmbiguousLegacyDataPVC(&pvcs.Items[i], name) {
			return false, nil
		}
	}
	return true, nil
}
