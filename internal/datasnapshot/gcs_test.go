package datasnapshot

import (
	"context"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
)

func TestGCSCreateSnapshotAuthModes(t *testing.T) {
	tests := []struct {
		name                 string
		config               *appsv1.GcsExportConfig
		wantServiceAccount   string
		wantCredentialsEnv   bool
		wantCredentialsVol   bool
		wantCredentialsMount bool
	}{
		{
			name: "credentials secret",
			config: &appsv1.GcsExportConfig{
				Bucket: "snapshots",
				CredentialsSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-creds"},
					Key:                  "credentials.json",
				},
			},
			wantCredentialsEnv:   true,
			wantCredentialsVol:   true,
			wantCredentialsMount: true,
		},
		{
			name: "workload identity service account",
			config: &appsv1.GcsExportConfig{
				Bucket:             "snapshots",
				ServiceAccountName: ptrTo("snapshot-publisher"),
			},
			wantServiceAccount: "snapshot-publisher",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{
				Compression: ptr.To(appsv1.TarballCompression(dataexporter.CompressionZstd)),
				GCS:         tt.config,
			})
			vs := &snapshotv1.VolumeSnapshot{
				TypeMeta:   metav1.TypeMeta{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot"},
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"},
				Status: &snapshotv1.VolumeSnapshotStatus{
					RestoreSize: resource.NewQuantity(1024, resource.BinarySI),
				},
			}

			err := provider.CreateSnapshot(context.Background(), "snapshot", vs)
			require.NoError(t, err)

			job := getJob(t, provider, "snapshot-upload")
			podSpec := job.Spec.Template.Spec
			container := podSpec.Containers[0]
			assert.Equal(t, tt.wantServiceAccount, podSpec.ServiceAccountName)
			assert.Equal(t, tt.wantCredentialsVol, hasVolume(podSpec.Volumes, "credentials"))
			assert.Equal(t, tt.wantCredentialsMount, hasVolumeMount(container.VolumeMounts, "credentials"))
			assert.Equal(t, tt.wantCredentialsEnv, hasEnv(container.Env, "GOOGLE_APPLICATION_CREDENTIALS"))
			assert.Equal(t, "zstd", envValue(container.Env, "COMPRESSION"))
		})
	}
}

func TestGCSDeleteSnapshotAuthModes(t *testing.T) {
	tests := []struct {
		name                 string
		config               *appsv1.GcsExportConfig
		wantServiceAccount   string
		wantCredentialsEnv   bool
		wantCredentialsVol   bool
		wantCredentialsMount bool
	}{
		{
			name: "credentials secret",
			config: &appsv1.GcsExportConfig{
				Bucket: "snapshots",
				CredentialsSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-creds"},
					Key:                  "credentials.json",
				},
			},
			wantCredentialsEnv:   true,
			wantCredentialsVol:   true,
			wantCredentialsMount: true,
		},
		{
			name: "workload identity service account",
			config: &appsv1.GcsExportConfig{
				Bucket:             "snapshots",
				ServiceAccountName: ptrTo("snapshot-publisher"),
			},
			wantServiceAccount: "snapshot-publisher",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: tt.config})

			err := provider.DeleteSnapshot(context.Background(), "snapshot")
			require.NoError(t, err)

			job := getJob(t, provider, "snapshot-delete")
			podSpec := job.Spec.Template.Spec
			container := podSpec.Containers[0]
			assert.Equal(t, tt.wantServiceAccount, podSpec.ServiceAccountName)
			assert.Equal(t, tt.wantCredentialsVol, hasVolume(podSpec.Volumes, "credentials"))
			assert.Equal(t, tt.wantCredentialsMount, hasVolumeMount(container.VolumeMounts, "credentials"))
			assert.Equal(t, tt.wantCredentialsEnv, hasEnv(container.Env, "GOOGLE_APPLICATION_CREDENTIALS"))
		})
	}
}

func TestGCSGetSnapshotStatusCleansTerminalUploadResources(t *testing.T) {
	tests := []struct {
		name       string
		createJob  bool
		wantStatus SnapshotStatus
	}{
		{name: "failed job", createJob: true, wantStatus: SnapshotFailed},
		{name: "missing job", wantStatus: SnapshotNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
				Bucket:             "snapshots",
				ServiceAccountName: ptr.To("snapshot-exporter"),
			}})
			if tt.createJob {
				_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
					Status:     batchv1.JobStatus{Failed: 1},
				}, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			_, err := provider.Client.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			status, err := provider.GetSnapshotStatus(context.Background(), "snapshot")
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, status)

			_, err = provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
			_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
		})
	}
}

func newTestGCSProvider(t *testing.T, cfg *appsv1.ExportTarballConfig) *GCS {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	owner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"},
	}

	return NewGcsSnapshotProvider(fake.NewSimpleClientset(), scheme, owner, "", cfg).(*GCS)
}

func getJob(t *testing.T, provider *GCS, name string) *batchv1.Job {
	t.Helper()

	job, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return job
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, volume := range volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

func hasVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	for _, mount := range mounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

func hasEnv(envs []corev1.EnvVar, name string) bool {
	for _, env := range envs {
		if env.Name == name {
			return true
		}
	}
	return false
}

func ptrTo[T any](v T) *T {
	return &v
}
