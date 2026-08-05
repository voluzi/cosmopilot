package chainnode

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/datasnapshot"
)

type snapshotExportReferenceUnavailableError struct {
	message string
}

var errSnapshotExportStatusUnavailable = errors.New("snapshot export status client is unavailable")

func (err *snapshotExportReferenceUnavailableError) Error() string {
	return err.message
}

func newSnapshotExportStatus(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) (appsv1.SnapshotExportStatus, error) {
	if chainNode.Spec.Persistence == nil || chainNode.Spec.Persistence.Snapshots == nil ||
		chainNode.Spec.Persistence.Snapshots.ExportTarball == nil {
		return appsv1.SnapshotExportStatus{}, fmt.Errorf("snapshot tarball export is not configured")
	}
	cfg := chainNode.Spec.Persistence.Snapshots.ExportTarball
	status := appsv1.SnapshotExportStatus{
		SnapshotName:   snapshot.Name,
		SnapshotUID:    snapshot.UID,
		ObjectName:     getTarballName(chainNode, snapshot),
		Phase:          appsv1.SnapshotExportPhaseUploading,
		Compression:    appsv1.TarballCompression(cfg.GetCompression()),
		DeleteOnExpire: cfg.DeleteWhenExpired(),
	}
	switch {
	case cfg.S3 != nil:
		status.Destination = appsv1.SnapshotExportDestination{
			Provider:       appsv1.SnapshotExportProviderS3,
			Bucket:         cfg.S3.Bucket,
			Region:         cfg.S3.Region,
			Endpoint:       cfg.S3.GetEndpoint(),
			ForcePathStyle: cfg.S3.ShouldForcePathStyle(),
		}
		if cfg.S3.CredentialsSecret != nil {
			status.Destination.CredentialsSecret = &appsv1.SnapshotExportSecretReference{Name: cfg.S3.CredentialsSecret.Name}
		}
		status.Destination.ServiceAccountName = ptr.Deref(cfg.S3.ServiceAccountName, "")
		status.SizeLimit = cfg.S3.GetSizeLimit()
		status.PartSize = cfg.S3.GetPartSize()
		status.ChunkSize = cfg.S3.GetChunkSize()
		status.BufferSize = cfg.S3.GetBufferSize()
		status.ConcurrentJobs = cfg.S3.GetConcurrentJobs()
	case cfg.GCS != nil:
		status.Destination = appsv1.SnapshotExportDestination{
			Provider:           appsv1.SnapshotExportProviderGCS,
			Bucket:             cfg.GCS.Bucket,
			ServiceAccountName: ptr.Deref(cfg.GCS.ServiceAccountName, ""),
		}
		if cfg.GCS.CredentialsSecret != nil {
			status.Destination.CredentialsSecret = &appsv1.SnapshotExportSecretReference{
				Name: cfg.GCS.CredentialsSecret.Name,
				Key:  cfg.GCS.CredentialsSecret.Key,
			}
		}
		status.SizeLimit = cfg.GCS.GetSizeLimit()
		status.PartSize = cfg.GCS.GetPartSize()
		status.ChunkSize = cfg.GCS.GetChunkSize()
		status.BufferSize = cfg.GCS.GetBufferSize()
		status.ConcurrentJobs = cfg.GCS.GetConcurrentJobs()
	default:
		return appsv1.SnapshotExportStatus{}, fmt.Errorf("no upload target defined")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		string(chainNode.UID), snapshot.Name, string(snapshot.UID), status.ObjectName,
		string(status.Destination.Provider), status.Destination.Bucket, status.Destination.Endpoint,
	}, "\x00")))
	status.ID = fmt.Sprintf("export-%x", digest[:8])
	return status, nil
}

func snapshotExportFor(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) *appsv1.SnapshotExportStatus {
	for i := range chainNode.Status.SnapshotExports {
		export := &chainNode.Status.SnapshotExports[i]
		if export.SnapshotName != snapshot.Name {
			continue
		}
		if export.SnapshotUID == "" || snapshot.UID == "" || export.SnapshotUID == snapshot.UID {
			return export
		}
	}
	return nil
}

