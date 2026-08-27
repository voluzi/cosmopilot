package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/test/e2e/apps"
)

var _ = Describe("Snapshot E2E", func() {
	Context("Volume Snapshots", func() {
		apps.ForEachApp("should create snapshots via persistence.snapshots config",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				chainNode := app.BuildChainNode(ns.Name)
				configureSnapshotPersistence(chainNode)

				err := Framework().Client().Create(Framework().Context(), chainNode)
				Expect(err).NotTo(HaveOccurred())

				// Wait for node to be running and generate some blocks
				WaitForChainNodeHeight(chainNode, 5)
				RefreshChainNode(chainNode)

				// Wait for cosmopilot to create at least one snapshot
				By("Waiting for cosmopilot to create a snapshot")
				Eventually(func() int {
					return countSnapshotsForPVC(ns.Name, chainNode.Name)
				}, 2*time.Minute, 10*time.Second).Should(BeNumerically(">=", 1))
			}),
		)

		apps.ForEachApp("should restore a ChainNode from a snapshot",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				chainNode := app.BuildChainNode(ns.Name)
				configureSnapshotPersistence(chainNode)

				err := Framework().Client().Create(Framework().Context(), chainNode)
				Expect(err).NotTo(HaveOccurred())

				// Wait for node to generate some blocks
				WaitForChainNodeHeight(chainNode, 5)

				// Wait for cosmopilot to create at least one ready snapshot
				By("Waiting for cosmopilot to create a snapshot")
				var snapshotName string
				Eventually(func() bool {
					snapshotName = findReadySnapshotForPVC(ns.Name, chainNode.Name)
					return snapshotName != ""
				}, 2*time.Minute, 10*time.Second).Should(BeTrue())

				// Delete the original ChainNode
				err = Framework().Client().Delete(Framework().Context(), chainNode)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNode to be deleted
				Eventually(func() bool {
					err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNode), chainNode)
					return err != nil
				}).Should(BeTrue())

				// Create a new ChainNode that restores from the snapshot
				restoredChainNode := app.BuildChainNode(ns.Name)
				if restoredChainNode.Spec.Persistence == nil {
					restoredChainNode.Spec.Persistence = &appsv1.Persistence{}
				}
				restoredChainNode.Spec.Persistence.StorageClassName = ptr.To("csi-hostpath-sc")
				restoredChainNode.Spec.Persistence.RestoreFromSnapshot = &appsv1.PvcSnapshot{
					Name: snapshotName,
				}
				// Remove validator init since we're restoring from snapshot
				restoredChainNode.Spec.Validator = nil
				// Set genesis to use data volume since genesis is already in the snapshot
				restoredChainNode.Spec.Genesis = &appsv1.GenesisConfig{
					UseDataVolume: ptr.To(true),
					ChainID:       ptr.To(app.ValidatorConfig.ChainID),
				}

				err = Framework().Client().Create(Framework().Context(), restoredChainNode)
				Expect(err).NotTo(HaveOccurred())

				// Wait for restored node to start generating blocks
				WaitForChainNodeHeight(restoredChainNode, 1)
			}),
		)
	})

	Context("Snapshot Retention", func() {
		apps.ForEachApp("should enforce retain count and delete old snapshots",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				chainNode := app.BuildChainNode(ns.Name)
				if chainNode.Spec.Persistence == nil {
					chainNode.Spec.Persistence = &appsv1.Persistence{}
				}
				chainNode.Spec.Persistence.StorageClassName = ptr.To("csi-hostpath-sc")
				chainNode.Spec.Persistence.Snapshots = &appsv1.VolumeSnapshotsConfig{
					Frequency:            "1m",             // Create snapshot every minute
					Retain:               ptr.To[int32](2), // Keep only 2 snapshots
					PreserveLastSnapshot: ptr.To(true),
					SnapshotClassName:    ptr.To("csi-hostpath-snapclass"),
				}

				err := Framework().Client().Create(Framework().Context(), chainNode)
				Expect(err).NotTo(HaveOccurred())

				// Wait for node to start generating blocks
				WaitForChainNodeHeight(chainNode, 5)
				RefreshChainNode(chainNode)

				// Retention only does anything once a third snapshot forces a deletion, and the live
				// count alone cannot tell "retention pruned back to 2" apart from "only 2 exist so
				// far" — so asserting on it directly would pass vacuously after two cycles. Watch the
				// names instead: once three distinct snapshots have been observed, a deletion must
				// have happened for the count to be 2. This replaces a flat sleep long enough to
				// cover three cycles, which cost four minutes on every app whatever the cluster did.
				By("Waiting until enough snapshots have been created for retention to apply")
				created := make(map[string]struct{})
				Eventually(func() int {
					for _, name := range snapshotNamesForPVC(ns.Name, chainNode.Name) {
						created[name] = struct{}{}
					}
					return len(created)
				}, 6*time.Minute, 5*time.Second).Should(BeNumerically(">=", 3),
					"Expected at least 3 snapshots to have been created before checking retention")

				// Verify retention policy is enforced (should have exactly 2 snapshots)
				Eventually(func() int {
					return countSnapshotsForPVC(ns.Name, chainNode.Name)
				}, 2*time.Minute, 10*time.Second).Should(Equal(2), "Expected exactly 2 snapshots due to retain policy")

				// Snapshots keep being taken on the configured frequency, so this set is never at
				// rest. The wait above returns the moment a third snapshot appears, and that moment
				// is exactly when the third one is seconds old and still provisioning — so reading
				// the list once here asks for readiness at the least likely instant of the cycle.
				// Poll for a moment when every surviving snapshot is ready instead.
				By("Waiting until every surviving snapshot is ready")
				Eventually(func() error {
					snapshotList := &snapshotv1.VolumeSnapshotList{}
					if err := Framework().Client().List(Framework().Context(), snapshotList, &client.ListOptions{
						Namespace: ns.Name,
					}); err != nil {
						return err
					}

					for _, snap := range snapshotList.Items {
						if snap.Spec.Source.PersistentVolumeClaimName == nil ||
							*snap.Spec.Source.PersistentVolumeClaimName != chainNode.Name {
							continue
						}
						if snap.Status == nil || snap.Status.ReadyToUse == nil || !*snap.Status.ReadyToUse {
							return fmt.Errorf("snapshot %s is not ready yet", snap.Name)
						}
					}
					return nil
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}),
		)
	})
})

