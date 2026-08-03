package datasnapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type SnapshotStatus string

type SnapshotJobPurpose string

type SnapshotJobIdentity struct {
	UID         types.UID
	PVCUID      types.UID
	Terminating bool
}

type SnapshotJob struct {
	Name        string
	UID         types.UID
	Purpose     SnapshotJobPurpose
	Terminating bool
	Upload      *SnapshotJobIdentity
	// Exporter is set when a listed deletion Job belongs to a previously configured
	// provider. Deletion Jobs are self-contained, so they can still be resumed and
	// cleaned up after the ChainNode switches providers.
	Exporter string
}

const (
	SnapshotSucceeded SnapshotStatus = "succeeded"
	SnapshotFailed    SnapshotStatus = "failed"
	SnapshotActive    SnapshotStatus = "active"
	SnapshotNotFound  SnapshotStatus = "notfound"

	labelExporter = "exporter"
	labelOwner    = "owner"
	labelType     = "type"

	labelCleanupExporter  = "cleanup-exporter"
	labelCleanupOwner     = "cleanup-owner"
	labelCleanupType      = "cleanup-type"
	labelCleanupUploadUID = "cleanup-upload-uid"
	labelCleanupPVCUID    = "cleanup-pvc-uid"

	SnapshotJobUpload SnapshotJobPurpose = "upload"
	SnapshotJobDelete SnapshotJobPurpose = "delete"

	typeUpload = string(SnapshotJobUpload)
	typeDelete = string(SnapshotJobDelete)
)

// ErrStaleJobReplaced reports that an existing snapshot Job is being deleted because it cannot
// converge to the desired state. Callers should requeue instead of failing the reconcile; the
// replacement Job is created once the stale one is gone.
var ErrStaleJobReplaced = errors.New("stale snapshot job deleted; it will be recreated")

// ErrStaleJobTerminating reports that a previously replaced stale Job still exists with a deletion
// timestamp. Callers should use a delayed requeue while foreground deletion finishes.
var ErrStaleJobTerminating = errors.New("stale snapshot job is still terminating")

// StaleJobReplacedError describes a stale Job whose deletion was successfully requested.
type StaleJobReplacedError struct {
	Purpose          string
	Namespace        string
	Name             string
	UID              types.UID
	ConflictingLabel string
	PreviousValue    string
	DesiredValue     string
}

func (err *StaleJobReplacedError) Error() string {
	if err.ProviderSwitch() {
		return fmt.Sprintf("%s job %s/%s (UID %s) for exporter %q replaced by desired exporter %q: %v",
			err.Purpose, err.Namespace, err.Name, err.UID, err.PreviousValue, err.DesiredValue, ErrStaleJobReplaced)
	}
	return fmt.Sprintf("%s job %s/%s (UID %s) had conflicting label %q: previous value %q, desired value %q: %v",
		err.Purpose, err.Namespace, err.Name, err.UID, err.ConflictingLabel, err.PreviousValue, err.DesiredValue, ErrStaleJobReplaced)
}

func (err *StaleJobReplacedError) Unwrap() error {
	return ErrStaleJobReplaced
}

// ProviderSwitch reports whether the exporter label caused the replacement.
func (err *StaleJobReplacedError) ProviderSwitch() bool {
	return err.ConflictingLabel == labelExporter
}

type SnapshotProvider interface {
	CreateSnapshot(context.Context, string, *snapshotv1.VolumeSnapshot) error
	GetSnapshotStatus(context.Context, string) (SnapshotStatus, error)
	GetSnapshotDeletionStatus(context.Context, SnapshotJob) (SnapshotStatus, error)
	CleanupSnapshot(context.Context, string) error
	DeleteSnapshot(context.Context, string) (SnapshotStatus, error)
	DeleteSnapshotForUpload(context.Context, SnapshotJob) (SnapshotJob, SnapshotStatus, error)
	CleanupSnapshotDeletion(context.Context, SnapshotJob) error
	ListSnapshots(ctx context.Context) ([]SnapshotJob, error)
}

