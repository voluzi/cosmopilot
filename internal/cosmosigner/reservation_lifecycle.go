package cosmosigner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsk8sv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

// ReservationOwnerFinalizer keeps a controller root available until its signing paths are gone and
// every reservation it owns has been deleted and confirmed absent.
const ReservationOwnerFinalizer = "cosmopilot.voluzi.com/consensus-key-reservation-cleanup"

// ErrConsensusKeyReservationRecoveryBlocked marks a stale reservation whose previous signing path
// is still present or cannot be attributed safely enough for automatic recovery.
var ErrConsensusKeyReservationRecoveryBlocked = errors.New("consensus key reservation recovery blocked")

// ManagedSigningPath names the exact namespaced workloads that can sign for one reservation claim.
// OneShotNames are checked as both batch Jobs and direct one-shot Pods because older releases used
// Pods while newer or externally restored workloads may use Jobs with Job-owned Pods.
type ManagedSigningPath struct {
	StatefulSetNames []string
	OneShotNames     []string
	PodNames         []string
}

// ManagedSigningPathCleanupResult reports whether cleanup is complete or why it must wait/fail
// closed. Blocked identifies a same-name object not controlled by the expected owner.
type ManagedSigningPathCleanupResult struct {
	Done    bool
	Waiting string
	Blocked string
}

// FinalizeConsensusKeySigningPaths removes managed signer workloads and confirms their pods are
// absent without deleting retained state claims. PVC lifecycle remains owned by its dedicated
// cleanup and retention policy.
func FinalizeConsensusKeySigningPaths(ctx context.Context, reader client.Reader, c client.Client, owner client.Object, namespace string) (bool, error) {
	signerNames := reservationOwnerSignerNames(owner)
	childControllerUIDs := make(map[types.UID]struct{})
	childWorkloadUIDs := make(map[types.UID]struct{})
	if _, ok := owner.(*appsv1.ChainNodeSet); ok {
		children := &appsv1.ChainNodeList{}
		if err := reader.List(ctx, children, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		for i := range children.Items {
			child := &children.Items[i]
			if metav1.IsControlledBy(child, owner) && child.GetUID() != "" {
				childControllerUIDs[child.GetUID()] = struct{}{}
			}
		}
	}
	statefulSets := &appsk8sv1.StatefulSetList{}
	if err := reader.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if !IsOwnedSignerStatefulSet(sts, owner) {
			continue
		}
		signerNames = append(signerNames, sts.GetName())
		if !sts.GetDeletionTimestamp().IsZero() {
			return false, nil
		}
		deleted, err := DeleteStatefulSet(ctx, c, owner, namespace, sts.GetName())
		if err != nil || !deleted {
			return false, err
		}
		remaining := &appsk8sv1.StatefulSet{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(sts), remaining); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
		gone, err := SignerPodsGone(ctx, reader, namespace, sts.GetName())
		if err != nil || !gone {
			return false, err
		}
	}

	jobs := &batchv1.JobList{}
	if err := reader.List(ctx, jobs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	oneShotNames := make([]string, 0)
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if controlledByAnyUID(job, childControllerUIDs) {
			if job.GetUID() != "" {
				childWorkloadUIDs[job.GetUID()] = struct{}{}
			}
			continue
		}
		if !managedSigningOneShotBelongsToRoot(job.GetName(), job.GetLabels(), owner) {
			continue
		}
		if !metav1.IsControlledBy(job, owner) {
			return false, fmt.Errorf("Job %s/%s matches a managed signing path but is not controlled by %T UID %q", job.GetNamespace(), job.GetName(), owner, owner.GetUID())
		}
		oneShotNames = append(oneShotNames, job.GetName())
	}

	ownedPods := &corev1.PodList{}
	if err := reader.List(ctx, ownedPods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	podNames := make([]string, 0)
	for i := range ownedPods.Items {
		pod := &ownedPods.Items[i]
		if controlledByAnyUID(pod, childControllerUIDs) || controlledByAnyUID(pod, childWorkloadUIDs) {
			continue
		}
		if jobName, ok := managedSigningOneShotPodJobName(pod.GetName()); ok &&
			managedSigningOneShotBelongsToRoot(jobName, pod.GetLabels(), owner) {
			oneShotNames = append(oneShotNames, jobName)
			continue
		}
		if managedSigningOneShotBelongsToRoot(pod.GetName(), pod.GetLabels(), owner) {
			oneShotNames = append(oneShotNames, pod.GetName())
			continue
		}
		if !metav1.IsControlledBy(pod, owner) {
			continue
		}
		if isManagedSigningOneShotName(pod.GetName()) {
			oneShotNames = append(oneShotNames, pod.GetName())
			continue
		}
		if signerPodBelongsToRoot(pod, owner) {
			podNames = append(podNames, pod.GetName())
		}
	}
	cleanup, err := CleanupManagedSigningPath(ctx, reader, c, owner, namespace, ManagedSigningPath{
		StatefulSetNames: signerNames,
		OneShotNames:     oneShotNames,
		PodNames:         podNames,
	})
	if err != nil {
		return false, err
	}
	if cleanup.Blocked != "" {
		return false, fmt.Errorf("%s", cleanup.Blocked)
	}
	if !cleanup.Done {
		return false, nil
	}

	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if controlledByAnyUID(pod, childControllerUIDs) || controlledByAnyUID(pod, childWorkloadUIDs) {
			continue
		}
		if signerPodBelongsToRoot(pod, owner) || podMatchesSignerNames(pod, signerNames) {
			return false, nil
		}
	}
	return true, nil
}

