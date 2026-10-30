package chainnode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/c2h5oh/datasize"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

func nativeConfigured(c *appsv1.ChainNode) bool {
	return c.SnapshotsEnabled() && c.Spec.Persistence.Snapshots.ExportTarball != nil && c.Spec.Persistence.Snapshots.ExportTarball.Protocol == "source-v1"
}
func identity(o client.Object) appsv1.ExportResourceIdentity {
	return appsv1.ExportResourceIdentity{Name: o.GetName(), UID: o.GetUID()}
}
func nativeKey(r exportprotocol.Recipe) client.ObjectKey {
	return client.ObjectKey{Namespace: r.Namespace, Name: r.ResourceName}
}

func (r *Reconciler) nativeRead(ctx context.Context, c *appsv1.ChainNode) (*appsv1.ChainNode, error) {
	if r.APIReader == nil {
		return nil, &nativeOwnerError{fmt.Errorf("native exports require an uncached APIReader")}
	}
	fresh := &appsv1.ChainNode{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(c), fresh); err != nil {
		return nil, &nativeOwnerError{err}
	}
	if fresh.UID != c.UID {
		return nil, &nativeOwnerError{fmt.Errorf("native authority UID changed")}
	}
	return fresh, nil
}
func (r *Reconciler) nativeSave(ctx context.Context, c, fresh *appsv1.ChainNode) error {
	syncSnapshotExportCleanupCondition(fresh)
	err := r.Status().Update(ctx, fresh)
	if err != nil {
		if apierrors.IsConflict(err) {
			return err
		}
		check, readErr := r.nativeRead(ctx, c)
		if readErr != nil {
			return readErr
		}
		if !nativeSerializedEqual(check.Status.SnapshotExportAuthority, fresh.Status.SnapshotExportAuthority) || !nativeSerializedEqual(check.Status.SnapshotExports, fresh.Status.SnapshotExports) || !nativeSerializedEqual(nativeOwnedConditions(check), nativeOwnedConditions(fresh)) {
			return err
		}
		fresh = check
	}
	mergeSnapshotExportOwnedStatus(c, fresh)
	refreshSnapshotExportResourceVersion(c, fresh)
	return nil
}
func (r *Reconciler) nativeTransition(ctx context.Context, c *appsv1.ChainNode, expected *appsv1.SnapshotExportAttempt, mutate func(*appsv1.SnapshotExportAttempt, *appsv1.ChainNode) error) error {
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	a := exportprotocol.FindAttempt(fresh, expected.Sequence, expected.ID)
	if a == nil || !reflect.DeepEqual(a, expected) {
		return fmt.Errorf("native attempt changed or missing")
	}
	if err := mutate(a, fresh); err != nil {
		return err
	}
	return r.nativeSave(ctx, c, fresh)
}
func (r *Reconciler) ensureNativeIdentity(ctx context.Context, c *appsv1.ChainNode) (appsv1.ExportResourceIdentity, error) {
	name := "export-" + string(c.UID)
	owner := []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: c.Name, UID: c.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, OwnerReferences: owner}, AutomountServiceAccountToken: ptr.To(false)}
	role := &rbacv1.Role{ObjectMeta: sa.ObjectMeta, Rules: []rbacv1.PolicyRule{{APIGroups: []string{appsv1.GroupVersion.Group}, Resources: []string{"chainnodes"}, ResourceNames: []string{c.Name}, Verbs: []string{"get"}}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: sa.ObjectMeta, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: c.Namespace}}}
	for _, o := range []client.Object{sa, role, binding} {
		want := o.DeepCopyObject().(client.Object)
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(o), o); apierrors.IsNotFound(err) {
			if err := r.Create(ctx, want); err != nil {
				return appsv1.ExportResourceIdentity{}, err
			}
			o = want
		} else if err != nil {
			return appsv1.ExportResourceIdentity{}, err
		}
		if !metav1.IsControlledBy(o, c) {
			return appsv1.ExportResourceIdentity{}, fmt.Errorf("native execution identity is not owned")
		}
		switch actual := o.(type) {
		case *corev1.ServiceAccount:
			sa = actual
			if ptr.Deref(sa.AutomountServiceAccountToken, true) {
				return appsv1.ExportResourceIdentity{}, fmt.Errorf("native ServiceAccount changed")
			}
		case *rbacv1.Role:
			if !reflect.DeepEqual(actual.Rules, want.(*rbacv1.Role).Rules) {
				return appsv1.ExportResourceIdentity{}, fmt.Errorf("native Role changed")
			}
		case *rbacv1.RoleBinding:
			if !reflect.DeepEqual(actual.RoleRef, want.(*rbacv1.RoleBinding).RoleRef) || !reflect.DeepEqual(actual.Subjects, want.(*rbacv1.RoleBinding).Subjects) {
				return appsv1.ExportResourceIdentity{}, fmt.Errorf("native RoleBinding changed")
			}
		}
	}
	return identity(sa), nil
}
func (r *Reconciler) nativeSource(ctx context.Context, recipe exportprotocol.Recipe, held bool, a *appsv1.SnapshotExportAttempt) (*snapshotv1.VolumeSnapshot, error) {
	s := &snapshotv1.VolumeSnapshot{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: recipe.Namespace, Name: recipe.Snapshot.Name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &nativeIdentityError{err}
		}
		return nil, err
	}
	if s.UID != recipe.Snapshot.UID || !s.DeletionTimestamp.IsZero() {
		return nil, &nativeIdentityError{fmt.Errorf("native snapshot incarnation changed")}
	}
	if s.Status != nil && (s.Status.BoundVolumeSnapshotContentName != nil && *s.Status.BoundVolumeSnapshotContentName != recipe.Content.Name || s.Status.RestoreSize != nil && s.Status.RestoreSize.String() != recipe.RestoreSize) {
		return nil, &nativeIdentityError{fmt.Errorf("native snapshot binding changed")}
	}
	if held {
		holders, err := nativeHolders(s)
		if err != nil {
			return nil, &nativeIdentityError{err}
		}
		if !controllerutil.ContainsFinalizer(s, exportprotocol.SourceFinalizer) || holders[a.ID] != nativeHolder(recipe, a) {
			return nil, &nativeIdentityError{fmt.Errorf("native source hold is missing")}
		}
	}
	content := &snapshotv1.VolumeSnapshotContent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: recipe.Content.Name}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &nativeIdentityError{err}
		}
		return nil, err
	}
	ref := content.Spec.VolumeSnapshotRef
	if content.UID != recipe.Content.UID || !content.DeletionTimestamp.IsZero() || content.Spec.Driver != recipe.Driver || ref.UID != s.UID || ref.Name != s.Name || ref.Namespace != s.Namespace {
		return nil, &nativeIdentityError{fmt.Errorf("native snapshot content reverse binding changed")}
	}
	if s.Status == nil || !ptr.Deref(s.Status.ReadyToUse, false) || s.Status.BoundVolumeSnapshotContentName == nil || s.Status.RestoreSize == nil {
		return nil, fmt.Errorf("native snapshot readiness is unavailable")
	}
	if content.Status == nil || !ptr.Deref(content.Status.ReadyToUse, false) || content.Status.SnapshotHandle == nil || *content.Status.SnapshotHandle == "" {
		return nil, fmt.Errorf("native snapshot content is not ready")
	}
	return s, nil
}
func (r *Reconciler) reserveNativeAttempt(ctx context.Context, c *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) error {
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	if !nativeConfigured(fresh) || !fresh.DeletionTimestamp.IsZero() {
		return fmt.Errorf("native export is disabled")
	}
	cfg := fresh.Spec.Persistence.Snapshots.ExportTarball
	if err := cfg.Validate("exportTarball"); err != nil {
		return err
	}
	if !controllerutil.ContainsFinalizer(fresh, exportprotocol.AuthorityFinalizer) {
		controllerutil.AddFinalizer(fresh, exportprotocol.AuthorityFinalizer)
		if err := r.Update(ctx, fresh); err != nil {
			return err
		}
		*c = *fresh
		return &nativeDeferred{reason: "RecoveryDeferred"}
	}
	if fresh.Status.SnapshotExportAuthority != nil {
		for _, a := range fresh.Status.SnapshotExportAuthority.Attempts {
			rec, err := exportprotocol.DecodeRecipe(&a)
			if err != nil {
				return err
			}
			if rec.Snapshot.UID == snapshot.UID && a.Phase != "Invalidated" {
				return nil
			}
		}
	}
	if existing := snapshotExportFor(fresh, snapshot); existing != nil && existing.ProvenanceVersion == 1 && (existing.Phase == appsv1.SnapshotExportPhaseUploaded || existing.Phase == appsv1.SnapshotExportPhaseDeleted) {
		return nil
	}
	if existing := snapshotExportFor(fresh, snapshot); existing != nil && existing.ProvenanceVersion != 1 {
		return fmt.Errorf("snapshot already has a legacy export")
	}
	sa, err := r.ensureNativeIdentity(ctx, fresh)
	if err != nil {
		return err
	}
	base, err := newSnapshotExportStatus(fresh, snapshot)
	if err != nil {
		return err
	}
	id := string(uuid.NewUUID())
	base.ID = "export-" + id
	base.ObjectName = "export-" + id
	base.ProvenanceVersion = 1
	s := &snapshotv1.VolumeSnapshot{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(snapshot), s); err != nil {
		return err
	}
	if s.UID != snapshot.UID || s.Status == nil || s.Status.BoundVolumeSnapshotContentName == nil || s.Status.RestoreSize == nil {
		return fmt.Errorf("native snapshot is not bound")
	}
	content := &snapshotv1.VolumeSnapshotContent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: *s.Status.BoundVolumeSnapshotContentName}, content); err != nil {
		return err
	}
	secret := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: cfg.S3.CredentialsSecret.Name}, secret); err != nil {
		return err
	}
	if len(secret.Data["AWS_ACCESS_KEY_ID"]) == 0 || len(secret.Data["AWS_SECRET_ACCESS_KEY"]) == 0 {
		return fmt.Errorf("native credential Secret lacks explicit credentials")
	}
	class := ""
	if original := s.Spec.Source.PersistentVolumeClaimName; original != nil {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: *original}, pvc); err != nil {
			return err
		}
		class = ptr.Deref(pvc.Spec.StorageClassName, "")
	}
	if class == "" {
		classes := &storagev1.StorageClassList{}
		if err := r.APIReader.List(ctx, classes); err != nil {
			return err
		}
		for _, sc := range classes.Items {
			if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
				if class != "" {
					return fmt.Errorf("native clone storage class is ambiguous")
				}
				class = sc.Name
			}
		}
	}
	if class == "" {
		return fmt.Errorf("native clone requires a resolved storage class")
	}
	recipe := exportprotocol.Recipe{Version: 1, Namespace: c.Namespace, Owner: identity(c), ExportID: base.ID, Snapshot: identity(s), Content: identity(content), Driver: content.Spec.Driver, RestoreSize: s.Status.RestoreSize.String(), StorageClass: class, ResourceName: "export-" + id, ObjectName: base.ObjectName, Destination: base.Destination, Credentials: identity(secret), ServiceAccount: sa, Compression: cfg.GetCompression(), SizeLimit: base.SizeLimit, PartSize: base.PartSize, ChunkSize: base.ChunkSize, BufferSize: base.BufferSize, ConcurrentJobs: base.ConcurrentJobs, Image: r.opts.GetDataExporterImage(), PriorityClass: r.opts.GetDefaultPriorityClassName(), DeleteOnExpire: base.DeleteOnExpire}
	if c.Spec.Config != nil {
		recipe.ImagePullSecrets = c.Spec.Config.ImagePullSecrets
	}
	if cfg.Resources != nil {
		recipe.Resources = *cfg.Resources.DeepCopy()
	} else {
		recipe.Resources.Requests = corev1.ResourceList{}
		chunk, err := datasize.ParseString(recipe.ChunkSize)
		if err != nil {
			return err
		}
		buffer, err := datasize.ParseString(recipe.BufferSize)
		if err != nil {
			return err
		}
		recipe.Resources.Requests[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(int64(chunk.Bytes())*int64(recipe.ConcurrentJobs+1), resource.BinarySI)
		recipe.Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(int64(buffer.Bytes())*2, resource.BinarySI)
	}
	for name, limit := range recipe.Resources.Limits {
		if _, ok := recipe.Resources.Requests[name]; !ok {
			if recipe.Resources.Requests == nil {
				recipe.Resources.Requests = corev1.ResourceList{}
			}
			recipe.Resources.Requests[name] = limit.DeepCopy()
		}
	}
	if height, err := strconv.ParseInt(s.Annotations[controllers.AnnotationDataHeight], 10, 64); err == nil && height > 0 {
		recipe.SourceHeight = strconv.FormatInt(height, 10)
		recipe.SourceHeightKind = "CaptureObserved"
	}
	if _, err := r.nativeSource(ctx, recipe, false, nil); err != nil {
		return err
	}
	if fresh.Status.SnapshotExportAuthority == nil {
		fresh.Status.SnapshotExportAuthority = &appsv1.SnapshotExportAuthority{Version: 1, Attempts: []appsv1.SnapshotExportAttempt{}}
	}
	auth := fresh.Status.SnapshotExportAuthority
	if auth.Version != 1 {
		return nativeLocal("InvalidAuthority", "", fmt.Errorf("unsupported native authority"))
	}
	if len(auth.Attempts) >= 32 {
		return r.deferNativeCapacity(ctx, c, "AttemptLimit")
	}
	if auth.LastSequence == math.MaxInt64 {
		return r.deferNativeCapacity(ctx, c, "SequenceExhausted")
	}
	encoded, digest, err := exportprotocol.EncodeRecipe(recipe)
	if err != nil {
		return err
	}
	a := appsv1.SnapshotExportAttempt{Sequence: auth.LastSequence + 1, ID: id, ExportID: base.ID, Recipe: encoded, RecipeSHA256: digest, Phase: "Reserved", CreatedAt: metav1.Now()}
	if err := exportprotocol.CheckEnvelopeBudget(&a, recipe); err != nil {
		return err
	}
	auth.LastSequence = a.Sequence
	auth.Attempts = append(auth.Attempts, a)
	fresh.Status.SnapshotExports = append(fresh.Status.SnapshotExports, base)
	budget, err := nativeProjectedStatusSize(fresh)
	if err != nil {
		return err
	}
	if budget > exportprotocol.MaxNativeStatus {
		return r.deferNativeCapacity(ctx, c, "StatusBudget")
	}
	apiMeta.RemoveStatusCondition(&fresh.Status.Conditions, appsv1.ConditionSnapshotExportCapacityExceeded)
	return r.nativeSave(ctx, c, fresh)
}

