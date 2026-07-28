package chainnode

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

// errYieldReconcile signals that a status write has moved the ChainNode on at the API server, so this
// pass must stop rather than continue with a stale resource version. Any later full Update of the
// caller's copy would conflict — and, on the snapshot path, would do so *after* a ready annotation had
// been persisted, leaving snapshotting-pvc set forever and blocking all future snapshots.
var errYieldReconcile = stderrors.New("chainnode status changed; yielding reconcile")

type SnapshotIntegrityStatus string

const (
	timeLayout = "20060102150405"

	snapshotIntegrityChecking  SnapshotIntegrityStatus = "checking"
	snapshotIntegrityOk        SnapshotIntegrityStatus = "ok"
	snapshotIntegrityCorrupted SnapshotIntegrityStatus = "corrupted"
)

func (r *Reconciler) ensureVolumeSnapshots(ctx context.Context, chainNode *appsv1.ChainNode, nodePodReady bool) error {
	logger := log.FromContext(ctx)

	// Records outlive the feature that created them: disabling snapshots (or losing the prerequisites
	// below) must not strand them, or their objects stay unreachable and status keeps growing. Run this
	// before the prerequisite check, against whatever snapshots still exist.
	if !chainNode.SnapshotsEnabled() || chainNode.Status.PvcSize == "" || chainNode.Status.LatestHeight == 0 {
		if len(chainNode.Status.ExportedTarballs) == 0 {
			return nil
		}
		snapshots, err := r.listNodeSnapshots(ctx, chainNode)
		if err != nil {
			return err
		}
		_, err = r.reconcileOrphanedTarballs(ctx, chainNode, snapshots)
		return err
	}

	// Get list of snapshots
	snapshots, err := r.listNodeSnapshots(ctx, chainNode)
	if err != nil {
		return err
	}

	// Sort snapshots by creation time, newest last
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].CreationTimestamp.Before(&snapshots[j].CreationTimestamp)
	})

	// Fix snapshotting status in case there are no snapshots for this node
	if len(snapshots) == 0 && volumeSnapshotInProgress(chainNode) {
		setSnapshotInProgress(chainNode, false)
		if err = r.Update(ctx, chainNode); err != nil {
			return err
		}
	}

	// Handle tarballs whose VolumeSnapshot is gone. Their record is the only remaining pointer to the
	// object, so deletion runs through it before it is retired.
	yieldReconcile, err := r.reconcileOrphanedTarballs(ctx, chainNode, snapshots)
	if err != nil {
		return err
	}
	if yieldReconcile {
		// Two reasons to stop here, both requiring a fresh start next pass:
		//
		// A status write advanced the object's resource version at the API server while this copy keeps
		// its own (deliberately, so a stale .spec cannot be written back). Carrying on would let a later
		// full Update of chainNode conflict *after* a snapshot's ready annotation had been persisted, and
		// the retry would skip the ready branch — leaving snapshotting-pvc set forever.
		//
		// Or a superseded tarball's delete Job is still running. It shares its name with the pending
		// retry, and its cleanup removes "<name>-upload", so starting the retry now would have its Job
		// deleted underneath it and be recorded as another failure.
		return nil
	}

	// Grab list of possible tarball names to make sure we delete any possible dangling jobs
	tarballNames := make([]string, 0)

	for _, snapshot := range snapshots {
		if chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() {
			tarballNames = append(tarballNames, getTarballName(chainNode, &snapshot))
		}

		switch {

		// If the snapshot does not have the ready annotation, we haven't processed it yet. So we check if it's ready
		// and if it is, we mark it as ready and register its timestamp in chainnode.
		// In case tarball export is enabled, we also start the export right away, unless integrity checks are enabled,
		// in that case integrity check starts first
		case snapshot.Annotations[controllers.AnnotationPvcSnapshotReady] == strconv.FormatBool(false) && isSnapshotReady(&snapshot):
			logger.Info("pvc snapshot has finished", "snapshot", snapshot.GetName())
			r.recorder.Eventf(chainNode,
				corev1.EventTypeNormal,
				appsv1.ReasonFinishedSnapshot,
				"Finished PVC snapshot %s", snapshot.GetName(),
			)

			// Update snapshot ready annotation
			snapshot.ObjectMeta.Annotations[controllers.AnnotationPvcSnapshotReady] = strconv.FormatBool(true)
			if err = r.Update(ctx, &snapshot); err != nil {
				return err
			}

			// Update ChainNode status
			setSnapshotInProgress(chainNode, false)
			setSnapshotTime(chainNode, snapshot.CreationTimestamp.Time)
			if err = r.Update(ctx, chainNode); err != nil {
				return err
			}

			// If verify is enabled, lets start it now. If not, let's start tarball export if its enabled
			if chainNode.Spec.Persistence.Snapshots.ShouldVerify() {
				logger.Info("starting data integrity check", "snapshot", snapshot.GetName())
				if err = r.startSnapshotIntegrityCheck(ctx, chainNode, &snapshot); err != nil {
					return err
				}
				snapshot.ObjectMeta.Annotations[controllers.AnnotationSnapshotIntegrityStatus] = string(snapshotIntegrityChecking)
				if err = r.Update(ctx, &snapshot); err != nil {
					return err
				}
				r.recorder.Eventf(chainNode,
					corev1.EventTypeNormal,
					appsv1.ReasonSnapshotIntegrityStart,
					"Starting snapshot %s integrity check", snapshot.GetName(),
				)
			} else if chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() {
				logger.Info("starting tarball export", "snapshot", snapshot.GetName())
				if err = r.exportTarball(ctx, chainNode, &snapshot); err != nil {
					return err
				}
				snapshot.ObjectMeta.Annotations[controllers.AnnotationExportingTarball] = strconv.FormatBool(true)
				if err = r.Update(ctx, &snapshot); err != nil {
					return err
				}
				r.recorder.Eventf(chainNode,
					corev1.EventTypeNormal,
					appsv1.ReasonTarballExportStart,
					"Exporting tarball %s from snapshot", getTarballName(chainNode, &snapshot),
				)
			}

		// Let's start the verification job if not started yet, and check if it has completed otherwise.
		case chainNode.Spec.Persistence.Snapshots.ShouldVerify() &&
			snapshot.Annotations[controllers.AnnotationSnapshotIntegrityStatus] != string(snapshotIntegrityOk) &&
			snapshot.Annotations[controllers.AnnotationSnapshotIntegrityStatus] != string(snapshotIntegrityCorrupted):

			status, err := r.getSnapshotIntegrityCheckStatus(ctx, chainNode, &snapshot)
			if err != nil {
				return err
			}

			switch status {
			case snapshotIntegrityChecking:
				logger.Info("data integrity check in progress", "snapshot", snapshot.GetName())

			case snapshotIntegrityOk:
				logger.Info("data integrity check finished successfully. Data is ok.", "snapshot", snapshot.GetName())
				snapshot.ObjectMeta.Annotations[controllers.AnnotationSnapshotIntegrityStatus] = string(snapshotIntegrityOk)
				if err = r.Update(ctx, &snapshot); err != nil {
					return err
				}
				// Persist the result first; clean up is best-effort so that a
				// transient delete failure does not lose the integrity status
				// and force the next reconcile to restart the check.
				if err = r.cleanUpSnapshotIntegrityCheck(ctx, &snapshot); err != nil {
					logger.Error(err, "failed to clean up integrity check resources", "snapshot", snapshot.GetName())
				}

				// Let's start the tarball export right now if it is enabled
				if chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() && snapshot.Annotations[controllers.AnnotationExportingTarball] == "" {
					logger.Info("starting tarball export", "snapshot", snapshot.GetName())
					if err = r.exportTarball(ctx, chainNode, &snapshot); err != nil {
						return err
					}
					snapshot.ObjectMeta.Annotations[controllers.AnnotationExportingTarball] = strconv.FormatBool(true)
					if err = r.Update(ctx, &snapshot); err != nil {
						return err
					}
					r.recorder.Eventf(chainNode,
						corev1.EventTypeNormal,
						appsv1.ReasonTarballExportStart,
						"Exporting tarball %s from snapshot", getTarballName(chainNode, &snapshot),
					)
				}

			case snapshotIntegrityCorrupted:
				logger.Info("data integrity check finished. Data is corrupted.", "snapshot", snapshot.GetName())
				snapshot.ObjectMeta.Annotations[controllers.AnnotationSnapshotIntegrityStatus] = string(snapshotIntegrityCorrupted)
				if err = r.Update(ctx, &snapshot); err != nil {
					return err
				}
				// Persist the result first; clean up is best-effort so that a
				// transient delete failure does not lose the integrity status
				// and force the next reconcile to restart the check.
				if err = r.cleanUpSnapshotIntegrityCheck(ctx, &snapshot); err != nil {
					logger.Error(err, "failed to clean up integrity check resources", "snapshot", snapshot.GetName())
				}
				logger.Info("re-creating snapshot")
				if err = r.Delete(ctx, &snapshot); err != nil {
					return err
				}
				return r.startNewSnapshot(ctx, chainNode)

			default:
				// Integrity check job was not started yet. Let's start it.
				logger.Info("starting data integrity check", "snapshot", snapshot.GetName())

				if err = r.startSnapshotIntegrityCheck(ctx, chainNode, &snapshot); err != nil {
					return err
				}
				snapshot.ObjectMeta.Annotations[controllers.AnnotationSnapshotIntegrityStatus] = string(snapshotIntegrityChecking)
				if err = r.Update(ctx, &snapshot); err != nil {
					return err
				}
				r.recorder.Eventf(chainNode,
					corev1.EventTypeNormal,
					appsv1.ReasonSnapshotIntegrityStart,
					"Starting snapshot %s integrity check", snapshot.GetName(),
				)
			}

		// If for some reason, there is an error starting the tarball export, it is never retried. So we do it here.
		case chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() &&
			(!chainNode.Spec.Persistence.Snapshots.ShouldVerify() || snapshot.Annotations[controllers.AnnotationSnapshotIntegrityStatus] == string(snapshotIntegrityOk)) &&
			snapshot.Annotations[controllers.AnnotationPvcSnapshotReady] == strconv.FormatBool(true) &&
			snapshot.Annotations[controllers.AnnotationExportingTarball] == "":
			logger.Info("starting tarball export", "snapshot", snapshot.GetName())
			if err = r.exportTarball(ctx, chainNode, &snapshot); err != nil {
				return err
			}
			snapshot.ObjectMeta.Annotations[controllers.AnnotationExportingTarball] = strconv.FormatBool(true)
			if err = r.Update(ctx, &snapshot); err != nil {
				return err
			}
			r.recorder.Eventf(chainNode,
				corev1.EventTypeNormal,
				appsv1.ReasonTarballExportStart,
				"Exporting tarball %s from snapshot", getTarballName(chainNode, &snapshot),
			)

		// A completed upload is persisted before cleanup so a controller restart cannot trigger another upload.
		case chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() &&
			snapshot.Annotations[controllers.AnnotationPvcSnapshotReady] == strconv.FormatBool(true) &&
			snapshot.Annotations[controllers.AnnotationExportingTarball] == strconv.FormatBool(true):
			ready, err := r.isTarballReady(ctx, chainNode, &snapshot)
			if err != nil {
				// A recorded failure already wrote status; stop before another snapshot in this same pass
				// persists its ready annotation and then loses the ChainNode update to a conflict.
				if stderrors.Is(err, errYieldReconcile) {
					return nil
				}
				r.recorder.Eventf(chainNode,
					corev1.EventTypeWarning,
					appsv1.ReasonTarballExportError,
					"Error on tarball export %v", err,
				)
				return err
			}
			if ready {
				logger.Info("finished tarball export", "snapshot", snapshot.GetName())
				if err = r.finishTarballExport(ctx, chainNode, &snapshot); err != nil {
					return err
				}
			}

		case chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() &&
			snapshot.Annotations[controllers.AnnotationPvcSnapshotReady] == strconv.FormatBool(true) &&
			snapshot.Annotations[controllers.AnnotationExportingTarball] == tarballUploaded:
			if err = r.finishTarballExport(ctx, chainNode, &snapshot); err != nil {
				return err
			}

		// Default case is checking if snapshot has expired (time-based retention).
		// If tarball is also set for deletion on expire it is also taken care here.
		default:
			if chainNode.Spec.Persistence.Snapshots.ShouldPreserveLastSnapshot() && len(snapshots) == 1 {
				logger.Info("skipping retention check to preserve last snapshot", "snapshot", snapshot.GetName(), "retention", snapshot.Annotations[controllers.AnnotationSnapshotRetention])
			} else {
				expired, err := isSnapshotExpired(&snapshot)
				if err != nil {
					return err
				}
				if expired {
					deleteTarball := chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() && chainNode.Spec.Persistence.Snapshots.ExportTarball.DeleteWhenExpired()
					if deleteTarball {
						deleted, deleteErr := r.isTarballDeleted(ctx, chainNode, &snapshot)
						if deleteErr != nil {
							return deleteErr
						}
						if !deleted {
							continue
						}
					}
					logger.Info("deleting expired pvc snapshot", "snapshot", snapshot.GetName(), "retention", snapshot.Annotations[controllers.AnnotationSnapshotRetention])
					if err = r.Delete(ctx, &snapshot); err != nil {
						return err
					}
					r.recorder.Eventf(chainNode,
						corev1.EventTypeNormal,
						appsv1.ReasonDeletedSnapshot,
						"Deleted expired PVC snapshot %s", snapshot.GetName(),
					)
					if deleteTarball {
						forgotten := r.cleanUpTarballDeletion(ctx, chainNode, &snapshot)
						r.recorder.Eventf(chainNode,
							corev1.EventTypeNormal,
							appsv1.ReasonTarballDeleted,
							"Deleted expired tarball %s", getTarballName(chainNode, &snapshot),
						)
						if forgotten {
							// That status write moved the object on at the API server. Continuing would let
							// startNewSnapshot below create a VolumeSnapshot whose in-progress annotation then
							// fails to persist, so later passes would keep creating more.
							return nil
						}
					}
				}
			}
		}
	}

	// Handle count-based retention (retain field). Re-list snapshots since some may have been deleted above.
	if retainCount := chainNode.Spec.Persistence.Snapshots.GetRetainCount(); retainCount != nil {
		snapshots, err = r.listNodeSnapshots(ctx, chainNode)
		if err != nil {
			return err
		}
		// Sort snapshots by creation time, newest last
		sort.Slice(snapshots, func(i, j int) bool {
			return snapshots[i].CreationTimestamp.Before(&snapshots[j].CreationTimestamp)
		})

		// Calculate how many snapshots to delete
		toDelete := len(snapshots) - int(*retainCount)
		if chainNode.Spec.Persistence.Snapshots.ShouldPreserveLastSnapshot() && toDelete >= len(snapshots) {
			// Ensure at least one snapshot is preserved
			toDelete = len(snapshots) - 1
		}

		// Delete oldest snapshots (from the beginning of sorted slice)
		for i := 0; i < toDelete; i++ {
			snapshot := snapshots[i]
			deleteTarball := chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() && chainNode.Spec.Persistence.Snapshots.ExportTarball.DeleteWhenExpired()
			if deleteTarball {
				deleted, deleteErr := r.isTarballDeleted(ctx, chainNode, &snapshot)
				if deleteErr != nil {
					return deleteErr
				}
				if !deleted {
					break
				}
			}
			logger.Info("deleting pvc snapshot due to retain count", "snapshot", snapshot.GetName(), "retain", *retainCount)
			if err = r.Delete(ctx, &snapshot); err != nil {
				return err
			}
			r.recorder.Eventf(chainNode,
				corev1.EventTypeNormal,
				appsv1.ReasonDeletedSnapshot,
				"Deleted PVC snapshot %s (exceeded retain count of %d)", snapshot.GetName(), *retainCount,
			)
			if deleteTarball {
				forgotten := r.cleanUpTarballDeletion(ctx, chainNode, &snapshot)
				r.recorder.Eventf(chainNode,
					corev1.EventTypeNormal,
					appsv1.ReasonTarballDeleted,
					"Deleted tarball %s (exceeded retain count)", getTarballName(chainNode, &snapshot),
				)
				if forgotten {
					// See above: yield rather than reach startNewSnapshot with a stale resource version.
					return nil
				}
			}
		}
	}

	// Remove any dangling jobs whose volumesnapshot does not exist anymore
	if chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() {
		exporter, err := r.getTarballExportProvider(chainNode)
		if err != nil {
			return err
		}
		tarballSnapshots, err := exporter.ListSnapshots(ctx)
		if err != nil {
			return err
		}
		for _, snapshot := range tarballSnapshots {
			if !utils.SliceContains[string](tarballNames, snapshot) {
				logger.Info("reconciling orphaned tarball deletion as volumesnapshot does not exist anymore", "snapshot", snapshot)
				status, deleteErr := exporter.DeleteSnapshot(ctx, snapshot)
				if deleteErr != nil {
					return deleteErr
				}
				switch status {
				case datasnapshot.SnapshotFailed:
					r.recorder.Eventf(chainNode,
						corev1.EventTypeWarning,
						appsv1.ReasonTarballDeleteError,
						"Failed deleting orphaned tarball %s; delete Job retained for inspection", snapshot,
					)
				case datasnapshot.SnapshotSucceeded:
					if err = exporter.CleanupSnapshotDeletion(ctx, snapshot); err != nil {
						return err
					}
					r.recorder.Eventf(chainNode,
						corev1.EventTypeNormal,
						appsv1.ReasonTarballDeleted,
						"Deleted orphaned tarball %s", snapshot,
					)
				}
			}
		}
	}

	// We don't want to have more than one snapshot being taken at the same time
	if volumeSnapshotInProgress(chainNode) {
		return nil
	}

	// Create a snapshot if it's time for that
	if shouldSnapshot(chainNode, nodePodReady) {
		logger.Info("creating new pvc snapshot")
		return r.startNewSnapshot(ctx, chainNode)
	}

	return nil
}