// configureSnapshotPersistence sets up a ChainNode for snapshot testing
func configureSnapshotPersistence(chainNode *appsv1.ChainNode) {
	if chainNode.Spec.Persistence == nil {
		chainNode.Spec.Persistence = &appsv1.Persistence{}
	}
	chainNode.Spec.Persistence.StorageClassName = ptr.To("csi-hostpath-sc")
	chainNode.Spec.Persistence.Snapshots = &appsv1.VolumeSnapshotsConfig{
		Frequency:         "1m", // Create snapshot every minute
		SnapshotClassName: ptr.To("csi-hostpath-snapclass"),
	}
}

// countSnapshotsForPVC counts the number of snapshots for a given PVC
func countSnapshotsForPVC(namespace, pvcName string) int {
	return len(snapshotNamesForPVC(namespace, pvcName))
}

// snapshotNamesForPVC returns the names of the snapshots currently existing for a given PVC.
// Callers that need to know a snapshot was taken at all — rather than how many survive right now —
// accumulate these across polls, since retention deletes them as it goes.
func snapshotNamesForPVC(namespace, pvcName string) []string {
	snapshotList := &snapshotv1.VolumeSnapshotList{}
	if err := Framework().Client().List(Framework().Context(), snapshotList, &client.ListOptions{
		Namespace: namespace,
	}); err != nil {
		return nil
	}

	names := make([]string, 0, len(snapshotList.Items))
	for _, snap := range snapshotList.Items {
		if snap.Spec.Source.PersistentVolumeClaimName != nil &&
			*snap.Spec.Source.PersistentVolumeClaimName == pvcName {
			names = append(names, snap.Name)
		}
	}
	return names
}

// findReadySnapshotForPVC finds a ready snapshot for a given PVC and returns its name
func findReadySnapshotForPVC(namespace, pvcName string) string {
	snapshotList := &snapshotv1.VolumeSnapshotList{}
	if err := Framework().Client().List(Framework().Context(), snapshotList, &client.ListOptions{
		Namespace: namespace,
	}); err != nil {
		return ""
	}

	for _, snap := range snapshotList.Items {
		if snap.Spec.Source.PersistentVolumeClaimName != nil &&
			*snap.Spec.Source.PersistentVolumeClaimName == pvcName &&
			snap.Status != nil &&
			snap.Status.ReadyToUse != nil &&
			*snap.Status.ReadyToUse {
			return snap.Name
		}
	}
	return ""
}
