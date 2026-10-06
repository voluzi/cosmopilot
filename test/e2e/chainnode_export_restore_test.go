package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
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
		snapshotConfig := source.Spec.Persistence.Snapshots
		source.Spec.Persistence.Snapshots = nil
		Expect(c.Create(ctx, source)).To(Succeed())
		WaitForChainNodeHeight(source, 5)
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
				return err
			}
			source.Spec.Persistence.Snapshots = snapshotConfig
			return c.Update(ctx, source)
		})).To(Succeed())
		var object, snapshotName string
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
				snapshotName = export.SnapshotName
				break
			}
		}
		snapshot := &snapshotv1.VolumeSnapshot{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: snapshotName}, snapshot)).To(Succeed())
		height, err := strconv.ParseInt(snapshot.Annotations[controllers.AnnotationDataHeight], 10, 64)
		Expect(err).NotTo(HaveOccurred())
		Expect(height).To(BeNumerically(">=", 5))
		statOutput, err := Framework().PodExec(ns.Name, "restore-store", "server", "mc", "stat", "--json", "local/backups/"+object)
		Expect(err).NotTo(HaveOccurred())
		var stat struct {
			Metadata map[string]string `json:"metadata"`
		}
		Expect(json.Unmarshal([]byte(statOutput), &stat)).To(Succeed())
		var storedHeight string
		for key, value := range stat.Metadata {
			if strings.EqualFold(key, "X-Amz-Meta-Cosmopilot-Height") || strings.EqualFold(key, "cosmopilot-height") {
				storedHeight = value
			}
		}
		Expect(storedHeight).To(Equal(strconv.FormatInt(height, 10)))
		digestOutput, err := Framework().PodExec(ns.Name, "restore-store", "server", "sh", "-c", `mc cp local/backups/"$1" /tmp/export.tar.gz >/dev/null && sha256sum /tmp/export.tar.gz`, "sh", object)
		Expect(err).NotTo(HaveOccurred())
		digest := strings.Fields(digestOutput)[0]
		Expect(digest).To(MatchRegexp("^[0-9a-f]{64}$"))
		restore := app.BuildChainNode(ns.Name)
		restore.Spec.Validator = nil
		genesisName := source.Spec.Genesis.GetConfigMapName(source.Status.ChainID)
		restore.Spec.Genesis = &appsv1.GenesisConfig{ConfigMap: ptr.To("restore-genesis-gate")}
		// A distinct base image makes historical image selection observable without changing the database binary.
		restore.Spec.App.Image = "busybox"
		restore.Spec.App.Version = ptr.To("latest")
		restore.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 1, Image: source.GetAppImage()}, {Height: height + 1000000, Image: source.GetAppImage()}}
		restore.Spec.Persistence = &appsv1.Persistence{StorageClassName: source.Spec.Persistence.StorageClassName, AdditionalVolumes: restore.GetPersistenceAdditionalVolumes()}
		restore.Spec.Persistence.Restore = &appsv1.SnapshotRestoreConfig{
			Snapshot:     appsv1.SnapshotRestoreSource{Provider: "s3", Bucket: "backups", Name: object, Region: "us-east-1", Endpoint: endpoint, ForcePathStyle: true, CredentialsSecret: &appsv1.SnapshotExportSecretReference{Name: credentials}},
			Verification: &appsv1.SnapshotRestoreVerification{SHA256: digest},
		}
		restore.Spec.Persistence.InitTimeout = ptr.To("5m")
		restore.Spec.Persistence.AdditionalInitCommands = []appsv1.InitCommand{{Command: []string{"sh", "-c"}, Args: []string{"while [ ! -f /temp/restore-release ]; do sleep 1; done"}}}
		Expect(c.Create(ctx, restore)).To(Succeed())
		initPod := &corev1.Pod{}
		initKey := client.ObjectKey{Namespace: ns.Name, Name: restore.Name + "-init-data"}
		waitForReportedHeight := func() {
			Eventually(func() string {
				if c.Get(ctx, initKey, initPod) != nil {
					return ""
				}
				for _, status := range initPod.Status.InitContainerStatuses {
					if status.Name == "data-restore" && status.State.Terminated != nil && status.State.Terminated.ExitCode == 0 {
						return status.State.Terminated.Message
					}
				}
				return ""
			}, 3*time.Minute, time.Second).Should(MatchJSON(fmt.Sprintf(`{"stage":"complete","message":"","height":"%d"}`, height)))
		}
		waitForReportedHeight()
		Expect(initPod.Spec.InitContainers[1].Image).To(Equal("busybox:latest"))
		RefreshChainNode(restore)
		Expect(restore.Status.LatestHeight).To(BeZero())
		retryTemplate := initPod.DeepCopy()
		oldInitUID := initPod.UID
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(restore), restore); err != nil {
				return err
			}
			restore.Spec.Persistence.Restore.Snapshot.Name = "unavailable-object.tar.gz"
			return c.Update(ctx, restore)
		})).To(Succeed())
		Expect(c.Delete(ctx, initPod)).To(Succeed())
		Eventually(func() bool { return c.Get(ctx, initKey, initPod) == nil && initPod.UID != oldInitUID }, 2*time.Minute, time.Second).Should(BeTrue())
		Expect(initPod.Spec.InitContainers[0].Args).To(ContainElement("unavailable-object.tar.gz"))
		waitForReportedHeight()
		Eventually(func() error {
			_, err := Framework().PodExec(ns.Name, initPod.Name, "init-command-0", "touch", "/temp/restore-release")
			return err
		}).Should(Succeed())
		bootstrapPVC := &corev1.PersistentVolumeClaim{}
		Eventually(func() string {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(restore), bootstrapPVC)).To(Succeed())
			return bootstrapPVC.Annotations[controllers.AnnotationDataInitialized]
		}).Should(Equal("true"))
		Expect(bootstrapPVC.Annotations[controllers.AnnotationDataHeight]).To(Equal(strconv.FormatInt(height, 10)))
		RefreshChainNode(restore)
		Expect(restore.Status.LatestHeight).To(Equal(height))
		Expect(restore.Status.Upgrades[0].Status).To(Equal(appsv1.UpgradeSkipped))
		Expect(restore.Status.Upgrades[1].Status).To(Equal(appsv1.UpgradeScheduled))
		Expect(restore.GetAppImage()).To(Equal(source.GetAppImage()))
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(restore), restore); err != nil {
				return err
			}
			restore.Spec.Genesis.ConfigMap = ptr.To(genesisName)
			restore.Spec.Persistence.Restore.Snapshot.Name = object
			return c.Update(ctx, restore)
		})).To(Succeed())
		WaitForChainNodeHeight(restore, height+5)
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
		Expect(pod.Spec.Containers[0].Image).To(Equal(source.GetAppImage()))
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

		_, err = Framework().PodExec(ns.Name, "restore-store", "server", "sh", "-c", `mc put /tmp/export.tar.gz local/backups/no-height.tar.gz && mc cp --attr "cosmopilot-height=$1" /tmp/export.tar.gz local/backups/override-height.tar.gz`, "sh", strconv.FormatInt(height+10, 10))
		Expect(err).NotTo(HaveOccurred())
		for _, tc := range []struct {
			name      string
			explicit  *int64
			effective int64
		}{
			{"override-height.tar.gz", ptr.To(height), height},
			{"no-height.tar.gz", nil, 0},
			{"no-height.tar.gz", ptr.To(height), height},
		} {
			candidate := restore.DeepCopy()
			candidate.ObjectMeta = metav1.ObjectMeta{GenerateName: "height-restore-", Namespace: ns.Name}
			candidate.Status = appsv1.ChainNodeStatus{}
			candidate.Spec.App = source.Spec.App
			candidate.Spec.Genesis.ConfigMap = ptr.To("restore-genesis-gate")
			candidate.Spec.Persistence.AdditionalInitCommands = nil
			candidate.Spec.Persistence.Restore.Snapshot.Name = tc.name
			candidate.Spec.Persistence.Restore.Height = tc.explicit
			Expect(c.Create(ctx, candidate)).To(Succeed())
			candidatePVC := &corev1.PersistentVolumeClaim{}
			Eventually(func() string {
				if c.Get(ctx, client.ObjectKeyFromObject(candidate), candidatePVC) != nil {
					return ""
				}
				return candidatePVC.Annotations[controllers.AnnotationDataInitialized]
			}, 3*time.Minute, time.Second).Should(Equal("true"))
			RefreshChainNode(candidate)
			Expect(candidate.Status.LatestHeight).To(Equal(tc.effective))
			Expect(candidatePVC.Annotations[controllers.AnnotationDataHeight]).To(Equal(strconv.FormatInt(tc.effective, 10)))
		}

		legacyName := restore.Name + "-legacy"
		legacyPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: legacyName, Namespace: ns.Name}, Spec: bootstrapPVC.Spec}
		legacyPVC.Spec.VolumeName = ""
		Expect(c.Create(ctx, legacyPVC)).To(Succeed())
		legacyPod := retryTemplate.DeepCopy()
		legacyPod.ObjectMeta = metav1.ObjectMeta{Name: legacyName + "-init-data", Namespace: ns.Name}
		legacyPod.Status = corev1.PodStatus{}
		legacyPod.Spec.NodeName = ""
		legacyPod.Spec.InitContainers = legacyPod.Spec.InitContainers[:1]
		legacyPod.Spec.InitContainers[0].Image = "ghcr.io/voluzi/dataexporter:2.1.0"
		var legacyVolumes []corev1.Volume
		for _, volume := range legacyPod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				if volume.Name != "data" {
					continue
				}
				volume.PersistentVolumeClaim.ClaimName = legacyName
			}
			legacyVolumes = append(legacyVolumes, volume)
		}
		legacyPod.Spec.Volumes = legacyVolumes
		Expect(c.Create(ctx, legacyPod)).To(Succeed())
		Eventually(func() corev1.PodPhase {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(legacyPod), legacyPod)).To(Succeed())
			return legacyPod.Status.Phase
		}, 3*time.Minute, time.Second).Should(Equal(corev1.PodSucceeded))
		Expect(legacyPod.Status.InitContainerStatuses[0].State.Terminated.Message).To(BeEmpty())
		legacy := restore.DeepCopy()
		legacy.ObjectMeta = metav1.ObjectMeta{Name: legacyName, Namespace: ns.Name}
		legacy.Status = appsv1.ChainNodeStatus{}
		legacy.Spec.App = source.Spec.App
		legacy.Spec.Genesis.ConfigMap = ptr.To("restore-genesis-gate")
		legacy.Spec.Persistence.AdditionalInitCommands = nil
		legacy.Spec.Persistence.AdditionalVolumes = nil
		Expect(c.Create(ctx, legacy)).To(Succeed())
		Eventually(func() string {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(legacyPVC), legacyPVC)).To(Succeed())
			return legacyPVC.Annotations[controllers.AnnotationDataInitialized]
		}).Should(Equal("true"))
		RefreshChainNode(legacy)
		Expect(legacy.Status.LatestHeight).To(BeZero())
		Expect(legacyPVC.Annotations[controllers.AnnotationDataHeight]).To(Equal("0"))

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
		Expect(badPVC.Annotations[controllers.AnnotationDataHeight]).To(Equal("0"))
		RefreshChainNode(bad)
		Expect(bad.Status.LatestHeight).To(BeZero())
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
