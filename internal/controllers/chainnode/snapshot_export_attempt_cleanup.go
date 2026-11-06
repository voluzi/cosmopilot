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
	// Repair precedes LocalCleanupState: a reservation requires it and then reads the durable failure count.
	if a.Phase == "Invalidated" {
		if err := r.failNativeSnapshotMarker(ctx, c, a, recipe); err != nil {
			return err
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
			mergeNativeFinalizerWrite(c, fresh)
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
		} else if a.Phase != "Uploading" && time.Since(a.CreatedAt.Time) > nativePreauthorizationDeadline {
			if err := r.invalidateNativeAttempt(ctx, c, a, "AuthorizationDeadlineExceeded"); err != nil {
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
		if a, err = r.nativeCurrentAttempt(ctx, c, a); err != nil {
			return err
		}
	}
	if stopping && !a.CleanupComplete && (a.CleanupState == "" || a.CleanupState == "Failed") {
		reason := "OwnerDeleting"
		if namespaceStopping {
			reason = "NamespaceTerminating"
		}
		if err := r.stopNativeCleanup(ctx, c, a, "Abandoned", reason); err != nil {
			return err
		}
		if a, err = r.nativeCurrentAttempt(ctx, c, a); err != nil {
			return err
		}
	}
	if err := r.cleanupNativeAttempt(ctx, c, a, recipe); err != nil {
		return err
	}
	if a, err = r.nativeCurrentAttempt(ctx, c, a); err != nil {
		return err
	}
	if stopping {
		return r.cleanupNativeDeletionResources(ctx, c, a, a.CleanupAttempt)
	}
	if err := r.reconcileNativeDeletion(ctx, c, a, recipe); err != nil {
		return err
	}
	if current := exportprotocol.FindAttempt(c, a.Sequence, a.ID); current != nil && current.CleanupState == "Failed" {
		return nativeCleanupFailure(current)
	}
	return nil
}
func (r *Reconciler) nativeCurrentAttempt(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt) (*appsv1.SnapshotExportAttempt, error) {
	fresh, err := r.nativeRead(ctx, c)
	if err != nil {
		return nil, err
	}
	current := exportprotocol.FindAttempt(fresh, a.Sequence, a.ID)
	if current == nil {
		return nil, nativeLocal("InvalidAuthority", a.ExportID, fmt.Errorf("terminal authority missing"))
	}
	return current.DeepCopy(), nil
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
	var candidates []client.Object
	for _, list := range []client.ObjectList{&batchv1.JobList{}, &corev1.PersistentVolumeClaimList{}} {
		if err := r.APIReader.List(ctx, list, client.InNamespace(key.Namespace), client.MatchingLabels{exportprotocol.OwnerNameLabel: key.Name}); err != nil {
			return err
		}
		switch l := list.(type) {
		case *batchv1.JobList:
			for i := range l.Items {
				candidates = append(candidates, &l.Items[i])
			}
		case *corev1.PersistentVolumeClaimList:
			for i := range l.Items {
				candidates = append(candidates, &l.Items[i])
			}
		}
	}
	snapshots := &snapshotv1.VolumeSnapshotList{}
	if err := r.APIReader.List(ctx, snapshots, client.InNamespace(key.Namespace)); err != nil && !apiMeta.IsNoMatchError(err) {
		return err
	}
	// Authority is read after listing: a reservation visible in any candidate is also visible here.
	c, err := r.nativeOrphanAuthority(ctx, key)
	if err != nil {
		return err
	}
	var pending error
	// Labels attribute cleanup; they never reconstruct worker authority.
	for _, obj := range candidates {
		labels := obj.GetLabels()
		active := false
		if c != nil && labels[exportprotocol.OwnerUIDLabel] == string(c.UID) && c.Status.SnapshotExportAuthority != nil {
			for _, a := range c.Status.SnapshotExportAuthority.Attempts {
				if a.ID == labels[exportprotocol.AttemptLabel] {
					active = !nativeRetiredCleanupJob(&a, obj.GetName())
					break
				}
			}
		}
		if active {
			continue
		}
		if controllerutil.ContainsFinalizer(obj, exportprotocol.AttemptFinalizer) {
			if err := r.deleteNativeObject(ctx, obj); err != nil {
				return err
			}
		}
		remaining := obj.DeepCopyObject().(client.Object)
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), remaining); err == nil {
			if pending == nil {
				pending = nativeLocal("RecoveryDeferred", "export-"+labels[exportprotocol.AttemptLabel], fmt.Errorf("native orphan %s remains after local cleanup", obj.GetName()))
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
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
	if s.Annotations[controllers.AnnotationExportingTarball] == tarballNativeFinished {
		return nil
	}
	s.Annotations[controllers.AnnotationExportingTarball] = tarballNativeFinished
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
	if a.CleanupComplete || a.CleanupState == "Acknowledged" || a.CleanupState == "Abandoned" {
		return r.cleanupNativeDeletionResources(ctx, c, a, a.CleanupAttempt)
	}
	if a.Phase != "Succeeded" {
		return nil
	}
	retired := a.CleanupAttempt - 1
	if a.CleanupState == "Failed" {
		retired = a.CleanupAttempt
	}
	if err := r.cleanupNativeDeletionResources(ctx, c, a, retired); err != nil {
		return err
	}
	if !c.DeletionTimestamp.IsZero() {
		return nil
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: c.Namespace}, namespace); err != nil {
		return err
	}
	if !namespace.DeletionTimestamp.IsZero() {
		return nil
	}
	if a.CleanupState == "Failed" {
		return r.retryNativeCleanup(ctx, c, a, recipe)
	}
	if a.CleanupAttempt > 0 {
		return r.observeNativeCleanup(ctx, c, a, recipe)
	}
	required, err := r.nativeDeletionRequired(ctx, c, a, recipe)
	if err != nil || !required {
		return err
	}
	return r.issueNativeCleanup(ctx, c, a, recipe)
}
func (r *Reconciler) retryNativeCleanup(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	if a.CleanupAttempt >= exportprotocol.MaxCleanupAttempts || r.snapshotDeleteTime().Before(a.CleanupIssuedAt.Add(snapshotDeleteRetryDelay(a.CleanupAttempt))) {
		return nativeCleanupFailure(a)
	}
	gone, err := r.nativeCleanupResourcesGone(ctx, c, a)
	if err != nil {
		return err
	}
	if !gone {
		return nativeCleanupFailure(a)
	}
	return r.issueNativeCleanup(ctx, c, a, recipe)
}
func nativeCleanupFailure(a *appsv1.SnapshotExportAttempt) error {
	if a.CleanupAttempt >= exportprotocol.MaxCleanupAttempts {
		return nativeLocal("CleanupFailed", a.ExportID, fmt.Errorf("%s; %d cleanup attempts exhausted, acknowledge to release", a.CleanupReason, a.CleanupAttempt))
	}
	return nativeLocal("CleanupFailed", a.ExportID, errors.New(a.CleanupReason))
}

// issueNativeCleanup persists the next numbered issuance, then performs its single Job POST.
func (r *Reconciler) issueNativeCleanup(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	if err := r.nativeCleanupIdentity(ctx, c, recipe); err != nil {
		if requestErr := r.requestNativeCleanup(ctx, c, a); requestErr != nil {
			return requestErr
		}
		return nativeLocal("ExecutionIdentityUnavailable", a.ExportID, err)
	}
	issued := a.DeepCopy()
	issued.CleanupAttempt++
	issuer := string(uuid.NewUUID())
	err := r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, fresh *appsv1.ChainNode) error {
		if current.Phase != "Succeeded" || current.CleanupComplete || (current.CleanupState != "" && current.CleanupState != "Failed") || !fresh.DeletionTimestamp.IsZero() {
			return fmt.Errorf("native cleanup issuance denied")
		}
		e := snapshotExportByID(fresh, current.ExportID)
		if e == nil {
			return fmt.Errorf("native cleanup catalog missing")
		}
		now := metav1.NewTime(r.snapshotDeleteTime())
		current.CleanupAttempt = issued.CleanupAttempt
		current.CleanupIssuer = issuer
		current.CleanupIssuedAt = &now
		current.CleanupJob, current.CleanupPod = nil, nil
		current.CleanupState, current.CleanupReason = "", ""
		current.RemoteCleanupState = "Required"
		e.RemoteCleanupState = "Required"
		e.Phase = appsv1.SnapshotExportPhaseDeleting
		return nil
	})
	if err != nil {
		return err
	}
	// Only this caller owns the freshly persisted random issuance witness.
	err = r.Create(ctx, datasnapshot.NativeCleanupJob(issued, recipe))
	if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err) {
		current, readErr := r.nativeCurrentAttempt(ctx, c, a)
		if readErr != nil {
			return readErr
		}
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

// requestNativeCleanup records a deletion duty whose prerequisites are unavailable, without consuming an issuance.
func (r *Reconciler) requestNativeCleanup(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt) error {
	if e := snapshotExportByID(c, a.ExportID); e != nil && e.Phase == appsv1.SnapshotExportPhaseDeleting && e.RemoteCleanupState == "Required" && a.RemoteCleanupState == "Required" {
		return nil
	}
	return r.nativeTransition(ctx, c, a, func(current *appsv1.SnapshotExportAttempt, fresh *appsv1.ChainNode) error {
		e := snapshotExportByID(fresh, current.ExportID)
		if e == nil {
			return fmt.Errorf("native cleanup catalog missing")
		}
		current.RemoteCleanupState = "Required"
		e.RemoteCleanupState = "Required"
		e.Phase = appsv1.SnapshotExportPhaseDeleting
		e.Message = current.ExportID + ": cleanup deferred; deletion remains unproven"
		return nil
	})
}
func (r *Reconciler) observeNativeCleanup(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, recipe exportprotocol.Recipe) error {
	unobserved := func() bool { return r.snapshotDeleteTime().Sub(a.CleanupIssuedAt.Time) >= 2*time.Minute }
	j := &batchv1.Job{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, a.CleanupAttempt)}, j); apierrors.IsNotFound(err) {
		if unobserved() {
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
	if a.CleanupPod == nil && (nativeJobTerminal(j) || selected != nil && nativePodTerminated(selected)) {
		return r.stopNativeCleanup(ctx, c, a, "Failed", "CleanupWorkerTerminated")
	}
	if selected == nil {
		if unobserved() {
			return r.stopNativeCleanup(ctx, c, a, "Failed", "IssuedCleanupMissing")
		}
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
	changed := false
	for _, a := range auth.Attempts {
		recipe, err := exportprotocol.DecodeRecipe(&a)
		if err != nil {
			return err
		}
		source, err := r.nativeExactSource(ctx, recipe.Namespace, recipe.Snapshot.Name, recipe.Snapshot.UID)
		if err != nil {
			return err
		}
		removable := nativeAttemptCompactable(&a, snapshotExportByID(fresh, a.ExportID), recipe, source)
		if removable {
			if a.Phase == "Invalidated" && source != nil {
				if err := r.failNativeSnapshotMarker(ctx, c, &a, recipe); err != nil {
					return err
				}
			}
			if removable, err = r.nativeCleanupResourcesGone(ctx, c, &a); err != nil {
				return err
			}
		}
		if removable {
			changed = true
		} else {
			kept = append(kept, a)
		}
	}
	exports := make([]appsv1.SnapshotExportStatus, 0, len(fresh.Status.SnapshotExports))
	var released []appsv1.SnapshotExportStatus
	for _, e := range fresh.Status.SnapshotExports {
		a := nativeAttemptFor(kept, e.ID)
		release := nativeHistoryReleasable(&e, a)
		if release {
			source, err := r.nativeExactSource(ctx, c.Namespace, e.SnapshotName, e.SnapshotUID)
			if err != nil {
				return err
			}
			release = source == nil
		}
		if release && a != nil {
			if release, err = r.nativeCleanupResourcesGone(ctx, c, a); err != nil {
				return err
			}
		}
		if !release {
			exports = append(exports, e)
			continue
		}
		released = append(released, e)
		kept = nativeWithoutAttempt(kept, e.ID)
		changed = true
	}
	if !changed {
		return nil
	}
	auth.Attempts = kept
	fresh.Status.SnapshotExports = exports
	if err := r.nativeSave(ctx, c, fresh); err != nil {
		return err
	}
	for i := range released {
		r.recordNativeRelease(c, &released[i])
	}
	return nil
}

// nativeAttemptCompactable reports whether authority has no future automatic duty; its catalog entry may stay.
func nativeAttemptCompactable(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus, recipe exportprotocol.Recipe, source *snapshotv1.VolumeSnapshot) bool {
	if !exportprotocol.Terminal(a) || a.LocalCleanupState != "Complete" {
		return false
	}
	if a.CleanupState == "Acknowledged" {
		return true
	}
	if a.CleanupState != "" {
		return false
	}
	if a.Phase == "Invalidated" {
		return a.RemoteCleanupState == "Complete"
	}
	if e == nil {
		return false
	}
	if a.CleanupComplete {
		return source == nil
	}
	// A split result has no descriptor, so its envelope stays until the whole history entry is released.
	if a.RemoteCleanupState != "Complete" || recipe.DeleteOnExpire || e.Archive == nil {
		return false
	}
	return source == nil || source.Annotations[controllers.AnnotationExportingTarball] == tarballNativeFinished
}

// nativeHistoryReleasable reports whether a catalog entry carries no remaining deletion, retry or acknowledgement duty.
func nativeHistoryReleasable(e *appsv1.SnapshotExportStatus, a *appsv1.SnapshotExportAttempt) bool {
	if e.ProvenanceVersion != 1 {
		return false
	}
	if a != nil {
		if !exportprotocol.Terminal(a) || a.LocalCleanupState != "Complete" {
			return false
		}
		switch {
		case a.CleanupState == "Acknowledged", a.CleanupComplete:
			return true
		case a.CleanupState != "":
			return false
		case a.Phase == "Invalidated":
			return a.RemoteCleanupState == "Complete"
		}
		return !e.DeleteOnExpire && a.RemoteCleanupState == "Complete"
	}
	switch e.Phase {
	case appsv1.SnapshotExportPhaseAcknowledged, appsv1.SnapshotExportPhaseDeleted:
		return true
	case appsv1.SnapshotExportPhaseUploaded:
		return !e.DeleteOnExpire && e.RemoteCleanupState == "Complete"
	case appsv1.SnapshotExportPhaseCleanupRequired:
		return e.Archive == nil && e.RemoteCleanupState == "Complete"
	}
	return false
}

// evictNativeHistory drops the oldest entry without duties; its bucket objects stay untouched.
func (r *Reconciler) evictNativeHistory(ctx context.Context, c, fresh *appsv1.ChainNode) (bool, error) {
	auth := fresh.Status.SnapshotExportAuthority
	if auth == nil {
		return false, nil
	}
	for i := range fresh.Status.SnapshotExports {
		e := fresh.Status.SnapshotExports[i]
		a := nativeAttemptFor(auth.Attempts, e.ID)
		if !nativeHistoryReleasable(&e, a) {
			continue
		}
		if a != nil {
			gone, err := r.nativeCleanupResourcesGone(ctx, c, a)
			if err != nil {
				return false, err
			}
			if !gone {
				continue
			}
		}
		// The marker is written first: a crash here leaves extra history, never an unmarked source.
		if err := r.persistNativeProgress(ctx, fresh, &e, a); err != nil {
			return false, err
		}
		auth.Attempts = nativeWithoutAttempt(auth.Attempts, e.ID)
		fresh.Status.SnapshotExports = append(fresh.Status.SnapshotExports[:i:i], fresh.Status.SnapshotExports[i+1:]...)
		if err := r.nativeSave(ctx, c, fresh); err != nil {
			return false, err
		}
		r.recordNativeRelease(c, &e)
		return true, nil
	}
	return false, nil
}

// persistNativeProgress records a source's terminal bookkeeping before its last status record disappears.
func (r *Reconciler) persistNativeProgress(ctx context.Context, fresh *appsv1.ChainNode, e *appsv1.SnapshotExportStatus, a *appsv1.SnapshotExportAttempt) error {
	s, err := r.nativeExactSource(ctx, fresh.Namespace, e.SnapshotName, e.SnapshotUID)
	if err != nil || s == nil || !s.DeletionTimestamp.IsZero() {
		return err
	}
	if latest := snapshotExportFor(fresh, s); latest == nil || latest.ID != e.ID {
		return nil
	}
	marker, failures := tarballNativeFailed, nativeFailedAttempts(s)
	if e.Archive != nil || (a != nil && a.Phase == "Succeeded") {
		marker = tarballNativeFinished
	} else if a != nil {
		failures = max(failures, int(a.UploadAttempt))
	}
	if s.Annotations[controllers.AnnotationExportingTarball] == marker && failures == nativeFailedAttempts(s) {
		return nil
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[controllers.AnnotationExportingTarball] = marker
	if marker == tarballNativeFailed {
		s.Annotations[controllers.AnnotationTarballExportAttempts] = strconv.Itoa(failures)
	}
	return r.Update(ctx, s)
}
func (r *Reconciler) recordNativeRelease(c *appsv1.ChainNode, e *appsv1.SnapshotExportStatus) {
	object := e.ObjectName
	if e.Archive != nil {
		object = e.Archive.ObjectKey
	}
	kind, outcome := corev1.EventTypeNormal, "retained in place"
	switch {
	case e.RemoteCleanupState == "Required":
		kind, outcome = corev1.EventTypeWarning, "never proven deleted"
	case e.Phase == appsv1.SnapshotExportPhaseDeleted:
		outcome = "deleted"
	case e.Archive == nil && e.Phase != appsv1.SnapshotExportPhaseUploaded:
		outcome = "never uploaded"
	}
	r.recorder.Eventf(c, kind, appsv1.ReasonSnapshotExportHistoryReleased, "Released export history %s: object %s in %s bucket %s %s", e.ID, object, e.Destination.Provider, e.Destination.Bucket, outcome)
}
func nativeAttemptFor(attempts []appsv1.SnapshotExportAttempt, exportID string) *appsv1.SnapshotExportAttempt {
	for i := range attempts {
		if attempts[i].ExportID == exportID {
			return &attempts[i]
		}
	}
	return nil
}
func nativeWithoutAttempt(attempts []appsv1.SnapshotExportAttempt, exportID string) []appsv1.SnapshotExportAttempt {
	kept := make([]appsv1.SnapshotExportAttempt, 0, len(attempts))
	for _, a := range attempts {
		if a.ExportID != exportID {
			kept = append(kept, a)
		}
	}
	return kept
}

// nativeExactSource returns the live source incarnation, or nil once it is gone or replaced.
func (r *Reconciler) nativeExactSource(ctx context.Context, namespace, name string, uid types.UID) (*snapshotv1.VolumeSnapshot, error) {
	s := &snapshotv1.VolumeSnapshot{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, s); apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if s.UID != uid {
		return nil, nil
	}
	return s, nil
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
	if s.UID != recipe.Snapshot.UID || !s.DeletionTimestamp.IsZero() {
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
	marker, failures := s.Annotations[controllers.AnnotationExportingTarball], nativeFailedAttempts(s)
	// Repair precedes LocalCleanupState, so an empty marker after it is an operator reset, not a lost write.
	lostMarker := marker == "" && current.LocalCleanupState != "Complete"
	staleCount := marker == tarballNativeFailed && failures < int(current.UploadAttempt)
	if marker != "true" && !lostMarker && !staleCount {
		return nil
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[controllers.AnnotationExportingTarball] = tarballNativeFailed
	s.Annotations[controllers.AnnotationTarballExportAttempts] = strconv.Itoa(max(failures, int(current.UploadAttempt)))
	if err := r.Update(ctx, s); err != nil {
		return err
	}
	if current.UploadAttempt >= tarballExportMaxAttempts {
		r.recorder.Eventf(c, corev1.EventTypeWarning, appsv1.ReasonTarballExportError, "Native export of snapshot %s failed %d times; %s", s.Name, current.UploadAttempt, tarballFailureAction(false))
	}
	return nil
}
func nativeFailedAttempts(s *snapshotv1.VolumeSnapshot) int {
	attempts, err := strconv.Atoi(s.Annotations[controllers.AnnotationTarballExportAttempts])
	if err != nil || attempts < 0 {
		return 0
	}
	return attempts
}

// nativeRetentionOnly reports a source with no export record and no cloud duty, so retention deletes only its CSI snapshot.
func nativeRetentionOnly(c *appsv1.ChainNode, s *snapshotv1.VolumeSnapshot) bool {
	if snapshotExportFor(c, s) != nil {
		return false
	}
	switch s.Annotations[controllers.AnnotationExportingTarball] {
	case tarballNativeFinished, tarballNativeFailed:
		return true
	case "":
		return nativeConfigured(c)
	}
	return false
}

// nativeRetryDue reports whether a failed native export still has automatic attempts left.
func nativeRetryDue(c *appsv1.ChainNode, s *snapshotv1.VolumeSnapshot) bool {
	if s.Annotations[controllers.AnnotationExportingTarball] != tarballNativeFailed || !nativeConfigured(c) || nativeFailedAttempts(s) >= tarballExportMaxAttempts {
		return false
	}
	expired, err := isSnapshotExpired(s)
	return err == nil && !expired
}
func (r *Reconciler) stopNativeCleanup(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, state, reason string) error {
	escalating := a.CleanupState == "Failed" && state != "Failed"
	if a.CleanupComplete || (a.CleanupState != "" && !escalating) {
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

// cleanupNativeDeletionResources removes issuances 1..through. Retired ordinals keep no recorded identity, so attribution labels bind them.
func (r *Reconciler) cleanupNativeDeletionResources(ctx context.Context, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt, through int32) error {
	if through < 1 {
		return nil
	}
	names := map[string]bool{}
	for attempt := int32(1); attempt <= through; attempt++ {
		name := exportprotocol.CleanupJobName(a.ID, attempt)
		names[name] = true
		j := &batchv1.Job{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, j); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if j.Labels[exportprotocol.OwnerUIDLabel] != string(c.UID) || j.Labels[exportprotocol.AttemptLabel] != a.ID {
			return fmt.Errorf("native cleanup resource attribution changed")
		}
		if a.CleanupJob != nil && a.CleanupJob.Name == name && a.CleanupJob.UID != j.UID {
			return fmt.Errorf("cleanup refuses substituted Job")
		}
		if err := r.deleteNativeObject(ctx, j); err != nil {
			return err
		}
	}
	if a.CleanupPod != nil && a.CleanupJob != nil && names[a.CleanupJob.Name] {
		if err := r.deleteNativeSelectedPod(ctx, c.Namespace, a.CleanupPod, a.CleanupJob.Name, a.CleanupJob.UID); err != nil {
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
		if owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || !names[owner.Name] {
			continue
		}
		if a.CleanupJob != nil && a.CleanupJob.Name == owner.Name && a.CleanupJob.UID != owner.UID {
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
func nativeRetiredCleanupJob(a *appsv1.SnapshotExportAttempt, name string) bool {
	for attempt := int32(1); attempt < a.CleanupAttempt; attempt++ {
		if name == exportprotocol.CleanupJobName(a.ID, attempt) {
			return true
		}
	}
	return false
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
	for attempt := int32(1); attempt <= exportprotocol.MaxCleanupAttempts; attempt++ {
		job := &batchv1.Job{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, attempt)}, job); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
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
			eligible := exportprotocol.Terminal(a) && !a.CleanupComplete && a.RemoteCleanupState == "Required" && (a.CleanupState == "" || a.CleanupState == "Failed")
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
