package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1k8s "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/cometbft"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	chainnodecontroller "github.com/voluzi/cosmopilot/v3/internal/controllers/chainnode"
	managedcosmosigner "github.com/voluzi/cosmopilot/v3/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v3/test/e2e/apps"
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
				signerName := fmt.Sprintf("%s-signer", cns.GetName())
				signerStatus := waitForCosmosignerApplied(cns, signerName)
				reservation := waitForConsensusKeyReservation(cns, signerStatus.PublicKey)
				validatorPodName := fmt.Sprintf("%s-validator", cns.GetName())
				validatorPod := waitForReadyCosmosignerTargetPod(
					ns.Name, validatorPodName, signerName, cns.Spec.App.App,
				)
				for cycle := 1; cycle <= 2; cycle++ {
					validatorPod = replaceCosmosignerTargetPodOnce(
						cns, validatorPod, signerName, cns.Spec.App.App, cycle,
					)
					assertConsensusKeyReservationUnchanged(reservation)
				}

				sts := &appsv1k8s.StatefulSet{}
				Eventually(func() error {
					return Framework().Client().Get(Framework().Context(),
						client.ObjectKey{Namespace: ns.Name, Name: signerName}, sts)
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

		It("should preserve signing identity and Raft state when moving a top-level signer into its validator group", func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			cns := buildNamedValidatorCosmosignerSet(app, ns.Name, 1)
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			WaitForChainNodeSetHeight(cns, 3)
			oldLogicalName := fmt.Sprintf("%s-signer", cns.Name)
			oldStatus := waitForCosmosignerApplied(cns, oldLogicalName)
			oldResourceName := appsv1.CosmosignerStatusResourceName(oldStatus)
			oldPVCs := signerPVCUIDs(ns.Name, oldResourceName, 1)
			oldHeight, err := observedChainNodeSetHeight(cns)
			Expect(err).NotTo(HaveOccurred())

			moveTopLevelCosmosignerIntoGroup(cns, "validators")

			newLogicalName := fmt.Sprintf("%s-validators-signer", cns.Name)
			newStatus := waitForCosmosignerApplied(cns, newLogicalName)
			Expect(newStatus.PublicKey).To(Equal(oldStatus.PublicKey))
			Expect(newStatus.ResourceName).To(Equal(oldResourceName))
			Expect(signerPVCUIDs(ns.Name, oldResourceName, 1)).To(Equal(oldPVCs))
			Eventually(func() bool {
				current := &appsv1.ChainNodeSet{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
					return false
				}
				return current.GetCosmosignerStatus(oldLogicalName) == nil
			}).Should(BeTrue())
			WaitForChainNodeSetHeight(cns, oldHeight)
		})

		It("should not recreate a migrated signer while the old signer pod is terminating", func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			cns := buildNamedValidatorCosmosignerSet(app, ns.Name, 1)
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			WaitForChainNodeSetHeight(cns, 3)
			oldLogicalName := fmt.Sprintf("%s-signer", cns.Name)
			oldStatus := waitForCosmosignerApplied(cns, oldLogicalName)
			oldResourceName := appsv1.CosmosignerStatusResourceName(oldStatus)
			oldPodName := oldResourceName + "-0"
			setPodTestFinalizer(ns.Name, oldPodName, true)
			DeferCleanup(func() { setPodTestFinalizer(ns.Name, oldPodName, false) })

			oldPod := &corev1.Pod{}
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: oldPodName}, oldPod)).To(Succeed())
			oldPodUID := oldPod.UID
			oldHeight, err := observedChainNodeSetHeight(cns)
			Expect(err).NotTo(HaveOccurred())

			moveTopLevelCosmosignerIntoGroup(cns, "validators")
			waitForTerminatingSignerPod(ns.Name, oldPodName, string(oldPodUID), false)

			newLogicalName := fmt.Sprintf("%s-validators-signer", cns.Name)
			Consistently(func() bool {
				pods, err := listSignerPods(ns.Name, oldResourceName)
				if err != nil || len(pods) != 1 {
					return false
				}
				current := &appsv1.ChainNodeSet{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
					return false
				}
				status := current.GetCosmosignerStatus(newLogicalName)
				return pods[0].UID == oldPodUID && pods[0].DeletionTimestamp != nil && status != nil && status.Migration != nil
			}, 10*time.Second, time.Second).Should(BeTrue())

			setPodTestFinalizer(ns.Name, oldPodName, false)
			newStatus := waitForCosmosignerApplied(cns, newLogicalName)
			Expect(newStatus.PublicKey).To(Equal(oldStatus.PublicKey))
			Expect(newStatus.ResourceName).To(Equal(oldResourceName))
			WaitForChainNodeSetHeight(cns, oldHeight)
		})

		It("should fail over a TLS-secured Raft leader and stop signing without quorum", func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			cns := buildNamedValidatorCosmosignerSet(app, ns.Name, 3)
			resourceName := fmt.Sprintf("%s-signer", cns.Name)
			validatorName := fmt.Sprintf("%s-validators-0", cns.Name)
			tlsSecretName := createRaftTLSSecret(ns.Name, resourceName)
			cns.Spec.Cosmosigner.RaftTLSSecret = ptr.To(tlsSecretName)
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			Eventually(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}).Should(BeNumerically(">", 3), "the initial TLS-secured Raft cluster should produce blocks")
			pods := waitForReadySignerPods(ns.Name, resourceName, 3)
			leaderName := waitForSignerLeader(ns.Name, resourceName, "")
			var leaderUID string
			for i := range pods {
				if pods[i].Name == leaderName {
					leaderUID = string(pods[i].UID)
				}
			}
			Expect(leaderUID).NotTo(BeEmpty())
			heightBeforeFailover, err := observedChainNodeHeight(ns.Name, validatorName)
			Expect(err).NotTo(HaveOccurred())

			leaderPod := &corev1.Pod{}
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: leaderName}, leaderPod)).To(Succeed())
			Expect(Framework().Client().Delete(Framework().Context(), leaderPod)).To(Succeed())
			newLeaderName := waitForSignerLeader(ns.Name, resourceName, leaderName)
			Expect(newLeaderName).NotTo(Equal(leaderName))
			Eventually(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}).Should(BeNumerically(">", heightBeforeFailover), "a surviving Raft replica should resume signing after leader deletion")
			pods = waitForReadySignerPods(ns.Name, resourceName, 3)

			heldNames := []string{newLeaderName}
			for i := range pods {
				if pods[i].Name != newLeaderName {
					heldNames = append(heldNames, pods[i].Name)
					break
				}
			}
			Expect(heldNames).To(HaveLen(2))
			heldUIDs := make(map[string]string, len(heldNames))
			for _, name := range heldNames {
				setPodTestFinalizer(ns.Name, name, true)
				DeferCleanup(func() { setPodTestFinalizer(ns.Name, name, false) })
				pod := &corev1.Pod{}
				Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: name}, pod)).To(Succeed())
				heldUIDs[name] = string(pod.UID)
				Expect(Framework().Client().Delete(Framework().Context(), pod)).To(Succeed())
			}
			for _, name := range heldNames {
				waitForTerminatingSignerPod(ns.Name, name, heldUIDs[name], true)
			}

			stableHeight := waitForStableChainNodeHeight(ns.Name, validatorName, 10*time.Second)
			Consistently(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}, 10*time.Second, time.Second).Should(Equal(stableHeight))

			setPodTestFinalizer(ns.Name, heldNames[0], false)
			waitForReplacementSignerPod(ns.Name, heldNames[0], heldUIDs[heldNames[0]])
			Eventually(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}).Should(BeNumerically(">", stableHeight), "restoring a second voter should restore Raft quorum and signing")

			setPodTestFinalizer(ns.Name, heldNames[1], false)
			waitForReadySignerPods(ns.Name, resourceName, 3)
		})

		It("should keep a two-sentry retarget migrating until the new target is served, then settle without pod churn", func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			cns := buildSentryRetargetCosmosignerSet(app, ns.Name, createCosmosignerSentryKeySecret(ns.Name))
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			// Fresh rollout: the signer dials sentry A, whose app only starts once the discovery
			// Service publishes that pod to the signer.
			WaitForChainNodeSetHeight(cns, 3)
			signerName := fmt.Sprintf("%s-signer", cns.Name)
			sentryA := fmt.Sprintf("%s-%s-0", cns.Name, cosmosignerSentryGroupA)
			sentryB := fmt.Sprintf("%s-%s-0", cns.Name, cosmosignerSentryGroupB)
			appliedOnA := waitForCosmosignerApplied(cns, signerName)
			Expect(appsv1.CosmosignerStatusResourceName(appliedOnA)).To(Equal(signerName))
			Expect(appliedOnA.TargetGroups).To(ConsistOf(cosmosignerSentryGroupA))
			reservation := waitForConsensusKeyReservation(cns, appliedOnA.PublicKey)
			waitForReadyCosmosignerTargetPod(ns.Name, sentryA, signerName, cns.Spec.App.App)
			waitForReadySignerPods(ns.Name, signerName, 1)

			retargetTopLevelCosmosigner(cns, cosmosignerSentryGroupB)
			appliedOnB := waitForCosmosignerRetargetCompletion(cns, signerName, sentryB, appliedOnA.AppliedDigest)

			// The retarget only changes which pods the signer dials, so it keeps the same key and the
			// same resource identity: a new public key here would mean the sentry stopped signing as
			// itself, and a new resource name would mean its raft state was abandoned.
			Expect(appliedOnB.PublicKey).To(Equal(appliedOnA.PublicKey))
			Expect(appsv1.CosmosignerStatusResourceName(appliedOnB)).To(Equal(signerName))
			Expect(appliedOnB.TargetGroups).To(ConsistOf(cosmosignerSentryGroupB))
			assertConsensusKeyReservationUnchanged(reservation)

			targetPod := waitForReadyCosmosignerTargetPod(ns.Name, sentryB, signerName, cns.Spec.App.App)
			assertNoCosmosignerDiscoveryPubKeyFailure(ns.Name, sentryB, cns.Spec.App.App, 1)
			signerPods := waitForReadySignerPods(ns.Name, signerName, 1)
			settledSignerUID := signerPods[0].UID

			// Sentry A must be released back to local signing, not left selected by the discovery
			// Service alongside B.
			Eventually(func(g Gomega) {
				pod := &corev1.Pod{}
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: sentryA}, pod,
				)).To(Succeed())
				g.Expect(pod.DeletionTimestamp).To(BeNil())
				g.Expect(pod.Labels).NotTo(HaveKey(controllers.LabelCosmosignerTarget))
				g.Expect(podReady(pod)).To(BeTrue())
			}, 3*time.Minute, time.Second).Should(Succeed())

			// A settled retarget stays settled: re-opening the migration, or replacing either the
			// signer pod or the freshly targeted pod again, is the recurring churn this guards.
			heightAfterRetarget, err := observedChainNodeSetHeight(cns)
			Expect(err).NotTo(HaveOccurred())
			Consistently(func(g Gomega) {
				current := &appsv1.ChainNodeSet{}
				g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current)).To(Succeed())
				status := current.GetCosmosignerStatus(signerName)
				g.Expect(status).NotTo(BeNil())
				g.Expect(status.Migration).To(BeNil(), "the settled retarget must not re-open a migration")

				pods, err := listSignerPods(ns.Name, signerName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods).To(HaveLen(1))
				g.Expect(pods[0].UID).To(Equal(settledSignerUID), "the signer pod must not churn after the retarget")

				pod := &corev1.Pod{}
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: sentryB}, pod,
				)).To(Succeed())
				g.Expect(pod.UID).To(Equal(targetPod.UID), "the new target pod must not be replaced again")
				g.Expect(podReady(pod)).To(BeTrue())
				g.Expect(cosmosignerDiscoveryGateSucceeded(pod)).To(BeTrue())
				restartCount, previousLogs, found := appContainerRestartDetails(ns.Name, pod, cns.Spec.App.App)
				g.Expect(found).To(BeTrue(), "app container %q status is missing", cns.Spec.App.App)
				g.Expect(restartCount).To(BeZero(), "the new target's app container restarted; previous logs:\n%s", previousLogs)
			}, 35*time.Second, 500*time.Millisecond).Should(Succeed())

			Eventually(func() (int64, error) {
				return observedChainNodeSetHeight(cns)
			}, time.Minute, time.Second).Should(BeNumerically(">", heightAfterRetarget),
				"the chain must keep advancing after the retarget settles")
		})

		// Serial: this spec restarts the shared Cosmopilot deployment, which every other spec depends
		// on, so it must not run alongside them.
		It("should migrate a ChainNodeSet validator from tmKMS Vault to a top-level cosmosigner across a controller restart", Serial, func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			tokenSecretName, caSecretName := CopyVaultSecretsToNamespace(ns.Name)
			keyName := fmt.Sprintf("%s-nodeset-cosmosigner-%s", app.ValidatorConfig.ChainID, RandString(6))
			cns := app.BuildChainNodeSetWithTmKMS(ns.Name, apps.TmKMSConfig{
				VaultAddress:    GetVaultAddress(),
				KeyName:         keyName,
				TokenSecretName: tokenSecretName,
				CASecretName:    caSecretName,
			})
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			// The legacy singleton validator is a generated child ChainNode, so the tmKMS-era waits
			// apply to it directly.
			validatorName := fmt.Sprintf("%s-validator", cns.Name)
			signerName := fmt.Sprintf("%s-signer", cns.Name)
			WaitForChainNodeSetHeight(cns, 3)
			WaitForTmkmsContainerRunning(&appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: validatorName},
			})
			Eventually(func() string {
				child := &appsv1.ChainNode{}
				if err := Framework().Client().Get(Framework().Context(),
					client.ObjectKey{Namespace: ns.Name, Name: validatorName}, child); err != nil {
					return ""
				}
				return child.Annotations[controllers.AnnotationVaultKeyUploaded]
			}).Should(Equal(controllers.StringValueTrue), "the cosmosigner can only adopt a key tmKMS already uploaded")

			var publicKey string
			var heightBeforeMigration int64
			Eventually(func(g Gomega) {
				current := &appsv1.ChainNodeSet{}
				g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current)).To(Succeed())
				publicKey = managedcosmosigner.CanonicalSDKPublicKey(current.Status.PubKey)
				g.Expect(publicKey).NotTo(BeEmpty())
				heightBeforeMigration = current.Status.LatestHeight
			}).Should(Succeed())
			reservation := waitForConsensusKeyReservation(cns, publicKey)
			Expect(reservation.Spec.Claim).To(Equal(validatorName),
				"the tmKMS-era validator child claims its key under the root ChainNodeSet")

			// Hold the old pod in Terminating so the migration parks in its break-before-make window:
			// the tmKMS signer is gone and the replacement cannot be created yet.
			tmkmsPod := &corev1.Pod{}
			Expect(Framework().Client().Get(Framework().Context(),
				client.ObjectKey{Namespace: ns.Name, Name: validatorName}, tmkmsPod)).To(Succeed())
			tmkmsPodUID := string(tmkmsPod.UID)
			setPodTestFinalizer(ns.Name, validatorName, true)
			DeferCleanup(func() { setPodTestFinalizer(ns.Name, validatorName, false) })

			migrateNodeSetValidatorToCosmosigner(cns, keyName, tokenSecretName, caSecretName)
			waitForBrokenTmKMSValidatorPod(ns.Name, validatorName, signerName, tmkmsPodUID)

			// A restart drops every cached decision, so the controller must re-derive the migration
			// from live state alone — including the reservation its own root already recorded against
			// the managed signer, which VLZ-799 read back as a conflicting legacy owner and deadlocked
			// on, leaving the validator quiesced with no replacement.
			restartCosmopilotController()
			setPodTestFinalizer(ns.Name, validatorName, false)

			replacement := waitForCosmosignerTargetedValidatorPod(ns.Name, validatorName, signerName, tmkmsPodUID, cns.Spec.App.App)
			Expect(replacement.Labels[controllers.LabelCosmosignerTarget]).To(Equal(signerName))
			assertNoCosmosignerDiscoveryPubKeyFailure(ns.Name, validatorName, cns.Spec.App.App, 1)

			signerStatus := waitForCosmosignerApplied(cns, signerName)
			Expect(signerStatus.PublicKey).To(Equal(publicKey),
				"the managed signer must adopt the tmKMS consensus key, not mint a new one")
			// Same-root alias matching keys on the recorded served group and fails closed without it, so
			// assert it directly: otherwise a regression that stopped recording it would surface only as
			// the replacement-pod wait timing out, with nothing pointing at the cause.
			Expect(signerStatus.ServingGroup).To(Equal(appsv1.ReservedValidatorGroupName),
				"the signer must record the validator group it serves for the child's reservation to be recognised as same-root")
			waitForReadySignerPods(ns.Name, signerName, 1)

			Eventually(func(g Gomega) {
				current := &appsv1.ChainNodeSet{}
				g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current)).To(Succeed())
				g.Expect(managedcosmosigner.CanonicalSDKPublicKey(current.Status.PubKey)).To(Equal(publicKey))
				g.Expect(current.Status.LatestHeight).To(BeNumerically(">", heightBeforeMigration),
					"the migrated validator must resume signing")
			}).Should(Succeed())

			// The sidecar's configuration must be removed, not merely left unused.
			Eventually(func() bool {
				configMap := &corev1.ConfigMap{}
				err := Framework().Client().Get(Framework().Context(),
					client.ObjectKey{Namespace: ns.Name, Name: validatorName + "-tmkms"}, configMap)
				return apierrors.IsNotFound(err)
			}).Should(BeTrue())

			// One consensus key, one reservation, held continuously across the whole migration: a
			// released-and-recreated reservation would mean the key went unguarded in between.
			assertConsensusKeyReservationUnchanged(reservation)
		})
	})
})