func (r *Reconciler) listNodeSnapshots(ctx context.Context, chainNode *appsv1.ChainNode) ([]snapshotv1.VolumeSnapshot, error) {
	listOption := client.MatchingLabels{controllers.LabelChainNode: chainNode.GetName()}
	list := &snapshotv1.VolumeSnapshotList{}
	if err := r.List(ctx, list, listOption); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *Reconciler) startNewSnapshot(ctx context.Context, chainNode *appsv1.ChainNode) error {
	snapshot, err := r.createSnapshot(ctx, chainNode)
	if err != nil {
		return err
	}

	// If snapshot is nil then it was not started
	if snapshot == nil {
		return nil
	}

	r.recorder.Eventf(chainNode,
		corev1.EventTypeNormal,
		appsv1.ReasonStartedSnapshot,
		"Started PVC snapshot %s", snapshot.GetName(),
	)

	setSnapshotInProgress(chainNode, true)
	if err := r.Update(ctx, chainNode); err != nil {
		return err
	}
	return r.updatePhase(ctx, chainNode, appsv1.PhaseChainNodeSnapshotting)
}

func (r *Reconciler) createSnapshot(ctx context.Context, chainNode *appsv1.ChainNode) (*snapshotv1.VolumeSnapshot, error) {
	logger := log.FromContext(ctx)

	if chainNode.Spec.Persistence.Snapshots.ShouldStopNode() {
		pod, err := r.getPodSpec(ctx, chainNode, "")
		if err != nil {
			return nil, err
		}

		ph := k8s.NewPodHelper(r.ClientSet, r.RestConfig, pod)
		if err := ph.Delete(ctx); err != nil {
			if !errors.IsNotFound(err) {
				return nil, err
			}
		} else {
			if err := ph.WaitForPodDeleted(ctx, timeoutPodDeleted); err != nil {
				return nil, err
			}
		}
	}
	if err := r.updateLatestHeight(ctx, chainNode); err != nil {
		// When this error happens, the most likely scenario is that pod is not running. So lets not throw the error and
		// let the rest of the reconcile loop handle the missing pod.
		logger.Error(err, "error getting latest height (pod is probably missing)")
		return nil, nil
	}
	snapshot := getVolumeSnapshotSpec(chainNode)
	return snapshot, r.Create(ctx, snapshot)
}

