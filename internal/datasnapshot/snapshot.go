package datasnapshot

import (
	"context"
	"fmt"

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

type SnapshotProvider interface {
	CreateSnapshot(context.Context, string, *snapshotv1.VolumeSnapshot) error
	GetSnapshotStatus(context.Context, string) (SnapshotStatus, error)
	CleanupSnapshot(context.Context, string) error
	DeleteSnapshot(context.Context, string) error
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
	actualJob, created, err := ensureUploadJob(ctx, client, owner, job)
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

func ensureUploadJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	desired *batchv1.Job,
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
		return nil, false, fmt.Errorf("get existing upload job: %w", err)
	}
	if !metav1.IsControlledBy(job, owner) {
		return nil, false, fmt.Errorf("upload job %s/%s is not controlled by snapshot owner %s", job.Namespace, job.Name, owner.GetName())
	}
	for key, value := range desired.Labels {
		if job.Labels[key] != value {
			return nil, false, fmt.Errorf("upload job %s/%s has conflicting label %s", job.Namespace, job.Name, key)
		}
	}
	return job, false, nil
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
