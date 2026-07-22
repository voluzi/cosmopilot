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

const (
	gcsExporter = "gcs-exporter"
)

type GCS struct {
	Client        kubernetes.Interface
	Scheme        *runtime.Scheme
	Owner         metav1.Object
	priorityClass string
	Config        *appsv1.GcsExportConfig
	ExportConfig  *appsv1.ExportTarballConfig
}

func NewGcsSnapshotProvider(client kubernetes.Interface, scheme *runtime.Scheme, owner metav1.Object, priorityClass string, cfg *appsv1.ExportTarballConfig) SnapshotProvider {
	return &GCS{
		Client:        client,
		Config:        cfg.GCS,
		ExportConfig:  cfg,
		Owner:         owner,
		Scheme:        scheme,
		priorityClass: priorityClass,
	}
}

// usesCredentialsSecret reports whether the snapshot Jobs authenticate to GCS using the mounted
// credentials secret. When false, the Jobs run as the configured ServiceAccount and rely on Workload
// Identity / Application Default Credentials instead.
func (gcs *GCS) usesCredentialsSecret() bool {
	return gcs.Config.CredentialsSecret != nil
}

// credentialsVolume returns the secret volume holding the GCS credentials, or nil when authenticating
// through Workload Identity.
func (gcs *GCS) credentialsVolume() []corev1.Volume {
	if !gcs.usesCredentialsSecret() {
		return nil
	}
	return []corev1.Volume{
		{
			Name: "credentials",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: gcs.Config.CredentialsSecret.Name,
				},
			},
		},
	}
}

// credentialsVolumeMount returns the mount for the credentials secret at /creds, or nil when
// authenticating through Workload Identity.
func (gcs *GCS) credentialsVolumeMount() []corev1.VolumeMount {
	if !gcs.usesCredentialsSecret() {
		return nil
	}
	return []corev1.VolumeMount{
		{
			Name:      "credentials",
			MountPath: "/creds",
		},
	}
}

// credentialsEnv returns the GOOGLE_APPLICATION_CREDENTIALS environment variable pointing at the
// mounted credentials secret, or nil when authenticating through Workload Identity.
func (gcs *GCS) credentialsEnv() []corev1.EnvVar {
	if !gcs.usesCredentialsSecret() {
		return nil
	}
	return []corev1.EnvVar{
		{
			Name:  "GOOGLE_APPLICATION_CREDENTIALS",
			Value: fmt.Sprintf("/creds/%s", gcs.Config.CredentialsSecret.Key),
		},
	}
}

// serviceAccountName returns the ServiceAccount the Job pods should run as. It is only set when
// authenticating through Workload Identity (no credentials secret); otherwise it is empty and the
// namespace default ServiceAccount is used, preserving the previous behavior.
func (gcs *GCS) serviceAccountName() string {
	if gcs.usesCredentialsSecret() || gcs.Config.ServiceAccountName == nil {
		return ""
	}
	return *gcs.Config.ServiceAccountName
}

func (gcs *GCS) CreateSnapshot(ctx context.Context, name string, vs *snapshotv1.VolumeSnapshot) error {
	if vs.Status.RestoreSize == nil {
		return fmt.Errorf("restore size is not available yet")
	}

	apiVersion := strings.Split(vs.APIVersion, "/")
	if len(apiVersion) == 0 {
		return fmt.Errorf("unsupported api version")
	}

	// Create job to compress and upload tarball
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-upload", name),
			Namespace: gcs.Owner.GetNamespace(),
			Labels: map[string]string{
				labelExporter: gcsExporter,
				labelOwner:    gcs.Owner.GetName(),
				labelType:     typeUpload,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To[int32](180),
			BackoffLimit:            ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					PriorityClassName:  gcs.priorityClass,
					ServiceAccountName: gcs.serviceAccountName(),
					Volumes: append([]corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: fmt.Sprintf("%s-upload", name),
								},
							},
						},
					}, gcs.credentialsVolume()...),
					Containers: []corev1.Container{
						{
							Name:            "dataexporter",
							Image:           "ghcr.io/voluzi/dataexporter:latest",
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: k8s.RestrictedSecurityContext(),
							Args:            []string{"gcs", "upload", "data", gcs.Config.Bucket, name},
							WorkingDir:      "/home/app",
							Env: append(gcs.credentialsEnv(),
								corev1.EnvVar{
									Name:  "COMPRESSION",
									Value: string(gcs.ExportConfig.GetCompression()),
								},
								corev1.EnvVar{
									Name:  "SIZE_LIMIT",
									Value: gcs.Config.GetSizeLimit(),
								},
								corev1.EnvVar{
									Name:  "PART_SIZE",
									Value: gcs.Config.GetPartSize(),
								},
								corev1.EnvVar{
									Name:  "CHUNK_SIZE",
									Value: gcs.Config.GetChunkSize(),
								},
								corev1.EnvVar{
									Name:  "BUFFER_SIZE",
									Value: gcs.Config.GetBufferSize(),
								},
								corev1.EnvVar{
									Name:  "CONCURRENT_JOBS",
									Value: strconv.Itoa(gcs.Config.GetConcurrentJobs()),
								},
							),
							VolumeMounts: append(gcs.credentialsVolumeMount(),
								corev1.VolumeMount{
									Name:      "data",
									MountPath: "/home/app/data",
								},
							),
						},
					},
				},
			},
		},
	}

	err := controllerutil.SetControllerReference(gcs.Owner, job, gcs.Scheme)
	if err != nil {
		return err
	}

	job, err = gcs.Client.BatchV1().Jobs(gcs.Owner.GetNamespace()).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Create PVC from Snapshot
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-upload", name),
			Namespace: gcs.Owner.GetNamespace(),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *vs.Status.RestoreSize,
				},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiVersion[0],
				Kind:     vs.Kind,
				Name:     vs.Name,
			},
		},
	}

	err = controllerutil.SetControllerReference(job, pvc, gcs.Scheme)
	if err != nil {
		if cleanupErr := gcs.cleanUp(ctx, name); cleanupErr != nil {
			return fmt.Errorf("set PVC owner reference: %w; clean up upload job: %v", err, cleanupErr)
		}
		return err
	}

	_, err = gcs.Client.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		if cleanupErr := gcs.cleanUp(ctx, name); cleanupErr != nil {
			return fmt.Errorf("create upload PVC: %w; clean up upload job: %v", err, cleanupErr)
		}
	}
	return err
}