func shouldSnapshot(chainNode *appsv1.ChainNode, nodePodReady bool) bool {
	switch {
	case chainNode.Spec.Persistence.Snapshots.ShouldDisableWhileSyncing() && chainNode.Status.Phase == appsv1.PhaseChainNodeSyncing:
		return false
	case chainNode.Spec.Persistence.Snapshots.ShouldDisableWhileUnhealthy() && !nodePodReady:
		return false
	case chainNode.Spec.Persistence.Snapshots.ShouldDisableWhileUnhealthy() && chainNode.Status.Phase != appsv1.PhaseChainNodeRunning:
		return false
	}

	period, err := strfmt.ParseDuration(chainNode.Spec.Persistence.Snapshots.Frequency)
	if err != nil {
		return false
	}
	lastSnapshotTime := getLastSnapshotTime(chainNode)
	if lastSnapshotTime.IsZero() {
		return chainNode.CreationTimestamp.UTC().Add(minimumTimeBeforeFirstSnapshot).Before(time.Now().UTC())
	}
	return lastSnapshotTime.Add(period).Before(time.Now().UTC())
}

func isSnapshotReady(snapshot *snapshotv1.VolumeSnapshot) bool {
	return snapshot != nil && snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse
}

func isSnapshotExpired(snapshot *snapshotv1.VolumeSnapshot) (bool, error) {
	retention, ok := snapshot.Annotations[controllers.AnnotationSnapshotRetention]
	if !ok {
		return false, nil
	}

	expiration, err := strfmt.ParseDuration(retention)
	if err != nil {
		return false, err
	}

	return snapshot.CreationTimestamp.UTC().Add(expiration).Before(time.Now().UTC()), nil
}

func getVolumeSnapshotSpec(chainNode *appsv1.ChainNode) *snapshotv1.VolumeSnapshot {
	spec := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getSnapshotName(chainNode),
			Namespace: chainNode.GetNamespace(),
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotReady: strconv.FormatBool(false),
				controllers.AnnotationDataHeight:       strconv.FormatInt(chainNode.Status.LatestHeight, 10),
			},
			Labels: WithChainNodeLabels(chainNode, map[string]string{
				controllers.LabelChainNode: chainNode.GetName(),
			}),
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: ptr.To(chainNode.GetName()),
			},
			VolumeSnapshotClassName: chainNode.Spec.Persistence.Snapshots.SnapshotClassName,
		},
	}

	if chainNode.Spec.Persistence.Snapshots.Retention != nil {
		spec.ObjectMeta.Annotations[controllers.AnnotationSnapshotRetention] = *chainNode.Spec.Persistence.Snapshots.Retention
	}

	return spec
}

func volumeSnapshotInProgress(chainNode *appsv1.ChainNode) bool {
	if chainNode.ObjectMeta.Annotations == nil {
		return false
	}
	v, ok := chainNode.ObjectMeta.Annotations[controllers.AnnotationPvcSnapshotInProgress]
	if !ok {
		return false
	}
	return v == strconv.FormatBool(true)
}

func setSnapshotInProgress(chainNode *appsv1.ChainNode, snapshotting bool) {
	if chainNode.ObjectMeta.Annotations == nil {
		chainNode.ObjectMeta.Annotations = make(map[string]string)
	}
	chainNode.ObjectMeta.Annotations[controllers.AnnotationPvcSnapshotInProgress] = strconv.FormatBool(snapshotting)
	if snapshotting {
		chainNode.Status.Phase = appsv1.PhaseChainNodeSnapshotting
	} else {
		chainNode.Status.Phase = appsv1.PhaseChainNodeRunning
	}
}

func setSnapshotTime(chainNode *appsv1.ChainNode, ts time.Time) {
	if chainNode.ObjectMeta.Annotations == nil {
		chainNode.ObjectMeta.Annotations = make(map[string]string)
	}
	chainNode.ObjectMeta.Annotations[controllers.AnnotationLastPvcSnapshot] = ts.UTC().Format(timeLayout)
}

