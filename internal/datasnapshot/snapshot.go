package datasnapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type SnapshotStatus string

const (
	SnapshotSucceeded SnapshotStatus = "succeeded"
	SnapshotFailed    SnapshotStatus = "failed"
	SnapshotActive    SnapshotStatus = "active"
	SnapshotNotFound  SnapshotStatus = "notfound"

	labelExporter = "exporter"
	labelOwner    = "owner"
	labelType     = "type"

	typeUpload = "upload"
	typeDelete = "delete"
)

// ErrStaleJobReplaced reports that an existing snapshot Job was deleted because its labels could not
// converge to the desired ones — the exporter label encodes the export provider, so a Job left behind
// by a previous provider never matches again. Callers should requeue instead of failing the reconcile:
// the replacement Job is created once the stale one is gone.
var ErrStaleJobReplaced = errors.New("stale snapshot job deleted; it will be recreated")

type SnapshotProvider interface {
	CreateSnapshot(context.Context, string, *snapshotv1.VolumeSnapshot) error
	GetSnapshotStatus(context.Context, string) (SnapshotStatus, error)
	CleanupSnapshot(context.Context, string) error
	DeleteSnapshot(context.Context, string) (SnapshotStatus, error)
	CleanupSnapshotDeletion(context.Context, string) error
	ListSnapshots(ctx context.Context) ([]string, error)
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
			// Labels are set at creation and never updated, so a mismatch can never converge — it means
			// the Job was created for a different desired state (e.g. another export provider). Exports
			// and deletions are idempotent, so drop the stale Job and let the next reconcile recreate it.
			if err = deleteSnapshotJob(ctx, client, job); err != nil {
				return nil, false, fmt.Errorf("delete stale %s job %s/%s: %w", purpose, job.Namespace, job.Name, err)
			}
			return nil, false, fmt.Errorf("%s job %s/%s had conflicting label %s: %w", purpose, job.Namespace, job.Name, key, ErrStaleJobReplaced)
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
	namespace, name, exporter string,
) (SnapshotStatus, error) {
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SnapshotNotFound, nil
		}
		return "", err
	}
	if job.Labels[labelExporter] != exporter {
		if err = deleteSnapshotJob(ctx, client, job); err != nil {
			return "", fmt.Errorf("delete stale upload job %s/%s: %w", job.Namespace, job.Name, err)
		}
		return "", fmt.Errorf("upload job %s/%s belongs to exporter %q, not %q: %w",
			job.Namespace, job.Name, job.Labels[labelExporter], exporter, ErrStaleJobReplaced)
	}
	return snapshotJobStatus(job), nil
}

func deleteSnapshotJob(ctx context.Context, client kubernetes.Interface, job *batchv1.Job) error {
	propagation := metav1.DeletePropagationForeground
	err := client.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
		Preconditions:     &metav1.Preconditions{UID: &job.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
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