func requireCosmosignerE2E() {
	if !Framework().Config().InstallVault {
		Skip("cosmosigner e2e is opt-in (enable with the Vault install flag)")
	}
}

func waitForConsensusKeyReservation(cns *appsv1.ChainNodeSet, publicKey string) *appsv1.ConsensusKeyReservation {
	var result *appsv1.ConsensusKeyReservation
	Eventually(func(g Gomega) {
		current := &appsv1.ChainNodeSet{}
		g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current)).To(Succeed())
		g.Expect(current.Status.ChainID).NotTo(BeEmpty())
		g.Expect(publicKey).NotTo(BeEmpty())

		reservation := &appsv1.ConsensusKeyReservation{}
		g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{
			Name: managedcosmosigner.ConsensusKeyReservationName(current.Status.ChainID, publicKey),
		}, reservation)).To(Succeed())
		g.Expect(reservation.Spec.ChainID).To(Equal(current.Status.ChainID))
		g.Expect(reservation.Spec.PublicKey).To(Equal(publicKey))
		g.Expect(reservation.Spec.OwnerUID).To(Equal(current.UID))
		g.Expect(reservation.Spec.OwnerKind).To(Equal("ChainNodeSet"))
		g.Expect(reservation.Spec.Namespace).To(Equal(current.Namespace))
		g.Expect(reservation.Spec.OwnerName).To(Equal(current.Name))
		g.Expect(reservation.Spec.Claim).NotTo(BeEmpty())
		result = reservation.DeepCopy()
	}).Should(Succeed())
	return result
}