func tarballNameForSnapshot(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) string {
	if export := snapshotExportFor(chainNode, snapshot); export != nil {
		return export.ObjectName
	}
	return getTarballName(chainNode, snapshot)
}

func snapshotExportUploading(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) bool {
	export := snapshotExportFor(chainNode, snapshot)
	return export != nil && export.Phase == appsv1.SnapshotExportPhaseUploading &&
		export.Destination.Provider != appsv1.SnapshotExportProviderUnknown
}

func snapshotExportCleanupAcknowledged(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) bool {
	export := snapshotExportFor(chainNode, snapshot)
	return export != nil && export.Phase == appsv1.SnapshotExportPhaseAcknowledged
}

func snapshotExportByObjectName(chainNode *appsv1.ChainNode, objectName string) *appsv1.SnapshotExportStatus {
	for i := range chainNode.Status.SnapshotExports {
		if chainNode.Status.SnapshotExports[i].ObjectName == objectName {
			return &chainNode.Status.SnapshotExports[i]
		}
	}
	return nil
}

func (r *Reconciler) ensureSnapshotExportStatus(
	ctx context.Context,
	chainNode *appsv1.ChainNode,
	snapshot *snapshotv1.VolumeSnapshot,
) (*appsv1.SnapshotExportStatus, error) {
	if export := snapshotExportFor(chainNode, snapshot); export != nil {
		return export, nil
	}
	_, err := r.mutateSnapshotExportStatus(ctx, chainNode, func(fresh *appsv1.ChainNode) (bool, error) {
		if snapshotExportFor(fresh, snapshot) != nil {
			return false, nil
		}
		export, createErr := newSnapshotExportStatus(fresh, snapshot)
		if createErr != nil {
			return false, createErr
		}
		fresh.Status.SnapshotExports = append(fresh.Status.SnapshotExports, export)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	export := snapshotExportFor(chainNode, snapshot)
	if export == nil {
		return nil, fmt.Errorf("snapshot export status was not persisted")
	}
	return export, nil
}

func (r *Reconciler) ensureUnknownSnapshotExportStatus(
	ctx context.Context,
	chainNode *appsv1.ChainNode,
	snapshot *snapshotv1.VolumeSnapshot,
) (*appsv1.SnapshotExportStatus, error) {
	if export := snapshotExportFor(chainNode, snapshot); export != nil {
		return export, nil
	}
	// The legacy upload may have used a suffix or destination that has since changed. Do not guess an
	// object name from mutable current configuration; the operator must identify the original object.
	objectName := ""
	// Without current export configuration, the original cleanup policy is unknowable. Fail closed so
	// retention cannot discard the only lifecycle evidence before explicit operator acknowledgement.
	deleteOnExpire := true
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.Snapshots != nil &&
		chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() {
		deleteOnExpire = chainNode.Spec.Persistence.Snapshots.ExportTarball.DeleteWhenExpired()
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{string(chainNode.UID), snapshot.Name, string(snapshot.UID), "unknown"}, "\x00")))
	id := fmt.Sprintf("export-%x", digest[:8])
	message := fmt.Sprintf(
		"snapshot export %s has no controller-owned object name or upload destination; identify and clean up the original object, then acknowledge with annotation %q=%q",
		id, controllers.AnnotationSnapshotExportCleanupAcknowledgement, id,
	)
	changed, err := r.mutateSnapshotExportStatus(ctx, chainNode, func(fresh *appsv1.ChainNode) (bool, error) {
		if snapshotExportFor(fresh, snapshot) != nil {
			return false, nil
		}
		fresh.Status.SnapshotExports = append(fresh.Status.SnapshotExports, appsv1.SnapshotExportStatus{
			ID:           id,
			SnapshotName: snapshot.Name,
			SnapshotUID:  snapshot.UID,
			ObjectName:   objectName,
			Destination: appsv1.SnapshotExportDestination{
				Provider: appsv1.SnapshotExportProviderUnknown,
			},
			Phase:          appsv1.SnapshotExportPhaseCleanupRequired,
			Message:        message,
			DeleteOnExpire: deleteOnExpire,
		})
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if changed {
		r.recorder.Eventf(chainNode, corev1.EventTypeWarning, appsv1.ReasonTarballCleanupRequired, "%s", message)
	}
	return snapshotExportFor(chainNode, snapshot), nil
}

func (r *Reconciler) setSnapshotExportPhase(
	ctx context.Context,
	chainNode *appsv1.ChainNode,
	id string,
	phase appsv1.SnapshotExportPhase,
	message string,
) (bool, error) {
	return r.mutateSnapshotExportStatus(ctx, chainNode, func(fresh *appsv1.ChainNode) (bool, error) {
		for i := range fresh.Status.SnapshotExports {
			export := &fresh.Status.SnapshotExports[i]
			if export.ID != id {
				continue
			}
			if export.Phase == phase && export.Message == message {
				return false, nil
			}
			if export.Phase != phase && !snapshotExportPhaseTransitionAllowed(export.Phase, phase) {
				return false, fmt.Errorf("snapshot export %q cannot transition from %q to %q", id, export.Phase, phase)
			}
			export.Phase = phase
			export.Message = message
			return true, nil
		}
		return false, nil
	})
}

func snapshotExportPhaseTransitionAllowed(from, to appsv1.SnapshotExportPhase) bool {
	switch from {
	case appsv1.SnapshotExportPhaseUploading:
		return to == appsv1.SnapshotExportPhaseUploaded || to == appsv1.SnapshotExportPhaseCleanupRequired || to == appsv1.SnapshotExportPhaseDeleted
	case appsv1.SnapshotExportPhaseUploaded:
		return to == appsv1.SnapshotExportPhaseDeleting || to == appsv1.SnapshotExportPhaseCleanupRequired || to == appsv1.SnapshotExportPhaseDeleted
	case appsv1.SnapshotExportPhaseDeleting:
		return to == appsv1.SnapshotExportPhaseCleanupRequired || to == appsv1.SnapshotExportPhaseDeleted
	case appsv1.SnapshotExportPhaseCleanupRequired:
		return to == appsv1.SnapshotExportPhaseDeleting || to == appsv1.SnapshotExportPhaseDeleted || to == appsv1.SnapshotExportPhaseAcknowledged
	default:
		return false
	}
}

func (r *Reconciler) removeSnapshotExport(ctx context.Context, chainNode *appsv1.ChainNode, id string) error {
	_, err := r.mutateSnapshotExportStatus(ctx, chainNode, func(fresh *appsv1.ChainNode) (bool, error) {
		kept := make([]appsv1.SnapshotExportStatus, 0, len(fresh.Status.SnapshotExports))
		for _, export := range fresh.Status.SnapshotExports {
			if export.ID != id {
				kept = append(kept, export)
			}
		}
		if len(kept) == len(fresh.Status.SnapshotExports) {
			return false, nil
		}
		fresh.Status.SnapshotExports = kept
		return true, nil
	})
	return err
}

func (r *Reconciler) mutateSnapshotExportStatus(
	ctx context.Context,
	chainNode *appsv1.ChainNode,
	mutate func(*appsv1.ChainNode) (bool, error),
) (bool, error) {
	if r.Client == nil {
		return false, errSnapshotExportStatusUnavailable
	}
	key := client.ObjectKeyFromObject(chainNode)
	changed := false
	var latest *appsv1.ChainNode
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &appsv1.ChainNode{}
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}
		mutated, err := mutate(fresh)
		if err != nil {
			return err
		}
		conditionsBefore := append([]metav1.Condition(nil), fresh.Status.Conditions...)
		syncSnapshotExportCleanupCondition(fresh)
		if !mutated && apiequality.Semantic.DeepEqual(conditionsBefore, fresh.Status.Conditions) {
			latest = fresh
			return nil
		}
		if err = r.Status().Update(ctx, fresh); err != nil {
			return err
		}
		changed = true
		latest = fresh
		return nil
	})
	if err != nil {
		return false, err
	}
	if latest != nil {
		mergeSnapshotExportOwnedStatus(chainNode, latest)
	}
	return changed, nil
}

