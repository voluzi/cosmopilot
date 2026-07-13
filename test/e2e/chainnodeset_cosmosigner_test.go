package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1k8s "k8s.io/api/apps/v1"

	"github.com/voluzi/cosmopilot/v2/test/e2e/apps"
)

var _ = Describe("ChainNodeSet Cosmosigner", func() {
	Context("Validator signing through a managed cosmosigner deployment", func() {
		apps.ForEachApp("should validate using cosmosigner (software backend) and produce blocks",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				// The cosmosigner image is heavy infra; gate it behind the same flag as Vault so the
				// default e2e run stays lean and this opt-in path is exercised explicitly.
				if !Framework().Config().InstallVault {
					Skip("cosmosigner e2e is opt-in (enable with the Vault install flag)")
				}

				cns := app.BuildChainNodeSetWithCosmosigner(ns.Name, apps.CosmosignerConfig{Replicas: 1})
				Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

				// The signer StatefulSet must be created and the chain must produce blocks, which only
				// happens if the validator node's remote signer is actually signing.
				WaitForChainNodeSetHeight(cns, 3)

				sts := &appsv1k8s.StatefulSet{}
				Eventually(func() error {
					return Framework().Client().Get(Framework().Context(),
						client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("%s-signer", cns.GetName())}, sts)
				}).Should(Succeed())
				Expect(*sts.Spec.Replicas).To(BeNumerically("==", 1))
			}),
		)

		apps.ForEachApp("should validate using cosmosigner (Vault backend, 3 replicas) and produce blocks",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				if !Framework().Config().InstallVault {
					Skip("Vault is not installed, skipping cosmosigner Vault test")
				}

				tokenSecretName, caSecretName := CopyVaultSecretsToNamespace(ns.Name)
				cns := app.BuildChainNodeSetWithCosmosigner(ns.Name, apps.CosmosignerConfig{
					Replicas: 3,
					Vault: &apps.TmKMSConfig{
						VaultAddress:    GetVaultAddress(),
						KeyName:         fmt.Sprintf("%s-cosmosigner", app.ValidatorConfig.ChainID),
						TokenSecretName: tokenSecretName,
						CASecretName:    caSecretName,
					},
				})
				Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

				WaitForChainNodeSetHeight(cns, 3)

				// Verify the key was imported into Vault exactly once (recorded in the signer's
				// per-signer status entry).
				RefreshChainNodeSet(cns)
				st := cns.GetCosmosignerStatus(fmt.Sprintf("%s-signer", cns.GetName()))
				Expect(st).NotTo(BeNil())
				Expect(st.KeyImported).NotTo(BeEmpty())
			}),
		)
	})
})