func assertConsensusKeyReservationUnchanged(want *appsv1.ConsensusKeyReservation) {
	Consistently(func(g Gomega) {
		current := &appsv1.ConsensusKeyReservation{}
		g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(want), current)).To(Succeed())
		g.Expect(current.UID).To(Equal(want.UID), "the consensus-key reservation must not be released and recreated")
		g.Expect(current.Spec).To(Equal(want.Spec), "the consensus-key reservation owner, claim and public key must remain unchanged")
	}, 5*time.Second, 500*time.Millisecond).Should(Succeed())
}

func buildNamedValidatorCosmosignerSet(app apps.TestApp, namespace string, replicas int32) *appsv1.ChainNodeSet {
	cns := app.BuildChainNodeSet(namespace, 0)
	cns.Name = fmt.Sprintf("e2e-nibiru-cosmosigner-%s", RandString(6))
	cns.GenerateName = ""
	validator := cns.Spec.Validator
	cns.Spec.Validator = nil
	cns.Spec.Nodes = []appsv1.NodeGroupSpec{{
		Name:      "validators",
		Instances: ptr.To(1),
		Validator: validator,
	}}
	cns.Spec.Cosmosigner = &appsv1.Cosmosigner{
		NodeGroups: []string{"validators"},
		Replicas:   ptr.To(replicas),
		Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{},
		},
	}
	return cns
}