func getLastSnapshotTime(chainNode *appsv1.ChainNode) time.Time {
	if s, ok := chainNode.ObjectMeta.Annotations[controllers.AnnotationLastPvcSnapshot]; ok {
		if ts, err := time.Parse(timeLayout, s); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

func getSnapshotName(chainNode *appsv1.ChainNode) string {
	name := chainNode.GetName()

	// When taking snapshots from a chainnode that belongs to chainnodeset group, we will only snapshot
	// from one of the group nodes, so we give it the group name instead.
	if group, ok := chainNode.Labels[controllers.LabelChainNodeSetGroup]; ok && group != "" {
		if nodeset, ok := chainNode.Labels[controllers.LabelChainNodeSet]; ok && nodeset != "" {
			name = fmt.Sprintf("%s-%s", nodeset, group)
		}

	}
	return fmt.Sprintf("%s-%s", name, time.Now().UTC().Format(timeLayout))
}

func (r *Reconciler) getTarballExportProvider(chainNode *appsv1.ChainNode) (datasnapshot.SnapshotProvider, error) {
	return r.tarballProviderFor(chainNode, chainNode.Spec.Persistence.Snapshots.ExportTarball)
}

func (r *Reconciler) tarballProviderFor(chainNode *appsv1.ChainNode, cfg *appsv1.ExportTarballConfig) (datasnapshot.SnapshotProvider, error) {
	clientSet := kubernetes.Interface(r.ClientSet)
	if r.snapshotClientSet != nil {
		clientSet = r.snapshotClientSet
	}
	switch {
	case cfg.GCS != nil:
		return datasnapshot.NewGcsSnapshotProvider(
			clientSet,
			r.Scheme,
			chainNode,
			r.opts.GetDefaultPriorityClassName(),
			cfg,
		), nil

	case cfg.S3 != nil:
		return datasnapshot.NewS3SnapshotProvider(
			clientSet,
			r.Scheme,
			chainNode,
			r.opts.GetDefaultPriorityClassName(),
			cfg,
		), nil

	default:
		return nil, fmt.Errorf("no upload target defined")
	}
}

// Tarball export destinations live in ChainNode.status.exportedTarballs, which only the controller
// writes. They were previously kept in an annotation on the VolumeSnapshot, but that object is writable
// by anyone with access to it: a forged record could point a deletion Job — which runs with the
// operator's cloud credentials — at a bucket, endpoint and object name of the attacker's choosing.
// Status is the operator-controlled state, so a recorded target is always one this controller wrote.

// recordedTarball returns the destination recorded for a snapshot, if any.
// recordedTarball returns the live destination for a snapshot: the one its current tarball was written
// to. Superseded entries are skipped — they describe stores a failed attempt wrote to and exist only so
// that leftover data stays cleanable.
func recordedTarball(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) *appsv1.ExportedTarball {
	for i := range chainNode.Status.ExportedTarballs {
		entry := &chainNode.Status.ExportedTarballs[i]
		if recordMatches(entry, snapshot) && !entry.Superseded {
			return entry
		}
	}
	return nil
}

// recordMatches reports whether a record belongs to this exact VolumeSnapshot. A snapshot deleted and
// recreated under the same name is a different object with its own tarball, so matching on name alone
// would let the new snapshot inherit the old record — and have its tarball orphaned when expiry deletes
// the object the old record names. Records written before UIDs were captured match on name only.
func recordMatches(entry *appsv1.ExportedTarball, snapshot *snapshotv1.VolumeSnapshot) bool {
	if entry.Snapshot != snapshot.GetName() {
		return false
	}
	if entry.SnapshotUID == "" || snapshot.GetUID() == "" {
		return true
	}
	return entry.SnapshotUID == snapshot.GetUID()
}

// deletedTarballName returns the object name to delete: the one recorded at upload time when present,
// falling back to the name derived from the current spec for snapshots exported before this was
// recorded. The recorded name embeds the suffix in force at upload, so editing exportTarball.suffix
// afterwards no longer makes deletion ask for an object that was never written.
func deletedTarballName(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) string {
	if recorded := recordedTarball(chainNode, snapshot); recorded != nil && recorded.Name != "" {
		return recorded.Name
	}
	return getTarballName(chainNode, snapshot)
}

// exportDestination describes where the configured export target currently points, in the same shape
// as a recorded entry so the two can be compared directly. Snapshot is left blank; the caller fills it.
func exportDestination(cfg *appsv1.ExportTarballConfig) (appsv1.ExportedTarball, bool) {
	switch {
	case cfg.GCS != nil:
		return appsv1.ExportedTarball{Provider: "gcs", Bucket: cfg.GCS.Bucket}, true
	case cfg.S3 != nil:
		return appsv1.ExportedTarball{
			Provider: "s3",
			Bucket:   cfg.S3.Bucket,
			Endpoint: ptr.Deref(cfg.S3.Endpoint, ""),
			Region:   cfg.S3.Region,
			// Resolve the effective value rather than storing the raw pointer, so a spec that spells out
			// the current default does not read as a change.
			ForcePathStyle: ptr.To(cfg.S3.ShouldForcePathStyle()),
		}, true
	default:
		return appsv1.ExportedTarball{}, false
	}
}

// sameLocation reports whether two records point at the same object store, including the routing
// settings needed to reach it. forcePathStyle and region matter: a MinIO-style endpoint reached with
// path-style URLs is typically unreachable with virtual-host addressing, so a change there means the
// delete Job must keep using the upload-time settings rather than the current ones.
func sameLocation(a, b appsv1.ExportedTarball) bool {
	return a.Provider == b.Provider &&
		a.Bucket == b.Bucket &&
		a.Endpoint == b.Endpoint &&
		a.Region == b.Region &&
		ptr.Deref(a.ForcePathStyle, false) == ptr.Deref(b.ForcePathStyle, false)
}

// tarballDeleteProvider resolves the exporter used to delete a tarball, preferring the destination
// recorded at upload time over the current spec. Snapshots exported before this was recorded have no
// entry, so they fall back to the configured provider — the previous behaviour.
//
// The returned bool reports whether the recorded destination differs from the configured one, so the
// caller can warn rather than silently treat a cross-store miss as a successful deletion.
func (r *Reconciler) tarballDeleteProvider(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) (datasnapshot.SnapshotProvider, bool, error) {
	configured := chainNode.Spec.Persistence.Snapshots.ExportTarball
	recorded := recordedTarball(chainNode, snapshot)
	if recorded == nil {
		provider, err := r.tarballProviderFor(chainNode, configured)
		return provider, false, err
	}

	current, ok := exportDestination(configured)
	if ok && sameLocation(current, *recorded) {
		exporter, err := r.tarballProviderFor(chainNode, configured)
		return exporter, false, err
	}

	// The tarball lives somewhere the spec no longer points at. Rebuild a config aimed at the recorded
	// store, keeping the credentials of the matching provider block in the spec.
	cfg, err := configForDestination(configured, recorded)
	if err != nil {
		return nil, true, err
	}
	exporter, err := r.tarballProviderFor(chainNode, cfg)
	return exporter, true, err
}

// configForDestination rebuilds an export config aimed at a recorded destination. Bucket and endpoint
// come from the controller-written status record; credentials come from the ChainNode spec, which is
// the only place they exist.
//
// Deleting from a store the spec no longer describes therefore works only while the spec still carries
// usable credentials for that provider. Otherwise this errors: the delete Job fails loudly and the
// caller reports a possible orphan, rather than running credential-less and reporting success.
func configForDestination(configured *appsv1.ExportTarballConfig, recorded *appsv1.ExportedTarball) (*appsv1.ExportTarballConfig, error) {
	// Credentials only exist in the spec, so without it there is no way to reach the store — and
	// DeepCopy on a nil receiver would return nil and panic on the next field write.
	if configured == nil {
		return nil, fmt.Errorf("tarball %q was uploaded to %s bucket %q but .spec.persistence.snapshots.exportTarball is no longer set, so it cannot be reached",
			recorded.Name, recorded.Provider, recorded.Bucket)
	}
	cfg := configured.DeepCopy()
	cfg.GCS = nil
	cfg.S3 = nil
	switch recorded.Provider {
	case "gcs":
		if configured.GCS == nil {
			return nil, fmt.Errorf("tarball was uploaded to GCS bucket %q but .spec has no gcs configuration to authenticate with", recorded.Bucket)
		}
		cfg.GCS = configured.GCS.DeepCopy()
		cfg.GCS.Bucket = recorded.Bucket
	case "s3":
		if configured.S3 == nil {
			return nil, fmt.Errorf("tarball was uploaded to S3 bucket %q but .spec has no s3 configuration to authenticate with", recorded.Bucket)
		}
		cfg.S3 = configured.S3.DeepCopy()
		cfg.S3.Bucket = recorded.Bucket
		if recorded.Endpoint == "" {
			cfg.S3.Endpoint = nil
		} else {
			cfg.S3.Endpoint = ptr.To(recorded.Endpoint)
		}
		// Routing must match the upload: a store reached with path-style URLs is usually unreachable
		// with virtual-host addressing, and the signing region is part of reaching it.
		if recorded.Region != "" {
			cfg.S3.Region = recorded.Region
		}
		if recorded.ForcePathStyle != nil {
			cfg.S3.ForcePathStyle = recorded.ForcePathStyle
		}
	default:
		return nil, fmt.Errorf("unknown recorded tarball provider %q", recorded.Provider)
	}
	return cfg, nil
}

func (r *Reconciler) exportTarball(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) error {
	exporter, err := r.getTarballExportProvider(chainNode)
	if err != nil {
		return err
	}
	// Record the destination against the config that creates this Job, before it runs. Recording at
	// completion instead would attribute the upload to whatever the spec says by then: if the provider
	// changes mid-upload, status is looked up by the shared Job name, so the new provider can observe the
	// old provider's Job succeeding and the tarball would be recorded at a store it was never written to.
	if err = r.recordTarballDestination(ctx, chainNode, snapshot); err != nil {
		return err
	}
	if err = exporter.CreateSnapshot(ctx, getTarballName(chainNode, snapshot), snapshot); err != nil {
		// No upload owns the record now: CreateSnapshot rolls back the resources it created. Leaving it
		// live would make it win over a retry aimed at a different store (records are immutable), and
		// deletion would then target the wrong place. Supersede rather than drop it, since a failure
		// partway through creation can still have left objects behind.
		if supersedeErr := r.supersedeFailedTarballRecord(ctx, chainNode, snapshot); supersedeErr != nil {
			return fmt.Errorf("%w; supersede tarball destination record: %v", err, supersedeErr)
		}
		return err
	}
	return nil
}

// recordTarballDestination stamps where this upload is being written, into controller-owned status, so
// deletion can target that store rather than whatever exportTarball currently points at.
func (r *Reconciler) recordTarballDestination(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) error {
	entry, ok := exportDestination(chainNode.Spec.Persistence.Snapshots.ExportTarball)
	if !ok {
		return nil
	}
	entry.Snapshot = snapshot.GetName()
	entry.SnapshotUID = snapshot.GetUID()
	// Pin the object name too: it embeds exportTarball.suffix, so editing the suffix later would make
	// deletion ask for a name that was never written and leave the real tarball behind.
	entry.Name = getTarballName(chainNode, snapshot)

	// The first record wins: the original upload may already have succeeded against the old store, and
	// repointing the record before that is known would aim deletion at the wrong place.
	//
	// Unless no upload Job ever existed for it. The controller can exit between committing this record
	// and creating the Job, and that record then has no owning upload — pinning a later retry to a
	// destination nothing was ever written to. Supersede it instead, so the retry records its own while
	// the stale entry stays cleanable.
	if existing := recordedTarball(chainNode, snapshot); existing != nil {
		if r.uploadJobExists(ctx, chainNode, existing.Name) {
			return nil
		}
		// Supersede, then fall through to record the replacement in the same pass. The caller creates the
		// upload Job immediately after this returns, and the exporting annotation stops this function
		// running again — so leaving without a live record would let a later bucket or endpoint change
		// send deletion to the wrong store and orphan whatever this upload writes.
		if err := r.supersedeFailedTarballRecord(ctx, chainNode, snapshot); err != nil {
			return err
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &appsv1.ChainNode{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(chainNode), fresh); err != nil {
			return err
		}
		for i := range fresh.Status.ExportedTarballs {
			if recordMatches(&fresh.Status.ExportedTarballs[i], snapshot) && !fresh.Status.ExportedTarballs[i].Superseded {
				// Another reconcile recorded it first; leave that record alone.
				return nil
			}
		}
		fresh.Status.ExportedTarballs = append(fresh.Status.ExportedTarballs, entry)
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}
		// Copy status only. Attaching fresh.ResourceVersion to the caller's copy would let a later
		// r.Update() of a stale .spec succeed instead of conflicting, silently reverting a concurrent
		// spec edit.
		chainNode.Status.ExportedTarballs = fresh.Status.ExportedTarballs
		return nil
	})
}

// reconcileOrphanedTarballs handles records whose VolumeSnapshot is gone — expired without
// deleteOnExpire, or deleted by hand. The record is the only remaining pointer to the object, and the
// dangling-Job sweep below cannot help: it lists Jobs through the *currently configured* exporter, so
// after a provider change it never sees a tarball left in the old store.
//
// When deletion is configured, each orphan is deleted through its own recorded destination and the
// record is dropped only once that succeeds. Otherwise the tarball is meant to outlive its snapshot,
// so the record is retired immediately — keeping it would grow status until it hit the object size
// limit and blocked every later status update and export.
// Reports whether the caller should yield this reconcile pass: either status was mutated (so this
// object's resource version is behind the API server's) or a superseded deletion is still running and
// its retry must not start yet.
func (r *Reconciler) reconcileOrphanedTarballs(ctx context.Context, chainNode *appsv1.ChainNode, snapshots []snapshotv1.VolumeSnapshot) (bool, error) {
	if len(chainNode.Status.ExportedTarballs) == 0 {
		return false, nil
	}
	logger := log.FromContext(ctx)
	// Keyed by identity, not just name: a snapshot recreated under the same name is a different object,
	// so the old record describes a tarball nothing points at any more.
	liveSnapshots := make([]*snapshotv1.VolumeSnapshot, 0, len(snapshots))
	for i := range snapshots {
		liveSnapshots = append(liveSnapshots, &snapshots[i])
	}
	stillLive := func(entry *appsv1.ExportedTarball) bool {
		for _, snapshot := range liveSnapshots {
			if recordMatches(entry, snapshot) {
				return true
			}
		}
		return false
	}
	// This runs before the snapshot prerequisites, so persistence/snapshots may be gone entirely — the
	// records outlive the spec that created them. Nil-check rather than dereference.
	deleteTarballs := chainNode.Spec.Persistence != nil &&
		chainNode.Spec.Persistence.Snapshots.ShouldExportTarballs() &&
		chainNode.Spec.Persistence.Snapshots.ExportTarball.DeleteWhenExpired()

	retire := make([]appsv1.ExportedTarball, 0)
	// Snapshots whose superseded deletion is still in flight; their retry must wait.
	deleting := make(map[string]struct{})
	for _, entry := range chainNode.Status.ExportedTarballs {
		// Two kinds of leftover: a record whose snapshot is gone, and a superseded record whose snapshot
		// is alive but whose tarball now lives elsewhere. Both point at data nothing else can find.
		if stillLive(&entry) && !entry.Superseded {
			continue
		}
		// A superseded entry describes a failed attempt, which can still have left real data behind — the
		// uploaders write parts before a concurrent chunk failure is noticed. Nothing else points at those
		// objects, so they are always cleaned up, whatever the retention setting says about live tarballs.
		if !deleteTarballs && !entry.Superseded {
			retire = append(retire, entry)
			continue
		}
		done, jobExists, err := r.deleteOrphanedTarball(ctx, chainNode, entry)
		if err != nil {
			// Keep the record: it is the only pointer left to the object. Hold the retry only when a
			// delete Job actually exists — that Job's cleanup would remove the retry's "<name>-upload".
			// A config error leaves nothing to clobber, and gating on it would freeze snapshots forever.
			if jobExists {
				deleting[entry.Snapshot] = struct{}{}
			}
			logger.Error(err, "failed deleting orphaned tarball", "snapshot", entry.Snapshot, "tarball", entry.Name)
			r.recorder.Eventf(chainNode,
				corev1.EventTypeWarning,
				appsv1.ReasonTarballDeleteError,
				"Failed deleting orphaned tarball %s from %s bucket %q: %v", entry.Name, entry.Provider, entry.Bucket, err,
			)
			continue
		}
		if done {
			retire = append(retire, entry)
			continue
		}
		if jobExists {
			// Delete Job still running. It shares the tarball name with the retry, and DeleteSnapshot's own
			// cleanup removes "<name>-upload" — which would be the retry's Job. Hold the retry until this
			// finishes rather than letting it be torn down mid-flight and recorded as a failure.
			deleting[entry.Snapshot] = struct{}{}
		}
	}
	if len(retire) == 0 {
		return len(deleting) > 0, nil
	}
	return true, r.retireTarballRecords(ctx, chainNode, retire)
}

// deleteOrphanedTarball runs a delete Job against the recorded destination, reporting whether the
// object is gone. Cleanup of the Job happens once it succeeds.
// deleteOrphanedTarball runs a delete Job against the recorded destination.
//
// It reports (done, jobExists, err). jobExists is what gates the retry: it is true only once a delete
// Job is actually present, because that Job shares the tarball name and its provider cleanup removes
// "<name>-upload" — the retry's own Job. Errors raised before any Job exists (a removed export config,
// unreachable credentials) must NOT gate anything: nothing can clobber the retry, and blocking on them
// would let one unreachable record freeze snapshots for the node indefinitely.
func (r *Reconciler) deleteOrphanedTarball(ctx context.Context, chainNode *appsv1.ChainNode, entry appsv1.ExportedTarball) (done, jobExists bool, err error) {
	var configured *appsv1.ExportTarballConfig
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.Snapshots != nil {
		configured = chainNode.Spec.Persistence.Snapshots.ExportTarball
	}
	cfg, err := configForDestination(configured, &entry)
	if err != nil {
		return false, false, err
	}
	exporter, err := r.tarballProviderFor(chainNode, cfg)
	if err != nil {
		return false, false, err
	}
	status, err := exporter.DeleteSnapshot(ctx, entry.Name)
	if err != nil {
		// DeleteSnapshot creates the Job, so a failure here may or may not have left one behind. Check
		// rather than assume: guessing either way is a freeze or a clobbered retry.
		return false, r.deleteJobExists(ctx, chainNode, entry), err
	}
	switch status {
	case datasnapshot.SnapshotSucceeded:
		if err = exporter.CleanupSnapshotDeletion(ctx, entry.Name); err != nil {
			return false, true, err
		}
		r.recorder.Eventf(chainNode,
			corev1.EventTypeNormal,
			appsv1.ReasonTarballDeleted,
			"Deleted orphaned tarball %s from %s bucket %q", entry.Name, entry.Provider, entry.Bucket,
		)
		return true, false, nil
	case datasnapshot.SnapshotFailed:
		// Retained for inspection, and re-polled every pass — each poll runs the cleanup that would
		// remove the retry's upload Job.
		return false, true, fmt.Errorf("delete job for tarball %s failed", entry.Name)
	default:
		// Still running; try again on the next reconcile.
		return false, true, nil
	}
}

// deleteJobExists reports whether a delete Job for this tarball, owned by this node, is present.
func (r *Reconciler) deleteJobExists(ctx context.Context, chainNode *appsv1.ChainNode, entry appsv1.ExportedTarball) bool {
	return r.ownsSnapshotJob(ctx, chainNode, fmt.Sprintf("%s-delete", entry.Name))
}

// uploadJobExists reports whether an upload Job for this tarball name, owned by this node, is present.
func (r *Reconciler) uploadJobExists(ctx context.Context, chainNode *appsv1.ChainNode, name string) bool {
	return r.ownsSnapshotJob(ctx, chainNode, fmt.Sprintf("%s-upload", name))
}

// ownsSnapshotJob reports whether a Job of this name exists AND is controlled by this ChainNode.
//
// Tarball names derive from chain ID and a second-resolution timestamp, so two ChainNodes in one
// namespace can collide. A foreign Job must not count: ensureSnapshotJob rejects it anyway, so treating
// it as ours would gate this node's retries on a Job it can neither use nor clean up — and a foreign
// failed Job, retained for inspection, would block this node's snapshots indefinitely.
func (r *Reconciler) ownsSnapshotJob(ctx context.Context, chainNode *appsv1.ChainNode, name string) bool {
	clientSet := r.snapshotClientSet
	if clientSet == nil {
		if r.ClientSet == nil {
			return false
		}
		clientSet = r.ClientSet
	}
	job, err := clientSet.BatchV1().Jobs(chainNode.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return metav1.IsControlledBy(job, chainNode)
}

// supersedeFailedTarballRecord marks the destination of a conclusively failed upload as superseded when
// the export will be retried.
//
// Neither keeping nor dropping the record outright is safe. Keeping it live would point deletion at the
// old store after a retry to a different target, orphaning the retry's tarball. Dropping it would
// discard the only pointer to whatever the failed attempt left in the old store — a partial multipart
// upload, or an object that landed just as the Job was declared failed. Marking it superseded keeps the
// old destination cleanable while freeing the retry to record its own.
//
// This applies to an exhausted retry limit too. tarballFailureAction tells the operator to clear the
// export annotations and retry by hand, and they may change bucket, endpoint, provider or suffix first;
// leaving the old record live would make it win over that manual retry (records are first-wins) and
// silently orphan whatever the retry uploads. Superseding loses nothing: the entry is still cleaned up,
// just no longer treated as this snapshot's live tarball.
func (r *Reconciler) supersedeFailedTarballRecord(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) error {
	if recordedTarball(chainNode, snapshot) == nil {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &appsv1.ChainNode{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(chainNode), fresh); err != nil {
			return err
		}
		changed := false
		for i := range fresh.Status.ExportedTarballs {
			entry := &fresh.Status.ExportedTarballs[i]
			if recordMatches(entry, snapshot) && !entry.Superseded {
				entry.Superseded = true
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}
		// Status only; see recordTarballDestination for why the resource version is not copied.
		chainNode.Status.ExportedTarballs = fresh.Status.ExportedTarballs
		return nil
	})
}

// recordKey identifies a single destination record: which snapshot it belongs to, and which object at
// which store it points at.
type recordKey struct {
	snapshot   string
	uid        types.UID
	name       string
	provider   string
	bucket     string
	endpoint   string
	superseded bool
}

func keyForRecord(entry appsv1.ExportedTarball) recordKey {
	return recordKey{
		snapshot:   entry.Snapshot,
		uid:        entry.SnapshotUID,
		name:       entry.Name,
		provider:   entry.Provider,
		bucket:     entry.Bucket,
		endpoint:   entry.Endpoint,
		superseded: entry.Superseded,
	}
}

// retireTarballRecords drops the given records from status. Entries are matched on snapshot name *and*
// superseded flag, so retiring a superseded leftover never removes the live record for the same
// snapshot (or the other way round).
func (r *Reconciler) retireTarballRecords(ctx context.Context, chainNode *appsv1.ChainNode, entries []appsv1.ExportedTarball) error {
	if len(entries) == 0 {
		return nil
	}
	// Keyed by the object a record points at, not just by snapshot: several superseded records can exist
	// for one snapshot (one per failed attempt at a different store), and retiring one must not discard
	// the others — their objects would then be untracked and unreachable by cleanup.
	drop := make(map[recordKey]struct{}, len(entries))
	for _, entry := range entries {
		drop[keyForRecord(entry)] = struct{}{}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &appsv1.ChainNode{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(chainNode), fresh); err != nil {
			return err
		}
		kept := make([]appsv1.ExportedTarball, 0, len(fresh.Status.ExportedTarballs))
		for _, entry := range fresh.Status.ExportedTarballs {
			if _, ok := drop[keyForRecord(entry)]; !ok {
				kept = append(kept, entry)
			}
		}
		if len(kept) == len(fresh.Status.ExportedTarballs) {
			return nil
		}
		fresh.Status.ExportedTarballs = kept
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}
		// Status only; see recordTarballDestination for why the resource version is not copied.
		chainNode.Status.ExportedTarballs = fresh.Status.ExportedTarballs
		return nil
	})
}

