package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
)

func assertTerminalNativeWorkerDeniedOnCSIClone(source *appsv1.ChainNode, archive *appsv1.SnapshotExportArchive) {
	GinkgoHelper()
	ctx := Framework().Context()
	c := Framework().Client()
	a := &appsv1.SnapshotExportAttempt{ID: archive.AttemptID, Sequence: archive.AttemptSequence, ExportID: archive.ExportID, Recipe: archive.Recipe, RecipeSHA256: archive.RecipeSHA256}
	recipe, err := exportprotocol.DecodeRecipe(a)
	Expect(err).NotTo(HaveOccurred())
	// This unbound late worker mounts a real CSI clone; HTTP-delay ordering is tested separately in envtest.
	recipe.ResourceName = "late-" + string(uuid.NewUUID())
	job := datasnapshot.NativeJob(a, recipe)
	job.Labels = nil
	job.Spec.Template.Labels = nil
	Expect(c.Create(ctx, job)).To(Succeed())
	pvc := datasnapshot.NativePVC(a, recipe, job.UID)
	pvc.Labels = nil
	Expect(c.Create(ctx, pvc)).To(Succeed())
	DeferCleanup(func() {
		_ = c.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
		_ = c.Delete(ctx, pvc)
		for _, o := range []client.Object{job, pvc} {
			if c.Get(ctx, client.ObjectKeyFromObject(o), o) == nil {
				o.SetFinalizers(nil)
				_ = c.Update(ctx, o)
			}
		}
	})
	var worker corev1.Pod
	Eventually(func() string {
		pods := &corev1.PodList{}
		if c.List(ctx, pods, client.InNamespace(source.Namespace)) != nil {
			return ""
		}
		for _, pod := range pods.Items {
			if metav1.IsControlledBy(&pod, job) {
				worker = pod
				return string(pod.Status.Phase)
			}
		}
		return ""
	}, 3*time.Minute, time.Second).Should(Equal(string(corev1.PodFailed)))
	Expect(worker.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
	Expect(worker.Spec.Containers[0].VolumeMounts[0].ReadOnly).To(BeTrue())
	Expect(worker.Status.ContainerStatuses).To(HaveLen(1))
	Expect(worker.Status.ContainerStatuses[0].State.Terminated).NotTo(BeNil())
	Expect(worker.Status.ContainerStatuses[0].State.Terminated.ExitCode).NotTo(BeZero())
	// A started container establishes that kubelet completed its required CSI volume mounts.
	Expect(worker.Status.ContainerStatuses[0].ContainerID).NotTo(BeEmpty())
	output, err := Framework().KubeClient().CoreV1().Pods(source.Namespace).GetLogs(worker.Name, &corev1.PodLogOptions{Container: "exporter"}).DoRaw(ctx)
	Expect(err).NotTo(HaveOccurred())
	Expect(string(output)).To(ContainSubstring("native authority denied"), fmt.Sprintf("late worker logs: %s", output))
}
