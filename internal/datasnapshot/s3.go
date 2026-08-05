package datasnapshot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
)

const s3Exporter = "s3-exporter"

// S3 manages snapshot export Jobs targeting Amazon S3 or compatible object stores.
type S3 struct {
	Client            kubernetes.Interface
	Scheme            *runtime.Scheme
	Owner             metav1.Object
	priorityClass     string
	dataExporterImage string
	imagePullSecrets  []corev1.LocalObjectReference
	Config            *appsv1.S3ExportConfig
	ExportConfig      *appsv1.ExportTarballConfig
}

func NewS3SnapshotProvider(
	client kubernetes.Interface,
	scheme *runtime.Scheme,
	owner metav1.Object,
	priorityClass, dataExporterImage string,
	imagePullSecrets []corev1.LocalObjectReference,
	cfg *appsv1.ExportTarballConfig,
) SnapshotProvider {
	return &S3{
		Client:            client,
		Scheme:            scheme,
		Owner:             owner,
		priorityClass:     priorityClass,
		dataExporterImage: dataExporterImage,
		imagePullSecrets:  imagePullSecrets,
		Config:            cfg.S3,
		ExportConfig:      cfg,
	}
}

func (provider *S3) serviceAccountName() string {
	if provider.Config.ServiceAccountName == nil {
		return ""
	}
	return *provider.Config.ServiceAccountName
}

func (provider *S3) credentialsEnvFrom() []corev1.EnvFromSource {
	if provider.Config.CredentialsSecret == nil {
		return nil
	}
	return []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: *provider.Config.CredentialsSecret,
		},
	}}
}

func (provider *S3) storageEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "AWS_REGION", Value: provider.Config.Region},
		{Name: "S3_ENDPOINT", Value: provider.Config.GetEndpoint()},
		{Name: "S3_FORCE_PATH_STYLE", Value: strconv.FormatBool(provider.Config.ShouldForcePathStyle())},
	}
}

func (provider *S3) uploadEnv() []corev1.EnvVar {
	return append(provider.storageEnv(),
		corev1.EnvVar{Name: "COMPRESSION", Value: string(provider.ExportConfig.GetCompression())},
		corev1.EnvVar{Name: "SIZE_LIMIT", Value: provider.Config.GetSizeLimit()},
		corev1.EnvVar{Name: "PART_SIZE", Value: provider.Config.GetPartSize()},
		corev1.EnvVar{Name: "CHUNK_SIZE", Value: provider.Config.GetChunkSize()},
		corev1.EnvVar{Name: "BUFFER_SIZE", Value: provider.Config.GetBufferSize()},
		corev1.EnvVar{Name: "CONCURRENT_JOBS", Value: strconv.Itoa(provider.Config.GetConcurrentJobs())},
	)
}

func (provider *S3) uploadJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-upload", name),
			Namespace: provider.Owner.GetNamespace(),
			Labels: map[string]string{
				labelExporter:    s3Exporter,
				labelOwner:       provider.Owner.GetName(),
				labelType:        typeUpload,
				labelDestination: provider.destinationLabel(),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					PriorityClassName:  provider.priorityClass,
					ServiceAccountName: provider.serviceAccountName(),
					ImagePullSecrets:   provider.imagePullSecrets,
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: fmt.Sprintf("%s-upload", name),
							},
						},
					}},
					Containers: []corev1.Container{{
						Name:            "dataexporter",
						Image:           provider.dataExporterImage,
						ImagePullPolicy: corev1.PullAlways,
						SecurityContext: k8s.RestrictedSecurityContext(),
						Args:            []string{"s3", "upload", "data", provider.Config.Bucket, name},
						WorkingDir:      "/home/app",
						Env:             provider.uploadEnv(),
						EnvFrom:         provider.credentialsEnvFrom(),
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/home/app/data",
						}},
					}},
				},
			},
		},
	}
}

func (provider *S3) CreateSnapshot(ctx context.Context, name string, snapshot *snapshotv1.VolumeSnapshot) error {
	if snapshot.Status.RestoreSize == nil {
		return fmt.Errorf("restore size is not available yet")
	}
	apiVersion := strings.Split(snapshot.APIVersion, "/")
	if len(apiVersion) == 0 {
		return fmt.Errorf("unsupported api version")
	}

	job := provider.uploadJob(name)
	if err := controllerutil.SetControllerReference(provider.Owner, job, provider.Scheme); err != nil {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-upload", name),
			Namespace: provider.Owner.GetNamespace(),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *snapshot.Status.RestoreSize},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiVersion[0],
				Kind:     snapshot.Kind,
				Name:     snapshot.Name,
			},
		},
	}
	return ensureUploadResources(ctx, provider.Client, provider.Scheme, provider.Owner, job, pvc)
}