func ensureUploadResources(
	ctx context.Context,
	client kubernetes.Interface,
	scheme *runtime.Scheme,
	owner metav1.Object,
	job *batchv1.Job,
	pvc *corev1.PersistentVolumeClaim,
) error {
	actualJob, created, err := ensureSnapshotJob(ctx, client, owner, job, "upload")
	if err != nil {
		return err
	}

	if err = controllerutil.SetControllerReference(actualJob, pvc, scheme); err != nil {
		return cleanUpNewUploadJob(ctx, client, actualJob, created, fmt.Errorf("set PVC owner reference: %w", err))
	}
	if err = ensureUploadPVC(ctx, client, actualJob, pvc); err != nil {
		return cleanUpNewUploadJob(ctx, client, actualJob, created, err)
	}
	return nil
}

func ensureSnapshotJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	desired *batchv1.Job,
	purpose string,
) (*batchv1.Job, bool, error) {
	jobs := client.BatchV1().Jobs(desired.Namespace)
	job, err := jobs.Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return job, true, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, false, err
	}

	job, err = jobs.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("get existing %s job: %w", purpose, err)
	}
	if !metav1.IsControlledBy(job, owner) {
		return nil, false, fmt.Errorf("%s job %s/%s is not controlled by snapshot owner %s", purpose, job.Namespace, job.Name, owner.GetName())
	}
	for key, value := range desired.Labels {
		if job.Labels[key] != value {
			if job.DeletionTimestamp != nil {
				return nil, false, fmt.Errorf("stale %s job %s/%s is terminating: %w", purpose, job.Namespace, job.Name, ErrStaleJobTerminating)
			}
			// Labels are set at creation and never updated, so a mismatch can never converge — it means
			// the Job was created for a different desired state (e.g. another export provider). Exports
			// and deletions are idempotent, so drop the stale Job and let the next reconcile recreate it.
			if err = deleteSnapshotJob(ctx, client, job); err != nil {
				return nil, false, fmt.Errorf("delete stale %s job %s/%s: %w", purpose, job.Namespace, job.Name, err)
			}
			return nil, false, &StaleJobReplacedError{
				Purpose:          purpose,
				Namespace:        job.Namespace,
				Name:             job.Name,
				UID:              job.UID,
				ConflictingLabel: key,
				PreviousValue:    job.Labels[key],
				DesiredValue:     value,
			}
		}
	}
	return job, false, nil
}

// uploadJobStatus reports the status of an existing upload Job, rejecting one that belongs to a
// different exporter. Polling looks the Job up by name, and the name does not encode the provider, so
// without this check an in-flight upload started by the previous provider would be read as progress
// towards the newly configured one — and its success would mark the export finished even though
// nothing was written to the new destination.
//
// The stale Job is removed so the next reconcile starts a real upload against the current provider.
func uploadJobStatus(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	namespace, name, exporter string,
) (SnapshotStatus, error) {
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SnapshotNotFound, nil
		}
		return "", err
	}
	// Tarball names derive from chain ID and a second-resolution timestamp, so two ChainNodes in one
	// namespace can collide on this Job name. Never delete a Job this node does not own — that would
	// terminate another node's active export. Mirrors the ownership guard in ensureSnapshotJob.
	if !metav1.IsControlledBy(job, owner) {
		return "", fmt.Errorf("upload job %s/%s is not controlled by snapshot owner %s",
			job.Namespace, job.Name, owner.GetName())
	}
	if job.Labels[labelExporter] != exporter {
		if job.DeletionTimestamp != nil {
			return "", fmt.Errorf("stale upload job %s/%s is terminating: %w", job.Namespace, job.Name, ErrStaleJobTerminating)
		}
		if err = deleteSnapshotJob(ctx, client, job); err != nil {
			return "", fmt.Errorf("delete stale upload job %s/%s: %w", job.Namespace, job.Name, err)
		}
		return "", &StaleJobReplacedError{
			Purpose:          typeUpload,
			Namespace:        job.Namespace,
			Name:             job.Name,
			UID:              job.UID,
			ConflictingLabel: labelExporter,
			PreviousValue:    job.Labels[labelExporter],
			DesiredValue:     exporter,
		}
	}
	return snapshotJobStatus(job), nil
}

func cleanupSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) error {
	if expected.Purpose != SnapshotJobDelete {
		return fmt.Errorf("snapshot job %q has purpose %q, expected %q", expected.Name, expected.Purpose, SnapshotJobDelete)
	}
	namespace := owner.GetNamespace()
	jobName := fmt.Sprintf("%s-delete", expected.Name)
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get snapshot deletion job %s/%s: %w", namespace, jobName, err)
	}
	if expected.UID != "" && job.UID != expected.UID {
		return fmt.Errorf("snapshot deletion job %s/%s has UID %s, expected listed UID %s",
			job.Namespace, job.Name, job.UID, expected.UID)
	}
	if !metav1.IsControlledBy(job, owner) {
		return fmt.Errorf("snapshot deletion job %s/%s is not controlled by snapshot owner %s",
			job.Namespace, job.Name, owner.GetName())
	}
	if job.DeletionTimestamp != nil {
		return fmt.Errorf("snapshot deletion job %s/%s is terminating: %w",
			job.Namespace, job.Name, ErrStaleJobTerminating)
	}
	for _, expected := range []struct {
		label   string
		desired string
	}{
		{label: labelExporter, desired: exporter},
		{label: labelOwner, desired: owner.GetName()},
		{label: labelType, desired: typeDelete},
	} {
		actual := job.Labels[expected.label]
		if actual == expected.desired {
			continue
		}
		if err = deleteSnapshotJob(ctx, client, job); err != nil {
			return fmt.Errorf("delete stale snapshot deletion job %s/%s: %w", job.Namespace, job.Name, err)
		}
		return &StaleJobReplacedError{
			Purpose:          typeDelete,
			Namespace:        job.Namespace,
			Name:             job.Name,
			UID:              job.UID,
			ConflictingLabel: expected.label,
			PreviousValue:    actual,
			DesiredValue:     expected.desired,
		}
	}
	if err = deleteSnapshotJob(ctx, client, job); err != nil {
		return fmt.Errorf("delete snapshot deletion job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

func deleteSnapshotJob(ctx context.Context, client kubernetes.Interface, job *batchv1.Job) error {
	propagation := metav1.DeletePropagationForeground
	err := client.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
		Preconditions:     &metav1.Preconditions{UID: &job.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func snapshotJobStatus(job *batchv1.Job) SnapshotStatus {
	failed := false
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return SnapshotSucceeded
		case batchv1.JobFailed:
			failed = true
		}
	}
	if failed {
		return SnapshotFailed
	}
	return SnapshotActive
}

func snapshotNameFromJob(job *batchv1.Job) string {
	switch job.Labels[labelType] {
	case typeUpload:
		return strings.TrimSuffix(job.Name, "-upload")
	case typeDelete:
		return strings.TrimSuffix(job.Name, "-delete")
	default:
		return job.Name
	}
}

func snapshotJobFromJob(job *batchv1.Job) SnapshotJob {
	snapshotJob := SnapshotJob{
		Name:        snapshotNameFromJob(job),
		UID:         job.UID,
		Purpose:     SnapshotJobPurpose(job.Labels[labelType]),
		Terminating: job.DeletionTimestamp != nil,
	}
	if snapshotJob.Purpose == SnapshotJobDelete && job.Labels[labelCleanupUploadUID] != "" {
		snapshotJob.Upload = &SnapshotJobIdentity{
			UID:    types.UID(job.Labels[labelCleanupUploadUID]),
			PVCUID: types.UID(job.Labels[labelCleanupPVCUID]),
		}
	}
	return snapshotJob
}

func snapshotJobsFromJobs(jobs []batchv1.Job, currentExporter ...string) []SnapshotJob {
	type groupedSnapshotJobs struct {
		preferred         SnapshotJob
		preferredExporter string
		upload            *SnapshotJobIdentity
		uploadExporter    string
	}

	uniqueJobs := make(map[string]groupedSnapshotJobs, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		snapshotJob := snapshotJobFromJob(job)
		group := uniqueJobs[snapshotJob.Name]
		if snapshotJob.Purpose == SnapshotJobUpload {
			group.upload = &SnapshotJobIdentity{UID: snapshotJob.UID, Terminating: snapshotJob.Terminating}
			group.uploadExporter = job.Labels[labelExporter]
		}
		if group.preferred.Purpose != SnapshotJobDelete || snapshotJob.Purpose == SnapshotJobDelete {
			group.preferred = snapshotJob
			group.preferredExporter = job.Labels[labelExporter]
		}
		uniqueJobs[snapshotJob.Name] = group
	}

	result := make([]SnapshotJob, 0, len(uniqueJobs))
	for _, group := range uniqueJobs {
		if group.preferred.Purpose == SnapshotJobDelete && group.preferred.Upload == nil &&
			group.preferredExporter == group.uploadExporter {
			group.preferred.Upload = group.upload
		} else if group.preferred.Purpose == SnapshotJobDelete && group.upload != nil &&
			group.preferredExporter == group.uploadExporter &&
			group.preferred.Upload.UID == group.upload.UID {
			group.preferred.Upload.Terminating = group.upload.Terminating
		}
		if len(currentExporter) > 0 && group.preferred.Purpose == SnapshotJobDelete &&
			group.preferredExporter != "" && group.preferredExporter != currentExporter[0] {
			group.preferred.Exporter = group.preferredExporter
		}
		result = append(result, group.preferred)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func listSnapshotJobs(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	exporter string,
) ([]SnapshotJob, error) {
	selector := labels.SelectorFromSet(map[string]string{labelOwner: owner.GetName()}).String()
	list, err := client.BatchV1().Jobs(owner.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}

	jobs := make([]batchv1.Job, 0, len(list.Items))
	for i := range list.Items {
		job := list.Items[i]
		// Upload Jobs need the currently configured provider to construct a matching
		// deletion workflow. Existing deletion Jobs are self-contained and must stay
		// discoverable after a provider switch so suspended workflows can resume.
		if job.Labels[labelExporter] == exporter || job.Labels[labelType] == typeDelete {
			jobs = append(jobs, job)
		}
	}
	return snapshotJobsFromJobs(jobs, exporter), nil
}

// ListSnapshotDeletionJobs returns owner-labelled deletion Jobs regardless of
// the currently configured exporter. Deletion Jobs are self-contained and may
// need to finish after tarball exports or snapshots have been disabled.
func ListSnapshotDeletionJobs(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
) ([]SnapshotJob, error) {
	return listSnapshotJobs(ctx, client, owner, "")
}

// ReconcileSnapshotDeletionJob resumes and reports a previously created
// deletion Job using the exporter identity persisted on the Job itself.
func ReconcileSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	job SnapshotJob,
) (SnapshotStatus, error) {
	if job.Exporter == "" {
		return "", fmt.Errorf("snapshot deletion job %q is missing its exporter identity", job.Name)
	}
	return reconcileSnapshotDeletionJob(ctx, client, owner, job, job.Exporter)
}

// CleanupSnapshotDeletionResources removes a completed deletion Job and any
// paired upload resources using the identities persisted on the deletion Job.
func CleanupSnapshotDeletionResources(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	job SnapshotJob,
) error {
	if job.Exporter == "" {
		return fmt.Errorf("snapshot deletion job %q is missing its exporter identity", job.Name)
	}
	return cleanupSnapshotDeletionResources(ctx, client, owner, job, job.Exporter)
}

func snapshotJobExporter(job SnapshotJob, currentExporter string) string {
	if job.Exporter != "" {
		return job.Exporter
	}
	return currentExporter
}

func getSnapshotUploadResources(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	name string,
	expected SnapshotJobIdentity,
	exporter string,
) (*batchv1.Job, *corev1.PersistentVolumeClaim, error) {
	if expected.UID == "" {
		return nil, nil, fmt.Errorf("snapshot upload job %q is missing its listed UID", name)
	}

	namespace := owner.GetNamespace()
	jobName := fmt.Sprintf("%s-upload", name)
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("get orphan upload job %s/%s: %w", namespace, jobName, err)
		}
		job = nil
	} else {
		if job.UID != expected.UID {
			return nil, nil, fmt.Errorf("orphan upload job %s/%s has UID %s, expected listed UID %s",
				job.Namespace, job.Name, job.UID, expected.UID)
		}
		if !metav1.IsControlledBy(job, owner) {
			return nil, nil, fmt.Errorf("orphan upload job %s/%s is not controlled by snapshot owner %s",
				job.Namespace, job.Name, owner.GetName())
		}
		for _, expectedLabel := range []struct {
			name  string
			value string
		}{
			{name: labelExporter, value: exporter},
			{name: labelOwner, value: owner.GetName()},
			{name: labelType, value: typeUpload},
		} {
			if job.Labels[expectedLabel.name] != expectedLabel.value {
				return nil, nil, fmt.Errorf("orphan upload job %s/%s has %s label %q, expected %q",
					job.Namespace, job.Name, expectedLabel.name, job.Labels[expectedLabel.name], expectedLabel.value)
			}
		}
	}

	pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return job, nil, nil
		}
		return nil, nil, fmt.Errorf("get orphan upload PVC %s/%s: %w", namespace, jobName, err)
	}
	controller := metav1.GetControllerOfNoCopy(pvc)
	if controller == nil || controller.APIVersion != batchv1.SchemeGroupVersion.String() || controller.Kind != "Job" ||
		controller.Name != jobName || controller.UID != expected.UID {
		return nil, nil, fmt.Errorf("orphan upload PVC %s/%s is not controlled by upload job UID %s",
			pvc.Namespace, pvc.Name, expected.UID)
	}
	if expected.PVCUID != "" && pvc.UID != expected.PVCUID {
		return nil, nil, fmt.Errorf("orphan upload PVC %s/%s has UID %s, expected recorded UID %s",
			pvc.Namespace, pvc.Name, pvc.UID, expected.PVCUID)
	}
	return job, pvc, nil
}

