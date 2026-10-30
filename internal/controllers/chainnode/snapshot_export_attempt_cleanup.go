package chainnode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
)

func nativeHolders(s *snapshotv1.VolumeSnapshot) (map[string]string, error) {
	holders := map[string]string{}
	raw := s.Annotations[exportprotocol.HoldAnnotation]
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &holders); err != nil {
			return nil, fmt.Errorf("invalid native source holders: %w", err)
		}
		if holders == nil {
			return nil, fmt.Errorf("null native source holders")
		}
	}
	return holders, nil
}
func nativeHolder(r exportprotocol.Recipe, a *appsv1.SnapshotExportAttempt) string {
	return strings.Join([]string{r.Namespace, r.Owner.Name, string(r.Owner.UID), strconv.FormatInt(a.Sequence, 10), a.ID}, "/")
}
func (r *Reconciler) invalidateNativeAttempt(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, reason string) error {
	if exportprotocol.Terminal(a) {
		return nil
	}
	return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, fresh *appsv1.ChainNode) error {
		if exportprotocol.Terminal(current) {
			return fmt.Errorf("native invalidation lost to terminal state")
		}
		authorized := current.AuthorizedAt != nil || current.Phase == "Uploading"
		current.Phase = "Invalidated"
		current.Reason = reason
		now := metav1.Now()
		current.CompletedAt = &now
		if authorized {
			current.RemoteCleanupState = "Required"
		} else {
			current.RemoteCleanupState = "Complete"
		}
		if export := snapshotExportByID(fresh, current.ExportID); export != nil {
			export.Phase = appsv1.SnapshotExportPhaseCleanupRequired
			export.Message = reason
			export.RemoteCleanupState = current.RemoteCleanupState
		}
		return nil
	})
}
func (r *Reconciler) releaseNativeHold(ctx context.Context, recipe exportprotocol.Recipe, a *appsv1.SnapshotExportAttempt) error {
	s := &snapshotv1.VolumeSnapshot{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: recipe.Namespace, Name: recipe.Snapshot.Name}, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.UID != recipe.Snapshot.UID {
		return nil
	}
	holders, err := nativeHolders(s)
	if err != nil {
		return err
	}
	if _, ok := holders[a.ID]; !ok {
		return nil
	}
	if holders[a.ID] != nativeHolder(recipe, a) {
		return fmt.Errorf("native source holder attribution changed")
	}
	delete(holders, a.ID)
	if len(holders) == 0 {
		delete(s.Annotations, exportprotocol.HoldAnnotation)
		controllerutil.RemoveFinalizer(s, exportprotocol.SourceFinalizer)
	} else {
		b, _ := json.Marshal(holders)
		s.Annotations[exportprotocol.HoldAnnotation] = string(b)
	}
	return r.Update(ctx, s)
}
func (r *Reconciler) deleteNativeObject(ctx context.Context, obj client.Object) error {
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	if err := r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &rv}, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	fresh := obj.DeepCopyObject().(client.Object)
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), fresh); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if fresh.GetUID() != uid {
		return nil
	}
	if controllerutil.ContainsFinalizer(fresh, exportprotocol.AttemptFinalizer) {
		controllerutil.RemoveFinalizer(fresh, exportprotocol.AttemptFinalizer)
		return r.Update(ctx, fresh)
	}
	return nil
}
func (r *Reconciler) cleanupNativeAttempt(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	if !exportprotocol.Terminal(a) {
		return fmt.Errorf("native cleanup requires terminal authority")
	}
	if a.Phase == "Invalidated" {
		if err := r.failNativeSnapshotMarker(ctx, c, a, recipe); err != nil {
			return err
		}
	}
	// Terminal authority is durable before source protection is released.
	if err := r.releaseNativeHold(ctx, recipe, a); err != nil {
		return err
	}
	for _, obj := range []client.Object{&batchv1.Job{}, &corev1.PersistentVolumeClaim{}} {
		if err := r.APIReader.Get(ctx, nativeKey(recipe), obj); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		labels := obj.GetLabels()
		if labels[exportprotocol.OwnerUIDLabel] != string(c.UID) || labels[exportprotocol.AttemptLabel] != a.ID {
			return fmt.Errorf("native cleanup resource attribution changed")
		}
		ref := a.Job
		if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			ref = a.ScratchPVC
		}
		if ref != nil && ref.UID != obj.GetUID() {
			return fmt.Errorf("native cleanup refuses substituted UID")
		}
		if err := r.deleteNativeObject(ctx, obj); err != nil {
			return err
		}
	}
	if a.Pod != nil && a.Job != nil {
		if err := r.deleteNativeSelectedPod(ctx, c.Namespace, a.Pod, recipe.ResourceName, a.Job.UID); err != nil {
			return err
		}
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels{exportprotocol.OwnerUIDLabel: string(c.UID), exportprotocol.AttemptLabel: a.ID}); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		owner := metav1.GetControllerOf(p)
		if owner != nil && owner.APIVersion == batchv1.SchemeGroupVersion.String() && owner.Kind == "Job" && owner.Name == recipe.ResourceName && (a.Job != nil && owner.UID == a.Job.UID) {
			if err := r.deleteNativeObject(ctx, p); err != nil {
				return err
			}
		}
	}
	for _, o := range []client.Object{&batchv1.Job{}, &corev1.PersistentVolumeClaim{}} {
		err := r.APIReader.Get(ctx, nativeKey(recipe), o)
		if err == nil {
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
	}
	remaining := &corev1.PodList{}
	if err := r.APIReader.List(ctx, remaining, client.InNamespace(c.Namespace), client.MatchingLabels{exportprotocol.OwnerUIDLabel: string(c.UID), exportprotocol.AttemptLabel: a.ID}); err != nil {
		return err
	}
	for i := range remaining.Items {
		p := &remaining.Items[i]
		owner := metav1.GetControllerOf(p)
		if owner != nil && owner.APIVersion == "batch/v1" && owner.Kind == "Job" && owner.Name == recipe.ResourceName && a.Job != nil && owner.UID == a.Job.UID {
			return nil
		}
	}
	if a.LocalCleanupState != "Complete" {
		return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
			current.LocalCleanupState = "Complete"
			return nil
		})
	}
	return nil
}
func (r *Reconciler) recoverNativeExports(ctx context.Context, c *appsv1.ChainNode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Status.SnapshotExportAuthority == nil && !nativeConfigured(c) && !controllerutil.ContainsFinalizer(c, exportprotocol.AuthorityFinalizer) {
		return nil
	}
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	mergeSnapshotExportOwnedStatus(c, fresh)
	refreshSnapshotExportResourceVersion(c, fresh)
	var pending error
	collect := func(reason, id string, err error) error {
		classified := nativeLocal(reason, id, err)
		var local *nativeOperationError
		if classified != nil && !errors.As(classified, &local) {
			return classified
		}
		if pending == nil {
			pending = classified
		}
		return nil
	}
	if err := collect("RecoveryDeferred", "", r.cleanupNativeOrphans(ctx, client.ObjectKeyFromObject(c))); err != nil {
		return err
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: c.Namespace}, namespace); err != nil {
		return nativeLocal("RecoveryDeferred", "", err)
	}
	stopping := !c.DeletionTimestamp.IsZero() || !namespace.DeletionTimestamp.IsZero()
	if err := collect("RecoveryDeferred", "", r.reconcileNativeAcknowledgements(ctx, c)); err != nil {
		return err
	}
	if auth := c.Status.SnapshotExportAuthority; auth != nil {
		if auth.Version != 1 {
			return nativeLocal("InvalidAuthority", "", fmt.Errorf("unsupported native authority version"))
		}
		attempts := append([]appsv1.SnapshotExportAttempt(nil), auth.Attempts...)
		for _, observed := range attempts {
			if err := ctx.Err(); err != nil {
				return err
			}
			fresh, err := r.nativeRead(ctx, c)
			if err != nil {
				return err
			}
			mergeSnapshotExportOwnedStatus(c, fresh)
			refreshSnapshotExportResourceVersion(c, fresh)
			a := exportprotocol.FindAttempt(c, observed.Sequence, observed.ID)
			if a == nil {
				continue
			}
			err = r.recoverNativeAttempt(ctx, c, a.DeepCopy(), stopping, !namespace.DeletionTimestamp.IsZero())
			if err := collect("RecoveryDeferred", a.ExportID, err); err != nil {
				return err
			}
		}
	}
	if !stopping {
		if err := collect("RecoveryDeferred", "", r.compactNativeAttempts(ctx, c)); err != nil {
			return err
		}
	}
	if stopping && controllerutil.ContainsFinalizer(c, exportprotocol.AuthorityFinalizer) {
		fresh, err := r.nativeRead(ctx, c)
		if err != nil {
			return err
		}
		ready := pending == nil
		if auth := fresh.Status.SnapshotExportAuthority; auth != nil {
			for _, a := range auth.Attempts {
				if !exportprotocol.Terminal(&a) || a.LocalCleanupState != "Complete" || (a.CleanupIssuer != "" && !a.CleanupComplete && a.CleanupState == "") {
					ready = false
					continue
				}
				gone, err := r.nativeCleanupResourcesGone(ctx, fresh, &a)
				if err != nil {
					if err := collect("RecoveryDeferred", a.ExportID, err); err != nil {
						return err
					}
					ready = false
				}
				if !gone {
					ready = false
				}
			}
		}
		if ready {
			controllerutil.RemoveFinalizer(fresh, exportprotocol.AuthorityFinalizer)
			if err := r.Update(ctx, fresh); err != nil {
				return nativeLocal("RecoveryDeferred", "", err)
			}
			*c = *fresh
		}
	}
	if pending != nil {
		return pending
	}
	if !stopping {
		return r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportDegraded, "", "")
	}
	return nil
}
func (r *Reconciler) recoverNativeAttempt(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, stopping, namespaceStopping bool) error {
	recipe, err := exportprotocol.DecodeRecipe(a)
	if err != nil {
		return nativeLocal("InvalidAuthority", a.ExportID, err)
	}
	if !exportprotocol.Terminal(a) {
		cancelled := stopping || !nativeConfigured(c)
		if !cancelled {
			cfg := c.Spec.Persistence.Snapshots.ExportTarball
			destination, err := newSnapshotExportStatus(c, &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: recipe.Snapshot.Name, UID: recipe.Snapshot.UID}})
			cancelled = err != nil || !nativeDestinationEqual(destination.Destination, recipe.Destination) || cfg.GetCompression() != recipe.Compression || cfg.S3.GetSizeLimit() != recipe.SizeLimit || cfg.S3.GetPartSize() != recipe.PartSize || cfg.S3.GetChunkSize() != recipe.ChunkSize || cfg.S3.GetBufferSize() != recipe.BufferSize || cfg.S3.GetConcurrentJobs() != recipe.ConcurrentJobs || cfg.DeleteWhenExpired() != recipe.DeleteOnExpire
		}
		if cancelled {
			if err := r.invalidateNativeAttempt(ctx, c, a, "PolicyOrOwnerChanged"); err != nil {
				return err
			}
		} else if _, err := r.nativeSource(ctx, recipe, a.Phase != "Reserved", a); err != nil {
			var lost *nativeIdentityError
			if !errors.As(err, &lost) {
				return nativeLocal("RecoveryDeferred", a.ExportID, err)
			}
			if err := r.invalidateNativeAttempt(ctx, c, a, "SourceBindingChanged"); err != nil {
				return err
			}
		} else if err := r.nativeCredentialIdentity(ctx, recipe); err != nil {
			var lost *nativeIdentityError
			if !errors.As(err, &lost) {
				return nativeLocal("RecoveryDeferred", a.ExportID, err)
			}
			if err := r.invalidateNativeAttempt(ctx, c, a, "ExecutionIdentityChanged"); err != nil {
				return err
			}
		} else {
			switch a.Phase {
			case "Reserved":
				return r.holdNativeSource(ctx, c, a, recipe)
			case "Held":
				if !a.JobCreateIssued {
					return r.issueNativeResource(ctx, c, a, recipe, true)
				}
				return r.bindNativeResources(ctx, c, a, recipe)
			case "ResourcesRequested":
				return r.bindNativeResources(ctx, c, a, recipe)
			case "Uploading":
				return r.publishNativeArchive(ctx, c, a, recipe)
			default:
				return nativeLocal("InvalidAuthority", a.ExportID, fmt.Errorf("unsupported native attempt phase"))
			}
		}
		fresh, err := r.nativeRead(ctx, c)
		if err != nil {
			return err
		}
		current := exportprotocol.FindAttempt(fresh, a.Sequence, a.ID)
		if current == nil {
			return nativeLocal("InvalidAuthority", a.ExportID, fmt.Errorf("terminal authority missing"))
		}
		a = current.DeepCopy()
	}
	if stopping && !a.CleanupComplete && a.CleanupState == "" {
		reason := "OwnerDeleting"
		if namespaceStopping {
			reason = "NamespaceTerminating"
		}
		if err := r.stopNativeCleanup(ctx, c, a, "Abandoned", reason); err != nil {
			return err
		}
		fresh, err := r.nativeRead(ctx, c)
		if err != nil {
			return err
		}
		a = exportprotocol.FindAttempt(fresh, a.Sequence, a.ID).DeepCopy()
	}
	if err := r.cleanupNativeAttempt(ctx, c, a, recipe); err != nil {
		return err
	}
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	a = exportprotocol.FindAttempt(fresh, a.Sequence, a.ID).DeepCopy()
	if stopping {
		return r.cleanupNativeDeletionResources(ctx, c, a, recipe)
	}
	if err := r.reconcileNativeDeletion(ctx, c, a, recipe); err != nil {
		return err
	}
	if current := exportprotocol.FindAttempt(c, a.Sequence, a.ID); current != nil && current.CleanupState == "Failed" {
		return nativeLocal("CleanupFailed", a.ExportID, fmt.Errorf("%s", current.CleanupReason))
	}
	return nil
}
func nativeDestinationEqual(a, b appsv1.SnapshotExportDestination) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