func mergeSnapshotExportOwnedStatus(chainNode, latest *appsv1.ChainNode) {
	latestCopy := latest.DeepCopy()
	chainNode.Status.SnapshotExports = latestCopy.Status.SnapshotExports
	cleanupCondition := apiMeta.FindStatusCondition(latestCopy.Status.Conditions, appsv1.ConditionSnapshotExportCleanup)
	conditions := make([]metav1.Condition, 0, len(chainNode.Status.Conditions)+1)
	insertedCleanupCondition := false
	for _, condition := range chainNode.Status.Conditions {
		if condition.Type != appsv1.ConditionSnapshotExportCleanup {
			conditions = append(conditions, condition)
			continue
		}
		if cleanupCondition != nil && !insertedCleanupCondition {
			conditions = append(conditions, *cleanupCondition.DeepCopy())
			insertedCleanupCondition = true
		}
	}
	if cleanupCondition != nil && !insertedCleanupCondition {
		conditions = append(conditions, *cleanupCondition.DeepCopy())
	}
	chainNode.Status.Conditions = conditions
}

func syncSnapshotExportCleanupCondition(chainNode *appsv1.ChainNode) {
	pending := make([]appsv1.SnapshotExportStatus, 0)
	for _, export := range chainNode.Status.SnapshotExports {
		if export.Phase == appsv1.SnapshotExportPhaseCleanupRequired {
			pending = append(pending, export)
		}
	}
	if len(pending) == 0 {
		apiMeta.RemoveStatusCondition(&chainNode.Status.Conditions, appsv1.ConditionSnapshotExportCleanup)
		return
	}
	message := pending[0].Message
	if len(pending) > 1 {
		message = fmt.Sprintf("%d snapshot exports require operator cleanup; first: %s", len(pending), message)
	}
	apiMeta.SetStatusCondition(&chainNode.Status.Conditions, metav1.Condition{
		Type:               appsv1.ConditionSnapshotExportCleanup,
		Status:             metav1.ConditionTrue,
		Reason:             appsv1.ReasonTarballCleanupRequired,
		Message:            message,
		ObservedGeneration: chainNode.Generation,
	})
}