// CleanupManagedSigningPath deletes only exact-name workloads controlled by owner. Every delete
// carries an object UID precondition and every absence check uses reader, which should be uncached
// for reservation release decisions.
func CleanupManagedSigningPath(ctx context.Context, reader client.Reader, c client.Client, owner client.Object, namespace string, path ManagedSigningPath) (ManagedSigningPathCleanupResult, error) {
	for _, name := range sortedUnique(path.StatefulSetNames) {
		sts := &appsk8sv1.StatefulSet{}
		key := client.ObjectKey{Namespace: namespace, Name: name}
		if err := reader.Get(ctx, key, sts); err != nil {
			if apierrors.IsNotFound(err) {
				gone, err := SignerPodsGone(ctx, reader, namespace, name)
				if err != nil {
					return ManagedSigningPathCleanupResult{}, err
				}
				if !gone {
					return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for signer Pods for StatefulSet %s/%s to be absent", namespace, name)}, nil
				}
				continue
			}
			return ManagedSigningPathCleanupResult{}, err
		}
		if !metav1.IsControlledBy(sts, owner) {
			return ManagedSigningPathCleanupResult{Blocked: fmt.Sprintf("StatefulSet %s/%s is not controlled by %T UID %q; refusing claim cleanup", namespace, name, owner, owner.GetUID())}, nil
		}
		if !sts.GetDeletionTimestamp().IsZero() {
			return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for StatefulSet %s/%s to be absent", namespace, name)}, nil
		}
		deleted, err := DeleteStatefulSet(ctx, c, owner, namespace, name)
		if err != nil {
			return ManagedSigningPathCleanupResult{}, err
		}
		if !deleted {
			return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for StatefulSet %s/%s and its signer Pods to terminate", namespace, name)}, nil
		}
		remaining := &appsk8sv1.StatefulSet{}
		if err := reader.Get(ctx, key, remaining); err == nil {
			return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for StatefulSet %s/%s to be absent", namespace, name)}, nil
		} else if !apierrors.IsNotFound(err) {
			return ManagedSigningPathCleanupResult{}, err
		}
		gone, err := SignerPodsGone(ctx, reader, namespace, name)
		if err != nil {
			return ManagedSigningPathCleanupResult{}, err
		}
		if !gone {
			return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for signer Pods for StatefulSet %s/%s to be absent", namespace, name)}, nil
		}
	}

	for _, name := range sortedUnique(path.OneShotNames) {
		result, err := cleanupManagedSigningJob(ctx, reader, c, owner, namespace, name)
		if err != nil || !result.Done {
			return result, err
		}
		result, err = cleanupDirectOwnedPod(ctx, reader, c, owner, namespace, name)
		if err != nil || !result.Done {
			return result, err
		}
	}

	for _, name := range sortedUnique(path.PodNames) {
		result, err := cleanupDirectOwnedPod(ctx, reader, c, owner, namespace, name)
		if err != nil || !result.Done {
			return result, err
		}
	}
	return ManagedSigningPathCleanupResult{Done: true}, nil
}