func (r *Reconciler) cleanupNativeOrphans(ctx context.Context, key client.ObjectKey) error {
	if r.APIReader == nil {
		return fmt.Errorf("native orphan recovery requires uncached reader")
	}
	var pending error
	// Labels attribute cleanup; they never reconstruct worker authority.
	for _, list := range []client.ObjectList{&batchv1.JobList{}, &corev1.PersistentVolumeClaimList{}} {
		if err := r.APIReader.List(ctx, list, client.InNamespace(key.Namespace), client.MatchingLabels{exportprotocol.OwnerNameLabel: key.Name}); err != nil {
			return err
		}
		var objects []client.Object
		switch l := list.(type) {
		case *batchv1.JobList:
			for i := range l.Items {
				objects = append(objects, &l.Items[i])
			}
		case *corev1.PersistentVolumeClaimList:
			for i := range l.Items {
				objects = append(objects, &l.Items[i])
			}
		}
		for _, obj := range objects {
			labels := obj.GetLabels()
			c, err := r.nativeOrphanAuthority(ctx, key)
			if err != nil {
				return err
			}
			active := false
			if c != nil && labels[exportprotocol.OwnerUIDLabel] == string(c.UID) && c.Status.SnapshotExportAuthority != nil {
				for _, a := range c.Status.SnapshotExportAuthority.Attempts {
					if a.ID == labels[exportprotocol.AttemptLabel] {
						active = true
						break
					}
				}
			}
			if !active {
				if controllerutil.ContainsFinalizer(obj, exportprotocol.AttemptFinalizer) {
					if err := r.deleteNativeObject(ctx, obj); err != nil {
						return err
					}
				}
				remaining := obj.DeepCopyObject().(client.Object)
				if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), remaining); err == nil {
					pending = nativeLocal("RecoveryDeferred", "export-"+labels[exportprotocol.AttemptLabel], fmt.Errorf("native orphan %s remains after local cleanup", obj.GetName()))
				} else if !apierrors.IsNotFound(err) {
					return err
				}
			}
		}
	}
	snapshots := &snapshotv1.VolumeSnapshotList{}
	if err := r.APIReader.List(ctx, snapshots, client.InNamespace(key.Namespace)); err != nil {
		return err
	}
	for i := range snapshots.Items {
		s := &snapshots.Items[i]
		if s.Annotations[exportprotocol.HoldAnnotation] == "" {
			continue
		}
		holders, err := nativeHolders(s)
		if err != nil {
			return err
		}
		changed := false
		for id, holder := range holders {
			parts := strings.Split(holder, "/")
			if len(parts) != 5 || parts[0] != key.Namespace || parts[1] != key.Name {
				continue
			}
			c, err := r.nativeOrphanAuthority(ctx, key)
			if err != nil {
				return err
			}
			active := false
			if c != nil && parts[2] == string(c.UID) {
				seq, _ := strconv.ParseInt(parts[3], 10, 64)
				a := exportprotocol.FindAttempt(c, seq, id)
				active = a != nil && !exportprotocol.Terminal(a)
			}
			if !active {
				delete(holders, id)
				changed = true
			}
		}
		if changed {
			if len(holders) == 0 {
				delete(s.Annotations, exportprotocol.HoldAnnotation)
				controllerutil.RemoveFinalizer(s, exportprotocol.SourceFinalizer)
			} else {
				b, _ := json.Marshal(holders)
				s.Annotations[exportprotocol.HoldAnnotation] = string(b)
			}
			if err := r.Update(ctx, s); err != nil {
				return err
			}
		}
	}
	return pending
}

