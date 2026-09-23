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

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

func requireUploadJobHardening(t *testing.T, job *batchv1.Job, resourceName corev1.ResourceName, want int64) {
	t.Helper()
	container := job.Spec.Template.Spec.Containers[0]
	request, ok := container.Resources.Requests[resourceName]
	require.True(t, ok, "export pod must request %s", resourceName)
	assert.Equal(t, want, request.Value())

	policy := job.Spec.PodFailurePolicy
	require.NotNil(t, policy, "evictions must not consume the export Job's only attempt")
	var ignoresDisruption, ignoresKills bool
	for _, rule := range policy.Rules {
		if rule.Action != batchv1.PodFailurePolicyActionIgnore {
			continue
		}
		for _, cond := range rule.OnPodConditions {
			ignoresDisruption = ignoresDisruption || cond.Type == corev1.DisruptionTarget
		}
		if rule.OnExitCodes != nil && rule.OnExitCodes.Operator == batchv1.PodFailurePolicyOnExitCodesOpIn {
			ignoresKills = assert.ElementsMatch(t, []int32{137, 143}, rule.OnExitCodes.Values)
		}
	}
	assert.True(t, ignoresDisruption)
	assert.True(t, ignoresKills)
	for _, rule := range policy.Rules {
		if rule.OnExitCodes != nil && rule.OnExitCodes.ContainerName != nil {
			assert.Equal(t, container.Name, *rule.OnExitCodes.ContainerName)
		}
	}
}

func TestUploadJobsRequestResourcesAndIgnoreDisruptions(t *testing.T) {
	gcs := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
		Bucket: "snapshots", ChunkSize: ptr.To("100MB"), ConcurrentJobs: ptr.To(3),
	}})
	requireUploadJobHardening(t, gcs.uploadJob("snapshot"), corev1.ResourceMemory, int64(4*100*datasize.MB))

	s3 := newTestS3Provider(t, &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{
		Bucket: "snapshots", Region: "eu-west-1", ChunkSize: ptr.To("64MB"), ConcurrentJobs: ptr.To(2),
	}})
	requireUploadJobHardening(t, s3.uploadJob("snapshot"), corev1.ResourceEphemeralStorage, int64(3*64*datasize.MB))
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