func cleanupSnapshotUploadResources(
	ctx context.Context,
	client kubernetes.Interface,
	job *batchv1.Job,
	pvc *corev1.PersistentVolumeClaim,
) error {
	if job != nil && job.DeletionTimestamp == nil {
		if err := deleteSnapshotJob(ctx, client, job); err != nil {
			return fmt.Errorf("delete orphan upload job %s/%s: %w", job.Namespace, job.Name, err)
		}
	}
	if pvc != nil {
		err := client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &pvc.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete orphan upload PVC %s/%s: %w", pvc.Namespace, pvc.Name, err)
		}
	}
	return nil
}

func setSnapshotDeletionUploadIdentity(
	job *batchv1.Job,
	owner metav1.Object,
	exporter string,
	upload SnapshotJobIdentity,
) {
	job.Labels[labelCleanupExporter] = exporter
	job.Labels[labelCleanupOwner] = owner.GetName()
	job.Labels[labelCleanupType] = typeUpload
	job.Labels[labelCleanupUploadUID] = string(upload.UID)
	if upload.PVCUID != "" {
		job.Labels[labelCleanupPVCUID] = string(upload.PVCUID)
	}
	suspend := true
	job.Spec.Suspend = &suspend
}

func snapshotDeletionUploadIdentity(
	job *batchv1.Job,
	owner metav1.Object,
	exporter string,
) (*SnapshotJobIdentity, error) {
	hasCleanupIdentity := false
	for _, label := range []string{
		labelCleanupExporter,
		labelCleanupOwner,
		labelCleanupType,
		labelCleanupUploadUID,
		labelCleanupPVCUID,
	} {
		if job.Labels[label] != "" {
			hasCleanupIdentity = true
			break
		}
	}
	if !hasCleanupIdentity {
		if job.Spec.Suspend != nil && *job.Spec.Suspend {
			return nil, fmt.Errorf("suspended snapshot deletion job %s/%s is missing its upload cleanup identity",
				job.Namespace, job.Name)
		}
		return nil, nil
	}

	for _, expectedLabel := range []struct {
		name  string
		value string
	}{
		{name: labelCleanupExporter, value: exporter},
		{name: labelCleanupOwner, value: owner.GetName()},
		{name: labelCleanupType, value: typeUpload},
	} {
		if job.Labels[expectedLabel.name] != expectedLabel.value {
			return nil, fmt.Errorf("snapshot deletion job %s/%s has %s label %q, expected %q",
				job.Namespace, job.Name, expectedLabel.name, job.Labels[expectedLabel.name], expectedLabel.value)
		}
	}
	uploadUID := types.UID(job.Labels[labelCleanupUploadUID])
	if uploadUID == "" {
		return nil, fmt.Errorf("snapshot deletion job %s/%s is missing %s label",
			job.Namespace, job.Name, labelCleanupUploadUID)
	}
	return &SnapshotJobIdentity{
		UID:    uploadUID,
		PVCUID: types.UID(job.Labels[labelCleanupPVCUID]),
	}, nil
}

func getSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) (*batchv1.Job, *SnapshotJobIdentity, error) {
	if expected.Purpose != SnapshotJobDelete {
		return nil, nil, fmt.Errorf("snapshot job %q has purpose %q, expected %q", expected.Name, expected.Purpose, SnapshotJobDelete)
	}

	namespace := owner.GetNamespace()
	jobName := fmt.Sprintf("%s-delete", expected.Name)
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, expected.Upload, nil
		}
		return nil, nil, fmt.Errorf("get snapshot deletion job %s/%s: %w", namespace, jobName, err)
	}
	if expected.UID != "" && job.UID != expected.UID {
		return nil, nil, fmt.Errorf("snapshot deletion job %s/%s has UID %s, expected listed UID %s",
			job.Namespace, job.Name, job.UID, expected.UID)
	}
	if !metav1.IsControlledBy(job, owner) {
		return nil, nil, fmt.Errorf("snapshot deletion job %s/%s is not controlled by snapshot owner %s",
			job.Namespace, job.Name, owner.GetName())
	}
	if job.DeletionTimestamp != nil {
		return nil, nil, fmt.Errorf("snapshot deletion job %s/%s is terminating: %w",
			job.Namespace, job.Name, ErrStaleJobTerminating)
	}
	for _, expectedLabel := range []struct {
		name  string
		value string
	}{
		{name: labelExporter, value: exporter},
		{name: labelOwner, value: owner.GetName()},
		{name: labelType, value: typeDelete},
	} {
		if job.Labels[expectedLabel.name] != expectedLabel.value {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s has %s label %q, expected %q",
				job.Namespace, job.Name, expectedLabel.name, job.Labels[expectedLabel.name], expectedLabel.value)
		}
	}
	upload, err := snapshotDeletionUploadIdentity(job, owner, exporter)
	if err != nil {
		return nil, nil, err
	}
	if expected.Upload != nil && upload != nil {
		if expected.Upload.UID != upload.UID {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s records upload UID %s, expected %s",
				job.Namespace, job.Name, upload.UID, expected.Upload.UID)
		}
		if expected.Upload.PVCUID != "" && upload.PVCUID != expected.Upload.PVCUID {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s records upload PVC UID %s, expected %s",
				job.Namespace, job.Name, upload.PVCUID, expected.Upload.PVCUID)
		}
	}
	return job, upload, nil
}

func reconcileSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) (SnapshotStatus, error) {
	deletionJob, upload, err := getSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	if err != nil {
		return "", err
	}
	if deletionJob == nil {
		return SnapshotNotFound, nil
	}
	if upload == nil {
		return snapshotJobStatus(deletionJob), nil
	}

	uploadJob, pvc, err := getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return "", err
	}
	suspended := deletionJob.Spec.Suspend != nil && *deletionJob.Spec.Suspend
	if !suspended {
		if uploadJob != nil {
			return "", fmt.Errorf("snapshot deletion job %s/%s is active while upload job %s/%s UID %s still exists",
				deletionJob.Namespace, deletionJob.Name, uploadJob.Namespace, uploadJob.Name, uploadJob.UID)
		}
		if pvc != nil {
			if err = cleanupSnapshotUploadResources(ctx, client, nil, pvc); err != nil {
				return "", err
			}
		}
		return snapshotJobStatus(deletionJob), nil
	}

	if err = cleanupSnapshotUploadResources(ctx, client, uploadJob, pvc); err != nil {
		return "", err
	}
	uploadJob, _, err = getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return "", err
	}
	if uploadJob != nil {
		return SnapshotActive, nil
	}

	updated := deletionJob.DeepCopy()
	activate := false
	updated.Spec.Suspend = &activate
	updated, err = client.BatchV1().Jobs(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return "", fmt.Errorf("activate snapshot deletion job %s/%s UID %s: %w",
			deletionJob.Namespace, deletionJob.Name, deletionJob.UID, err)
	}
	if updated.UID != deletionJob.UID {
		return "", fmt.Errorf("activated snapshot deletion job %s/%s has UID %s, expected %s",
			updated.Namespace, updated.Name, updated.UID, deletionJob.UID)
	}
	return snapshotJobStatus(updated), nil
}

