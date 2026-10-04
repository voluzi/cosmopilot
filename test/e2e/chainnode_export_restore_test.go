package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/pkg/environ"
	"github.com/voluzi/cosmopilot/v5/test/e2e/apps"
)

var _ = Describe("Exported snapshot restore E2E", func() {
	It("should bootstrap from an S3 export and preserve data on pod recreation", WithApp(func(app apps.TestApp, ns *corev1.Namespace) {
		ctx := Framework().Context()
		c := Framework().Client()
		endpoint, credentials := createRestoreObjectStore(ns)
		source := app.BuildChainNode(ns.Name)
		configureSnapshotPersistence(source)
		source.Spec.Persistence.Snapshots.Frequency = "1h"
		source.Spec.Persistence.Snapshots.StopNode = ptr.To(true)
		source.Spec.Persistence.Snapshots.ExportTarball = &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{
			Bucket: "backups", Region: "us-east-1", Endpoint: ptr.To(endpoint), ForcePathStyle: ptr.To(true), CredentialsSecret: &corev1.LocalObjectReference{Name: credentials}, ChunkSize: ptr.To("5MB"), ConcurrentJobs: ptr.To(1), BufferSize: ptr.To("1MB"),
		}}
		Expect(c.Create(ctx, source)).To(Succeed())
		WaitForChainNodeHeight(source, 5)
		var object string
		Eventually(func() string {
			RefreshChainNode(source)
			for _, export := range source.Status.SnapshotExports {
				if export.Phase == appsv1.SnapshotExportPhaseUploaded {
					return export.ObjectName + ".tar.gz"
				}
			}
			return ""
		}, 5*time.Minute, 5*time.Second).ShouldNot(BeEmpty())
		for _, export := range source.Status.SnapshotExports {
			if export.Phase == appsv1.SnapshotExportPhaseUploaded {
				object = export.ObjectName + ".tar.gz"
				break
			}
		}
		digestOutput, err := Framework().PodExec(ns.Name, "restore-store", "server", "sh", "-c", `mc cp local/backups/"$1" /tmp/export.tar.gz >/dev/null && sha256sum /tmp/export.tar.gz`, "sh", object)
		Expect(err).NotTo(HaveOccurred())
		digest := strings.Fields(digestOutput)[0]
		Expect(digest).To(MatchRegexp("^[0-9a-f]{64}$"))
		restore := app.BuildChainNode(ns.Name)
		restore.Spec.Validator = nil
		restore.Spec.Genesis = &appsv1.GenesisConfig{ConfigMap: ptr.To(source.Spec.Genesis.GetConfigMapName(source.Status.ChainID))}
		restore.Spec.Persistence = &appsv1.Persistence{StorageClassName: source.Spec.Persistence.StorageClassName, AdditionalVolumes: restore.GetPersistenceAdditionalVolumes()}
		restore.Spec.Persistence.Restore = &appsv1.SnapshotRestoreConfig{
			Snapshot:     appsv1.SnapshotRestoreSource{Provider: "s3", Bucket: "backups", Name: object, Region: "us-east-1", Endpoint: endpoint, ForcePathStyle: true, CredentialsSecret: &appsv1.SnapshotExportSecretReference{Name: credentials}},
			Verification: &appsv1.SnapshotRestoreVerification{SHA256: digest},
		}
		restore.Spec.Persistence.InitTimeout = ptr.To("5m")
		Expect(c.Create(ctx, restore)).To(Succeed())
		WaitForChainNodeHeight(restore, 5)
		RefreshChainNode(restore)
		firstHeight := restore.Status.LatestHeight
		WaitForChainNodeHeight(restore, firstHeight+5)
		Eventually(func() bool {
			RefreshChainNode(source)
			RefreshChainNode(restore)
			return source.Status.LatestHeight-restore.Status.LatestHeight <= 5
		}, 2*time.Minute, 5*time.Second).Should(BeTrue())
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(restore), pvc)).To(Succeed())
		uid := pvc.UID
		Expect(pvc.Annotations[controllers.AnnotationDataInitialized]).To(Equal("true"))
		pod := &corev1.Pod{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(restore), pod)).To(Succeed())
		oldPodUID := pod.UID
		RefreshChainNode(restore)
		restartHeight := restore.Status.LatestHeight
		Expect(c.Delete(ctx, pod)).To(Succeed())
		Eventually(func() bool {
			if c.Get(ctx, client.ObjectKeyFromObject(restore), pod) != nil {
				return false
			}
			return pod.UID != oldPodUID
		}, 2*time.Minute, time.Second).Should(BeTrue())
		WaitForPodReady(ns.Name, restore.Name)
		WaitForChainNodeHeight(restore, restartHeight+5)
		Expect(c.Get(ctx, client.ObjectKeyFromObject(restore), pvc)).To(Succeed())
		Expect(pvc.UID).To(Equal(uid))
		Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: restore.Name + "-init-data"}, &corev1.Pod{}))).To(BeTrue())

		bad := restore.DeepCopy()
		bad.ObjectMeta = metav1.ObjectMeta{GenerateName: "bad-restore-", Namespace: ns.Name}
		bad.Status = appsv1.ChainNodeStatus{}
		bad.Spec.Persistence.Restore.Verification.SHA256 = strings.Repeat("0", 64)
		Expect(c.Create(ctx, bad)).To(Succeed())
		Eventually(func() bool {
			events := &corev1.EventList{}
			Expect(c.List(ctx, events, client.InNamespace(ns.Name))).To(Succeed())
			for _, event := range events.Items {
				if event.InvolvedObject.Name == bad.Name && strings.Contains(event.Message, "SHA-256 mismatch") {
					return true
				}
			}
			return false
		}, 3*time.Minute, 2*time.Second).Should(BeTrue())
		Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(bad), &corev1.Pod{}))).To(BeTrue())
		badPVC := &corev1.PersistentVolumeClaim{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(bad), badPVC)).To(Succeed())
		Expect(badPVC.Annotations[controllers.AnnotationDataInitialized]).To(Equal("false"))
		RefreshChainNode(source)
		sourceHeight := source.Status.LatestHeight
		WaitForChainNodeHeight(source, sourceHeight+5)
	}))
})