func (r *Reconciler) finishNativeSnapshotMarker(ctx context.Context, c *appsv1.ChainNode, s *snapshotv1.VolumeSnapshot) error {
	e := snapshotExportFor(c, s)
	if e == nil || e.ProvenanceVersion != 1 || e.Phase != appsv1.SnapshotExportPhaseUploaded {
		return nil
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	if s.Annotations[controllers.AnnotationExportingTarball] == tarballFinished {
		return nil
	}
	s.Annotations[controllers.AnnotationExportingTarball] = tarballFinished
	return r.Update(ctx, s)
}

func (r *Reconciler) nativeDeletionRequired(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) (bool, error) {
	if !recipe.DeleteOnExpire {
		return false, nil
	}
	if !c.DeletionTimestamp.IsZero() {
		return false, nil
	}
	s := &snapshotv1.VolumeSnapshot{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: recipe.Namespace, Name: recipe.Snapshot.Name}, s)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if s.UID != recipe.Snapshot.UID || !s.DeletionTimestamp.IsZero() {
		return true, nil
	}
	export := snapshotExportByID(c, a.ExportID)
	return export != nil && export.Phase == appsv1.SnapshotExportPhaseDeleting, nil
}
func (r *Reconciler) reconcileNativeDeletion(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	if a.CleanupComplete || a.CleanupState != "" {
		if err := r.cleanupNativeDeletionResources(ctx, c, a, recipe); err != nil {
			return err
		}
		if a.CleanupState == "Failed" {
			return nativeLocal("CleanupFailed", a.ExportID, fmt.Errorf("%s", a.CleanupReason))
		}
		return nil
	}
	if !c.DeletionTimestamp.IsZero() {
		return nil
	}
	if a.Phase != "Succeeded" {
		return nil
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: c.Namespace}, namespace); err != nil {
		return err
	}
	if !namespace.DeletionTimestamp.IsZero() {
		return nil
	}
	key := client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}
	required, err := r.nativeDeletionRequired(ctx, c, a, recipe)
	if err != nil || !required {
		return err
	}
	if err := r.nativeCredentialIdentity(ctx, recipe); err != nil {
		var unavailable *nativeIdentityError
		if errors.As(err, &unavailable) {
			if stopErr := r.stopNativeCleanup(ctx, c, a, "Failed", "ExecutionIdentityUnavailable"); stopErr != nil {
				return stopErr
			}
		}
		return nativeLocal("ExecutionIdentityUnavailable", a.ExportID, err)
	}
	if a.CleanupIssuer == "" {
		issuer := string(uuid.NewUUID())
		err := r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, fresh *appsv1.ChainNode) error {
			if current.CleanupIssuer != "" || current.CleanupState != "" || current.Phase != "Succeeded" || !fresh.DeletionTimestamp.IsZero() {
				return fmt.Errorf("native cleanup issuance denied")
			}
			e := snapshotExportByID(fresh, current.ExportID)
			if e == nil {
				return fmt.Errorf("native cleanup catalog missing")
			}
			current.CleanupIssuer = issuer
			now := metav1.Now()
			current.CleanupIssuedAt = &now
			current.RemoteCleanupState = "Required"
			e.RemoteCleanupState = "Required"
			e.Phase = appsv1.SnapshotExportPhaseDeleting
			return nil
		})
		if err != nil {
			return err
		}
		err = r.Create(ctx, datasnapshot.NativeCleanupJob(a, recipe))
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err) {
			fresh, readErr := r.nativeRead(ctx, c)
			if readErr != nil {
				return readErr
			}
			current := exportprotocol.FindAttempt(fresh, a.Sequence, a.ID)
			if stopErr := r.stopNativeCleanup(ctx, c, current, "Failed", "CleanupCreateRejected"); stopErr != nil {
				return stopErr
			}
			return nativeLocal("CleanupFailed", a.ExportID, err)
		}
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	j := &batchv1.Job{}
	if err := r.APIReader.Get(ctx, key, j); apierrors.IsNotFound(err) {
		if a.CleanupIssuedAt != nil && time.Since(a.CleanupIssuedAt.Time) >= 2*time.Minute {
			return r.stopNativeCleanup(ctx, c, a, "Failed", "IssuedCleanupMissing")
		}
		return nil
	} else if err != nil {
		return err
	}
	if err := datasnapshot.ValidateNativeCleanupJob(j, a, recipe); err != nil {
		return r.stopNativeCleanup(ctx, c, a, "Failed", "CleanupJobChanged")
	}
	for _, condition := range j.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return r.stopNativeCleanup(ctx, c, a, "Failed", "CleanupJobFailed")
		}
	}
	if a.CleanupJob == nil {
		return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
			ref := identity(j)
			current.CleanupJob = &ref
			return nil
		})
	}
	if j.UID != a.CleanupJob.UID {
		return r.stopNativeCleanup(ctx, c, a, "Failed", "CleanupJobUIDChanged")
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels{exportprotocol.OwnerUIDLabel: string(c.UID), exportprotocol.AttemptLabel: a.ID}); err != nil {
		return err
	}
	var selected *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if nativePodOwnedBy(p, j) {
			if selected != nil {
				return r.stopNativeCleanup(ctx, c, a, "Failed", "ReplacementCleanupPod")
			}
			selected = p
		}
	}
	if selected == nil {
		return nil
	}
	if !selected.DeletionTimestamp.IsZero() || datasnapshot.ValidateNativeCleanupPodSpec(&selected.Spec, a, recipe) != nil {
		return r.stopNativeCleanup(ctx, c, a, "Failed", "AdmittedCleanupPodChanged")
	}
	if a.CleanupPod == nil {
		return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, _ *appsv1.ChainNode) error {
			ref := identity(selected)
			current.CleanupPod = &ref
			return nil
		})
	}
	if selected.UID != a.CleanupPod.UID {
		return r.stopNativeCleanup(ctx, c, a, "Failed", "CleanupPodUIDChanged")
	}
	complete := false
	for _, cond := range j.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return r.stopNativeCleanup(ctx, c, a, "Failed", "CleanupJobFailed")
		}
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			complete = true
		}
	}
	if !complete {
		return nil
	}
	if selected.Status.Phase != corev1.PodSucceeded || len(selected.Status.ContainerStatuses) != 1 || selected.Status.ContainerStatuses[0].Name != "exporter" || selected.Status.ContainerStatuses[0].State.Terminated == nil || selected.Status.ContainerStatuses[0].State.Terminated.ExitCode != 0 || selected.Status.ContainerStatuses[0].RestartCount != 0 {
		return r.stopNativeCleanup(ctx, c, a, "Failed", "CleanupAcknowledgementMissing")
	}
	return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, fresh *appsv1.ChainNode) error {
		e := snapshotExportByID(fresh, current.ExportID)
		if e == nil || e.Phase != appsv1.SnapshotExportPhaseDeleting {
			return fmt.Errorf("native cleanup publication denied")
		}
		if current.CleanupState != "" || !fresh.DeletionTimestamp.IsZero() {
			return fmt.Errorf("native cleanup stopped")
		}
		current.CleanupComplete = true
		current.RemoteCleanupState = "Complete"
		e.Phase = appsv1.SnapshotExportPhaseDeleted
		e.RemoteCleanupState = "Complete"
		return nil
	})
}

