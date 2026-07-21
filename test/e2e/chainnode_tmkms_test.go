package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1k8s "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/test/e2e/apps"
)

var _ = Describe("ChainNode TMKMS", func() {
	Context("Validator with TMKMS Vault Provider", func() {
		apps.ForEachApp("should start validator using TMKMS with Vault and produce blocks",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				// Skip if Vault is not installed
				if !Framework().Config().InstallVault {
					Skip("Vault is not installed, skipping TMKMS test")
				}

				// Copy Vault secrets to the test namespace
				tokenSecretName, caSecretName := CopyVaultSecretsToNamespace(ns.Name)

				// Build ChainNode with TMKMS configuration
				vaultAddr := GetVaultAddress()
				keyName := fmt.Sprintf("%s-validator", app.ValidatorConfig.ChainID)

				tmkmsConfig := apps.TmKMSConfig{
					VaultAddress:    vaultAddr,
					KeyName:         keyName,
					TokenSecretName: tokenSecretName,
					CASecretName:    caSecretName,
				}

				chainNode := app.BuildChainNodeWithTmKMS(ns.Name, tmkmsConfig)

				// Create ChainNode
				err := Framework().Client().Create(Framework().Context(), chainNode)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNode to reach running phase
				WaitForChainNodeRunning(chainNode)

				// Verify blocks are being produced (validator is signing via TMKMS)
				WaitForChainNodeHeight(chainNode, 3)

				// Verify TMKMS container is healthy
				WaitForTmkmsContainerRunning(chainNode)

				// Verify key was uploaded to Vault
				RefreshChainNode(chainNode)
				Expect(chainNode.Annotations).To(HaveKey("cosmopilot.voluzi.com/vault-key-uploaded"))
			}),
		)

		It("should migrate an active tmKMS Vault validator to cosmosigner without changing its consensus key", func() {
			if !Framework().Config().InstallVault {
				Skip("Vault is not installed, skipping tmKMS to cosmosigner migration test")
			}
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			tokenSecretName, caSecretName := CopyVaultSecretsToNamespace(ns.Name)
			keyName := fmt.Sprintf("%s-tmkms-cosmosigner-%s", app.ValidatorConfig.ChainID, RandString(6))
			tmkmsConfig := apps.TmKMSConfig{
				VaultAddress:    GetVaultAddress(),
				KeyName:         keyName,
				TokenSecretName: tokenSecretName,
				CASecretName:    caSecretName,
			}
			chainNode := app.BuildChainNodeWithTmKMS(ns.Name, tmkmsConfig)
			Expect(Framework().Client().Create(Framework().Context(), chainNode)).To(Succeed())

			WaitForChainNodeHeight(chainNode, 3)
			WaitForTmkmsContainerRunning(chainNode)
			Eventually(func() bool {
				RefreshChainNode(chainNode)
				return chainNode.Annotations[controllers.AnnotationVaultKeyUploaded] == "true"
			}).Should(BeTrue())
			RefreshChainNode(chainNode)
			originalPublicKey := chainNode.Status.PubKey
			originalHeight := chainNode.Status.LatestHeight
			Expect(originalPublicKey).NotTo(BeEmpty())

			Eventually(func() error {
				current := &appsv1.ChainNode{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNode), current); err != nil {
					return err
				}
				current.Spec.Validator.TmKMS = nil
				current.Spec.Cosmosigner = &appsv1.Cosmosigner{
					Replicas: ptr.To(int32(1)),
					Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
						Address: tmkmsConfig.VaultAddress,
						KeyName: keyName,
						TokenSecret: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName},
							Key:                  "token",
						},
						CertificateSecret: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: caSecretName},
							Key:                  "ca.crt",
						},
					}},
				}
				return Framework().Client().Update(Framework().Context(), current)
			}).Should(Succeed())

			signerName := fmt.Sprintf("%s-signer", chainNode.Name)
			overlapObserved := false
			Eventually(func() bool {
				pod := &corev1.Pod{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNode), pod); err != nil {
					return false
				}
				hasTmKMS := false
				for _, container := range pod.Spec.Containers {
					if container.Name == "tmkms" {
						hasTmKMS = true
					}
				}
				targetedByCosmosigner := pod.Labels[controllers.LabelCosmosignerTarget] == signerName
				if hasTmKMS && targetedByCosmosigner {
					overlapObserved = true
				}
				return !hasTmKMS && targetedByCosmosigner
			}).Should(BeTrue())
			Expect(overlapObserved).To(BeFalse(), "a live tmKMS pod must never become a cosmosigner discovery target")

			waitForReadySignerPods(ns.Name, signerName, 1)
			sts := &appsv1k8s.StatefulSet{}
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: signerName}, sts)).To(Succeed())
			Expect(sts.Status.ReadyReplicas).To(BeNumerically("==", 1))

			Eventually(func(g Gomega) {
				current := &appsv1.ChainNode{}
				g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNode), current)).To(Succeed())
				g.Expect(current.Status.PubKey).To(Equal(originalPublicKey))
				g.Expect(current.Status.CosmosignerPublicKey).NotTo(BeEmpty())
				g.Expect(current.Status.CosmosignerMigration).To(BeNil())
				g.Expect(current.Status.LatestHeight).To(BeNumerically(">", originalHeight))
			}).Should(Succeed())

			Eventually(func() bool {
				configMap := &corev1.ConfigMap{}
				err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: chainNode.Name + "-tmkms"}, configMap)
				return apierrors.IsNotFound(err)
			}).Should(BeTrue())
		})
	})
})