func createRestoreObjectStore(ns *corev1.Namespace) (string, string) {
	ctx := Framework().Context()
	c := Framework().Client()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "restore-store-auth", Namespace: ns.Name}, StringData: map[string]string{
		"AWS_ACCESS_KEY_ID": "restore-test-access", "AWS_SECRET_ACCESS_KEY": "restore-test-secret",
	}}
	Expect(c.Create(ctx, secret)).To(Succeed())
	auth := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: key}}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "restore-store", Namespace: ns.Name, Labels: map[string]string{"app": "restore-store"}}, Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(int64(65532))},
		Volumes:         []corev1.Volume{{Name: "store", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		Containers: []corev1.Container{
			{Name: "server", Image: environ.GetString("MINIO_IMAGE", "cosmopilot-e2e/minio:2025-10-15"), ImagePullPolicy: corev1.PullIfNotPresent, Args: []string{"server", "/data"}, Env: []corev1.EnvVar{{Name: "MC_CONFIG_DIR", Value: "/tmp/mc"}, {Name: "AWS_ACCESS_KEY_ID", ValueFrom: auth("AWS_ACCESS_KEY_ID")}, {Name: "AWS_SECRET_ACCESS_KEY", ValueFrom: auth("AWS_SECRET_ACCESS_KEY")}, {Name: "MINIO_ROOT_USER", ValueFrom: auth("AWS_ACCESS_KEY_ID")}, {Name: "MINIO_ROOT_PASSWORD", ValueFrom: auth("AWS_SECRET_ACCESS_KEY")}}, VolumeMounts: []corev1.VolumeMount{{Name: "store", MountPath: "/data"}}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/minio/health/ready", Port: intstr.FromInt32(9000)}}}},
		},
	}}
	Expect(c.Create(ctx, pod)).To(Succeed())
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "restore-store", Namespace: ns.Name}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "restore-store"}, Ports: []corev1.ServicePort{{Port: 9000, TargetPort: intstr.FromInt32(9000)}}}}
	Expect(c.Create(ctx, service)).To(Succeed())
	Eventually(func() bool {
		Expect(c.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady {
				return condition.Status == corev1.ConditionTrue
			}
		}
		return false
	}, 3*time.Minute, 2*time.Second).Should(BeTrue())
	_, err := Framework().PodExec(ns.Name, pod.Name, "server", "sh", "-c", `mc alias set local http://localhost:9000 "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" && mc mb local/backups`)
	Expect(err).NotTo(HaveOccurred())
	return fmt.Sprintf("http://restore-store.%s.svc:9000", ns.Name), secret.Name
}
