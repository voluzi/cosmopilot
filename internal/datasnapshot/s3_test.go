package datasnapshot

import (
	"context"
	"errors"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
)

func TestS3CreateSnapshotAuthAndStorageOptions(t *testing.T) {
	tests := []struct {
		name               string
		config             *appsv1.S3ExportConfig
		wantServiceAccount string
		wantSecretEnvFrom  string
	}{
		{
			name: "default AWS credential chain",
			config: &appsv1.S3ExportConfig{
				Bucket: "snapshots",
				Region: "eu-west-1",
			},
		},
		{
			name: "access keys",
			config: &appsv1.S3ExportConfig{
				Bucket:            "snapshots",
				Region:            "eu-west-1",
				CredentialsSecret: &corev1.LocalObjectReference{Name: "aws-credentials"},
			},
			wantSecretEnvFrom: "aws-credentials",
		},
		{
			name: "IRSA",
			config: &appsv1.S3ExportConfig{
				Bucket:             "snapshots",
				Region:             "eu-west-1",
				ServiceAccountName: ptr.To("snapshot-exporter"),
			},
			wantServiceAccount: "snapshot-exporter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			export := &appsv1.ExportTarballConfig{
				Compression: ptr.To(appsv1.TarballCompression(dataexporter.CompressionLz4)),
				S3:          tt.config,
			}
			provider := newTestS3Provider(t, export)
			vs := testVolumeSnapshot()

			require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", vs))

			job := getS3Job(t, provider, "snapshot-upload")
			assert.Nil(t, job.Spec.TTLSecondsAfterFinished)
			podSpec := job.Spec.Template.Spec
			container := podSpec.Containers[0]
			assert.Equal(t, tt.wantServiceAccount, podSpec.ServiceAccountName)
			assert.Equal(t, tt.wantSecretEnvFrom, secretEnvFromName(container.EnvFrom))
			assert.Equal(t, []string{"s3", "upload", "data", "snapshots", "snapshot"}, container.Args)
			assert.Equal(t, "eu-west-1", envValue(container.Env, "AWS_REGION"))
			assert.Equal(t, "lz4", envValue(container.Env, "COMPRESSION"))
		})
	}
}

func TestS3CreateSnapshotS3CompatibleOptions(t *testing.T) {
	export := &appsv1.ExportTarballConfig{
		S3: &appsv1.S3ExportConfig{
			Bucket:         "snapshots",
			Region:         "us-east-1",
			Endpoint:       ptr.To("http://minio.storage.svc:9000"),
			ForcePathStyle: ptr.To(true),
		},
	}
	provider := newTestS3Provider(t, export)
	require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))

	container := getS3Job(t, provider, "snapshot-upload").Spec.Template.Spec.Containers[0]
	assert.Equal(t, "http://minio.storage.svc:9000", envValue(container.Env, "S3_ENDPOINT"))
	assert.Equal(t, "true", envValue(container.Env, "S3_FORCE_PATH_STYLE"))
}

func TestS3DeleteSnapshotUsesSameAuthentication(t *testing.T) {
	export := &appsv1.ExportTarballConfig{
		S3: &appsv1.S3ExportConfig{
			Bucket:            "snapshots",
			Region:            "eu-west-1",
			CredentialsSecret: &corev1.LocalObjectReference{Name: "aws-credentials"},
		},
	}
	provider := newTestS3Provider(t, export)
	require.NoError(t, provider.DeleteSnapshot(context.Background(), "snapshot"))

	container := getS3Job(t, provider, "snapshot-delete").Spec.Template.Spec.Containers[0]
	assert.Equal(t, "aws-credentials", secretEnvFromName(container.EnvFrom))
	assert.Equal(t, []string{"s3", "delete", "snapshots", "snapshot"}, container.Args)
	assert.Equal(t, "/app", container.WorkingDir)
}

func TestS3GetSnapshotStatusPreservesUploadResources(t *testing.T) {
	tests := []struct {
		name       string
		createJob  bool
		jobStatus  batchv1.JobStatus
		wantStatus SnapshotStatus
	}{
		{name: "failed job", createJob: true, jobStatus: batchv1.JobStatus{Failed: 1}, wantStatus: SnapshotFailed},
		{name: "successful job", createJob: true, jobStatus: batchv1.JobStatus{Succeeded: 1}, wantStatus: SnapshotSucceeded},
		{name: "missing job", wantStatus: SnapshotNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{
				Bucket: "snapshots",
				Region: "eu-west-1",
			}})
			if tt.createJob {
				_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
					Status:     tt.jobStatus,
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
			if tt.createJob {
				require.NoError(t, err)
			} else {
				assert.True(t, apierrors.IsNotFound(err))
			}
			_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestS3CleanupSnapshotDeletesUploadResources(t *testing.T) {
	provider := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}})
	require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))

	require.NoError(t, provider.CleanupSnapshot(context.Background(), "snapshot"))

	_, err := provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestS3CreateSnapshotIsIdempotent(t *testing.T) {
	tests := []struct {
		name      string
		deletePVC bool
	}{
		{name: "existing job and PVC"},
		{name: "existing job with missing PVC", deletePVC: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}})
			require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))
			if tt.deletePVC {
				require.NoError(t, provider.Client.CoreV1().PersistentVolumeClaims("default").Delete(context.Background(), "snapshot-upload", metav1.DeleteOptions{}))
			}

			require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))
			_, err := provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestS3CreateSnapshotRejectsForeignPVCWithoutDeletingIt(t *testing.T) {
	provider := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}})
	_, err := provider.Client.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	err = provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot())
	require.ErrorContains(t, err, "not controlled by upload job")

	_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestS3CreateSnapshotRejectsForeignJob(t *testing.T) {
	provider := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}})
	_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	err = provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot())
	require.ErrorContains(t, err, "not controlled by snapshot owner")

	_, err = provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestS3CreateSnapshotCleansJobWhenPVCCreationFails(t *testing.T) {
	provider := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{
		Bucket: "snapshots",
		Region: "eu-west-1",
	}})
	client := provider.Client.(*fake.Clientset)
	client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("PVC create failed")
	})

	err := provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot())
	require.ErrorContains(t, err, "PVC create failed")

	_, err = provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func newTestS3Provider(t *testing.T, cfg *appsv1.ExportTarballConfig) *S3 {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	owner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"},
	}
	return NewS3SnapshotProvider(fake.NewSimpleClientset(), scheme, owner, "", cfg).(*S3)
}

func testVolumeSnapshot() *snapshotv1.VolumeSnapshot {
	return &snapshotv1.VolumeSnapshot{
		TypeMeta:   metav1.TypeMeta{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot"},
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"},
		Status: &snapshotv1.VolumeSnapshotStatus{
			RestoreSize: resource.NewQuantity(1024, resource.BinarySI),
		},
	}
}

func getS3Job(t *testing.T, provider *S3, name string) *batchv1.Job {
	t.Helper()
	job, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return job
}

func secretEnvFromName(sources []corev1.EnvFromSource) string {
	for _, source := range sources {
		if source.SecretRef != nil {
			return source.SecretRef.Name
		}
	}
	return ""
}

func envValue(envs []corev1.EnvVar, name string) string {
	for _, env := range envs {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}