func (r *Reconciler) requireSnapshotExportCleanup(
	ctx context.Context,
	chainNode *appsv1.ChainNode,
	export *appsv1.SnapshotExportStatus,
	message string,
) error {
	changed, err := r.setSnapshotExportPhase(ctx, chainNode, export.ID, appsv1.SnapshotExportPhaseCleanupRequired, message)
	if err != nil {
		return err
	}
	if changed {
		r.recorder.Eventf(chainNode, corev1.EventTypeWarning, appsv1.ReasonTarballCleanupRequired, "%s", message)
	}
	return nil
}

func exportConfigForStatus(export *appsv1.SnapshotExportStatus) (*appsv1.ExportTarballConfig, error) {
	cfg := &appsv1.ExportTarballConfig{Compression: ptr.To(export.Compression)}
	switch export.Destination.Provider {
	case appsv1.SnapshotExportProviderS3:
		s3 := &appsv1.S3ExportConfig{
			Bucket:         export.Destination.Bucket,
			Region:         export.Destination.Region,
			ForcePathStyle: ptr.To(export.Destination.ForcePathStyle),
		}
		if export.Destination.Endpoint != "" {
			s3.Endpoint = ptr.To(export.Destination.Endpoint)
		}
		if export.Destination.CredentialsSecret != nil {
			s3.CredentialsSecret = &corev1.LocalObjectReference{Name: export.Destination.CredentialsSecret.Name}
		}
		if export.Destination.ServiceAccountName != "" {
			s3.ServiceAccountName = ptr.To(export.Destination.ServiceAccountName)
		}
		if export.SizeLimit != "" {
			s3.SizeLimit = ptr.To(export.SizeLimit)
		}
		if export.PartSize != "" {
			s3.PartSize = ptr.To(export.PartSize)
		}
		if export.ChunkSize != "" {
			s3.ChunkSize = ptr.To(export.ChunkSize)
		}
		if export.BufferSize != "" {
			s3.BufferSize = ptr.To(export.BufferSize)
		}
		if export.ConcurrentJobs > 0 {
			s3.ConcurrentJobs = ptr.To(export.ConcurrentJobs)
		}
		cfg.S3 = s3
	case appsv1.SnapshotExportProviderGCS:
		gcs := &appsv1.GcsExportConfig{Bucket: export.Destination.Bucket}
		if export.Destination.CredentialsSecret != nil {
			gcs.CredentialsSecret = &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: export.Destination.CredentialsSecret.Name},
				Key:                  export.Destination.CredentialsSecret.Key,
			}
		}
		if export.Destination.ServiceAccountName != "" {
			gcs.ServiceAccountName = ptr.To(export.Destination.ServiceAccountName)
		}
		if export.SizeLimit != "" {
			gcs.SizeLimit = ptr.To(export.SizeLimit)
		}
		if export.PartSize != "" {
			gcs.PartSize = ptr.To(export.PartSize)
		}
		if export.ChunkSize != "" {
			gcs.ChunkSize = ptr.To(export.ChunkSize)
		}
		if export.BufferSize != "" {
			gcs.BufferSize = ptr.To(export.BufferSize)
		}
		if export.ConcurrentJobs > 0 {
			gcs.ConcurrentJobs = ptr.To(export.ConcurrentJobs)
		}
		cfg.GCS = gcs
	default:
		return nil, fmt.Errorf("snapshot export %q has unknown provider %q", export.ID, export.Destination.Provider)
	}
	return cfg, nil
}