func moveTopLevelCosmosignerIntoGroup(cns *appsv1.ChainNodeSet, groupName string) {
	Eventually(func() error {
		current := &appsv1.ChainNodeSet{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
			return err
		}
		if current.Spec.Cosmosigner == nil {
			for i := range current.Spec.Nodes {
				if current.Spec.Nodes[i].Name == groupName && current.Spec.Nodes[i].Cosmosigner != nil {
					return nil
				}
			}
			return fmt.Errorf("top-level cosmosigner is absent without a replacement in group %q", groupName)
		}
		replacement := current.Spec.Cosmosigner.DeepCopy()
		replacement.NodeGroups = nil
		for i := range current.Spec.Nodes {
			if current.Spec.Nodes[i].Name == groupName {
				current.Spec.Nodes[i].Cosmosigner = replacement
				current.Spec.Cosmosigner = nil
				return Framework().Client().Update(Framework().Context(), current)
			}
		}
		return fmt.Errorf("node group %q not found", groupName)
	}).Should(Succeed())
}

// migrateNodeSetValidatorToCosmosigner switches the legacy singleton validator from its tmKMS
// sidecar to a top-level cosmosigner over the same Vault key. Both must move in a single update: the
// webhook rejects a spec carrying .spec.validator.tmKMS and .spec.cosmosigner at once, which is also
// what makes the switch a break-before-make rather than an overlap.
func migrateNodeSetValidatorToCosmosigner(cns *appsv1.ChainNodeSet, keyName, tokenSecretName, caSecretName string) {
	Eventually(func() error {
		current := &appsv1.ChainNodeSet{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
			return err
		}
		if current.Spec.Validator == nil {
			return fmt.Errorf("the legacy singleton validator is absent")
		}
		current.Spec.Validator.TmKMS = nil
		current.Spec.Cosmosigner = &appsv1.Cosmosigner{
			Replicas: ptr.To[int32](1),
			Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: GetVaultAddress(),
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
}

// waitForBrokenTmKMSValidatorPod waits until the tmKMS validator pod is being torn down, which is
// where a break-before-make migration is at its most exposed: the old signing path is gone and the
// replacement does not exist yet.
func waitForBrokenTmKMSValidatorPod(namespace, name, signerName, uid string) {
	Eventually(func(g Gomega) {
		pod := &corev1.Pod{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod,
		)).To(Succeed())
		assertNoTmKMSSigningOverlap(pod, signerName)
		g.Expect(string(pod.UID)).To(Equal(uid), "the tmKMS pod was replaced before the test observed the break")
		g.Expect(pod.DeletionTimestamp).NotTo(BeNil())
	}, 8*time.Minute, time.Second).Should(Succeed())
}

// waitForCosmosignerTargetedValidatorPod waits for the replacement validator pod that closes a
// tmKMS-to-cosmosigner migration: a new pod, without the sidecar, serving the managed signer.
func waitForCosmosignerTargetedValidatorPod(namespace, name, signerName, previousUID, appContainer string) *corev1.Pod {
	var result *corev1.Pod
	Eventually(func(g Gomega) {
		pod := &corev1.Pod{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod,
		)).To(Succeed())
		assertNoTmKMSSigningOverlap(pod, signerName)
		g.Expect(string(pod.UID)).NotTo(Equal(previousUID), "the tmKMS pod must be replaced, not adopted")
		g.Expect(pod.DeletionTimestamp).To(BeNil())
		g.Expect(podHasTmKMSContainer(pod)).To(BeFalse())
		g.Expect(pod.Labels[controllers.LabelCosmosignerTarget]).To(Equal(signerName))
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		g.Expect(podReady(pod)).To(BeTrue())
		g.Expect(cosmosignerDiscoveryGateSucceeded(pod)).To(BeTrue())
		restartCount, previousLogs, found := appContainerRestartDetails(namespace, pod, appContainer)
		g.Expect(found).To(BeTrue(), "app container %q status is missing", appContainer)
		g.Expect(restartCount).To(BeZero(), "the replacement app container restarted; previous logs:\n%s", previousLogs)
		result = pod.DeepCopy()
	}, 8*time.Minute, time.Second).Should(Succeed())
	return result
}

// assertNoTmKMSSigningOverlap fails at the sample that observes a pod running its tmKMS sidecar while
// also selected as a cosmosigner target. Two signing paths holding one consensus key at the same
// instant is the double-sign the break-before-make migration exists to prevent, so it must fail where
// it is seen rather than be retried past.
func assertNoTmKMSSigningOverlap(pod *corev1.Pod, signerName string) {
	Expect(podHasTmKMSContainer(pod) && pod.Labels[controllers.LabelCosmosignerTarget] == signerName).To(BeFalse(),
		"pod %q carries the tmKMS sidecar and the cosmosigner target label at the same time", pod.Name)
}

func podHasTmKMSContainer(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == "tmkms" {
			return true
		}
	}
	return false
}

