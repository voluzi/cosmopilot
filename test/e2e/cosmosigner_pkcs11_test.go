package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1k8s "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v5/pkg/environ"
	"github.com/voluzi/cosmopilot/v5/test/e2e/apps"
)

var _ = Describe("PKCS11 SoftHSM", Label("cosmosigner", "pkcs11"), func() {
	It("produces blocks with the token key and holds a wrong PIN live without restarts", func() {
		image := environ.GetString("PKCS11_TEST_IMAGE", "")
		if image == "" {
			Skip("set PKCS11_TEST_IMAGE to a locally built SoftHSM fixture image")
		}
		Expect(Framework().LoadImage(image)).To(Succeed())
		ns := CreateTestNamespace()
		ctx := Framework().Context()
		cl := Framework().Client()
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: ns.Name}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
		Expect(cl.Create(ctx, pvc)).To(Succeed())
		tokenVolume := corev1.Volume{Name: "token", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}
		tokenMount := corev1.VolumeMount{Name: "token", MountPath: "/token"}
		fixture := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "token-fixture", Namespace: ns.Name}, Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(1000)), RunAsGroup: ptr.To(int64(1000)), FSGroup: ptr.To(int64(1000))},
			Containers:      []corev1.Container{{Name: "fixture", Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sh", "-c", "sleep infinity"}, VolumeMounts: []corev1.VolumeMount{tokenMount}}}, Volumes: []corev1.Volume{tokenVolume}}}
		Expect(cl.Create(ctx, fixture)).To(Succeed())
		Eventually(func() error { _, err := Framework().PodExec(ns.Name, fixture.Name, "fixture", "true"); return err }).Should(Succeed())
		output, err := Framework().PodExec(ns.Name, fixture.Name, "fixture", "/usr/local/bin/provision-token")
		Expect(err).NotTo(HaveOccurred())
		publicKey, err := cosmosigner.ParsePublicKeyOutput(output)
		Expect(err).NotTo(HaveOccurred())
		app := apps.Nibiru()
		genesisPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "genesis-fixture", Namespace: ns.Name}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "genesis", Image: app.AppSpec.GetImage(), Command: []string{"sh", "-ec", `nibid init fixture --chain-id pkcs11-e2e --home /tmp/chain
nibid keys add validator --keyring-backend test --home /tmp/chain
address=$(nibid keys show validator -a --keyring-backend test --home /tmp/chain)
nibid genesis add-genesis-account "$address" 1000000000000unibi --home /tmp/chain
nibid genesis add-sudo-root-account "$address" --home /tmp/chain
pubkey=$(printf '{"@type":"/cosmos.crypto.ed25519.PubKey","key":"%s"}' "$CONSENSUS_PUBLIC_KEY")
nibid genesis gentx validator 100000000unibi --pubkey "$pubkey" --chain-id pkcs11-e2e --keyring-backend test --home /tmp/chain
nibid genesis collect-gentxs --home /tmp/chain
touch /tmp/genesis-ready
sleep infinity`}, Env: []corev1.EnvVar{{Name: "CONSENSUS_PUBLIC_KEY", Value: publicKey}}}}}}
		Expect(cl.Create(ctx, genesisPod)).To(Succeed())
		Eventually(func() error {
			_, err := Framework().PodExec(ns.Name, genesisPod.Name, "genesis", "test", "-f", "/tmp/genesis-ready")
			return err
		}).Should(Succeed())
		genesis, err := Framework().PodExec(ns.Name, genesisPod.Name, "genesis", "cat", "/tmp/chain/config/genesis.json")
		Expect(err).NotTo(HaveOccurred())
		Expect(genesis).To(ContainSubstring(publicKey))
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "token-genesis", Namespace: ns.Name}, Data: map[string]string{"genesis.json": genesis}}
		Expect(cl.Create(ctx, cm)).To(Succeed())
		pin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "token-pin", Namespace: ns.Name}, Data: map[string][]byte{"pin": []byte("123456")}}
		Expect(cl.Create(ctx, pin)).To(Succeed())
		cns := app.BuildChainNodeSet(ns.Name, 0)
		cns.Spec.Nodes = []appsv1.NodeGroupSpec{}
		cns.Spec.Validator.Init = nil
		cns.Spec.Genesis = &appsv1.GenesisConfig{ConfigMap: ptr.To(cm.Name)}
		cns.Spec.Cosmosigner = &appsv1.Cosmosigner{Image: ptr.To(image), Replicas: ptr.To(int32(1)), Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{
			Module: "/usr/lib/softhsm/libsofthsm2.so", TokenLabel: "validator-token", KeyLabel: "consensus", KeyID: "01", PublicKey: publicKey, PINSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: pin.Name}, Key: "pin"},
		}}, Env: []corev1.EnvVar{{Name: "SOFTHSM2_CONF", Value: "/token/softhsm2.conf"}}, Volumes: []corev1.Volume{tokenVolume}, VolumeMounts: []corev1.VolumeMount{tokenMount}}
		Expect(cl.Create(ctx, cns)).To(Succeed())
		WaitForChainNodeSetHeight(cns, 3)
		name := cns.Name + "-signer"
		status := waitForCosmosignerApplied(cns, name)
		Expect(status.PublicKey).To(Equal(publicKey))
		oldPVCs := signerPVCUIDs(ns.Name, name, 1)
		marker, err := Framework().PodExec(ns.Name, name+"-0", "cosmosigner", "cat", "/data/cluster-binding.json")
		Expect(err).NotTo(HaveOccurred())
		Expect(marker).To(ContainSubstring(publicKey))
		sts := &appsv1k8s.StatefulSet{}
		Expect(cl.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: name}, sts)).To(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
		Eventually(func() error {
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pin), pin); err != nil {
				return err
			}
			pin.Data["pin"] = []byte("wrong-pin")
			return cl.Update(ctx, pin)
		}).Should(Succeed())
		pod := &corev1.Pod{}
		Expect(cl.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: name + "-0"}, pod)).To(Succeed())
		oldUID := pod.UID
		Expect(cl.Delete(ctx, pod)).To(Succeed())
		Eventually(func() bool {
			if cl.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: name + "-0"}, pod) != nil || pod.UID == oldUID {
				return false
			}
			out, err := Framework().PodExec(ns.Name, pod.Name, "cosmosigner", "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:8080/readyz")
			return err == nil && out == "503"
		}).Should(BeTrue())
		heldUID := pod.UID
		Consistently(func() error {
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
				return err
			}
			if pod.UID != heldUID || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].RestartCount != 0 || pod.Status.ContainerStatuses[0].Ready {
				return fmt.Errorf("PIN hold changed process or became ready")
			}
			for path, want := range map[string]string{"livez": "200", "readyz": "503"} {
				out, err := Framework().PodExec(ns.Name, pod.Name, "cosmosigner", "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:8080/"+path)
				if err != nil || strings.TrimSpace(out) != want {
					return fmt.Errorf("%s: status %q, error %v", path, out, err)
				}
			}
			return nil
		}, 30*time.Second, 2*time.Second).Should(Succeed())
		Eventually(func() error {
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pin), pin); err != nil {
				return err
			}
			pin.Data["pin"] = []byte("123456")
			return cl.Update(ctx, pin)
		}).Should(Succeed())
		Expect(cl.Delete(ctx, pod)).To(Succeed())
		height, err := observedChainNodeSetHeight(cns)
		Expect(err).NotTo(HaveOccurred())
		WaitForChainNodeSetHeight(cns, height+3)
		Expect(signerPVCUIDs(ns.Name, name, 1)).To(Equal(oldPVCs))
		current, err := Framework().PodExec(ns.Name, name+"-0", "cosmosigner", "cat", "/data/cluster-binding.json")
		Expect(err).NotTo(HaveOccurred())
		Expect(current).To(Equal(marker))
	})
})