func (r *Reconciler) tarballProviderForExport(
	chainNode *appsv1.ChainNode,
	export *appsv1.SnapshotExportStatus,
) (datasnapshot.SnapshotProvider, error) {
	cfg, err := exportConfigForStatus(export)
	if err != nil {
		return nil, err
	}
	return r.tarballProviderForConfig(chainNode, cfg)
}

func (r *Reconciler) validateSnapshotExportReferences(
	ctx context.Context,
	chainNode *appsv1.ChainNode,
	export *appsv1.SnapshotExportStatus,
) error {
	clientSet := r.snapshotKubernetesClient()
	if clientSet == nil {
		return fmt.Errorf("snapshot export Kubernetes client is unavailable")
	}
	if secretRef := export.Destination.CredentialsSecret; secretRef != nil {
		secret, err := clientSet.CoreV1().Secrets(chainNode.Namespace).Get(ctx, secretRef.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return &snapshotExportReferenceUnavailableError{message: fmt.Sprintf(
					"snapshot export %s requires cleanup of %s, but credentials Secret %q is unavailable; restore it or acknowledge cleanup with annotation %q=%q",
					export.ID, describeSnapshotExport(export), secretRef.Name, controllers.AnnotationSnapshotExportCleanupAcknowledgement, export.ID,
				)}
			}
			return fmt.Errorf("get snapshot export credentials Secret %q: %w", secretRef.Name, err)
		}
		requiredKeys := make([]string, 0, 2)
		switch export.Destination.Provider {
		case appsv1.SnapshotExportProviderGCS:
			if secretRef.Key != "" {
				requiredKeys = append(requiredKeys, secretRef.Key)
			}
		case appsv1.SnapshotExportProviderS3:
			requiredKeys = append(requiredKeys, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
		}
		for _, key := range requiredKeys {
			if len(secret.Data[key]) == 0 {
				return &snapshotExportReferenceUnavailableError{message: fmt.Sprintf(
					"snapshot export %s requires cleanup of %s, but credentials Secret %q does not contain key %q; restore it or acknowledge cleanup with annotation %q=%q",
					export.ID, describeSnapshotExport(export), secretRef.Name, key,
					controllers.AnnotationSnapshotExportCleanupAcknowledgement, export.ID,
				)}
			}
		}
	}
	if serviceAccount := export.Destination.ServiceAccountName; serviceAccount != "" {
		_, err := clientSet.CoreV1().ServiceAccounts(chainNode.Namespace).Get(ctx, serviceAccount, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return &snapshotExportReferenceUnavailableError{message: fmt.Sprintf(
					"snapshot export %s requires cleanup of %s, but ServiceAccount %q is unavailable; restore it or acknowledge cleanup with annotation %q=%q",
					export.ID, describeSnapshotExport(export), serviceAccount,
					controllers.AnnotationSnapshotExportCleanupAcknowledgement, export.ID,
				)}
			}
			return fmt.Errorf("get snapshot export ServiceAccount %q: %w", serviceAccount, err)
		}
	}
	return nil
}