const (
	cosmopilotNamespace      = "cosmopilot-system"
	cosmopilotDeploymentName = "cosmopilot"
)

// restartCosmopilotController deletes the controller pods and waits for their replacements to become
// ready, leaving the operator with no cached state about work already in flight.
func restartCosmopilotController() {
	By("restarting the Cosmopilot controller")
	deployment := &appsv1k8s.Deployment{}
	Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{
		Namespace: cosmopilotNamespace, Name: cosmopilotDeploymentName,
	}, deployment)).To(Succeed())
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	Expect(err).NotTo(HaveOccurred())
	listOptions := []client.ListOption{
		client.InNamespace(cosmopilotNamespace),
		client.MatchingLabelsSelector{Selector: selector},
	}

	running := &corev1.PodList{}
	Expect(Framework().Client().List(Framework().Context(), running, listOptions...)).To(Succeed())
	Expect(running.Items).NotTo(BeEmpty(), "the Cosmopilot controller has no pods to restart")
	previousUIDs := make(map[string]struct{}, len(running.Items))
	for i := range running.Items {
		previousUIDs[string(running.Items[i].UID)] = struct{}{}
		Expect(Framework().Client().Delete(Framework().Context(), &running.Items[i])).To(Succeed())
	}

	Eventually(func(g Gomega) {
		pods := &corev1.PodList{}
		g.Expect(Framework().Client().List(Framework().Context(), pods, listOptions...)).To(Succeed())
		g.Expect(pods.Items).To(HaveLen(len(previousUIDs)))
		for i := range pods.Items {
			g.Expect(previousUIDs).NotTo(HaveKey(string(pods.Items[i].UID)))
			g.Expect(pods.Items[i].DeletionTimestamp).To(BeNil())
			g.Expect(podReady(&pods.Items[i])).To(BeTrue())
		}
	}, 3*time.Minute, time.Second).Should(Succeed())
}

const (
	cosmosignerSentryGroupA = "sentry-a"
	cosmosignerSentryGroupB = "sentry-b"
)