func cleanupManagedSigningJob(ctx context.Context, reader client.Reader, c client.Client, owner client.Object, namespace, name string) (ManagedSigningPathCleanupResult, error) {
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := reader.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return managedSigningJobPodsGone(ctx, reader, namespace, name)
		}
		return ManagedSigningPathCleanupResult{}, err
	}
	if !metav1.IsControlledBy(job, owner) {
		return ManagedSigningPathCleanupResult{Blocked: fmt.Sprintf("Job %s/%s is not controlled by %T UID %q; refusing claim cleanup", namespace, name, owner, owner.GetUID())}, nil
	}
	if job.GetUID() == "" {
		return ManagedSigningPathCleanupResult{}, fmt.Errorf("owned Job %s/%s has no UID; refusing an unguarded delete", namespace, name)
	}

	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return ManagedSigningPathCleanupResult{}, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !strings.HasPrefix(pod.GetName(), name+"-") {
			continue
		}
		controller := metav1.GetControllerOf(pod)
		if controller == nil || controller.Kind != "Job" || controller.Name != job.GetName() || controller.UID != job.GetUID() {
			return ManagedSigningPathCleanupResult{Blocked: fmt.Sprintf("Job Pod %s/%s does not belong to current Job UID %q; refusing claim cleanup", pod.GetNamespace(), pod.GetName(), job.GetUID())}, nil
		}
	}

	uid := job.GetUID()
	if job.GetDeletionTimestamp().IsZero() {
		foreground := metav1.DeletePropagationForeground
		if err := c.Delete(ctx, job, client.Preconditions{UID: &uid}, client.PropagationPolicy(foreground)); err != nil && !apierrors.IsNotFound(err) {
			return ManagedSigningPathCleanupResult{}, err
		}
	}
	remaining := &batchv1.Job{}
	if err := reader.Get(ctx, key, remaining); err == nil {
		return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for Job %s/%s to terminate", namespace, name)}, nil
	} else if !apierrors.IsNotFound(err) {
		return ManagedSigningPathCleanupResult{}, err
	}
	return managedSigningJobPodsGone(ctx, reader, namespace, name)
}

func managedSigningJobPodsGone(ctx context.Context, reader client.Reader, namespace, name string) (ManagedSigningPathCleanupResult, error) {
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return ManagedSigningPathCleanupResult{}, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if strings.HasPrefix(pod.GetName(), name+"-") {
			return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for orphaned Job Pod %s/%s to terminate", pod.GetNamespace(), pod.GetName())}, nil
		}
	}
	return ManagedSigningPathCleanupResult{Done: true}, nil
}

func cleanupDirectOwnedPod(ctx context.Context, reader client.Reader, c client.Client, owner client.Object, namespace, name string) (ManagedSigningPathCleanupResult, error) {
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := reader.Get(ctx, key, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ManagedSigningPathCleanupResult{Done: true}, nil
		}
		return ManagedSigningPathCleanupResult{}, err
	}
	if !metav1.IsControlledBy(pod, owner) {
		return ManagedSigningPathCleanupResult{Blocked: fmt.Sprintf("Pod %s/%s is not controlled by %T UID %q; refusing claim cleanup", namespace, name, owner, owner.GetUID())}, nil
	}
	if pod.GetUID() == "" {
		return ManagedSigningPathCleanupResult{}, fmt.Errorf("owned Pod %s/%s has no UID; refusing an unguarded delete", namespace, name)
	}
	uid := pod.GetUID()
	if pod.GetDeletionTimestamp().IsZero() {
		if err := c.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return ManagedSigningPathCleanupResult{}, err
		}
	}
	remaining := &corev1.Pod{}
	if err := reader.Get(ctx, key, remaining); err == nil {
		return ManagedSigningPathCleanupResult{Waiting: fmt.Sprintf("waiting for Pod %s/%s to terminate", namespace, name)}, nil
	} else if !apierrors.IsNotFound(err) {
		return ManagedSigningPathCleanupResult{}, err
	}
	return ManagedSigningPathCleanupResult{Done: true}, nil
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func isManagedSigningOneShotName(name string) bool {
	return strings.HasSuffix(name, "-tmkms-generate-identity") ||
		strings.HasSuffix(name, "-tmkms-vault-upload") ||
		strings.HasSuffix(name, "-import") || strings.HasSuffix(name, "-pubkey")
}

func managedSigningOneShotPodJobName(name string) (string, bool) {
	lastIndex := -1
	jobName := ""
	for _, marker := range []string{
		"-tmkms-generate-identity", "-tmkms-vault-upload", "-import", "-pubkey",
	} {
		if index := strings.LastIndex(name, marker+"-"); index > lastIndex {
			lastIndex = index
			jobName = name[:index+len(marker)]
		}
	}
	return jobName, lastIndex >= 0
}