func describeSnapshotExport(export *appsv1.SnapshotExportStatus) string {
	destination := fmt.Sprintf("%s bucket %q object %q", export.Destination.Provider, export.Destination.Bucket, export.ObjectName)
	if export.Destination.Endpoint != "" {
		destination += fmt.Sprintf(" at %s", export.Destination.Endpoint)
	}
	return destination
}

func acknowledgedSnapshotExportIDs(chainNode *appsv1.ChainNode) map[string]struct{} {
	acknowledged := make(map[string]struct{})
	for _, id := range strings.Split(chainNode.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement], ",") {
		if id = strings.TrimSpace(id); id != "" {
			acknowledged[id] = struct{}{}
		}
	}
	return acknowledged
}

func (r *Reconciler) reconcileSnapshotExportAcknowledgements(ctx context.Context, chainNode *appsv1.ChainNode) error {
	if len(acknowledgedSnapshotExportIDs(chainNode)) == 0 {
		return nil
	}
	requested := make(map[string]struct{})
	applied := make(map[string]struct{})
	changed, err := r.mutateSnapshotExportStatus(ctx, chainNode, func(fresh *appsv1.ChainNode) (bool, error) {
		requested = acknowledgedSnapshotExportIDs(fresh)
		applied = make(map[string]struct{})
		mutated := false
		for i := range fresh.Status.SnapshotExports {
			export := &fresh.Status.SnapshotExports[i]
			if _, ok := requested[export.ID]; !ok {
				continue
			}
			switch export.Phase {
			case appsv1.SnapshotExportPhaseCleanupRequired:
				export.Phase = appsv1.SnapshotExportPhaseAcknowledged
				export.Message = "cleanup explicitly acknowledged by operator without deletion proof"
				applied[export.ID] = struct{}{}
				mutated = true
			}
		}
		return mutated, nil
	})
	if err != nil {
		return err
	}
	if changed {
		for id := range applied {
			r.recorder.Eventf(chainNode, corev1.EventTypeWarning, appsv1.ReasonTarballCleanupAcknowledged,
				"Snapshot export cleanup %s was explicitly acknowledged without deletion proof", id)
		}
	}
	return r.removeSnapshotExportAcknowledgementIDs(ctx, chainNode, requested)
}

func (r *Reconciler) removeSnapshotExportAcknowledgementIDs(
	ctx context.Context,
	chainNode *appsv1.ChainNode,
	ids map[string]struct{},
) error {
	if len(ids) == 0 {
		return nil
	}
	if r.Client == nil {
		return errSnapshotExportStatusUnavailable
	}
	key := client.ObjectKeyFromObject(chainNode)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &appsv1.ChainNode{}
		if getErr := r.Get(ctx, key, fresh); getErr != nil {
			return getErr
		}
		remaining := make([]string, 0)
		for _, id := range strings.Split(fresh.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement], ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, remove := ids[id]; !remove {
				remaining = append(remaining, id)
			}
		}
		if len(remaining) == len(acknowledgedSnapshotExportIDs(fresh)) {
			return nil
		}
		base := fresh.DeepCopy()
		if len(remaining) == 0 {
			delete(fresh.Annotations, controllers.AnnotationSnapshotExportCleanupAcknowledgement)
		} else {
			fresh.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement] = strings.Join(remaining, ",")
		}
		if patchErr := r.Patch(ctx, fresh, client.MergeFrom(base)); patchErr != nil {
			return patchErr
		}
		return nil
	})
}