func (r *Reconciler) compactNativeAttempts(ctx context.Context, c *appsv1.ChainNode) error {
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	auth := fresh.Status.SnapshotExportAuthority
	if auth == nil {
		return nil
	}
	kept := make([]appsv1.SnapshotExportAttempt, 0, len(auth.Attempts))
	prune := map[string]bool{}
	changed := false
	for _, a := range auth.Attempts {
		recipe, err := exportprotocol.DecodeRecipe(&a)
		if err != nil {
			return err
		}
		e := snapshotExportByID(fresh, a.ExportID)
		removable := exportprotocol.Terminal(&a) && a.LocalCleanupState == "Complete" && a.RemoteCleanupState == "Complete" && a.CleanupState == ""
		sourceGone, err := r.nativeSourceGone(ctx, recipe.Namespace, recipe.Snapshot)
		if err != nil {
			return err
		}
		if a.Phase == "Succeeded" {
			removable = removable && e != nil && (e.Archive != nil || a.CleanupComplete) && (!recipe.DeleteOnExpire || a.CleanupComplete && sourceGone)
		}
		if removable {
			if a.Phase == "Invalidated" && !sourceGone {
				if err := r.failNativeSnapshotMarker(ctx, c, &a, recipe); err != nil {
					return err
				}
			}
			gone, err := r.nativeCleanupResourcesGone(ctx, c, &a)
			if err != nil {
				return err
			}
			removable = removable && gone
		}
		if removable {
			changed = true
			if sourceGone && (a.Phase == "Invalidated" || a.CleanupComplete) {
				prune[a.ExportID] = true
			}
		} else {
			kept = append(kept, a)
		}
	}
	exports := make([]appsv1.SnapshotExportStatus, 0, len(fresh.Status.SnapshotExports))
	for _, e := range fresh.Status.SnapshotExports {
		if e.ProvenanceVersion == 1 && e.Archive == nil && e.RemoteCleanupState == "Complete" && e.Phase == appsv1.SnapshotExportPhaseCleanupRequired {
			hasAttempt := false
			for _, a := range kept {
				if a.ExportID == e.ID {
					hasAttempt = true
					break
				}
			}
			if !hasAttempt {
				gone, err := r.nativeSourceGone(ctx, c.Namespace, appsv1.ExportResourceIdentity{Name: e.SnapshotName, UID: e.SnapshotUID})
				if err != nil {
					return err
				}
				if gone {
					prune[e.ID] = true
				}
			}
		}
		if prune[e.ID] {
			changed = true
		} else {
			exports = append(exports, e)
		}
	}
	if !changed {
		return nil
	}
	auth.Attempts = kept
	fresh.Status.SnapshotExports = exports
	return r.nativeSave(ctx, c, fresh)
}
func (r *Reconciler) nativeSourceGone(ctx context.Context, namespace string, ref appsv1.ExportResourceIdentity) (bool, error) {
	s := &snapshotv1.VolumeSnapshot{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, s); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return s.UID != ref.UID, nil
}