// EnsureConsensusKeyReservationOwnerFinalizer persists the reservation lifecycle finalizer on the
// exact root UID. The reader must bypass the manager cache so a successful return proves the
// finalizer exists in API-server state before any reservation can be created.
func EnsureConsensusKeyReservationOwnerFinalizer(ctx context.Context, reader client.Reader, writer client.Writer, owner client.Object) (bool, error) {
	if owner.GetUID() == "" {
		return false, fmt.Errorf("consensus-key reservation owner %s/%s has no UID", owner.GetNamespace(), owner.GetName())
	}
	fresh, ok := owner.DeepCopyObject().(client.Object)
	if !ok {
		return false, fmt.Errorf("consensus-key reservation owner %T is not a Kubernetes object", owner)
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(owner), fresh); err != nil {
		return false, err
	}
	if fresh.GetUID() != owner.GetUID() {
		return false, fmt.Errorf("consensus-key reservation owner %s/%s changed UID from %q to %q", owner.GetNamespace(), owner.GetName(), owner.GetUID(), fresh.GetUID())
	}
	if !fresh.GetDeletionTimestamp().IsZero() {
		return false, fmt.Errorf("consensus-key reservation owner %s/%s is already terminating", owner.GetNamespace(), owner.GetName())
	}
	if controllerutil.ContainsFinalizer(fresh, ReservationOwnerFinalizer) {
		owner.SetResourceVersion(fresh.GetResourceVersion())
		owner.SetFinalizers(append([]string(nil), fresh.GetFinalizers()...))
		return false, nil
	}
	base, ok := fresh.DeepCopyObject().(client.Object)
	if !ok {
		return false, fmt.Errorf("consensus-key reservation owner %T cannot be copied", owner)
	}
	controllerutil.AddFinalizer(fresh, ReservationOwnerFinalizer)
	if err := writer.Patch(ctx, fresh, client.MergeFrom(base)); err != nil {
		return false, err
	}
	confirmed, ok := owner.DeepCopyObject().(client.Object)
	if !ok {
		return false, fmt.Errorf("consensus-key reservation owner %T cannot be copied", owner)
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(owner), confirmed); err != nil {
		return false, err
	}
	if confirmed.GetUID() != owner.GetUID() || !controllerutil.ContainsFinalizer(confirmed, ReservationOwnerFinalizer) {
		return false, fmt.Errorf("consensus-key reservation finalizer was not persisted on exact owner UID %q", owner.GetUID())
	}
	owner.SetResourceVersion(confirmed.GetResourceVersion())
	owner.SetFinalizers(append([]string(nil), confirmed.GetFinalizers()...))
	return true, nil
}

// ReleaseConsensusKeyReservations deletes reservations belonging to the exact root identity with a
// reservation-object UID precondition and confirms each delete through the uncached reader.
func ReleaseConsensusKeyReservations(ctx context.Context, reader client.Reader, writer client.Writer, owner client.Object) ([]string, bool, error) {
	kind, err := reservationOwnerKind(owner)
	if err != nil {
		return nil, false, err
	}
	reservations := &appsv1.ConsensusKeyReservationList{}
	if err := reader.List(ctx, reservations); err != nil {
		return nil, false, err
	}
	sort.Slice(reservations.Items, func(i, j int) bool {
		return reservations.Items[i].GetName() < reservations.Items[j].GetName()
	})

	released := make([]string, 0)
	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.OwnerUID != owner.GetUID() {
			continue
		}
		if reservation.Spec.OwnerKind != kind {
			return released, false, fmt.Errorf("reservation %q matches owner UID %q but has inconsistent owner kind %q", reservation.GetName(), owner.GetUID(), reservation.Spec.OwnerKind)
		}
		done, err := ReleaseConsensusKeyReservationClaim(ctx, reader, writer, owner, reservation)
		if err != nil || !done {
			return released, false, err
		}
		released = append(released, reservation.GetName())
	}

	confirmed := &appsv1.ConsensusKeyReservationList{}
	if err := reader.List(ctx, confirmed); err != nil {
		return released, false, err
	}
	for i := range confirmed.Items {
		if confirmed.Items[i].Spec.OwnerUID == owner.GetUID() {
			return released, false, nil
		}
	}
	return released, true, nil
}

