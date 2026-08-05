package cosmosigner

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cosmopilotv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestProtectRetainedStatePVCsUpgradesOnlyVerifiedClaims(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	bound := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace, Labels: pvcOwnerLabels(name, owner.UID),
	}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "volume-0"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	pending := bound.DeepCopy()
	pending.Name = dataVolumeName + "-" + name + "-1"
	pending.Spec.VolumeName = ""
	pending.Status.Phase = corev1.ClaimPending
	foreign := bound.DeepCopy()
	foreign.Name = dataVolumeName + "-" + name + "-2"
	foreign.Labels = pvcOwnerLabels(name, "other-uid")
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(bound, pending, foreign).Build()

	changed, err := ProtectRetainedStatePVCs(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a verified legacy claim must be protected")
	}
	for _, tc := range []struct {
		name           string
		wantFinalizer  bool
		wantAttributed bool
	}{{bound.Name, true, true}, {pending.Name, false, true}, {foreign.Name, false, false}} {
		fresh := &corev1.PersistentVolumeClaim{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: tc.name}, fresh); err != nil {
			t.Fatal(err)
		}
		if got := controllerutil.ContainsFinalizer(fresh, RetainedStateFinalizer); got != tc.wantFinalizer {
			t.Fatalf("claim %q protected = %v, want %v", tc.name, got, tc.wantFinalizer)
		}
		if got := resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(owner), resourcecleanup.ClassCosmosignerState); got != tc.wantAttributed {
			t.Fatalf("claim %q attributed = %v, want %v", tc.name, got, tc.wantAttributed)
		}
		if tc.wantAttributed && fresh.Annotations[resourcecleanup.AnnotationResourceOwnerUID] != string(owner.UID) {
			t.Fatalf("claim %q resource owner UID = %q, want %q", tc.name, fresh.Annotations[resourcecleanup.AnnotationResourceOwnerUID], owner.UID)
		}
	}
}

func TestProtectRetainedStatePVCsRetiresLegacyTemplate(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Generation: 1,
			OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &zero,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
				Name: dataVolumeName, Labels: pvcOwnerLabels(name, owner.UID),
			}}},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 0},
	}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	pending, err := ProtectRetainedStatePVCs(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("a live legacy PVC template must keep owner preparation pending")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy-template StatefulSet must be retired before reconciliation resumes, got %v", err)
	}
}

func TestProtectRetainedStatePVCsRetiresTemplateMissingRootAttribution(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	zero := int32(0)
	claim := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName, Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, Generation: 1, OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{
		Replicas:                             &zero,
		PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType, WhenScaled: appsv1.RetainPersistentVolumeClaimRetentionPolicyType},
		Template:                             corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}}, VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim},
	}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 0}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	pending, err := ProtectRetainedStatePVCs(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("an unattributed immutable template must be retired before scaling can resume")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unattributed template StatefulSet must be retired, got %v", err)
	}
}

func TestQuiesceOwnerWaitsForAlreadyDeletingStatefulSet(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	now := metav1.Now()
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, UID: "sts-uid", DeletionTimestamp: &now, Finalizers: []string{"hold"},
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}}}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	done, err := QuiesceOwner(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("an already-deleting StatefulSet must be observed absent before cleanup advances")
	}
}

func TestQuiesceOwnerRecognizesSignerWhenTemplateLabelsDrift(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: "sts-uid",
			OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &zero,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				labelAppName: "edited", labelInstance: "edited",
			}}},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
				Name: dataVolumeName, Labels: pvcOwnerLabels(name, owner.UID),
			}}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	done := false
	for attempts := 0; attempts < 4 && !done; attempts++ {
		var err error
		done, err = QuiesceOwner(context.Background(), c, owner, namespace)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("label-drifted owned signer did not quiesce")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("label-drifted owned signer must be deleted, got %v", err)
	}
}

func TestQuiesceOwnerFailsClosedOnAttributedSignerWithDriftedController(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: namespace, UID: types.UID("foreign-uid")}}
	claim := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName, Labels: pvcOwnerLabels(name, owner.UID),
	}}
	resourcecleanup.Stamp(&claim, resourcecleanup.RootOwnerFor(owner), resourcecleanup.ClassCosmosignerState)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, UID: "sts-uid",
		OwnerReferences: []metav1.OwnerReference{{UID: foreign.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim}}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	done, err := QuiesceOwner(context.Background(), c, owner, namespace)
	if err == nil {
		t.Fatal("attributed signer with a drifted controller must fail closed")
	}
	if done {
		t.Fatal("unsafe signer ownership must not report quiesced")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("drifted signer StatefulSet was modified or deleted: %v", err)
	}
}