func (r *Reconciler) nativeOrphanAuthority(ctx context.Context, key client.ObjectKey) (*appsv1.ChainNode, error) {
	owner := &appsv1.ChainNode{}
	err := r.APIReader.Get(ctx, key, owner)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, &nativeOwnerError{err}
	}
	return owner, nil
}

func (r *Reconciler) failNativeSnapshotMarker(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	s := &snapshotv1.VolumeSnapshot{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: recipe.Namespace, Name: recipe.Snapshot.Name}, s); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if s.UID != recipe.Snapshot.UID || !s.DeletionTimestamp.IsZero() || s.Annotations[controllers.AnnotationExportingTarball] != "true" {
		return nil
	}
	// Read authority after the marker so a newer retry cannot inherit an older catalog view.
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	current := exportprotocol.FindAttempt(fresh, a.Sequence, a.ID)
	e := snapshotExportFor(fresh, s)
	if current == nil || current.Phase != "Invalidated" || current.ExportID != a.ExportID || e == nil || e.ProvenanceVersion != 1 || e.SnapshotUID != s.UID || e.ID != current.ExportID {
		return nil
	}
	s.Annotations[controllers.AnnotationExportingTarball] = tarballFailed
	return r.Update(ctx, s)
}
func (r *Reconciler) stopNativeCleanup(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, state, reason string) error {
	if a.CleanupComplete || a.CleanupState != "" {
		return nil
	}
	return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, fresh *appsv1.ChainNode) error {
		if !exportprotocol.Terminal(current) {
			return fmt.Errorf("cleanup stop requires terminal upload")
		}
		current.CleanupState = state
		current.CleanupReason = reason
		if state == "Failed" {
			apiMeta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{Type: appsv1.ConditionSnapshotExportDegraded, Status: metav1.ConditionTrue, Reason: "CleanupFailed", Message: current.ExportID + ": " + reason, ObservedGeneration: fresh.Generation})
		}
		if current.CleanupIssuer != "" || state == "Failed" {
			current.RemoteCleanupState = "Required"
		}
		if e := snapshotExportByID(fresh, current.ExportID); e != nil {
			e.RemoteCleanupState = current.RemoteCleanupState
			if current.RemoteCleanupState == "Required" {
				e.Message = current.ExportID + ": " + reason + "; deletion remains unproven"
			}
		}
		return nil
	})
}
func (r *Reconciler) cleanupNativeDeletionResources(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	key := client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}
	j := &batchv1.Job{}
	if err := r.APIReader.Get(ctx, key, j); err == nil {
		if a.CleanupJob != nil && a.CleanupJob.UID != j.UID {
			return fmt.Errorf("cleanup refuses substituted Job")
		}
		if a.CleanupJob == nil {
			if err := datasnapshot.ValidateNativeCleanupJob(j, a, recipe); err != nil {
				return err
			}
		}
		if err := r.deleteNativeObject(ctx, j); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if a.CleanupPod != nil && a.CleanupJob != nil {
		if err := r.deleteNativeSelectedPod(ctx, c.Namespace, a.CleanupPod, key.Name, a.CleanupJob.UID); err != nil {
			return err
		}
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels{exportprotocol.OwnerUIDLabel: string(c.UID), exportprotocol.AttemptLabel: a.ID}); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		owner := metav1.GetControllerOf(p)
		if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.Name != key.Name {
			continue
		}
		uid := j.UID
		if a.CleanupJob != nil {
			uid = a.CleanupJob.UID
		}
		if uid == "" || owner.UID != uid {
			return fmt.Errorf("cleanup refuses unverified Pod owner")
		}
		if a.CleanupPod != nil && a.CleanupPod.Name == p.Name && a.CleanupPod.UID != p.UID {
			return fmt.Errorf("cleanup refuses substituted Pod")
		}
		if err := r.deleteNativeObject(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
func (r *Reconciler) nativeCleanupResourcesGone(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt) (bool, error) {
	recipe, err := exportprotocol.DecodeRecipe(a)
	if err != nil {
		return false, err
	}
	for _, obj := range []client.Object{&batchv1.Job{}, &corev1.PersistentVolumeClaim{}} {
		if err := r.APIReader.Get(ctx, nativeKey(recipe), obj); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	for _, ref := range []*appsv1.ExportResourceIdentity{a.Pod, a.CleanupPod} {
		if ref == nil {
			continue
		}
		p := &corev1.Pod{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: ref.Name}, p); err == nil {
			if p.UID == ref.UID {
				return false, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	job := &batchv1.Job{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}, job); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels{exportprotocol.OwnerUIDLabel: string(c.UID), exportprotocol.AttemptLabel: a.ID}); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}
func (r *Reconciler) reconcileNativeAcknowledgements(ctx context.Context, c *appsv1.ChainNode) error {
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	applied := map[string]bool{}
	changed := false
	for _, token := range strings.Split(fresh.Annotations[exportprotocol.CleanupAckAnnotation], ",") {
		id := strings.TrimSpace(token)
		if id == "" {
			continue
		}
		e := snapshotExportByID(fresh, id)
		if e == nil || e.ProvenanceVersion != 1 {
			continue
		}
		if fresh.Status.SnapshotExportAuthority == nil {
			continue
		}
		for i := range fresh.Status.SnapshotExportAuthority.Attempts {
			a := &fresh.Status.SnapshotExportAuthority.Attempts[i]
			if a.ExportID != id {
				continue
			}
			if a.CleanupState == "Acknowledged" && e.Phase == appsv1.SnapshotExportPhaseAcknowledged {
				applied[id] = true
				continue
			}
			eligible := a.CleanupState == "Failed" || (a.Phase == "Invalidated" && a.RemoteCleanupState == "Required" && a.CleanupState == "")
			if !eligible {
				continue
			}
			a.CleanupState = "Acknowledged"
			a.CleanupReason = "OperatorAcknowledgedWithoutDeletionProof"
			a.RemoteCleanupState = "Required"
			e.RemoteCleanupState = "Required"
			e.Phase = appsv1.SnapshotExportPhaseAcknowledged
			e.Message = id + ": cleanup acknowledged without deletion proof"
			applied[id] = true
			changed = true
		}
	}
	if changed {
		if err := r.nativeSave(ctx, c, fresh); err != nil {
			return err
		}
	}
	if len(applied) == 0 {
		return nil
	}
	fresh, err = r.nativeRead(ctx, c)
	if err != nil {
		return err
	}
	remaining := []string{}
	for _, token := range strings.Split(fresh.Annotations[exportprotocol.CleanupAckAnnotation], ",") {
		if !applied[strings.TrimSpace(token)] {
			remaining = append(remaining, token)
		}
	}
	if len(remaining) == 0 {
		delete(fresh.Annotations, exportprotocol.CleanupAckAnnotation)
	} else {
		fresh.Annotations[exportprotocol.CleanupAckAnnotation] = strings.Join(remaining, ",")
	}
	if err := r.Update(ctx, fresh); err != nil {
		return err
	}
	mergeSnapshotExportOwnedStatus(c, fresh)
	refreshSnapshotExportResourceVersion(c, fresh)
	c.Annotations = fresh.Annotations
	return nil
}

func (r *Reconciler) deleteNativeSelectedPod(ctx context.Context, namespace string, ref *appsv1.ExportResourceIdentity, jobName string, jobUID types.UID) error {
	p := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, p); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if p.UID != ref.UID {
		return fmt.Errorf("native selected Pod UID changed")
	}
	owner := metav1.GetControllerOf(p)
	if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.Name != jobName || owner.UID != jobUID {
		return fmt.Errorf("native selected Pod owner changed")
	}
	return r.deleteNativeObject(ctx, p)
}