// buildSentryRetargetCosmosignerSet builds a ChainNodeSet with a self-signing validator and two
// interchangeable sentry groups, plus a sentry-mode signer dialing the first of them. The validator
// signs locally and keeps producing blocks throughout, so the chain height stays an independent
// witness of the retarget rather than a restatement of it.
func buildSentryRetargetCosmosignerSet(app apps.TestApp, namespace, keySecretName string) *appsv1.ChainNodeSet {
	cns := app.BuildChainNodeSet(namespace, 0)
	cns.Name = fmt.Sprintf("e2e-nibiru-retarget-%s", RandString(6))
	cns.GenerateName = ""

	// Both sentries reuse the generated fullnode group's config, so the targeted group is the only
	// difference between A and B.
	template := cns.Spec.Nodes[0]
	groups := make([]appsv1.NodeGroupSpec, 0, 2)
	for _, name := range []string{cosmosignerSentryGroupA, cosmosignerSentryGroupB} {
		group := template.DeepCopy()
		group.Name = name
		group.Instances = ptr.To(1)
		groups = append(groups, *group)
	}
	cns.Spec.Nodes = groups

	cns.Spec.Cosmosigner = &appsv1.Cosmosigner{
		NodeGroups: []string{cosmosignerSentryGroupA},
		Replicas:   ptr.To[int32](1),
		Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(keySecretName)},
		},
	}
	return cns
}

// createCosmosignerSentryKeySecret creates the out-of-band consensus key a sentry-mode signer must
// carry: targeting no validator, it has no controller-registered key to reuse.
func createCosmosignerSentryKeySecret(namespace string) string {
	key, err := cometbft.GeneratePrivKey()
	Expect(err).NotTo(HaveOccurred())
	const name = "e2e-cosmosigner-sentry-priv-key"
	Expect(Framework().Client().Create(Framework().Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{chainnodecontroller.PrivKeyFilename: key},
	})).To(Succeed())
	return name
}

func retargetTopLevelCosmosigner(cns *appsv1.ChainNodeSet, groups ...string) {
	Eventually(func() error {
		current := &appsv1.ChainNodeSet{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
			return err
		}
		if current.Spec.Cosmosigner == nil {
			return fmt.Errorf("top-level cosmosigner is absent")
		}
		current.Spec.Cosmosigner.NodeGroups = append([]string(nil), groups...)
		return Framework().Client().Update(Framework().Context(), current)
	}).Should(Succeed())
}

// waitForCosmosignerRetargetCompletion drives a target-group retarget to completion while asserting
// its ordering guarantee: the migration must stay open until the newly targeted pod is running,
// ready and past its discovery gate. It samples directly rather than through Eventually because a
// premature completion has to fail at the sample that observes it — a retry would let the new target
// turn healthy in the meantime and hide it.
func waitForCosmosignerRetargetCompletion(cns *appsv1.ChainNodeSet, logicalName, targetPodName, previousDigest string) *appsv1.CosmosignerStatus {
	const (
		pollInterval = 500 * time.Millisecond
		pollTimeout  = 8 * time.Minute
	)
	deadline := time.Now().Add(pollTimeout)
	for {
		current := &appsv1.ChainNodeSet{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
			Expect(time.Now()).To(BeTemporally("<", deadline),
				"timed out reading the ChainNodeSet while waiting for the retarget to %s: %v", targetPodName, err)
			time.Sleep(pollInterval)
			continue
		}
		status := current.GetCosmosignerStatus(logicalName)
		Expect(status).NotTo(BeNil(), "signer %q lost its status entry during the retarget", logicalName)
		switch {
		case status.Migration != nil:
		case status.AppliedDigest != "" && status.AppliedDigest != previousDigest:
			Expect(cosmosignerTargetPodServed(
				cns.GetNamespace(), targetPodName, appsv1.CosmosignerStatusResourceName(status),
			)).To(BeTrue(), "the retarget completed before %s was running, ready and past its discovery gate", targetPodName)
			return status.DeepCopy()
		}
		Expect(time.Now()).To(BeTemporally("<", deadline),
			"timed out waiting for the retarget to %s to complete", targetPodName)
		time.Sleep(pollInterval)
	}
}

// cosmosignerTargetPodServed reports whether a pod is a live endpoint of the given signer: labelled
// for it, running, ready, and past the discovery gate that exits only once the discovery Service
// publishes this pod to the signer.
func cosmosignerTargetPodServed(namespace, name, signerName string) bool {
	pod := &corev1.Pod{}
	if err := Framework().Client().Get(
		Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod,
	); err != nil {
		return false
	}
	return pod.DeletionTimestamp == nil &&
		pod.Labels[controllers.LabelCosmosignerTarget] == signerName &&
		pod.Status.Phase == corev1.PodRunning &&
		podReady(pod) &&
		cosmosignerDiscoveryGateSucceeded(pod)
}

func waitForCosmosignerApplied(cns *appsv1.ChainNodeSet, logicalName string) *appsv1.CosmosignerStatus {
	var result *appsv1.CosmosignerStatus
	Eventually(func(g Gomega) {
		current := &appsv1.ChainNodeSet{}
		g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current)).To(Succeed())
		status := current.GetCosmosignerStatus(logicalName)
		g.Expect(status).NotTo(BeNil())
		if status == nil {
			return
		}
		g.Expect(status.AppliedDigest).NotTo(BeEmpty())
		g.Expect(status.PublicKey).NotTo(BeEmpty())
		g.Expect(status.Migration).To(BeNil())
		result = status.DeepCopy()
	}).Should(Succeed())
	return result
}

func signerPVCUIDs(namespace, resourceName string, replicas int32) map[string]string {
	uids := make(map[string]string, replicas)
	for ordinal := int32(0); ordinal < replicas; ordinal++ {
		name := fmt.Sprintf("data-%s-%d", resourceName, ordinal)
		pvc := &corev1.PersistentVolumeClaim{}
		Eventually(func() error {
			return Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pvc)
		}).Should(Succeed())
		uids[name] = string(pvc.UID)
	}
	return uids
}

func listSignerPods(namespace, resourceName string) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := Framework().Client().List(
		Framework().Context(),
		list,
		client.InNamespace(namespace),
		client.MatchingLabels(managedcosmosigner.InstanceLabels(resourceName)),
	); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