func (r *Reconciler) holdNativeSource(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	s, err := r.nativeSource(ctx, recipe, false, a)
	if err != nil {
		return err
	}
	holders, err := nativeHolders(s)
	if err != nil {
		return err
	}
	holders[a.ID] = nativeHolder(recipe, a)
	b, _ := json.Marshal(holders)
	if len(b) > 16*1024 {
		return fmt.Errorf("source holder capacity exhausted")
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[exportprotocol.HoldAnnotation] = string(b)
	controllerutil.AddFinalizer(s, exportprotocol.SourceFinalizer)
	if err := r.Update(ctx, s); err != nil {
		if apierrors.IsConflict(err) {
			return err
		}
		confirmed, readErr := r.nativeSource(ctx, recipe, true, a)
		if readErr != nil {
			return readErr
		}
		s = confirmed
	}
	return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
		if current.Phase != "Reserved" {
			return fmt.Errorf("native hold transition denied")
		}
		current.Phase = "Held"
		current.SourceHoldResourceVersion = s.ResourceVersion
		return nil
	})
}
func (r *Reconciler) issueNativeResource(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe, job bool) error {
	if _, err := r.nativeSource(ctx, recipe, true, a); err != nil {
		return err
	}
	issuer := string(uuid.NewUUID())
	err := r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
		if current.Phase != "Held" && current.Phase != "ResourcesRequested" {
			return fmt.Errorf("native resource issuance denied")
		}
		if job {
			if current.JobCreateIssued {
				return fmt.Errorf("native job already issued")
			}
			current.JobCreateIssued = true
			current.JobCreateIssuer = issuer
		} else {
			if current.PVCCreateIssued || current.Job == nil {
				return fmt.Errorf("native PVC issuance denied")
			}
			current.PVCCreateIssued = true
			current.PVCCreateIssuer = issuer
		}
		current.Phase = "ResourcesRequested"
		return nil
	})
	if err != nil {
		return err
	}
	// Only this caller owns the freshly persisted random issuance witness.
	if job {
		err = r.Create(ctx, datasnapshot.NativeJob(a, recipe))
	} else {
		err = r.Create(ctx, datasnapshot.NativePVC(a, recipe, a.Job.UID))
	}
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
func (r *Reconciler) bindNativeResources(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	j := &batchv1.Job{}
	err := r.APIReader.Get(ctx, nativeKey(recipe), j)
	if apierrors.IsNotFound(err) {
		if a.Job != nil {
			return r.invalidateNativeAttempt(ctx, c, a, "JobMissing")
		}
		if time.Since(a.CreatedAt.Time) > 2*time.Minute {
			return r.invalidateNativeAttempt(ctx, c, a, "IssuedResourceMissing")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := datasnapshot.ValidateNativeJob(j, a, recipe); err != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "JobRecipeChanged")
	}
	if a.Job == nil {
		return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
			ref := identity(j)
			current.Job = &ref
			return nil
		})
	}
	if a.Job.UID != j.UID {
		return r.invalidateNativeAttempt(ctx, c, a, "JobUIDChanged")
	}
	if !a.PVCCreateIssued {
		return r.issueNativeResource(ctx, c, a, recipe, false)
	}
	p := &corev1.PersistentVolumeClaim{}
	err = r.APIReader.Get(ctx, nativeKey(recipe), p)
	if apierrors.IsNotFound(err) {
		if a.ScratchPVC != nil {
			return r.invalidateNativeAttempt(ctx, c, a, "PVCMissing")
		}
		if time.Since(a.CreatedAt.Time) > 2*time.Minute {
			return r.invalidateNativeAttempt(ctx, c, a, "IssuedResourceMissing")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := datasnapshot.ValidateNativePVC(p, a, recipe, j.UID); err != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "PVCRecipeChanged")
	}
	if a.ScratchPVC == nil {
		return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
			ref := identity(p)
			current.ScratchPVC = &ref
			return nil
		})
	}
	if a.ScratchPVC.UID != p.UID {
		return r.invalidateNativeAttempt(ctx, c, a, "PVCUIDChanged")
	}
	if a.ScratchPV != nil && p.Spec.VolumeName != a.ScratchPV.Name {
		return r.invalidateNativeAttempt(ctx, c, a, "PVBindingChanged")
	}
	if p.Status.Phase != corev1.ClaimBound || p.Spec.VolumeName == "" {
		if time.Since(a.CreatedAt.Time) > 2*time.Minute {
			return nativeLocal("RecoveryDeferred", a.ExportID, fmt.Errorf("native scratch PVC is awaiting binding"))
		}
		return nil
	}
	pv := &corev1.PersistentVolume{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: p.Spec.VolumeName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			if a.ScratchPV != nil {
				return r.invalidateNativeAttempt(ctx, c, a, "PVMissing")
			}
			if time.Since(a.CreatedAt.Time) > 2*time.Minute {
				return nativeLocal("RecoveryDeferred", a.ExportID, fmt.Errorf("native scratch PV has not been observed"))
			}
			return nil
		}
		return err
	}
	if err := validateNativePV(pv, p, recipe); err != nil || a.ScratchPV != nil && a.ScratchPV.UID != pv.UID {
		return r.invalidateNativeAttempt(ctx, c, a, "PVBindingChanged")
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels{exportprotocol.OwnerUIDLabel: string(c.UID), exportprotocol.AttemptLabel: a.ID}); err != nil {
		return err
	}
	var selected *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if nativePodOwnedBy(pod, j) {
			if selected != nil {
				return r.invalidateNativeAttempt(ctx, c, a, "ReplacementPod")
			}
			selected = pod
		}
	}
	if selected == nil {
		if a.Pod != nil {
			return r.invalidateNativeAttempt(ctx, c, a, "PodMissing")
		}
		if time.Since(a.CreatedAt.Time) > 2*time.Minute {
			return nativeLocal("RecoveryDeferred", a.ExportID, fmt.Errorf("native worker Pod has not been observed"))
		}
		return nil
	}
	if a.Pod != nil && a.Pod.UID != selected.UID || !selected.DeletionTimestamp.IsZero() || datasnapshot.ValidateNativePodSpec(&selected.Spec, a, recipe) != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "AdmittedPodChanged")
	}
	if _, err := r.nativeSource(ctx, recipe, true, a); err != nil {
		var lost *nativeIdentityError
		if !errors.As(err, &lost) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "SourceBindingChanged")
	}
	if err := r.nativeCredentialIdentity(ctx, recipe); err != nil {
		var lost *nativeIdentityError
		if !errors.As(err, &lost) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "ExecutionIdentityChanged")
	}
	return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
		pvRef, podRef := identity(pv), identity(selected)
		current.ScratchPV = &pvRef
		current.Pod = &podRef
		current.Phase = "Uploading"
		now := metav1.Now()
		current.AuthorizedAt = &now
		return nil
	})
}
func validateNativePV(pv *corev1.PersistentVolume, p *corev1.PersistentVolumeClaim, recipe exportprotocol.Recipe) error {
	ref := pv.Spec.ClaimRef
	if pv.Status.Phase != corev1.VolumeBound || !pv.DeletionTimestamp.IsZero() || ref == nil || ref.UID != p.UID || ref.Name != p.Name || ref.Namespace != p.Namespace || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != recipe.Driver || pv.Spec.StorageClassName != recipe.StorageClass || ptr.Deref(pv.Spec.VolumeMode, corev1.PersistentVolumeFilesystem) != corev1.PersistentVolumeFilesystem {
		return fmt.Errorf("native PV binding changed")
	}
	return nil
}
func (r *Reconciler) nativeCredentialIdentity(ctx context.Context, recipe exportprotocol.Recipe) error {
	for _, obj := range []client.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: recipe.Credentials.Name, Namespace: recipe.Namespace}}, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: recipe.ServiceAccount.Name, Namespace: recipe.Namespace}}} {
		ref := recipe.Credentials
		if _, ok := obj.(*corev1.ServiceAccount); ok {
			ref = recipe.ServiceAccount
		}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if apierrors.IsNotFound(err) {
				return &nativeIdentityError{err}
			}
			return err
		}
		if obj.GetUID() != ref.UID || !obj.GetDeletionTimestamp().IsZero() {
			return &nativeIdentityError{fmt.Errorf("native execution identity changed")}
		}
	}
	return nil
}
func (r *Reconciler) publishNativeArchive(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	if _, err := r.nativeSource(ctx, recipe, true, a); err != nil {
		var lost *nativeIdentityError
		if !errors.As(err, &lost) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "SourceBindingChanged")
	}
	if err := r.nativeCredentialIdentity(ctx, recipe); err != nil {
		var lost *nativeIdentityError
		if !errors.As(err, &lost) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "ExecutionIdentityChanged")
	}
	j := &batchv1.Job{}
	if err := r.APIReader.Get(ctx, nativeKey(recipe), j); err != nil {
		if !apierrors.IsNotFound(err) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "JobMissing")
	}
	if a.Job == nil || j.UID != a.Job.UID || datasnapshot.ValidateNativeJob(j, a, recipe) != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "JobChanged")
	}
	p := &corev1.PersistentVolumeClaim{}
	if err := r.APIReader.Get(ctx, nativeKey(recipe), p); err != nil {
		if !apierrors.IsNotFound(err) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "PVCMissing")
	}
	if a.ScratchPVC == nil || p.UID != a.ScratchPVC.UID || datasnapshot.ValidateNativePVC(p, a, recipe, j.UID) != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "PVCChanged")
	}
	if a.ScratchPV == nil || p.Spec.VolumeName == "" || p.Spec.VolumeName != a.ScratchPV.Name {
		return r.invalidateNativeAttempt(ctx, c, a, "PVChanged")
	}
	pv := &corev1.PersistentVolume{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: p.Spec.VolumeName}, pv); err != nil {
		if !apierrors.IsNotFound(err) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "PVMissing")
	}
	if a.ScratchPV == nil || pv.UID != a.ScratchPV.UID || validateNativePV(pv, p, recipe) != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "PVChanged")
	}
	pod := &corev1.Pod{}
	if a.Pod == nil {
		return r.invalidateNativeAttempt(ctx, c, a, "PodMissing")
	}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: a.Pod.Name}, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return nativeLocal("RecoveryDeferred", a.ExportID, err)
		}
		return r.invalidateNativeAttempt(ctx, c, a, "PodMissing")
	}
	if pod.UID != a.Pod.UID || !pod.DeletionTimestamp.IsZero() || !nativePodOwnedBy(pod, j) || datasnapshot.ValidateNativePodSpec(&pod.Spec, a, recipe) != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "PodChanged")
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels{exportprotocol.OwnerUIDLabel: string(c.UID), exportprotocol.AttemptLabel: a.ID}); err != nil {
		return err
	}
	for _, other := range pods.Items {
		if nativePodOwnedBy(&other, j) && other.UID != pod.UID {
			return r.invalidateNativeAttempt(ctx, c, a, "ReplacementPod")
		}
	}
	for _, cond := range j.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return r.invalidateNativeAttempt(ctx, c, a, "JobFailed")
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		return r.invalidateNativeAttempt(ctx, c, a, "WorkerFailed")
	}
	complete := false
	for _, cond := range j.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			complete = true
		}
	}
	if !complete {
		return nil
	}
	if pod.Status.Phase != corev1.PodSucceeded || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].Name != "exporter" || pod.Status.ContainerStatuses[0].RestartCount != 0 || pod.Status.ContainerStatuses[0].State.Terminated == nil || pod.Status.ContainerStatuses[0].State.Terminated.ExitCode != 0 {
		return r.invalidateNativeAttempt(ctx, c, a, "WorkerResultMissing")
	}
	envelope, err := exportprotocol.DecodeEnvelope([]byte(pod.Status.ContainerStatuses[0].State.Terminated.Message))
	if err != nil || exportprotocol.ValidateEnvelope(envelope, a, recipe) != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "WorkerResultInvalid")
	}
	now := metav1.Now()
	archive, err := exportprotocol.NewArchive(a, recipe, envelope, now)
	if err != nil {
		return r.invalidateNativeAttempt(ctx, c, a, "DescriptorInvalid")
	}

	return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, fresh *appsv1.ChainNode) error {
		if current.Phase != "Uploading" {
			return fmt.Errorf("native publication denied")
		}
		export := snapshotExportByID(fresh, a.ExportID)
		if export == nil || export.ProvenanceVersion != 1 || export.Phase != appsv1.SnapshotExportPhaseUploading {
			return fmt.Errorf("native catalog authority missing")
		}
		export.Archive = archive
		export.Phase = appsv1.SnapshotExportPhaseUploaded
		current.Phase = "Succeeded"
		current.CompletedAt = &now
		current.CompletedEnvelope = pod.Status.ContainerStatuses[0].State.Terminated.Message
		if envelope.Result.CleanupRequired {
			current.RemoteCleanupState = "Required"
			export.RemoteCleanupState = "Required"
		} else {
			current.RemoteCleanupState = "Complete"
			export.RemoteCleanupState = "Complete"
		}
		return nil
	})
}