func (provider *S3) destinationLabel() string {
	credentialsSecret := ""
	if provider.Config.CredentialsSecret != nil {
		credentialsSecret = provider.Config.CredentialsSecret.Name
	}
	return SnapshotDestinationLabel(
		string(appsv1.SnapshotExportProviderS3),
		provider.Config.Bucket,
		provider.Config.Region,
		provider.Config.GetEndpoint(),
		provider.Config.ShouldForcePathStyle(),
		"credentialsSecret", credentialsSecret,
		"serviceAccount", provider.serviceAccountName(),
	)
}

func (provider *S3) GetSnapshotStatus(ctx context.Context, name string) (SnapshotStatus, error) {
	return uploadJobStatusForDesired(ctx, provider.Client, provider.Owner, provider.uploadJob(name))
}

func (provider *S3) GetSnapshotDeletionStatus(ctx context.Context, snapshotJob SnapshotJob) (SnapshotStatus, error) {
	return reconcileSnapshotDeletionJob(ctx, provider.Client, provider.Owner, snapshotJob, snapshotJobExporter(snapshotJob, s3Exporter))
}

func (provider *S3) CleanupSnapshot(ctx context.Context, name string) error {
	return provider.cleanUp(ctx, name)
}

func (provider *S3) DeleteSnapshot(ctx context.Context, name string) (SnapshotStatus, error) {
	job, err := provider.ensureSnapshotDeletion(ctx, name, nil)
	if err != nil {
		return "", err
	}
	if err = provider.cleanUp(ctx, name); err != nil {
		return "", err
	}
	return snapshotJobStatus(job), nil
}

func (provider *S3) DeleteSnapshotForUpload(ctx context.Context, upload SnapshotJob) (SnapshotJob, SnapshotStatus, error) {
	if upload.Purpose != SnapshotJobUpload {
		return SnapshotJob{}, "", fmt.Errorf("snapshot job %q has purpose %q, expected %q",
			upload.Name, upload.Purpose, SnapshotJobUpload)
	}
	uploadIdentity := SnapshotJobIdentity{UID: upload.UID, Terminating: upload.Terminating}
	_, pvc, err := getSnapshotUploadResources(ctx, provider.Client, provider.Owner, upload.Name, uploadIdentity, s3Exporter)
	if err != nil {
		return SnapshotJob{}, "", err
	}
	if pvc != nil {
		uploadIdentity.PVCUID = pvc.UID
	}
	job, err := provider.ensureSnapshotDeletion(ctx, upload.Name, &uploadIdentity)
	if err != nil {
		return SnapshotJob{}, "", err
	}
	deletion := snapshotJobFromJob(job)
	status, err := reconcileSnapshotDeletionJob(ctx, provider.Client, provider.Owner, deletion, s3Exporter)
	if err != nil {
		return deletion, "", err
	}
	return deletion, status, nil
}

func (provider *S3) ensureSnapshotDeletion(
	ctx context.Context,
	name string,
	upload *SnapshotJobIdentity,
) (*batchv1.Job, error) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-delete", name),
			Namespace: provider.Owner.GetNamespace(),
			Labels: map[string]string{
				labelExporter:    s3Exporter,
				labelOwner:       provider.Owner.GetName(),
				labelType:        typeDelete,
				labelDestination: provider.destinationLabel(),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](5),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					PriorityClassName:  provider.priorityClass,
					ServiceAccountName: provider.serviceAccountName(),
					ImagePullSecrets:   provider.imagePullSecrets,
					Containers: []corev1.Container{{
						Name:            "dataexporter",
						Image:           provider.dataExporterImage,
						ImagePullPolicy: corev1.PullAlways,
						SecurityContext: k8s.RestrictedSecurityContext(),
						Args:            []string{"s3", "delete", provider.Config.Bucket, name},
						WorkingDir:      "/app",
						Env:             append(provider.storageEnv(), corev1.EnvVar{Name: "CONCURRENT_JOBS", Value: strconv.Itoa(provider.Config.GetConcurrentJobs())}),
						EnvFrom:         provider.credentialsEnvFrom(),
					}},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(provider.Owner, job, provider.Scheme); err != nil {
		return nil, err
	}
	if upload != nil {
		setSnapshotDeletionUploadIdentity(job, provider.Owner, s3Exporter, *upload)
	}
	job, _, err := ensureSnapshotJob(ctx, provider.Client, provider.Owner, job, "delete")
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (provider *S3) CleanupSnapshotDeletion(ctx context.Context, snapshotJob SnapshotJob) error {
	return cleanupSnapshotDeletionResources(ctx, provider.Client, provider.Owner, snapshotJob, snapshotJobExporter(snapshotJob, s3Exporter))
}

func (provider *S3) cleanUp(ctx context.Context, name string) error {
	propagation := metav1.DeletePropagationForeground
	err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Delete(ctx, fmt.Sprintf("%s-upload", name), metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	err = provider.Client.CoreV1().PersistentVolumeClaims(provider.Owner.GetNamespace()).Delete(ctx, fmt.Sprintf("%s-upload", name), metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (provider *S3) ListSnapshots(ctx context.Context) ([]SnapshotJob, error) {
	return listSnapshotJobs(ctx, provider.Client, provider.Owner, s3Exporter, provider.destinationLabel())
}