func (gcs *GCS) GetSnapshotStatus(ctx context.Context, name string) (SnapshotStatus, error) {
	job, err := gcs.Client.BatchV1().Jobs(gcs.Owner.GetNamespace()).Get(ctx, fmt.Sprintf("%s-upload", name), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return SnapshotNotFound, gcs.cleanUp(ctx, name)
		}
		return "", err
	}

	switch {
	case job.Status.Active > 0:
		return SnapshotActive, nil

	case job.Status.Failed > 0:
		return SnapshotFailed, gcs.cleanUp(ctx, name)

	case job.Status.Succeeded >= 1:
		return SnapshotSucceeded, gcs.cleanUp(ctx, name)

	default:
		return "", fmt.Errorf("could not determine job status")
	}
}

func (gcs *GCS) cleanUp(ctx context.Context, name string) error {
	// Use Foreground propagation so the api-server waits for the owned PVC to be
	// removed before the Job goes away. With Background propagation the cascade
	// is handed off to the garbage collector and the PVC (and its underlying PV
	// and disk) can be left behind intermittently. See issue #27.
	propagation := metav1.DeletePropagationForeground
	err := gcs.Client.BatchV1().Jobs(gcs.Owner.GetNamespace()).Delete(ctx, fmt.Sprintf("%s-upload", name), metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	// Belt-and-braces: explicitly delete the PVC in case the Job was already
	// gone (e.g. reaped by TTLSecondsAfterFinished) which leaves the PVC with a
	// dangling owner reference that the garbage collector won't always clean up
	// before the underlying disk is released.
	err = gcs.Client.CoreV1().PersistentVolumeClaims(gcs.Owner.GetNamespace()).Delete(ctx, fmt.Sprintf("%s-upload", name), metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (gcs *GCS) DeleteSnapshot(ctx context.Context, name string) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-delete", name),
			Namespace: gcs.Owner.GetNamespace(),
			Labels: map[string]string{
				labelExporter: gcsExporter,
				labelOwner:    gcs.Owner.GetName(),
				labelType:     typeDelete,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To[int32](60),
			BackoffLimit:            ptr.To[int32](5),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					PriorityClassName:  gcs.priorityClass,
					ServiceAccountName: gcs.serviceAccountName(),
					Volumes:            gcs.credentialsVolume(),
					Containers: []corev1.Container{
						{
							Name:            "dataexporter",
							Image:           "ghcr.io/voluzi/dataexporter:latest",
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: k8s.RestrictedSecurityContext(),
							Args:            []string{"gcs", "delete", gcs.Config.Bucket, name},
							WorkingDir:      "/app",
							Env: append(gcs.credentialsEnv(),
								corev1.EnvVar{
									Name:  "CONCURRENT_JOBS",
									Value: strconv.Itoa(gcs.Config.GetConcurrentJobs()),
								},
							),
							VolumeMounts: gcs.credentialsVolumeMount(),
						},
					},
				},
			},
		},
	}

	err := controllerutil.SetControllerReference(gcs.Owner, job, gcs.Scheme)
	if err != nil {
		return err
	}

	if _, err = gcs.Client.BatchV1().Jobs(gcs.Owner.GetNamespace()).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return err
	}
	return gcs.cleanUp(ctx, name)
}

func (gcs *GCS) ListSnapshots(ctx context.Context) ([]string, error) {
	listOptions := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			labelExporter: gcsExporter,
			labelOwner:    gcs.Owner.GetName(),
			labelType:     typeUpload, // We only list uploading jobs since the deleting jobs are auto deleted
		}).String(),
	}
	list, err := gcs.Client.BatchV1().Jobs(gcs.Owner.GetNamespace()).List(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	snapshotNames := make([]string, len(list.Items))
	for i, job := range list.Items {
		snapshotNames[i] = strings.TrimSuffix(job.GetName(), "-upload")
	}
	return snapshotNames, nil
}