// forgetTarballDestination drops the record once the tarball is gone, so status does not grow without
// bound as snapshots are rotated.
func (r *Reconciler) forgetTarballDestination(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) (bool, error) {
	// Retire only this snapshot's live record. Filtering by name would also take the record of a
	// same-named predecessor whose tarball is still out there, and any superseded entry still waiting
	// for its own cleanup — orphaning both.
	live := recordedTarball(chainNode, snapshot)
	if live == nil {
		return false, nil
	}
	return true, r.retireTarballRecords(ctx, chainNode, []appsv1.ExportedTarball{*live})
}

func (r *Reconciler) isTarballReady(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) (bool, error) {
	exporter, err := r.getTarballExportProvider(chainNode)
	if err != nil {
		return false, err
	}

	status, err := exporter.GetSnapshotStatus(ctx, getTarballName(chainNode, snapshot))
	if err != nil {
		// The upload Job belonged to a previous exporter and was deleted. Clear the export state instead
		// of letting the next poll see the intentionally removed Job as SnapshotNotFound and charge it as
		// another failed attempt — that could exhaust the retry limit and permanently mark the export
		// failed without ever starting one for the newly configured provider.
		if stderrors.Is(err, datasnapshot.ErrStaleJobReplaced) {
			if resetErr := r.resetTarballExport(ctx, snapshot); resetErr != nil {
				return false, resetErr
			}
		}
		return false, err
	}

	switch status {
	case datasnapshot.SnapshotNotFound:
		if cleanupErr := r.cleanUpTarballExport(ctx, chainNode, snapshot); cleanupErr != nil {
			return false, fmt.Errorf("clean up missing tarball export job: %w", cleanupErr)
		}
		retry, updateErr := r.recordTarballExportFailure(ctx, snapshot)
		if updateErr != nil {
			return false, updateErr
		}
		if err = r.supersedeFailedTarballRecord(ctx, chainNode, snapshot); err != nil {
			return false, err
		}
		r.recorder.Eventf(chainNode,
			corev1.EventTypeWarning,
			appsv1.ReasonTarballExportError,
			"Tarball %s export job not found; %s", getTarballName(chainNode, snapshot), tarballFailureAction(retry),
		)
		return false, errYieldReconcile

	case datasnapshot.SnapshotFailed:
		if cleanupErr := r.cleanUpTarballExport(ctx, chainNode, snapshot); cleanupErr != nil {
			return false, fmt.Errorf("clean up failed tarball export job: %w", cleanupErr)
		}
		retry, updateErr := r.recordTarballExportFailure(ctx, snapshot)
		if updateErr != nil {
			return false, updateErr
		}
		if err = r.supersedeFailedTarballRecord(ctx, chainNode, snapshot); err != nil {
			return false, err
		}
		r.recorder.Eventf(chainNode,
			corev1.EventTypeWarning,
			appsv1.ReasonTarballExportError,
			"Tarball %s export failed; %s", getTarballName(chainNode, snapshot), tarballFailureAction(retry),
		)
		return false, errYieldReconcile

	case datasnapshot.SnapshotSucceeded:
		r.recorder.Eventf(chainNode,
			corev1.EventTypeNormal,
			appsv1.ReasonTarballExportFinish,
			"Finished exporting tarball %s", getTarballName(chainNode, snapshot),
		)
		return true, nil

	default:
		return false, nil
	}
}