func TestNamespaceTerminationDoesNotDeleteForeignSameInstancePod(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "owned-sts", OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}}}, Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}}}}
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, UID: "foreign-pod", Labels: InstanceLabels(name), OwnerReferences: []metav1.OwnerReference{{UID: "foreign-sts", Controller: boolPointer(true)}}}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts, foreign).Build()

	done, err := QuiesceOwnerForNamespaceTermination(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("a same-instance pod not controlled by the owned StatefulSet must block cleanup")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.Pod{}); err != nil {
		t.Fatalf("foreign signer pod was deleted: %v", err)
	}
}

func TestNamespaceTerminationRecognizesSignerPodWhenLabelsDrift(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, UID: "owned-sts",
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: namespace, UID: "pod-uid",
		Labels: map[string]string{labelAppName: "edited", labelInstance: "edited"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "StatefulSet", Name: name, UID: sts.UID, Controller: boolPointer(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts, pod).Build()

	done, err := QuiesceOwnerForNamespaceTermination(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("label-drifted signer pod prevented namespace termination cleanup from converging")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("label-drifted signer pod must be absent, got %v", err)
	}
}

func TestNamespaceTerminationIgnoresSameNamePodForOrphanedStatePVC(t *testing.T) {
	const namespace, name = "default", "orphaned-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: namespace, UID: "foreign-pod",
		Labels: map[string]string{labelAppName: "unrelated", labelInstance: "unrelated"},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc, foreign).Build()

	done, err := QuiesceOwnerForNamespaceTermination(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("an unrelated same-name pod must not block cleanup for an orphaned state PVC")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.Pod{}); err != nil {
		t.Fatalf("unrelated same-name pod was deleted: %v", err)
	}
}

func TestNamespaceTerminationBlocksOrphanPodStillMountingRetainedState(t *testing.T) {
	const namespace, name = "default", "orphaned-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	claimName := dataVolumeName + "-" + name + "-0"
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: claimName, Namespace: namespace,
		Labels: map[string]string{labelOwnerUID: string(owner.UID)}, Finalizers: []string{RetainedStateFinalizer},
	}}
	// Labels and controller reference are both gone, so only the mounted claim ties this pod to the
	// owner's signing state.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, UID: "pod-uid"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: dataVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
			},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc, pod).Build()

	done, err := QuiesceOwnerForNamespaceTermination(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("a pod still mounting the retained state claim must block state-finalizer release")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("unverified signer pod must remain untouched: %v", err)
	}
}

func TestNamespaceTerminationBlocksLabelStrippedSignerPodDerivedFromStatePVC(t *testing.T) {
	const namespace, name = "default", "orphaned-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: map[string]string{labelOwnerUID: string(owner.UID)}, Finalizers: []string{RetainedStateFinalizer},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: namespace, UID: "pod-uid",
		Labels: map[string]string{labelAppName: "edited", labelInstance: "edited"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "StatefulSet", Name: name, UID: "missing-sts-uid", Controller: boolPointer(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc, pod).Build()

	done, err := QuiesceOwnerForNamespaceTermination(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("a PVC-attributed signer pod must block state-finalizer release when its StatefulSet is absent")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("unverified signer pod must remain untouched: %v", err)
	}
}

func TestNamespaceTerminationBlocksSignerPodWhenStatePVCInstanceLabelDrifts(t *testing.T) {
	const namespace, name = "default", "orphaned-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: map[string]string{
			labelOwnerUID: string(owner.UID),
			labelInstance: "edited-instance",
		}, Finalizers: []string{RetainedStateFinalizer},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: namespace, UID: "pod-uid",
		Labels: map[string]string{labelAppName: "edited", labelInstance: "edited"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "StatefulSet", Name: name, UID: "missing-sts-uid", Controller: boolPointer(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc, pod).Build()

	done, err := QuiesceOwnerForNamespaceTermination(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("the canonical PVC name must override a drifted instance label and block finalizer release")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("unverified signer pod must remain untouched: %v", err)
	}
}

func TestFinalizeOwnerWaitsForSignerThenDeletesRetainedClaims(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{
		Replicas: &zero,
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}},
	}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 0, Replicas: 0}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts, pvc).Build()

	done, err := FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyDelete)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("finalization must observe StatefulSet deletion before releasing claims")
	}
	for attempts := 0; attempts < 4 && !done; attempts++ {
		done, err = FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyDelete)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("owner finalization did not remove retained signer state")
	}
}