// ReleaseConsensusKeyReservationClaim deletes one exact reservation after its caller has confirmed
// that claim's signing path is absent. The reservation and owner identities are revalidated before
// the UID-preconditioned delete.
func ReleaseConsensusKeyReservationClaim(ctx context.Context, reader client.Reader, writer client.Writer, owner client.Object, reservation *appsv1.ConsensusKeyReservation) (bool, error) {
	kind, err := reservationOwnerKind(owner)
	if err != nil {
		return false, err
	}
	if reservation.Spec.OwnerUID != owner.GetUID() || reservation.Spec.OwnerKind != kind ||
		reservation.Spec.Namespace != owner.GetNamespace() || reservation.Spec.OwnerName != owner.GetName() ||
		reservation.Spec.Claim == "" {
		return false, fmt.Errorf("reservation %q does not match exact owner %s %s/%s UID %q", reservation.GetName(), kind, owner.GetNamespace(), owner.GetName(), owner.GetUID())
	}
	if reservation.GetUID() == "" {
		return false, fmt.Errorf("reservation %q has no object UID; refusing an unguarded delete", reservation.GetName())
	}
	uid := reservation.GetUID()
	if err := writer.Delete(ctx, reservation, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	remaining := &appsv1.ConsensusKeyReservation{}
	if err := reader.Get(ctx, client.ObjectKey{Name: reservation.GetName()}, remaining); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// HasConsensusKeyReservationsForOwner reports whether the exact root UID already owns a
// reservation. It supports upgrade recovery when the reservation predates the lifecycle finalizer.
func HasConsensusKeyReservationsForOwner(ctx context.Context, reader client.Reader, owner client.Object) (bool, error) {
	kind, err := reservationOwnerKind(owner)
	if err != nil {
		return false, err
	}
	reservations := &appsv1.ConsensusKeyReservationList{}
	if err := reader.List(ctx, reservations); err != nil {
		return false, err
	}
	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.OwnerUID != owner.GetUID() {
			continue
		}
		if reservation.Spec.OwnerKind != kind || reservation.Spec.Namespace != owner.GetNamespace() ||
			reservation.Spec.OwnerName != owner.GetName() || reservation.Spec.Claim == "" {
			return false, fmt.Errorf("reservation %q matches owner UID %q but has inconsistent owner or claim metadata", reservation.GetName(), owner.GetUID())
		}
		return true, nil
	}
	return false, nil
}

func recoverStaleConsensusKeyReservation(ctx context.Context, reader client.Reader, writer client.Writer, reservation *appsv1.ConsensusKeyReservation, chainID, publicKey string, holder ReservationHolder) (bool, error) {
	if reservation.Spec.OwnerUID == holder.UID || reservation.Spec.ChainID != chainID ||
		reservation.Spec.PublicKey != publicKey || reservation.GetName() != ConsensusKeyReservationName(chainID, publicKey) {
		return false, nil
	}
	if reservation.Spec.OwnerKind != holder.Kind || reservation.Spec.Namespace != holder.Namespace ||
		reservation.Spec.OwnerName != holder.Name {
		return false, nil
	}
	if reservation.Spec.OwnerUID == "" ||
		(reservation.Spec.OwnerKind != "ChainNode" && reservation.Spec.OwnerKind != "ChainNodeSet") ||
		reservation.Spec.Namespace == "" || reservation.Spec.OwnerName == "" || reservation.Spec.Claim == "" {
		return false, fmt.Errorf("%w: reservation %q has incomplete stale-owner metadata; refusing automatic recovery", ErrConsensusKeyReservationConflict, reservation.GetName())
	}
	if reservation.Spec.Claim != holder.Claim {
		return false, nil
	}
	stale, err := reservationOwnerIsStale(ctx, reader, reservation)
	if err != nil {
		return false, err
	}
	if !stale {
		return false, nil
	}
	gone, blockedBy, err := staleReservationSigningPathsGone(ctx, reader, reservation, holder)
	if err != nil {
		return false, err
	}
	if !gone {
		return false, &reservationRecoveryBlockedError{reservation: reservation.GetName(), detail: blockedBy}
	}
	if err := ensureNoConflictingReservationClaim(ctx, reader, chainID, publicKey, holder); err != nil {
		return false, err
	}
	if err := ensureNoLegacyConsensusKeyOwner(ctx, reader, chainID, publicKey, holder); err != nil {
		return false, err
	}
	if reservation.GetUID() == "" {
		return false, nil
	}
	uid := reservation.GetUID()
	if err := writer.Delete(ctx, reservation, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	remaining := &appsv1.ConsensusKeyReservation{}
	if err := reader.Get(ctx, client.ObjectKey{Name: reservation.GetName()}, remaining); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func reservationOwnerIsStale(ctx context.Context, reader client.Reader, reservation *appsv1.ConsensusKeyReservation) (bool, error) {
	key := client.ObjectKey{Namespace: reservation.Spec.Namespace, Name: reservation.Spec.OwnerName}
	var owner client.Object
	switch reservation.Spec.OwnerKind {
	case "ChainNode":
		owner = &appsv1.ChainNode{}
	case "ChainNodeSet":
		owner = &appsv1.ChainNodeSet{}
	default:
		return false, fmt.Errorf("%w: reservation %q has unsupported owner kind %q", ErrConsensusKeyReservationConflict, reservation.GetName(), reservation.Spec.OwnerKind)
	}
	if err := reader.Get(ctx, key, owner); err == nil {
		return owner.GetUID() != reservation.Spec.OwnerUID, nil
	} else if apierrors.IsNotFound(err) {
		return true, nil
	} else {
		return false, err
	}
}

func staleReservationSigningPathsGone(ctx context.Context, reader client.Reader, reservation *appsv1.ConsensusKeyReservation, holder ReservationHolder) (bool, string, error) {
	currentHolderControllerUIDs := make(map[types.UID]struct{})
	nodes := &appsv1.ChainNodeList{}
	if err := reader.List(ctx, nodes, client.InNamespace(reservation.Spec.Namespace)); err != nil {
		return false, "", err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.GetUID() == reservation.Spec.OwnerUID || controlledByUID(node, reservation.Spec.OwnerUID) {
			return false, fmt.Sprintf("ChainNode %s/%s is still controlled by stale owner UID %q", node.GetNamespace(), node.GetName(), reservation.Spec.OwnerUID), nil
		}
		if node.GetName() == reservation.Spec.Claim ||
			(reservation.Spec.OwnerKind == "ChainNode" && node.GetName() == reservation.Spec.OwnerName) {
			if heldByReservationHolder(node, holder) {
				if node.GetUID() != "" {
					currentHolderControllerUIDs[node.GetUID()] = struct{}{}
				}
				continue
			}
			return false, fmt.Sprintf("ChainNode %s/%s matches stale claim %q but is not attributable to owner UID %q", node.GetNamespace(), node.GetName(), reservation.Spec.Claim, reservation.Spec.OwnerUID), nil
		}
	}

	statefulSets := &appsk8sv1.StatefulSetList{}
	if err := reader.List(ctx, statefulSets, client.InNamespace(reservation.Spec.Namespace)); err != nil {
		return false, "", err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if controlledByUID(sts, reservation.Spec.OwnerUID) {
			return false, fmt.Sprintf("StatefulSet %s/%s is still controlled by stale owner UID %q", sts.GetNamespace(), sts.GetName(), reservation.Spec.OwnerUID), nil
		}
		if staleReservationStatefulSetMatches(sts, reservation) {
			if heldByReservationHolder(sts, holder) {
				if sts.GetUID() != "" {
					currentHolderControllerUIDs[sts.GetUID()] = struct{}{}
				}
				continue
			}
			return false, fmt.Sprintf("StatefulSet %s/%s matches the stale signer labels but is not attributable to owner UID %q", sts.GetNamespace(), sts.GetName(), reservation.Spec.OwnerUID), nil
		}
	}

	jobs := &batchv1.JobList{}
	if err := reader.List(ctx, jobs, client.InNamespace(reservation.Spec.Namespace)); err != nil {
		return false, "", err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if controlledByUID(job, reservation.Spec.OwnerUID) {
			return false, fmt.Sprintf("Job %s/%s is still controlled by stale owner UID %q", job.GetNamespace(), job.GetName(), reservation.Spec.OwnerUID), nil
		}
		if staleReservationJobMatches(job, reservation) {
			if heldByReservationHolder(job, holder) {
				if job.GetUID() != "" {
					currentHolderControllerUIDs[job.GetUID()] = struct{}{}
				}
				continue
			}
			return false, fmt.Sprintf("Job %s/%s matches stale claim %q but is not attributable to owner UID %q", job.GetNamespace(), job.GetName(), reservation.Spec.Claim, reservation.Spec.OwnerUID), nil
		}
	}

	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(reservation.Spec.Namespace)); err != nil {
		return false, "", err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if controlledByUID(pod, reservation.Spec.OwnerUID) {
			return false, fmt.Sprintf("Pod %s/%s is still controlled by stale owner UID %q", pod.GetNamespace(), pod.GetName(), reservation.Spec.OwnerUID), nil
		}
		if staleReservationPodMatches(pod, reservation) {
			controller := metav1.GetControllerOf(pod)
			_, heldThroughController := currentHolderControllerUIDs[func() types.UID {
				if controller == nil {
					return ""
				}
				return controller.UID
			}()]
			if heldByReservationHolder(pod, holder) || heldThroughController {
				continue
			}
			return false, fmt.Sprintf("Pod %s/%s matches stale claim %q but is not attributable to owner UID %q", pod.GetNamespace(), pod.GetName(), reservation.Spec.Claim, reservation.Spec.OwnerUID), nil
		}
	}
	return true, "", nil
}

func heldByReservationHolder(obj metav1.Object, holder ReservationHolder) bool {
	if holder.Kind == "ChainNode" && obj.GetUID() == holder.UID &&
		obj.GetNamespace() == holder.Namespace && obj.GetName() == holder.Name {
		return true
	}
	return controlledByUID(obj, holder.UID)
}

type reservationRecoveryBlockedError struct {
	reservation string
	detail      string
}

func (e *reservationRecoveryBlockedError) Error() string {
	return fmt.Sprintf("%v: stale reservation %q cannot be recovered automatically: %s", ErrConsensusKeyReservationRecoveryBlocked, e.reservation, e.detail)
}

func (e *reservationRecoveryBlockedError) Unwrap() []error {
	return []error{ErrConsensusKeyReservationConflict, ErrConsensusKeyReservationRecoveryBlocked}
}

func (e *reservationRecoveryBlockedError) Is(target error) bool {
	return target == ErrConsensusKeyReservationConflict || target == ErrConsensusKeyReservationRecoveryBlocked
}

func staleReservationJobMatches(job *batchv1.Job, reservation *appsv1.ConsensusKeyReservation) bool {
	if !isManagedSigningOneShotName(job.GetName()) {
		return false
	}
	if matches, exact := staleReservationSignerLabelAttribution(job.GetLabels(), reservation); exact {
		return matches
	}
	if strings.HasPrefix(job.GetName(), reservation.Spec.Claim+"-") {
		return true
	}
	if reservation.Spec.OwnerKind == "ChainNode" {
		return strings.HasPrefix(job.GetName(), reservation.Spec.OwnerName+"-signer-")
	}
	return strings.HasPrefix(job.GetName(), reservation.Spec.OwnerName+"-")
}

func staleReservationSignerLabelAttribution(labels map[string]string, reservation *appsv1.ConsensusKeyReservation) (bool, bool) {
	chainNode := labels["chain-node"]
	nodeSet := labels["nodeset"]
	if reservation.Spec.OwnerKind == "ChainNode" {
		if chainNode != "" {
			return chainNode == reservation.Spec.OwnerName, true
		}
		if nodeSet != "" {
			return false, true
		}
		return false, false
	}
	if nodeSet != "" {
		return nodeSet == reservation.Spec.OwnerName, true
	}
	if chainNode != "" {
		return chainNode == reservation.Spec.Claim, true
	}
	return false, false
}

func staleReservationStatefulSetMatches(sts *appsk8sv1.StatefulSet, reservation *appsv1.ConsensusKeyReservation) bool {
	if matches, exact := staleReservationSignerLabelAttribution(sts.Spec.Template.GetLabels(), reservation); exact {
		return matches
	}
	if matches, exact := staleReservationSignerLabelAttribution(sts.GetLabels(), reservation); exact {
		return matches
	}
	if reservation.Spec.OwnerKind == "ChainNode" {
		return sts.GetName() == reservation.Spec.OwnerName+"-signer"
	}
	return strings.HasPrefix(sts.GetName(), reservation.Spec.OwnerName+"-") && strings.HasSuffix(sts.GetName(), "-signer")
}

func staleReservationPodMatches(pod *corev1.Pod, reservation *appsv1.ConsensusKeyReservation) bool {
	labelsMatch, exactLabels := staleReservationSignerLabelAttribution(pod.GetLabels(), reservation)
	if exactLabels && !labelsMatch {
		return false
	}
	if pod.GetName() == reservation.Spec.Claim ||
		(reservation.Spec.OwnerKind == "ChainNode" && pod.GetName() == reservation.Spec.OwnerName) {
		return true
	}
	if staleReservationOneShotPodMatches(pod.GetName(), reservation) {
		return true
	}
	if staleReservationSignerReplicaPodMatches(pod.GetName(), reservation) {
		return true
	}
	if pod.GetLabels()["app.kubernetes.io/name"] != "cosmosigner" {
		return false
	}
	if exactLabels {
		return labelsMatch
	}
	instance := pod.GetLabels()["app.kubernetes.io/instance"]
	if reservation.Spec.OwnerKind == "ChainNode" {
		return instance == reservation.Spec.OwnerName+"-signer"
	}
	return strings.HasPrefix(instance, reservation.Spec.OwnerName+"-")
}

func staleReservationSignerReplicaPodMatches(name string, reservation *appsv1.ConsensusKeyReservation) bool {
	separator := strings.LastIndexByte(name, '-')
	if separator < 0 || separator == len(name)-1 {
		return false
	}
	for _, char := range name[separator+1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	statefulSetName := name[:separator]
	if reservation.Spec.OwnerKind == "ChainNode" {
		return statefulSetName == reservation.Spec.OwnerName+"-signer"
	}
	return strings.HasPrefix(statefulSetName, reservation.Spec.OwnerName+"-") && strings.HasSuffix(statefulSetName, "-signer")
}

func staleReservationOneShotPodMatches(name string, reservation *appsv1.ConsensusKeyReservation) bool {
	if isManagedSigningOneShotName(name) {
		return staleReservationJobMatches(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name}}, reservation)
	}
	managedJobName, ok := managedSigningOneShotPodJobName(name)
	if !ok {
		return false
	}
	return staleReservationJobMatches(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: managedJobName}}, reservation)
}

