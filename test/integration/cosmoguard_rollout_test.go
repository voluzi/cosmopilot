package integration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/cometbft"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

var _ = Describe("CosmoGuard config rollout", func() {
	for _, placement := range []string{"ChainNodeSet", "ChainNode"} {
		It("watches user ConfigMap updates for "+placement, WithNamespace(func(ns *corev1.Namespace) {
			ctx, c := Framework().Context(), Framework().Client()
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "guard-rules", Namespace: ns.Name}, Data: map[string]string{"rules.yaml": "lcd: {rules: [{paths: [/old], action: allow}]}"}}
			Expect(c.Create(ctx, cm)).To(Succeed())
			cfg := &appsv1.Config{ReconcilePeriod: ptr.To("1h"), CosmoGuard: &appsv1.CosmoGuardConfig{Enable: true, Config: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name}, Key: "rules.yaml"}}}
			name := "guarded"
			guardName := name + "-cg"
			if placement == "ChainNodeSet" {
				nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name}, Spec: appsv1.ChainNodeSetSpec{App: DefaultChainNodeSetTestApp, Genesis: NewGenesisConfigWithChainID("guard-test"), Nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1), Config: cfg}}}}
				Expect(c.Create(ctx, nodeSet)).To(Succeed())
				guardName = name + "-fullnodes-cg"
			} else {
				// A pending signer keeps this real controller before the configuration helper that needs
				// a kubelet. Its hourly requeue cannot satisfy the ConfigMap-only assertions below.
				key, err := cometbft.GeneratePrivKey()
				Expect(err).NotTo(HaveOccurred())
				Expect(c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "signing-key", Namespace: ns.Name}, Data: map[string][]byte{"priv_validator_key.json": key}})).To(Succeed())
				pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Annotations: map[string]string{controllers.AnnotationDataInitialized: "true", controllers.AnnotationGenesisDownloaded: "true", controllers.AnnotationGenesisChainID: "guard-test", controllers.AnnotationDataHeight: "0"}}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
				Expect(c.Create(ctx, pvc)).To(Succeed())
				node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name}, Spec: appsv1.ChainNodeSpec{App: DefaultChainNodeTestApp, Genesis: NewGenesisConfigWithChainID("guard-test"), Config: cfg, Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("signing-key")}}}}}
				Expect(c.Create(ctx, node)).To(Succeed())
			}
			stsKey := client.ObjectKey{Namespace: ns.Name, Name: guardName}
			read := func() *k8sappsv1.StatefulSet {
				sts := &k8sappsv1.StatefulSet{}
				Expect(c.Get(ctx, stsKey, sts)).To(Succeed())
				return sts
			}
			Eventually(func() string {
				sts := &k8sappsv1.StatefulSet{}
				if c.Get(ctx, stsKey, sts) != nil {
					return ""
				}
				return sts.Annotations[controllers.AnnotationCosmoGuardConfigDigest]
			}).ShouldNot(BeEmpty())
			changeTimeout := 3 * time.Second
			if placement == "ChainNode" {
				// Service reconciliation also attempts a snapshot request to the absent node.
				changeTimeout = 10 * time.Second
			}
			initial := read()
			container := initial.Spec.Template.Spec.Containers[0]
			Expect(container.Image).To(Equal("ghcr.io/voluzi/cosmoguard:6.0.0"))
			for _, quantities := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
				Expect(quantities.Cpu().Cmp(resource.MustParse("500m"))).To(BeZero())
				Expect(quantities.Memory().Cmp(resource.MustParse("500Mi"))).To(BeZero())
			}
			Expect(initial.Spec.Template.Spec.TerminationGracePeriodSeconds).NotTo(BeNil())
			Expect(*initial.Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(30)))
			// Stay well within the node set's 15-second periodic requeue, after initial events settle.
			Consistently(func() string { return read().ResourceVersion }, 2*time.Second, 100*time.Millisecond).Should(Equal(initial.ResourceVersion))
			update := func(raw string) {
				Expect(c.Get(ctx, client.ObjectKeyFromObject(cm), cm)).To(Succeed())
				cm.Data["rules.yaml"] = raw
				Expect(c.Update(ctx, cm)).To(Succeed())
			}
			update("lcd: {rules: [{paths: [/new], action: deny}]}")
			Eventually(func() string { return read().Annotations[controllers.AnnotationCosmoGuardConfigDigest] }, changeTimeout, 50*time.Millisecond).ShouldNot(Equal(initial.Annotations[controllers.AnnotationCosmoGuardConfigDigest]))
			Expect(read().Spec.Template).To(Equal(initial.Spec.Template))
			update("auth: {enable: true}\nlcd: {rules: [{paths: [/new], action: deny}]}")
			Eventually(func() string { return read().Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart] }, changeTimeout, 50*time.Millisecond).ShouldNot(BeEmpty())
			rolled := read()
			Consistently(func() corev1.PodTemplateSpec { return read().Spec.Template }, time.Second, 100*time.Millisecond).Should(Equal(rolled.Spec.Template))
			update("auth: [invalid")
			Consistently(func() corev1.PodTemplateSpec { return read().Spec.Template }, time.Second, 100*time.Millisecond).Should(Equal(rolled.Spec.Template))
			Expect(read().Annotations[controllers.AnnotationCosmoGuardConfigDigest]).To(Equal(rolled.Annotations[controllers.AnnotationCosmoGuardConfigDigest]))
		}))
	}
})