func TestFinalizeOwnerRetainsStateAfterQuiescingSigner(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, UID: "sts-uid", OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{
		Replicas: &zero,
		PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}},
	}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 0, Replicas: 0}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace, UID: "pvc-uid",
		Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts, pvc).Build()

	done := false
	for attempts := 0; attempts < 5 && !done; attempts++ {
		var err error
		done, err = FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyRetain)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("owner finalization did not converge")
	}
	retained := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), retained); err != nil {
		t.Fatalf("retained state PVC was deleted: %v", err)
	}
	if controllerutil.ContainsFinalizer(retained, RetainedStateFinalizer) {
		t.Fatal("retained state PVC kept the signer-only deletion guard after quiescence")
	}
	if !resourcecleanup.IsAttributed(retained, resourcecleanup.RootOwnerFor(owner), resourcecleanup.ClassCosmosignerState) {
		t.Fatalf("retained state PVC lost root attribution: %v", retained.Annotations)
	}
}

func TestFinalizeOwnerDeletesOwnedKeyJobPods(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	jobPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-" + importJobSuffix, Namespace: namespace, UID: "job-uid",
		Labels:          InstanceLabels(name),
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(jobPod).Build()

	done, err := FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyRetain)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("owner finalization did not converge after deleting its key job pod")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(jobPod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned key job pod must be absent before durable cleanup, got %v", err)
	}
}

func TestFinalizeOwnerBlocksOnOrphanedKeyJobPodForOwnedSigner(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, UID: "sts-uid",
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}}}}
	jobPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-" + importJobSuffix, Namespace: namespace, UID: "job-uid",
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts, jobPod).Build()

	done, err := FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyDelete)
	if err == nil {
		t.Fatal("an orphaned deterministic signer key-job pod must fail closed")
	}
	if done {
		t.Fatal("cleanup must not complete while an orphaned signer key-job pod is live")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(jobPod), &corev1.Pod{}); err != nil {
		t.Fatalf("orphaned key-job pod was modified or deleted: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("signer StatefulSet was torn down before orphaned key-job remediation: %v", err)
	}
}

func TestFinalizeOwnerDeletesOwnedKeyJobPodWhenLabelsDrift(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	jobPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-" + pubkeyJobSuffix, Namespace: namespace, UID: "job-uid",
		Labels:          map[string]string{labelAppName: "edited", labelInstance: "edited"},
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(jobPod).Build()

	done, err := FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyRetain)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("owner finalization did not converge after deleting its label-drifted key job pod")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(jobPod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned key job pod must be absent before durable cleanup despite label drift, got %v", err)
	}
}

func TestQuiesceOwnerForNamespaceTerminationDoesNotWaitForScaleObservation(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	replicas := int32(3)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, UID: "sts-uid",
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{
		Replicas: &replicas,
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: namespace, UID: "pod-uid", Labels: InstanceLabels(name),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: name, UID: sts.UID, Controller: boolPointer(true)}},
	}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace, UID: "pvc-uid",
		Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts, pod, pvc).Build()

	done, err := QuiesceOwnerForNamespaceTermination(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("namespace termination quiescence did not converge with immediate fake-client deletion")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("signer pod must be absent, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("signer StatefulSet must be absent, got %v", err)
	}
	retained := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), retained); err != nil {
		t.Fatalf("state PVC must remain protected until the caller releases it: %v", err)
	}
	if !controllerutil.ContainsFinalizer(retained, RetainedStateFinalizer) {
		t.Fatal("namespace quiescence released durable state before the caller's cleanup phase")
	}
}

func TestHasOwnedSignerStateIgnoresNonSignerStatefulSet(t *testing.T) {
	const namespace = "default"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "application", Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	found, err := HasOwnedSignerState(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a non-signer StatefulSet must not arm Cosmosigner owner cleanup")
	}
}

func TestFinalizeOwnerIgnoresNonSignerStatefulSet(t *testing.T) {
	const namespace = "default"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "application", Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	done, err := FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyDelete)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("non-signer StatefulSets must not delay Cosmosigner owner cleanup")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("non-signer StatefulSet was modified or deleted: %v", err)
	}
}

func TestFinalizeOwnerFindsRetainedPVCWithoutAppLabel(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: map[string]string{
			labelInstance: name,
			labelOwnerUID: string(owner.UID),
		},
		Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc).Build()

	done := false
	for attempts := 0; attempts < 4 && !done; attempts++ {
		var err error
		done, err = FinalizeOwner(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyDelete)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("owner finalization did not remove the retained claim with a stripped app label")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retained claim with a stripped app label must be deleted, got %v", err)
	}
}