func managedSigningOneShotBelongsToRoot(name string, labels map[string]string, owner client.Object) bool {
	if !isManagedSigningOneShotName(name) {
		return false
	}
	switch owner.(type) {
	case *appsv1.ChainNode:
		return strings.HasPrefix(name, owner.GetName()+"-") || labels["chain-node"] == owner.GetName()
	case *appsv1.ChainNodeSet:
		return strings.HasPrefix(name, owner.GetName()+"-") || labels["nodeset"] == owner.GetName()
	default:
		return false
	}
}

func signerPodBelongsToRoot(pod *corev1.Pod, owner client.Object) bool {
	if pod.GetLabels()["app.kubernetes.io/name"] != "cosmosigner" {
		return false
	}
	switch owner.(type) {
	case *appsv1.ChainNode:
		return pod.GetLabels()["chain-node"] == owner.GetName()
	case *appsv1.ChainNodeSet:
		return pod.GetLabels()["nodeset"] == owner.GetName()
	default:
		return false
	}
}

func reservationOwnerSignerNames(owner client.Object) []string {
	switch typed := owner.(type) {
	case *appsv1.ChainNode:
		return []string{typed.GetName() + "-signer"}
	case *appsv1.ChainNodeSet:
		names := make([]string, 0, len(typed.ResolveCosmosigners())+len(typed.Status.Cosmosigners))
		for _, signer := range typed.ResolveCosmosigners() {
			names = append(names, typed.CosmosignerResourceName(signer))
		}
		for i := range typed.Status.Cosmosigners {
			names = append(names, appsv1.CosmosignerStatusResourceName(&typed.Status.Cosmosigners[i]))
		}
		return sortedUnique(names)
	default:
		return nil
	}
}

func podMatchesSignerNames(pod *corev1.Pod, signerNames []string) bool {
	for _, name := range signerNames {
		if isStatefulSetReplicaPodName(pod.GetName(), name) {
			return true
		}
	}
	return false
}

func controlledByUID(obj metav1.Object, uid types.UID) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil && owner.UID == uid
}

func controlledByAnyUID(obj metav1.Object, uids map[types.UID]struct{}) bool {
	owner := metav1.GetControllerOf(obj)
	if owner == nil {
		return false
	}
	_, ok := uids[owner.UID]
	return ok
}

func reservationOwnerKind(owner client.Object) (string, error) {
	switch owner.(type) {
	case *appsv1.ChainNode:
		return "ChainNode", nil
	case *appsv1.ChainNodeSet:
		return "ChainNodeSet", nil
	default:
		return "", fmt.Errorf("unsupported consensus-key reservation owner %T", owner)
	}
}
