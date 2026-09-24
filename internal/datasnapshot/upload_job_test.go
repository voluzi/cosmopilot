package datasnapshot

import (
	"testing"

	"github.com/c2h5oh/datasize"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
)

func requireUploadJobHardening(t *testing.T, job *batchv1.Job, wantRequests corev1.ResourceList) {
	t.Helper()
	requests := job.Spec.Template.Spec.Containers[0].Resources.Requests
	require.Len(t, requests, len(wantRequests))
	for name, want := range wantRequests {
		got, ok := requests[name]
		require.True(t, ok, "export pod must request %s", name)
		assert.Zero(t, want.Cmp(got), "%s: want %s, got %s", name, want.String(), got.String())
	}

	// Only disruptions are ignored: an OOM kill or exporter error must still fail the Job.
	require.Equal(t, &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
		Action: batchv1.PodFailurePolicyActionIgnore,
		OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{{
			Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
		}},
	}}}, job.Spec.PodFailurePolicy)
}

func TestUploadJobsRequestResourcesAndIgnoreDisruptions(t *testing.T) {
	gcs := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
		Bucket: "snapshots", ChunkSize: ptr.To("100MB"), BufferSize: ptr.To("10MB"), ConcurrentJobs: ptr.To(3),
	}})
	requireUploadJobHardening(t, gcs.uploadJob("snapshot"), corev1.ResourceList{
		corev1.ResourceMemory: *resource.NewQuantity(int64(4*100*datasize.MB+3*10*datasize.MB+3*16*datasize.MB), resource.BinarySI),
	})

	s3 := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{
		Bucket: "snapshots", Region: "eu-west-1", ChunkSize: ptr.To("64MB"), BufferSize: ptr.To("16MB"), ConcurrentJobs: ptr.To(2),
	}})
	requireUploadJobHardening(t, s3.uploadJob("snapshot"), corev1.ResourceList{
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(int64(3*64*datasize.MB), resource.BinarySI),
		corev1.ResourceMemory:           *resource.NewQuantity(int64(2*16*datasize.MB), resource.BinarySI),
	})
}

func TestUploadJobsUseConfiguredResources(t *testing.T) {
	custom := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
	gcs := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}, Resources: custom})
	assert.Equal(t, *custom, gcs.uploadJob("snapshot").Spec.Template.Spec.Containers[0].Resources)

	s3 := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}, Resources: custom})
	assert.Equal(t, *custom, s3.uploadJob("snapshot").Spec.Template.Spec.Containers[0].Resources)
}

func TestDeletionJobFromUploadDropsExportResources(t *testing.T) {
	gcs := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	upload := gcs.uploadJob("snapshot")
	require.NotEmpty(t, upload.Spec.Template.Spec.Containers[0].Resources.Requests)

	deletion, err := deletionJobFromUpload(upload, gcs.Owner, gcsExporter, SnapshotJobIdentity{})
	require.NoError(t, err)
	assert.Empty(t, deletion.Spec.Template.Spec.Containers[0].Resources)
}

func TestUploadRequestRejectsOverflow(t *testing.T) {
	_, ok := uploadRequest("8EB", 2)
	assert.False(t, ok)
	quantity, ok := uploadRequest("1GB", 3)
	require.True(t, ok)
	assert.Equal(t, int64(3*datasize.GB), quantity.Value())
}