func waitForReadySignerPods(namespace, resourceName string, replicas int) []corev1.Pod {
	var result []corev1.Pod
	Eventually(func(g Gomega) {
		pods, err := listSignerPods(namespace, resourceName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods).To(HaveLen(replicas))
		for i := range pods {
			g.Expect(pods[i].DeletionTimestamp).To(BeNil())
			g.Expect(podReady(&pods[i])).To(BeTrue())
		}
		result = append([]corev1.Pod(nil), pods...)
	}).Should(Succeed())
	return result
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func waitForReadyCosmosignerTargetPod(namespace, name, signerName, appContainer string) *corev1.Pod {
	var result *corev1.Pod
	Eventually(func(g Gomega) {
		pod := &corev1.Pod{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod,
		)).To(Succeed())
		g.Expect(pod.Labels[controllers.LabelCosmosignerTarget]).To(Equal(signerName))
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		g.Expect(podReady(pod)).To(BeTrue())
		g.Expect(cosmosignerDiscoveryGateSucceeded(pod)).To(BeTrue())
		restartCount, previousLogs, found := appContainerRestartDetails(namespace, pod, appContainer)
		g.Expect(found).To(BeTrue(), "app container %q status is missing", appContainer)
		g.Expect(restartCount).To(BeZero(), "initial app container restarted; previous logs:\n%s", previousLogs)
		result = pod.DeepCopy()
	}, 2*time.Minute, 500*time.Millisecond).Should(Succeed())
	return result
}

func replaceCosmosignerTargetPodOnce(cns *appsv1.ChainNodeSet, current *corev1.Pod, signerName, appContainer string, cycle int) *corev1.Pod {
	By(fmt.Sprintf("restarting Cosmosigner target pod, cycle %d", cycle))
	namespace := cns.GetNamespace()
	ctx, cancel := context.WithTimeout(Framework().Context(), 2*time.Minute)
	defer cancel()

	podWatch, err := Framework().KubeClient().CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", current.Name).String(),
		ResourceVersion: current.ResourceVersion,
	})
	Expect(err).NotTo(HaveOccurred())
	defer podWatch.Stop()

	oldUID := string(current.UID)
	Expect(Framework().Client().Delete(Framework().Context(), current.DeepCopy())).To(Succeed())

	replacementUIDs := map[string]struct{}{}
	var replacement *corev1.Pod
	for replacement == nil {
		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("timed out waiting for Cosmosigner target pod replacement in cycle %d: %v", cycle, ctx.Err()))
		case event, ok := <-podWatch.ResultChan():
			if !ok {
				Fail(fmt.Sprintf("Cosmosigner target pod watch closed before cycle %d replacement became ready", cycle))
			}
			if event.Type == watch.Error {
				Fail(fmt.Sprintf("Cosmosigner target pod watch failed in cycle %d: %v", cycle, apierrors.FromObject(event.Object)))
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || string(pod.UID) == oldUID {
				continue
			}
			replacementUIDs[string(pod.UID)] = struct{}{}
			Expect(replacementUIDs).To(HaveLen(1), "cycle %d created more than one replacement Pod UID", cycle)
			if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && podReady(pod) && cosmosignerDiscoveryGateSucceeded(pod) {
				replacement = pod.DeepCopy()
			}
		}
	}

	Expect(replacement.Labels[controllers.LabelCosmosignerTarget]).To(Equal(signerName))
	Expect(string(replacement.UID)).NotTo(Equal(oldUID))
	restartCount, previousLogs, found := appContainerRestartDetails(namespace, replacement, appContainer)
	Expect(found).To(BeTrue(), "cycle %d app container %q status is missing", cycle, appContainer)
	Expect(restartCount).To(BeZero(), "cycle %d app container restarted; previous logs:\n%s", cycle, previousLogs)
	heightAtReplacement, err := observedChainNodeSetHeight(cns)
	Expect(err).NotTo(HaveOccurred())
	Eventually(func() (int64, error) {
		return observedChainNodeSetHeight(cns)
	}, time.Minute, time.Second).Should(BeNumerically(">", heightAtReplacement),
		"cycle %d chain height must advance after the replacement becomes ready", cycle)

	Consistently(func(g Gomega) {
		pod := &corev1.Pod{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: replacement.Name}, pod,
		)).To(Succeed())
		g.Expect(string(pod.UID)).To(Equal(string(replacement.UID)), "cycle %d must not create a second replacement", cycle)
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		g.Expect(podReady(pod)).To(BeTrue())
		g.Expect(cosmosignerDiscoveryGateSucceeded(pod)).To(BeTrue())
		restartCount, previousLogs, found := appContainerRestartDetails(namespace, pod, appContainer)
		g.Expect(found).To(BeTrue(), "cycle %d app container %q status is missing", cycle, appContainer)
		g.Expect(restartCount).To(BeZero(), "cycle %d app container restarted; previous logs:\n%s", cycle, previousLogs)
	}, 35*time.Second, 500*time.Millisecond).Should(Succeed())
	assertNoCosmosignerDiscoveryPubKeyFailure(namespace, replacement.Name, appContainer, cycle)

	return replacement
}

func cosmosignerDiscoveryGateSucceeded(pod *corev1.Pod) bool {
	for i := range pod.Status.InitContainerStatuses {
		status := &pod.Status.InitContainerStatuses[i]
		if status.Name == chainnodecontroller.CosmosignerDiscoveryWaitContainerName {
			return status.State.Terminated != nil && status.State.Terminated.ExitCode == 0
		}
	}
	return false
}