func (r *Reconciler) recordTarballExportFailure(ctx context.Context, snapshot *snapshotv1.VolumeSnapshot) (bool, error) {
	if snapshot.Annotations == nil {
		snapshot.Annotations = make(map[string]string)
	}
	attempts, parseErr := strconv.Atoi(snapshot.Annotations[controllers.AnnotationTarballExportAttempts])
	if parseErr != nil || attempts < 0 {
		attempts = 0
	}
	attempts++
	snapshot.Annotations[controllers.AnnotationTarballExportAttempts] = strconv.Itoa(attempts)
	retry := attempts < tarballExportMaxAttempts
	if retry {
		delete(snapshot.Annotations, controllers.AnnotationExportingTarball)
	} else {
		snapshot.Annotations[controllers.AnnotationExportingTarball] = tarballFailed
	}
	return retry, r.Update(ctx, snapshot)
}

// resetTarballExport clears the export markers so the next reconcile starts a fresh upload against the
// currently configured provider, without the attempt counter carried over from the replaced Job.
func (r *Reconciler) resetTarballExport(ctx context.Context, snapshot *snapshotv1.VolumeSnapshot) error {
	if snapshot.Annotations == nil {
		return nil
	}
	_, exporting := snapshot.Annotations[controllers.AnnotationExportingTarball]
	_, attempts := snapshot.Annotations[controllers.AnnotationTarballExportAttempts]
	if !exporting && !attempts {
		return nil
	}
	delete(snapshot.Annotations, controllers.AnnotationExportingTarball)
	delete(snapshot.Annotations, controllers.AnnotationTarballExportAttempts)
	return r.Update(ctx, snapshot)
}

