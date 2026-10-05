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
		Expect(c.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Status.Phase).To(Equal(appsv1.PhaseChainNodeInitData))
		pvc := GetPVC(ns.Name, node.Name)
		Expect(pvc.Annotations[controllers.AnnotationDataInitialized]).To(Equal("false"))
		pod.Status.Phase = corev1.PodSucceeded
		Expect(c.Status().Update(ctx, pod)).To(Succeed())
		Eventually(func() string {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return pvc.Annotations[controllers.AnnotationDataInitialized]
		}, 45*time.Second).Should(Equal("true"))
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
