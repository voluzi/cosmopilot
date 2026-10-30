package datasnapshot

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
)

func TestNativeWorkerRejectsAdmittedArchiveChanges(t *testing.T) {
	a := &appsv1.SnapshotExportAttempt{ID: "attempt", Sequence: 1}
	r := exportprotocol.Recipe{Namespace: "ns", Owner: appsv1.ExportResourceIdentity{Name: "owner", UID: "owner-uid"}, ResourceName: "export-attempt", Image: "exporter:test", Credentials: appsv1.ExportResourceIdentity{Name: "credentials", UID: "secret"}, ServiceAccount: appsv1.ExportResourceIdentity{Name: "export-owner", UID: "sa"}}
	r.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}
	job := NativeJob(a, r)
	cleanup := NativeCleanupJob(a, r)
	require.Equal(t, "/app", cleanup.Spec.Template.Spec.Containers[0].WorkingDir)
	require.Nil(t, job.Spec.PodFailurePolicy)
	for _, cleanupMode := range []bool{false, true} {
		spec := job.Spec.Template.Spec.DeepCopy()
		validate := ValidateNativePodSpec
		if cleanupMode {
			spec = cleanup.Spec.Template.Spec.DeepCopy()
			validate = ValidateNativeCleanupPodSpec
		}
		spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")}
		spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
		require.NoError(t, validate(spec, a, r))
		spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("32Mi")
		require.Error(t, validate(spec, a, r))
	}

	require.Contains(t, job.Finalizers, exportprotocol.AttemptFinalizer)
	require.NoError(t, ValidateNativePodSpec(&job.Spec.Template.Spec, a, r))
	for _, tc := range []struct {
		name   string
		change func(*corev1.PodSpec)
	}{
		{"writable mount", func(p *corev1.PodSpec) { p.Containers[0].VolumeMounts[0].ReadOnly = false }},
		{"command", func(p *corev1.PodSpec) { p.Containers[0].Command = []string{"sh"} }},
		{"sidecar", func(p *corev1.PodSpec) {
			p.Containers = append(p.Containers, corev1.Container{Name: "sidecar", Image: "other"})
		}},
		{"init", func(p *corev1.PodSpec) { p.InitContainers = []corev1.Container{{Name: "init", Image: "other"}} }},
		{"envFrom", func(p *corev1.PodSpec) { p.Containers[0].EnvFrom = []corev1.EnvFromSource{{Prefix: "OTHER"}} }},
		{"identity", func(p *corev1.PodSpec) { p.ServiceAccountName = "application" }},
		{"replacement source", func(p *corev1.PodSpec) { p.Volumes[0].PersistentVolumeClaim.ClaimName = "replacement" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := job.Spec.Template.Spec.DeepCopy()
			tc.change(p)
			require.Error(t, ValidateNativePodSpec(p, a, r))
		})
	}
	pvc := NativePVC(a, r, types.UID("job-uid"))
	require.Contains(t, pvc.Finalizers, exportprotocol.AttemptFinalizer)
	require.Equal(t, types.UID("job-uid"), pvc.OwnerReferences[0].UID)
}