func cleanupSnapshotDeletionResources(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) error {
	if expected.Upload == nil {
		return cleanupSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	}

	deletionJob, upload, err := getSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	if err != nil {
		return err
	}
	if upload == nil {
		upload = expected.Upload
	}
	if upload == nil {
		return cleanupSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	}
	if deletionJob != nil && deletionJob.Spec.Suspend != nil && *deletionJob.Spec.Suspend {
		return fmt.Errorf("snapshot deletion job %s/%s is still suspended", deletionJob.Namespace, deletionJob.Name)
	}
	job, pvc, err := getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return err
	}
	if err = cleanupSnapshotUploadResources(ctx, client, job, pvc); err != nil {
		return err
	}
	job, _, err = getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return err
	}
	if job != nil {
		return nil
	}
	if deletionJob == nil {
		return nil
	}
	if err = deleteSnapshotJob(ctx, client, deletionJob); err != nil {
		return fmt.Errorf("delete snapshot deletion job %s/%s: %w", deletionJob.Namespace, deletionJob.Name, err)
	}
	return nil
}

func ensureUploadPVC(
	ctx context.Context,
	client kubernetes.Interface,
	job *batchv1.Job,
	desired *corev1.PersistentVolumeClaim,
) error {
	claims := client.CoreV1().PersistentVolumeClaims(desired.Namespace)
	pvc, err := claims.Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create upload PVC: %w", err)
	}

	pvc, err = claims.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get existing upload PVC: %w", err)
	}
	if !metav1.IsControlledBy(pvc, job) {
		return fmt.Errorf("upload PVC %s/%s is not controlled by upload job %s", pvc.Namespace, pvc.Name, job.Name)
	}
	if !apiequality.Semantic.DeepEqual(pvc.Spec.DataSource, desired.Spec.DataSource) {
		return fmt.Errorf("upload PVC %s/%s has a conflicting snapshot data source", pvc.Namespace, pvc.Name)
	}
	return nil
}

func cleanUpNewUploadJob(
	ctx context.Context,
	client kubernetes.Interface,
	job *batchv1.Job,
	created bool,
	cause error,
) error {
	if !created {
		return cause
	}
	propagation := metav1.DeletePropagationForeground
	if err := client.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("%w; clean up upload job: %v", cause, err)
	}
	return cause
}
