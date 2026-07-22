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
	"k8s.io/apimachinery/pkg/labels"
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
	Client        kubernetes.Interface
	Scheme        *runtime.Scheme
	Owner         metav1.Object
	priorityClass string
	Config        *appsv1.S3ExportConfig
	ExportConfig  *appsv1.ExportTarballConfig
}

func NewS3SnapshotProvider(
	client kubernetes.Interface,
	scheme *runtime.Scheme,
	owner metav1.Object,
	priorityClass string,
	cfg *appsv1.ExportTarballConfig,
) SnapshotProvider {
	return &S3{
		Client:        client,
		Scheme:        scheme,
		Owner:         owner,
		priorityClass: priorityClass,
		Config:        cfg.S3,
		ExportConfig:  cfg,
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

func (provider *S3) CreateSnapshot(ctx context.Context, name string, snapshot *snapshotv1.VolumeSnapshot) error {
	if snapshot.Status.RestoreSize == nil {
		return fmt.Errorf("restore size is not available yet")
	}
	apiVersion := strings.Split(snapshot.APIVersion, "/")
	if len(apiVersion) == 0 {
		return fmt.Errorf("unsupported api version")
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-upload", name),
			Namespace: provider.Owner.GetNamespace(),
			Labels: map[string]string{
				labelExporter: s3Exporter,
				labelOwner:    provider.Owner.GetName(),
				labelType:     typeUpload,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To[int32](180),
			BackoffLimit:            ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					PriorityClassName:  provider.priorityClass,
					ServiceAccountName: provider.serviceAccountName(),
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
						Image:           "ghcr.io/voluzi/dataexporter:latest",
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
	if err := controllerutil.SetControllerReference(provider.Owner, job, provider.Scheme); err != nil {
		return err
	}
	createdJob, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
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
	if err := controllerutil.SetControllerReference(createdJob, pvc, provider.Scheme); err != nil {
		return err
	}
	_, err = provider.Client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	return err
}

func (provider *S3) GetSnapshotStatus(ctx context.Context, name string) (SnapshotStatus, error) {
	job, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Get(ctx, fmt.Sprintf("%s-upload", name), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return SnapshotNotFound, provider.cleanUp(ctx, name)
		}
		return "", err
	}
	switch {
	case job.Status.Active > 0:
		return SnapshotActive, nil
	case job.Status.Failed > 0:
		return SnapshotFailed, provider.cleanUp(ctx, name)
	case job.Status.Succeeded >= 1:
		return SnapshotSucceeded, provider.cleanUp(ctx, name)
	default:
		return "", fmt.Errorf("could not determine job status")
	}
}

func (provider *S3) DeleteSnapshot(ctx context.Context, name string) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-delete", name),
			Namespace: provider.Owner.GetNamespace(),
			Labels: map[string]string{
				labelExporter: s3Exporter,
				labelOwner:    provider.Owner.GetName(),
				labelType:     typeDelete,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To[int32](60),
			BackoffLimit:            ptr.To[int32](5),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					PriorityClassName:  provider.priorityClass,
					ServiceAccountName: provider.serviceAccountName(),
					Containers: []corev1.Container{{
						Name:            "dataexporter",
						Image:           "ghcr.io/voluzi/dataexporter:latest",
						ImagePullPolicy: corev1.PullAlways,
						SecurityContext: k8s.RestrictedSecurityContext(),
						Args:            []string{"s3", "delete", provider.Config.Bucket, name},
						WorkingDir:      "/home/app",
						Env:             append(provider.storageEnv(), corev1.EnvVar{Name: "CONCURRENT_JOBS", Value: strconv.Itoa(provider.Config.GetConcurrentJobs())}),
						EnvFrom:         provider.credentialsEnvFrom(),
					}},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(provider.Owner, job, provider.Scheme); err != nil {
		return err
	}
	if _, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return err
	}
	return provider.cleanUp(ctx, name)
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

func (provider *S3) ListSnapshots(ctx context.Context) ([]string, error) {
	selector := labels.SelectorFromSet(map[string]string{
		labelExporter: s3Exporter,
		labelOwner:    provider.Owner.GetName(),
		labelType:     typeUpload,
	}).String()
	list, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	names := make([]string, len(list.Items))
	for i, job := range list.Items {
		names[i] = strings.TrimSuffix(job.Name, "-upload")
	}
	return names, nil
}