func nativeSerializedEqual(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func nativePodOwnedBy(pod *corev1.Pod, job *batchv1.Job) bool {
	owner := metav1.GetControllerOf(pod)
	return owner != nil && owner.APIVersion == batchv1.SchemeGroupVersion.String() && owner.Kind == "Job" && owner.Name == job.Name && owner.UID == job.UID && pod.Namespace == job.Namespace
}

// Owner errors must survive every native-local boundary.
type nativeOwnerError struct{ error }

func (e *nativeOwnerError) Unwrap() error { return e.error }

type nativeOperationError struct {
	reason, exportID string
	err              error
}

func (e *nativeOperationError) Error() string {
	return fmt.Sprintf("%s %s: %v", e.reason, e.exportID, e.err)
}
func (e *nativeOperationError) Unwrap() error { return e.err }

type nativeDeferred struct{ reason string }

func (e *nativeDeferred) Error() string { return "native export deferred: " + e.reason }
func nativeLocal(reason, id string, err error) error {
	if err == nil {
		return nil
	}
	var owner *nativeOwnerError
	if errors.As(err, &owner) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var local *nativeOperationError
	if errors.As(err, &local) {
		return err
	}
	return &nativeOperationError{reason, id, err}
}
func (r *Reconciler) nativeCondition(ctx context.Context, c *appsv1.ChainNode, kind, reason, message string) error {
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	before := nativeOwnedConditions(fresh)
	if reason == "" {
		apiMeta.RemoveStatusCondition(&fresh.Status.Conditions, kind)
	} else {
		if len(message) > 256 {
			message = message[:256]
		}
		apiMeta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{Type: kind, Status: metav1.ConditionTrue, Reason: reason, Message: message, ObservedGeneration: fresh.Generation})
	}
	if reflect.DeepEqual(before, nativeOwnedConditions(fresh)) {
		mergeSnapshotExportOwnedStatus(c, fresh)
		refreshSnapshotExportResourceVersion(c, fresh)
		return nil
	}
	return r.nativeSave(ctx, c, fresh)
}
func (r *Reconciler) deferNativeCapacity(ctx context.Context, c *appsv1.ChainNode, reason string) error {
	if err := r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportCapacityExceeded, reason, "Native export reservation declined: "+reason); err != nil {
		return err
	}
	return &nativeDeferred{reason: reason}
}
func (r *Reconciler) handleNativeOutcome(ctx context.Context, c *appsv1.ChainNode, err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	var deferred *nativeDeferred
	if errors.As(err, &deferred) {
		return true, nil
	}
	var local *nativeOperationError
	if !errors.As(err, &local) {
		return false, err
	}
	if err := r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportDegraded, local.reason, local.Error()); err != nil {
		return false, err
	}
	// Revalidate owner identity before independent core reconciliation proceeds.
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return false, err
	}
	mergeSnapshotExportOwnedStatus(c, fresh)
	refreshSnapshotExportResourceVersion(c, fresh)
	return true, nil
}
func nativeProjectedStatusSize(c *appsv1.ChainNode) (int, error) {
	projected := c.DeepCopy()
	if projected.Status.SnapshotExportAuthority == nil {
		b, err := json.Marshal(projected.Status)
		return len(b), err
	}
	for i := range projected.Status.SnapshotExportAuthority.Attempts {
		a := &projected.Status.SnapshotExportAuthority.Attempts[i]
		recipe, err := exportprotocol.DecodeRecipe(a)
		if err != nil {
			return 0, err
		}
		if !exportprotocol.Terminal(a) {
			ref := func(name string) *appsv1.ExportResourceIdentity {
				return &appsv1.ExportResourceIdentity{Name: name, UID: types.UID(strings.Repeat("u", 128))}
			}
			if a.Job == nil {
				a.Job = ref(recipe.ResourceName)
			}
			if a.ScratchPVC == nil {
				a.ScratchPVC = ref(recipe.ResourceName)
			}
			if a.Pod == nil {
				a.Pod = ref(strings.Repeat("p", 253))
			}
			if a.ScratchPV == nil {
				a.ScratchPV = ref(strings.Repeat("v", 253))
			}
			if a.SourceHoldResourceVersion == "" {
				a.SourceHoldResourceVersion = strings.Repeat("1", 128)
			}
			a.JobCreateIssued = true
			a.PVCCreateIssued = true
			a.JobCreateIssuer = strings.Repeat("i", 36)
			a.PVCCreateIssuer = strings.Repeat("i", 36)
			now := metav1.Now()
			a.AuthorizedAt = &now
			a.CompletedAt = &now
			envelope := exportprotocol.EnvelopeFor(a, recipe)
			envelope.Result = dataexporter.UploadResult{Version: 1, Layout: "single", ObjectCount: 1, ObjectKey: recipe.ObjectName + recipe.Compression.Extension(), SHA256: strings.Repeat("f", 64), SizeBytes: math.MaxInt64, Compression: recipe.Compression, CleanupRequired: true}
			encoded, err := exportprotocol.EncodeEnvelope(envelope)
			if err != nil {
				return 0, err
			}
			a.CompletedEnvelope = string(encoded)
			e := snapshotExportByID(projected, a.ExportID)
			if e != nil && e.Archive == nil {
				e.Archive, err = exportprotocol.NewArchive(a, recipe, envelope, now)
				if err != nil {
					return 0, err
				}
			}
		}
		a.Reason = strings.Repeat("r", 256)
		a.LocalCleanupState = "Complete"
		a.RemoteCleanupState = "Required"
		if a.CleanupIssuer == "" {
			a.CleanupIssuer = strings.Repeat("i", 36)
		}
		if a.CleanupIssuedAt == nil {
			now := metav1.Now()
			a.CleanupIssuedAt = &now
		}
		if a.CleanupJob == nil {
			a.CleanupJob = &appsv1.ExportResourceIdentity{Name: "delete-" + a.ID, UID: types.UID(strings.Repeat("u", 128))}
		}
		if a.CleanupPod == nil {
			a.CleanupPod = &appsv1.ExportResourceIdentity{Name: strings.Repeat("p", 253), UID: types.UID(strings.Repeat("u", 128))}
		}
		a.CleanupComplete = true
		a.CleanupState = "Acknowledged"
		a.CleanupReason = strings.Repeat("r", 256)
	}
	b, err := json.Marshal(projected.Status)
	return len(b) + 4096, err
}

type nativeIdentityError struct{ error }

func (e *nativeIdentityError) Unwrap() error { return e.error }

func nativeOwnedConditions(c *appsv1.ChainNode) []metav1.Condition {
	conditions := []metav1.Condition{}
	for _, kind := range []string{appsv1.ConditionSnapshotExportCleanup, appsv1.ConditionSnapshotExportDegraded, appsv1.ConditionSnapshotExportCapacityExceeded} {
		if condition := apiMeta.FindStatusCondition(c.Status.Conditions, kind); condition != nil {
			conditions = append(conditions, *condition)
		}
	}
	return conditions
}