func appContainerRestartDetails(namespace string, pod *corev1.Pod, appContainer string) (int32, string, bool) {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name != appContainer {
			continue
		}
		if status.RestartCount == 0 {
			return 0, "", true
		}
		logs, err := Framework().KubeClient().CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: appContainer,
			Previous:  true,
		}).DoRaw(Framework().Context())
		if err != nil {
			return status.RestartCount, fmt.Sprintf("failed to read previous logs: %v", err), true
		}
		return status.RestartCount, string(logs), true
	}
	return 0, "", false
}

func assertNoCosmosignerDiscoveryPubKeyFailure(namespace, podName, appContainer string, cycle int) {
	logs, err := Framework().KubeClient().CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: appContainer,
	}).DoRaw(Framework().Context())
	Expect(err).NotTo(HaveOccurred())
	lower := strings.ToLower(string(logs))
	for _, signature := range []string{
		"can't get pubkey",
		"can't-get-pubkey",
		"cannot get pubkey",
		"cannot-get-pubkey",
		"exhausted all attempts to get pubkey",
		"failed to get private validator pubkey",
	} {
		Expect(lower).NotTo(ContainSubstring(signature), "cycle %d app logs contain a remote-signer public-key startup failure", cycle)
	}
	for _, line := range strings.Split(lower, "\n") {
		publicKeyLine := strings.Contains(line, "pubkey") || strings.Contains(line, "public key") || strings.Contains(line, "public-key")
		timeoutLine := strings.Contains(line, "timeout") || strings.Contains(line, "timed out") || strings.Contains(line, "deadline exceeded")
		Expect(publicKeyLine && timeoutLine).To(BeFalse(), "cycle %d app logs contain a public-key timeout line: %q", cycle, line)
	}
}

const e2eHoldFinalizer = "e2e.cosmopilot.voluzi.com/hold"

func setPodTestFinalizer(namespace, name string, present bool) {
	Eventually(func() error {
		pod := &corev1.Pod{}
		err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod)
		if apierrors.IsNotFound(err) && !present {
			return nil
		}
		if err != nil {
			return err
		}
		hasFinalizer := false
		for _, finalizer := range pod.Finalizers {
			if finalizer == e2eHoldFinalizer {
				hasFinalizer = true
			}
		}
		if hasFinalizer == present {
			return nil
		}
		if present {
			pod.Finalizers = append(pod.Finalizers, e2eHoldFinalizer)
		} else {
			filtered := pod.Finalizers[:0]
			for _, finalizer := range pod.Finalizers {
				if finalizer != e2eHoldFinalizer {
					filtered = append(filtered, finalizer)
				}
			}
			pod.Finalizers = filtered
		}
		return Framework().Client().Update(Framework().Context(), pod)
	}).Should(Succeed())
}

func waitForTerminatingSignerPod(namespace, name, uid string, requireNotReady bool) {
	Eventually(func() bool {
		pod := &corev1.Pod{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			return false
		}
		if string(pod.UID) != uid || pod.DeletionTimestamp == nil {
			return false
		}
		return !requireNotReady || !podReady(pod)
	}).Should(BeTrue())
}

func waitForReplacementSignerPod(namespace, name, oldUID string) {
	Eventually(func() bool {
		pod := &corev1.Pod{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			return false
		}
		return string(pod.UID) != oldUID && pod.DeletionTimestamp == nil && podReady(pod)
	}).Should(BeTrue())
}

func observedChainNodeSetHeight(cns *appsv1.ChainNodeSet) (int64, error) {
	current := &appsv1.ChainNodeSet{}
	if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
		return 0, err
	}
	return current.Status.LatestHeight, nil
}

func observedChainNodeHeight(namespace, name string) (int64, error) {
	current := &appsv1.ChainNode{}
	if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, current); err != nil {
		return 0, err
	}
	return current.Status.LatestHeight, nil
}

func waitForStableChainNodeHeight(namespace, name string, stableFor time.Duration) int64 {
	var height int64
	var unchangedSince time.Time
	Eventually(func() (bool, error) {
		current, err := observedChainNodeHeight(namespace, name)
		if err != nil {
			return false, err
		}
		if unchangedSince.IsZero() || current != height {
			height = current
			unchangedSince = time.Now()
			return false, nil
		}
		return time.Since(unchangedSince) >= stableFor, nil
	}, 45*time.Second, time.Second).Should(BeTrue())
	return height
}

func waitForSignerLeader(namespace, resourceName, excludedPod string) string {
	var leader string
	Eventually(func() string {
		pods, err := listSignerPods(namespace, resourceName)
		if err != nil {
			return ""
		}
		for i := range pods {
			if pods[i].Name == excludedPod || pods[i].DeletionTimestamp != nil {
				continue
			}
			logs, err := Framework().KubeClient().CoreV1().Pods(namespace).GetLogs(pods[i].Name, &corev1.PodLogOptions{
				Container: "cosmosigner",
			}).DoRaw(Framework().Context())
			if err == nil && strings.Contains(string(logs), "serving remote signer") {
				leader = pods[i].Name
				return leader
			}
		}
		return ""
	}, 2*time.Minute, 2*time.Second).ShouldNot(BeEmpty())
	return leader
}

func createRaftTLSSecret(namespace, resourceName string) string {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cosmosigner e2e raft ca"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: resourceName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		DNSNames: []string{
			fmt.Sprintf("*.%s.%s.svc", resourceName, namespace),
			fmt.Sprintf("%s.%s.svc", resourceName, namespace),
		},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	Expect(err).NotTo(HaveOccurred())

	name := "cosmosigner-raft-tls"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
			"ca.crt":                pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		},
	}
	Expect(Framework().Client().Create(Framework().Context(), secret)).To(Succeed())
	return name
}