func TestFinalizeStateGatesAttributedPVCOnPodAfterLabelDrift(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace, UID: "pvc-uid",
		Labels:     map[string]string{labelInstance: "edited", labelOwnerUID: "edited"},
		Finalizers: []string{RetainedStateFinalizer},
	}}
	resourcecleanup.Stamp(pvc, resourcecleanup.RootOwnerFor(owner), resourcecleanup.ClassCosmosignerState)
	resourcecleanup.StampResourceOwner(pvc, owner.UID)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, UID: "pod-uid"}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc, pod).Build()

	done, err := FinalizeState(context.Background(), c, owner, namespace, cosmopilotv1.DeletionPolicyDelete)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("attributed state must remain protected while its deterministic signer pod exists")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("attributed state PVC was deleted while its signer pod remained: %v", err)
	}
}

func TestReleaseOwnerStateFinalizersUsesStableAttributionAfterLabelDrift(t *testing.T) {
	const namespace = "default"
	scheme := lockScheme(t)
	if err := cosmopilotv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	root := &cosmopilotv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: namespace, UID: types.UID("set-uid")}}
	owner := &cosmopilotv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	if err := controllerutil.SetControllerReference(root, owner, scheme); err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-owner-signer-0", Namespace: namespace,
		Labels:     map[string]string{labelInstance: "drifted", labelOwnerUID: "foreign"},
		Finalizers: []string{RetainedStateFinalizer},
	}}
	resourcecleanup.Stamp(pvc, resourcecleanup.RootOwnerFor(owner), resourcecleanup.ClassCosmosignerState)
	resourcecleanup.StampResourceOwner(pvc, owner.UID)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	if err := ReleaseOwnerStateFinalizers(context.Background(), c, owner, namespace); err != nil {
		t.Fatal(err)
	}
	fresh := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), fresh); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(fresh, RetainedStateFinalizer) {
		t.Fatal("stable attribution must release the retained-state finalizer after label drift")
	}
}

func TestReleaseOwnerStateFinalizersDerivesSignerNameAfterInstanceLabelDrift(t *testing.T) {
	const namespace = "default"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-owner-signer-0", Namespace: namespace,
		Labels:     map[string]string{labelInstance: "drifted", labelOwnerUID: string(owner.UID)},
		Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc).Build()

	if err := ReleaseOwnerStateFinalizers(context.Background(), c, owner, namespace); err != nil {
		t.Fatal(err)
	}
	fresh := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), fresh); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(fresh, RetainedStateFinalizer) {
		t.Fatal("canonical owner-labeled state PVC must release its finalizer despite mutable instance-label drift")
	}
}

func TestAttributeOwnedStatePVCsDerivesSignerNameAfterInstanceLabelDrift(t *testing.T) {
	const namespace = "default"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-owner-signer-0", Namespace: namespace,
		Labels:     map[string]string{labelInstance: "drifted", labelOwnerUID: string(owner.UID)},
		Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc).Build()

	changed, err := attributeOwnedStatePVCs(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("canonical owner-labeled state PVC must be attributed despite mutable instance-label drift")
	}
	fresh := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), fresh); err != nil {
		t.Fatal(err)
	}
	if !resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(owner), resourcecleanup.ClassCosmosignerState) {
		t.Fatal("legacy state PVC was not attributed")
	}
	if fresh.Annotations[resourcecleanup.AnnotationResourceOwnerUID] != string(owner.UID) {
		t.Fatal("legacy state PVC did not record its signer owner UID")
	}
}

func TestReleaseOwnerStateFinalizersPreservesSiblingStateWithSharedRoot(t *testing.T) {
	const namespace = "default"
	scheme := lockScheme(t)
	if err := cosmopilotv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	root := &cosmopilotv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: namespace, UID: types.UID("set-uid")}}
	owner := &cosmopilotv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "child-a", Namespace: namespace, UID: types.UID("child-a-uid")}}
	if err := controllerutil.SetControllerReference(root, owner, scheme); err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-child-b-signer-0", Namespace: namespace,
		Finalizers: []string{RetainedStateFinalizer},
	}}
	resourcecleanup.Stamp(pvc, resourcecleanup.RootOwnerFor(owner), resourcecleanup.ClassCosmosignerState)
	resourcecleanup.StampResourceOwner(pvc, types.UID("child-b-uid"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	if err := ReleaseOwnerStateFinalizers(context.Background(), c, owner, namespace); err != nil {
		t.Fatal(err)
	}
	fresh := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), fresh); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(fresh, RetainedStateFinalizer) {
		t.Fatal("a child must not release a sibling's Cosmosigner state guard")
	}
}

func boolPointer(v bool) *bool { return &v }

func TestDeletePVCsReleasesRetainedStateFinalizer(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc).Build()

	if err := DeletePVCs(context.Background(), c, owner, namespace, name); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retained state PVC must be deleted after its finalizer is released, got %v", err)
	}
}
