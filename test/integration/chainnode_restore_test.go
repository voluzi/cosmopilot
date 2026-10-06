package integration

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

var _ = Describe("Exported snapshot initialization", func() {
	It("initializes a fresh PVC, preserves initialized data on config changes, and restores a replacement PVC", WithNamespace(func(ns *corev1.Namespace) {
		ctx := Framework().Context()
		c := Framework().Client()
		// Leave genesis unavailable so envtest does not enter the blocking configuration helper,
		// which needs a kubelet to execute it. This test observes only data initialization.

		node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSpec{
			App:         DefaultChainNodeTestApp,
			Genesis:     &appsv1.GenesisConfig{ConfigMap: ptr.To("unavailable-genesis")},
			Persistence: &appsv1.Persistence{Restore: &appsv1.SnapshotRestoreConfig{Snapshot: appsv1.SnapshotRestoreSource{Provider: "s3", Bucket: "backups", Name: "snapshot.tar.gz"}}},
		}}
		Expect(c.Create(ctx, node)).To(Succeed())
		pod := &corev1.Pod{}
		initKey := client.ObjectKey{Namespace: ns.Name, Name: node.Name + "-init-data"}
		Eventually(func() error { return c.Get(ctx, initKey, pod) }).Should(Succeed())
		Expect(pod.Spec.InitContainers[0].Name).To(Equal("data-restore"))
		Expect(pod.Spec.InitContainers[0].Args).To(ContainElement("snapshot.tar.gz"))
		Expect(pod.Spec.InitContainers[0].Args).NotTo(ContainElement("--sha256"))
		initialDeadline := *pod.Spec.ActiveDeadlineSeconds
		Expect(c.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Status.Phase).To(Equal(appsv1.PhaseChainNodeInitData))
		pvc := GetPVC(ns.Name, node.Name)
		Expect(pvc.Annotations[controllers.AnnotationDataInitialized]).To(Equal("false"))
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"stage":"complete","message":"","height":"150"}`}}}}
		Expect(c.Status().Update(ctx, pod)).To(Succeed())
		requestRestoreReconcile(node)
		Expect(node.Spec.Persistence.InitTimeout).To(BeNil())
		Eventually(func() string {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return pvc.Annotations[controllers.AnnotationDataInitialized]
		}, 45*time.Second).Should(Equal("true"))
		Expect(pvc.Annotations[controllers.AnnotationDataHeight]).To(Equal("150"))
		Expect(c.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Status.LatestHeight).To(Equal(int64(150)))
		Eventually(func() bool { return apierrors.IsNotFound(c.Get(ctx, initKey, &corev1.Pod{})) }).Should(BeTrue())
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
				return err
			}
			node.Spec.Persistence.Restore.Snapshot.Name = "replacement.tar.zst"
			return c.Update(ctx, node)
		})).To(Succeed())
		Consistently(func() bool { return c.Get(ctx, initKey, &corev1.Pod{}) == nil }, time.Second, 100*time.Millisecond).Should(BeFalse())
		oldUID := pvc.UID
		Expect(c.Delete(ctx, pvc)).To(Succeed())
		// Envtest has PVC protection admission but no controller to release an unused claim.
		Expect(c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		controllerutil.RemoveFinalizer(pvc, pvcProtectionFinalizer)
		Expect(c.Update(ctx, pvc)).To(Succeed())
		Eventually(func() bool {
			if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
				return false
			}
			return pvc.UID != oldUID
		}).Should(BeTrue())
		Expect(pvc.Annotations[controllers.AnnotationDataInitialized]).To(Equal("false"))
		Eventually(func() error { return c.Get(ctx, initKey, pod) }).Should(Succeed())
		Expect(pod.Spec.InitContainers[0].Args).To(ContainElement("replacement.tar.zst"))
		Expect(*pod.Spec.ActiveDeadlineSeconds).To(Equal(initialDeadline))
		Expect(pvc.Annotations[controllers.AnnotationDataHeight]).To(Equal("0"))
		Expect(c.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Status.LatestHeight).To(Equal(int64(150)))
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
		Expect(c.Status().Update(ctx, pod)).To(Succeed())
		requestRestoreReconcile(node)
		Eventually(func() string {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return pvc.Annotations[controllers.AnnotationDataInitialized]
		}).Should(Equal("true"))
		Expect(pvc.Annotations[controllers.AnnotationDataHeight]).To(Equal("0"))
		Expect(c.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Status.LatestHeight).To(BeZero())
	}))

	It("classifies upgrades propagated from ChainNodeSet after the single init Pod succeeds", WithNamespace(func(ns *corev1.Namespace) {
		ctx := Framework().Context()
		c := Framework().Client()
		set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodeSetPrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSetSpec{
			App:     DefaultChainNodeSetTestApp,
			Genesis: &appsv1.GenesisConfig{ConfigMap: ptr.To("set-genesis")},
			Nodes: []appsv1.NodeGroupSpec{{Name: "restore", Instances: ptr.To(1), Persistence: &appsv1.Persistence{
				Restore:                &appsv1.SnapshotRestoreConfig{Snapshot: appsv1.SnapshotRestoreSource{Provider: "s3", Bucket: "backups", Name: "snapshot.tar.gz"}},
				AdditionalInitCommands: []appsv1.InitCommand{{Command: []string{"echo"}, Args: []string{"after restore"}}, {Image: ptr.To("commands:custom"), Command: []string{"echo"}, Args: []string{"custom"}}},
			}}},
		}}
		genesis := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "set-genesis", Namespace: ns.Name}, Data: map[string]string{"genesis.json": `{"chain_id":"restore-chain"}`}}
		Expect(c.Create(ctx, genesis)).To(Succeed())
		Expect(c.Create(ctx, set)).To(Succeed())
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(set), set); err != nil {
				return err
			}
			set.Status.Upgrades = []appsv1.Upgrade{{Height: 100, Image: "app:v2", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeCompleted}, {Height: 200, Image: "app:v3", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled}}
			return c.Status().Update(ctx, set)
		})).To(Succeed())
		node := &appsv1.ChainNode{}
		nodeKey := client.ObjectKey{Namespace: ns.Name, Name: set.Name + "-restore-0"}
		Eventually(func() int {
			if err := c.Get(ctx, nodeKey, node); err != nil {
				return 0
			}
			return len(node.Spec.App.Upgrades)
		}).Should(Equal(2))
		// Hold configuration generation after restore; envtest has no kubelet for helper execution.
		Expect(c.Delete(ctx, genesis)).To(Succeed())
		pod := &corev1.Pod{}
		initKey := client.ObjectKey{Namespace: ns.Name, Name: node.Name + "-init-data"}
		Eventually(func() error { return c.Get(ctx, initKey, pod) }).Should(Succeed())
		Expect(pod.Spec.InitContainers).To(HaveLen(3))
		Expect(pod.Spec.InitContainers[0].Name).To(Equal("data-restore"))
		Expect(pod.Spec.InitContainers[1].Image).To(Equal(set.Spec.App.GetImage()))
		Expect(pod.Spec.InitContainers[2].Image).To(Equal("commands:custom"))
		pod.Status.Phase = corev1.PodRunning
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"stage":"complete","message":"","height":"150"}`}}}}
		Expect(c.Status().Update(ctx, pod)).To(Succeed())
		requestRestoreReconcile(node)
		pvc := GetPVC(ns.Name, node.Name)
		Consistently(func() string {
			Expect(c.Get(ctx, nodeKey, node)).To(Succeed())
			Expect(node.Status.LatestHeight).To(BeZero())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return pvc.Annotations[controllers.AnnotationDataInitialized]
		}, time.Second, 100*time.Millisecond).Should(Equal("false"))
		pod.Status.Phase = corev1.PodSucceeded
		Expect(c.Status().Update(ctx, pod)).To(Succeed())
		requestRestoreReconcile(node)
		Eventually(func() string {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return pvc.Annotations[controllers.AnnotationDataInitialized]
		}).Should(Equal("true"))
		Expect(pvc.Annotations[controllers.AnnotationDataHeight]).To(Equal("150"))
		Expect(c.Get(ctx, nodeKey, node)).To(Succeed())
		Expect(node.Status.LatestHeight).To(Equal(int64(150)))
		Expect(node.Status.Upgrades).To(HaveLen(2))
		Expect(node.Status.Upgrades[0].Status).To(Equal(appsv1.UpgradeSkipped))
		Expect(node.Status.Upgrades[1].Status).To(Equal(appsv1.UpgradeScheduled))
		Expect(node.GetAppImage()).To(Equal("app:v2"))
	}))

	It("validates source shapes, optional SHA-256, and restore height through the CRD", WithNamespace(func(ns *corev1.Namespace) {
		for _, tc := range []struct {
			provider, bucket, name, digest string
			height                         *int64
			valid                          bool
		}{
			{"s3", "backups", "snapshot.tar", "", nil, true},
			{"gcs", "backups", "snapshot.tar", strings.Repeat("A", 64), nil, true},
			{"ftp", "backups", "snapshot.tar", "", nil, false},
			{"s3", "", "snapshot.tar", "", nil, false},
			{"s3", "backups", "", "", nil, false},
			{"s3", "backups", "snapshot.tar", "not-a-digest", nil, false},
			{"s3", "backups", "snapshot.tar", "", ptr.To[int64](-1), false},
			{"s3", "backups", "snapshot.tar", "", ptr.To[int64](0), true},
			{"s3", "backups", "snapshot.tar", "", ptr.To[int64](123), true},
		} {
			node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSpec{App: DefaultChainNodeTestApp, Genesis: &appsv1.GenesisConfig{ConfigMap: ptr.To("unavailable-genesis")}, Persistence: &appsv1.Persistence{Restore: &appsv1.SnapshotRestoreConfig{Snapshot: appsv1.SnapshotRestoreSource{Provider: tc.provider, Bucket: tc.bucket, Name: tc.name}}}}}
			if tc.digest != "" {
				node.Spec.Persistence.Restore.Verification = &appsv1.SnapshotRestoreVerification{SHA256: tc.digest}
			}
			node.Spec.Persistence.Restore.Height = tc.height
			err := Framework().Client().Create(Framework().Context(), node)
			if tc.valid {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}
		}
	}))
})

func requestRestoreReconcile(node *appsv1.ChainNode) {
	// Pod status changes do not pass the controller's generation filter; a spec update avoids
	// waiting for the 30-second poll without changing the init Pod's execution settings.
	c := Framework().Client()
	ctx := Framework().Context()
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
			return err
		}
		if node.Spec.Config == nil {
			node.Spec.Config = &appsv1.Config{}
		}
		if node.Spec.Config.PodAnnotations == nil {
			node.Spec.Config.PodAnnotations = map[string]string{}
		}
		value := "1"
		if node.Spec.Config.PodAnnotations["test.cosmopilot.voluzi.com/reconcile"] == value {
			value = "2"
		}
		node.Spec.Config.PodAnnotations["test.cosmopilot.voluzi.com/reconcile"] = value
		return c.Update(ctx, node)
	})).To(Succeed())
}