func tarballFailureAction(retry bool) string {
	if retry {
		return "retrying"
	}
	return "retry limit reached; remove the export annotations to retry"
}

func (r *Reconciler) finishTarballExport(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) error {
	if snapshot.Annotations[controllers.AnnotationExportingTarball] != tarballUploaded {
		snapshot.Annotations[controllers.AnnotationExportingTarball] = tarballUploaded
		delete(snapshot.Annotations, controllers.AnnotationTarballExportAttempts)
		// The destination was recorded when the upload started, against the config that created the Job.
		if err := r.Update(ctx, snapshot); err != nil {
			return err
		}
	}
	if err := r.cleanUpTarballExport(ctx, chainNode, snapshot); err != nil {
		return err
	}
	snapshot.Annotations[controllers.AnnotationExportingTarball] = tarballFinished
	return r.Update(ctx, snapshot)
}

func (r *Reconciler) cleanUpTarballExport(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) error {
	exporter, err := r.getTarballExportProvider(chainNode)
	if err != nil {
		return err
	}
	return exporter.CleanupSnapshot(ctx, getTarballName(chainNode, snapshot))
}

func (r *Reconciler) deleteTarball(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) (datasnapshot.SnapshotStatus, bool, error) {
	exporter, relocated, err := r.tarballDeleteProvider(chainNode, snapshot)
	if err != nil {
		return "", relocated, err
	}
	status, err := exporter.DeleteSnapshot(ctx, deletedTarballName(chainNode, snapshot))
	return status, relocated, err
}

func (r *Reconciler) isTarballDeleted(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) (bool, error) {
	status, relocated, err := r.deleteTarball(ctx, chainNode, snapshot)
	if err != nil {
		return false, err
	}
	switch status {
	case datasnapshot.SnapshotFailed:
		// A delete aimed at a store the spec no longer points at is the likeliest failure here: the
		// credentials or endpoint may be gone. Name the destination so the leak is actionable instead of
		// looking like a generic delete error.
		if relocated {
			r.recorder.Eventf(chainNode,
				corev1.EventTypeWarning,
				appsv1.ReasonTarballDeleteError,
				"Failed deleting tarball %s from its recorded destination %s; the object may be orphaned there",
				deletedTarballName(chainNode, snapshot),
				describeRecordedTarball(chainNode, snapshot),
			)
		} else {
			r.recorder.Eventf(chainNode,
				corev1.EventTypeWarning,
				appsv1.ReasonTarballDeleteError,
				"Failed deleting tarball %s; delete Job retained for inspection", deletedTarballName(chainNode, snapshot),
			)
		}
	case datasnapshot.SnapshotSucceeded:
		if relocated {
			r.recorder.Eventf(chainNode,
				corev1.EventTypeNormal,
				appsv1.ReasonTarballDeleted,
				"Deleted tarball %s from its recorded destination %s, which differs from the configured export target",
				deletedTarballName(chainNode, snapshot),
				describeRecordedTarball(chainNode, snapshot),
			)
		}
	}
	return status == datasnapshot.SnapshotSucceeded, nil
}

// Reports whether it mutated status, so the caller yields rather than continuing with an object whose
// resource version is behind the API server's.
func (r *Reconciler) cleanUpTarballDeletion(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) bool {
	// Resolve the same provider the delete Job was created with, so cleanup targets that Job rather than
	// one named for the currently configured exporter.
	exporter, _, err := r.tarballDeleteProvider(chainNode, snapshot)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get tarball exporter for delete Job cleanup", "snapshot", snapshot.GetName())
		return false
	}
	if err = exporter.CleanupSnapshotDeletion(ctx, deletedTarballName(chainNode, snapshot)); err != nil {
		log.FromContext(ctx).Error(err, "failed to clean up tarball delete Job", "snapshot", snapshot.GetName())
	}
	// The tarball is gone, so drop its record: status must not accumulate an entry per rotated snapshot.
	forgotten, err := r.forgetTarballDestination(ctx, chainNode, snapshot)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to forget tarball destination", "snapshot", snapshot.GetName())
	}
	return forgotten
}

// describeRecordedTarball renders the recorded destination for events, so an orphaned object is
// actionable without reading status by hand.
func describeRecordedTarball(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) string {
	recorded := recordedTarball(chainNode, snapshot)
	if recorded == nil {
		return "the configured export target"
	}
	if recorded.Endpoint != "" {
		return fmt.Sprintf("%s bucket %q at %s", recorded.Provider, recorded.Bucket, recorded.Endpoint)
	}
	return fmt.Sprintf("%s bucket %q", recorded.Provider, recorded.Bucket)
}

func getTarballName(chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) string {
	name := fmt.Sprintf("%s-%s", chainNode.Status.ChainID, snapshot.CreationTimestamp.UTC().Format(timeLayout))
	if chainNode.Spec.Persistence.Snapshots.ExportTarball.Suffix != nil {
		name += "-" + *chainNode.Spec.Persistence.Snapshots.ExportTarball.Suffix
	}
	return name
}

func (r *Reconciler) startSnapshotIntegrityCheck(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) error {
	if snapshot.Status.RestoreSize == nil {
		return fmt.Errorf("restore size is not available yet")
	}

	apiVersion := strings.Split(snapshot.APIVersion, "/")
	if len(apiVersion) == 0 {
		return fmt.Errorf("unsupported api version")
	}

	var sidecarRestartAlways = corev1.ContainerRestartPolicyAlways

	// Create job to verify data integrity
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-ichk", snapshot.GetName()),
			Namespace: chainNode.GetNamespace(),
			Labels: map[string]string{
				volumeSnapshot: snapshot.GetName(),
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To[int32](60),
			BackoffLimit:            ptr.To[int32](1),
			PodFailurePolicy: &batchv1.PodFailurePolicy{
				Rules: []batchv1.PodFailurePolicyRule{
					// 1) Count real checker failures (anything except 137/143)
					{
						Action: batchv1.PodFailurePolicyActionCount,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							ContainerName: ptr.To("start-checker"),
							Operator:      batchv1.PodFailurePolicyOnExitCodesOpNotIn,
							Values:        []int32{137, 143},
						},
					},
					// 2) Ignore kube-initiated disruptions (eviction/drain/preemption)
					{
						Action: batchv1.PodFailurePolicyActionIgnore,
						OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
							{
								Type:   corev1.DisruptionTarget,
								Status: corev1.ConditionTrue,
							},
						},
					},
					// 3) Ignore external kill paths (OOM/SIGTERM)
					{
						Action: batchv1.PodFailurePolicyActionIgnore,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:   []int32{137, 143},
						},
					},
				},
			},
			Completions: ptr.To[int32](1),
			Parallelism: ptr.To[int32](1),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:         corev1.RestartPolicyNever,
					PriorityClassName:     r.opts.GetDefaultPriorityClassName(),
					ShareProcessNamespace: ptr.To(true),
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: fmt.Sprintf("%s-ichk", snapshot.GetName()),
								},
							},
						},
						{
							Name: "config-empty-dir",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: chainNode.GetName(),
									},
								},
							},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:            "init-config",
							Image:           "busybox",
							Command:         []string{"sh"},
							SecurityContext: k8s.RestrictedSecurityContext(),
							Args: []string{
								"-c",
								fmt.Sprintf(
									"cp -rL /node-config/* /home/app/config/;"+
										"sed -i 's/iavl-lazy-loading =.*/iavl-lazy-loading = false/g' /home/app/config/app.toml;"+
										"echo '{\"chain_id\":%q}' > /home/app/config/genesis.json",
									chainNode.Status.ChainID,
								),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config-empty-dir",
									MountPath: "/home/app/config",
								},
								{
									Name:      "config",
									MountPath: "/node-config",
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    lightContainerCpuResources,
									corev1.ResourceMemory: lightContainerMemoryResources,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    lightContainerCpuResources,
									corev1.ResourceMemory: lightContainerMemoryResources,
								},
							},
						},
						{
							Name:            chainNode.Spec.App.App,
							Image:           chainNode.GetAppImage(),
							ImagePullPolicy: chainNode.Spec.App.GetImagePullPolicy(),
							RestartPolicy:   &sidecarRestartAlways,
							SecurityContext: k8s.RestrictedSecurityContext(),
							Command:         []string{chainNode.Spec.App.App},
							Args:            []string{"start", "--grpc-only", "--home", "/home/app"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/home/app/data",
								},
								{
									Name:      "config-empty-dir",
									MountPath: "/home/app/config",
								},
							},
							Resources: chainNode.Spec.Persistence.Snapshots.Resources,
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "start-checker",
							Image:           "busybox",
							ImagePullPolicy: chainNode.Spec.App.GetImagePullPolicy(),
							SecurityContext: k8s.RestrictedSecurityContext(),
							Command:         []string{"sh"},
							Args: []string{
								"-c",
								"if ! pidof " + chainNode.Spec.App.App + " > /dev/null; then " +
									"echo '" + chainNode.Spec.App.App + " not running'; exit 1; " +
									"fi; " +
									"APP_PID=$(pidof " + chainNode.Spec.App.App + "); " +
									"echo 'Initial " + chainNode.Spec.App.App + " PID: '$APP_PID; " +
									"while true; do " +
									"if nc -z localhost 9090; then " +
									"echo 'Data is ok'; exit 0; " +
									"fi; " +
									"if ! pidof " + chainNode.Spec.App.App + " > /dev/null || [ $(pidof " + chainNode.Spec.App.App + ") -ne $APP_PID ]; then " +
									"echo '" + chainNode.Spec.App.App + " failed or restarted'; exit 1; " +
									"fi; " +
									"sleep 2; " +
									"done",
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    lightContainerCpuResources,
									corev1.ResourceMemory: lightContainerMemoryResources,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    lightContainerCpuResources,
									corev1.ResourceMemory: lightContainerMemoryResources,
								},
							},
						},
					},
					NodeSelector: chainNode.Spec.Persistence.Snapshots.NodeSelector,
					Affinity:     chainNode.Spec.Persistence.Snapshots.Affinity,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return err
	}

	job, err := r.ClientSet.BatchV1().Jobs(chainNode.GetNamespace()).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Create PVC from Snapshot
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-ichk", snapshot.GetName()),
			Namespace: snapshot.GetNamespace(),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *snapshot.Status.RestoreSize,
				},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiVersion[0],
				Kind:     snapshot.Kind,
				Name:     snapshot.Name,
			},
		},
	}

	if err = controllerutil.SetControllerReference(job, pvc, r.Scheme); err != nil {
		return err
	}

	if _, err = r.ClientSet.CoreV1().PersistentVolumeClaims(pvc.GetNamespace()).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return err
	}

	snapshot.ObjectMeta.Annotations[controllers.AnnotationSnapshotIntegrityStatus] = string(snapshotIntegrityChecking)
	return r.Update(ctx, snapshot)
}

// cleanUpSnapshotIntegrityCheck deletes the integrity-check Job and PVC eagerly
// (Foreground propagation + explicit PVC delete) so the underlying disk is
// released as soon as the check finishes. Relying solely on
// TTLSecondsAfterFinished + Kubernetes garbage collection has been observed to
// leave orphaned PVCs/PVs intermittently (issue #27).
func (r *Reconciler) cleanUpSnapshotIntegrityCheck(ctx context.Context, snapshot *snapshotv1.VolumeSnapshot) error {
	jobName := fmt.Sprintf("%s-ichk", snapshot.GetName())
	pvcName := fmt.Sprintf("%s-ichk", snapshot.GetName())

	propagation := metav1.DeletePropagationForeground
	if err := r.ClientSet.BatchV1().Jobs(snapshot.GetNamespace()).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil && !errors.IsNotFound(err) {
		return err
	}

	if err := r.ClientSet.CoreV1().PersistentVolumeClaims(snapshot.GetNamespace()).Delete(ctx, pvcName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *Reconciler) getSnapshotIntegrityCheckStatus(ctx context.Context, chainNode *appsv1.ChainNode, snapshot *snapshotv1.VolumeSnapshot) (SnapshotIntegrityStatus, error) {
	listOptions := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			volumeSnapshot: snapshot.GetName(),
		}).String(),
	}
	list, err := r.ClientSet.BatchV1().Jobs(snapshot.GetNamespace()).List(ctx, listOptions)
	if err != nil {
		return "", err
	}

	if len(list.Items) == 0 {
		// No job is running
		return "", nil
	}
	job := list.Items[0]
	if job.Status.Failed > 0 {
		return snapshotIntegrityCorrupted, nil
	}
	if job.Status.Succeeded > 0 {
		return snapshotIntegrityOk, nil
	}
	return snapshotIntegrityChecking, nil
}
