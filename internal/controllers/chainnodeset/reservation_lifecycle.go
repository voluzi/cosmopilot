package chainnodeset

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsk8sv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

func (r *Reconciler) prepareConsensusKeyReservationOwner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	needsFinalizer := nodeSet.Spec.Validator != nil || len(nodeSet.ResolveCosmosigners()) > 0 || len(nodeSet.Status.Cosmosigners) > 0
	if !needsFinalizer {
		for i := range nodeSet.Spec.Nodes {
			if nodeSet.Spec.Nodes[i].Validator != nil {
				needsFinalizer = true
				break
			}
		}
	}
	var err error
	if !needsFinalizer {
		needsFinalizer, err = cosmosigner.HasConsensusKeyReservationsForOwner(ctx, r.uncachedReader(), nodeSet)
		if err != nil {
			return false, err
		}
	}
	if !needsFinalizer {
		return false, nil
	}
	return cosmosigner.EnsureConsensusKeyReservationOwnerFinalizer(ctx, r.uncachedReader(), r.Client, nodeSet)
}

func (r *Reconciler) ensureConsensusKeyReservation(ctx context.Context, nodeSet *appsv1.ChainNodeSet, chainID, publicKey string, holder cosmosigner.ReservationHolder) error {
	result, err := cosmosigner.EnsureConsensusKeyReservationWithResult(ctx, r.uncachedReader(), r.Client, chainID, publicKey, holder)
	if err != nil {
		if (errors.Is(err, cosmosigner.ErrConsensusKeyReservationRecoveryBlocked) ||
			errors.Is(err, cosmosigner.ErrConsensusKeyReservationConflict)) && r.recorder != nil {
			r.recorder.Eventf(nodeSet, corev1.EventTypeWarning, appsv1.ReasonConsensusKeyReservationBlocked, "%v", err)
		}
		return err
	}
	if result.RecoveredReservation != "" && r.recorder != nil {
		r.recorder.Eventf(nodeSet, corev1.EventTypeNormal, appsv1.ReasonConsensusKeyReservationRecovered,
			"Recovered stale ConsensusKeyReservation %s after confirming owner UID %s has no signing path", result.RecoveredReservation, holder.UID)
	}
	return nil
}

func (r *Reconciler) reconcileConsensusKeyReservationClaims(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	desiredClaims := desiredConsensusKeyReservationClaims(nodeSet)
	reservations := &appsv1.ConsensusKeyReservationList{}
	if err := r.uncachedReader().List(ctx, reservations); err != nil {
		return false, err
	}
	sort.Slice(reservations.Items, func(i, j int) bool {
		return reservations.Items[i].GetName() < reservations.Items[j].GetName()
	})

	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.OwnerUID != nodeSet.GetUID() {
			continue
		}
		if reservation.Spec.OwnerKind != "ChainNodeSet" || reservation.Spec.Namespace != nodeSet.GetNamespace() ||
			reservation.Spec.OwnerName != nodeSet.GetName() || reservation.Spec.Claim == "" {
			return false, fmt.Errorf("reservation %q matches ChainNodeSet UID %q but has inconsistent owner or claim metadata", reservation.GetName(), nodeSet.GetUID())
		}
		if _, desired := desiredClaims[reservation.Spec.Claim]; desired {
			continue
		}

		gone, err := r.consensusKeyReservationClaimSigningPathsGone(ctx, nodeSet, reservation)
		if err != nil || !gone {
			return false, err
		}
		released, err := cosmosigner.ReleaseConsensusKeyReservationClaim(ctx, r.uncachedReader(), r.Client, nodeSet, reservation)
		if err != nil || !released {
			return false, err
		}
		if r.recorder != nil {
			r.recorder.Eventf(nodeSet, corev1.EventTypeNormal, appsv1.ReasonConsensusKeyReservationReleased,
				"ConsensusKeyReservation %s released after claim %q signing-path teardown", reservation.GetName(), reservation.Spec.Claim)
		}
	}
	return true, nil
}

func desiredConsensusKeyReservationClaims(nodeSet *appsv1.ChainNodeSet) map[string]struct{} {
	claims := make(map[string]struct{})
	for _, signer := range nodeSet.ResolveCosmosigners() {
		claims[nodeSetCosmosignerReservationClaim(nodeSet, signer)] = struct{}{}
	}
	if nodeSet.Spec.Validator != nil && !nodeSet.IsCosmosignerTargetGroup(appsv1.ReservedValidatorGroupName) {
		claims[validatorNodeName(nodeSet, appsv1.ReservedValidatorGroupName, 0)] = struct{}{}
	}
	for i := range nodeSet.Spec.Nodes {
		group := &nodeSet.Spec.Nodes[i]
		if group.Validator == nil || nodeSet.IsCosmosignerTargetGroup(group.Name) {
			continue
		}
		for ordinal := 0; ordinal < group.GetInstances(); ordinal++ {
			claims[validatorNodeName(nodeSet, group.Name, ordinal)] = struct{}{}
		}
	}
	return claims
}

func (r *Reconciler) retiringConsensusKeyReservationPublicKeys(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (map[string]struct{}, error) {
	reservations := &appsv1.ConsensusKeyReservationList{}
	if err := r.uncachedReader().List(ctx, reservations); err != nil {
		return nil, err
	}
	desiredClaims := desiredConsensusKeyReservationClaims(nodeSet)
	publicKeys := make(map[string]struct{})
	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.OwnerUID != nodeSet.GetUID() {
			continue
		}
		if reservation.Spec.OwnerKind != "ChainNodeSet" || reservation.Spec.Namespace != nodeSet.GetNamespace() ||
			reservation.Spec.OwnerName != nodeSet.GetName() || reservation.Spec.Claim == "" || reservation.Spec.PublicKey == "" {
			return nil, fmt.Errorf("reservation %q matches ChainNodeSet UID %q but has inconsistent owner, claim, or public-key metadata", reservation.GetName(), nodeSet.GetUID())
		}
		if _, desired := desiredClaims[reservation.Spec.Claim]; desired {
			continue
		}
		publicKeys[reservation.Spec.PublicKey] = struct{}{}
	}
	return publicKeys, nil
}

func cosmosignerStatusHasReservation(status *appsv1.CosmosignerStatus, reservationPublicKeys map[string]struct{}) bool {
	if status == nil {
		return false
	}
	if status.ResourceName != "" && status.PublicKey == "" && len(reservationPublicKeys) > 0 {
		return true
	}
	if _, ok := reservationPublicKeys[status.PublicKey]; ok && status.PublicKey != "" {
		return true
	}
	if status.Migration != nil && status.Migration.DesiredPublicKey != "" {
		_, ok := reservationPublicKeys[status.Migration.DesiredPublicKey]
		return ok
	}
	return false
}

func (r *Reconciler) consensusKeyReservationClaimSigningPathsGone(ctx context.Context, nodeSet *appsv1.ChainNodeSet, reservation *appsv1.ConsensusKeyReservation) (bool, error) {
	claim := reservation.Spec.Claim
	child := &appsv1.ChainNode{}
	key := client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: claim}
	if err := r.uncachedReader().Get(ctx, key, child); err == nil {
		owner := metav1.GetControllerOf(child)
		if owner == nil || owner.UID != nodeSet.GetUID() {
			return false, fmt.Errorf("ChainNode %s/%s matches undesired reservation claim %q but is not controlled by ChainNodeSet UID %q", child.GetNamespace(), child.GetName(), claim, nodeSet.GetUID())
		}
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}

	pod := &corev1.Pod{}
	if err := r.uncachedReader().Get(ctx, key, pod); err == nil {
		return false, fmt.Errorf("Pod %s/%s still matches undesired reservation claim %q", pod.GetNamespace(), pod.GetName(), claim)
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}

	desiredSignerResources := make(map[string]struct{})
	desiredSignerClaims := make(map[string]string)
	for _, signer := range nodeSet.ResolveCosmosigners() {
		name := nodeSet.CosmosignerResourceName(signer)
		desiredSignerResources[name] = struct{}{}
		desiredSignerClaims[signer.Name] = nodeSetCosmosignerReservationClaim(nodeSet, signer)
	}

	matchedResource := ""
	retiredSignerResources := make(map[string]struct{})
	for i := range nodeSet.Status.Cosmosigners {
		status := &nodeSet.Status.Cosmosigners[i]
		if _, desired := desiredSignerClaims[status.Name]; !desired && status.ResourceName != "" && status.PublicKey == "" {
			retiredSignerResources[appsv1.CosmosignerStatusResourceName(status)] = struct{}{}
		}
		matchesKey := status.PublicKey == reservation.Spec.PublicKey ||
			(status.Migration != nil && status.Migration.DesiredPublicKey == reservation.Spec.PublicKey)
		if !matchesKey {
			continue
		}
		if desiredClaim, desired := desiredSignerClaims[status.Name]; desired && desiredClaim != claim {
			return false, fmt.Errorf("cosmosigner status %q associates consensus key reservation %q with desired claim %q, not recorded claim %q", status.Name, reservation.GetName(), desiredClaim, claim)
		}
		resourceName := appsv1.CosmosignerStatusResourceName(status)
		if matchedResource != "" && matchedResource != resourceName {
			return false, fmt.Errorf("consensus key reservation %q matches multiple cosmosigner resources %q and %q", reservation.GetName(), matchedResource, resourceName)
		}
		matchedResource = resourceName
	}

	resourcesToCleanup := retiredSignerResources
	if matchedResource != "" {
		resourcesToCleanup[matchedResource] = struct{}{}
	}
	resourceNames := make([]string, 0, len(resourcesToCleanup))
	for resourceName := range resourcesToCleanup {
		resourceNames = append(resourceNames, resourceName)
	}
	sort.Strings(resourceNames)
	for _, resourceName := range resourceNames {
		cleanup, err := cosmosigner.CleanupManagedSigningPath(ctx, r.uncachedReader(), r.Client, nodeSet, nodeSet.GetNamespace(), cosmosigner.ManagedSigningPath{
			StatefulSetNames: []string{resourceName},
			OneShotNames:     []string{resourceName + "-import", resourceName + "-pubkey"},
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
	}

	statefulSets := &appsk8sv1.StatefulSetList{}
	if err := r.uncachedReader().List(ctx, statefulSets, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if !cosmosigner.IsOwnedSignerStatefulSet(sts, nodeSet) &&
			!cosmosigner.IsOwnedDeterministicSignerStatefulSet(sts, nodeSet) {
			continue
		}
		if _, desired := desiredSignerResources[sts.GetName()]; desired {
			continue
		}
		return false, fmt.Errorf("owned cosmosigner StatefulSet %s/%s cannot be attributed safely while releasing reservation claim %q", sts.GetNamespace(), sts.GetName(), claim)
	}

	jobs := &batchv1.JobList{}
	if err := r.uncachedReader().List(ctx, jobs, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if managedClaimOneShotName(job.GetName(), claim) ||
			managedRetiredNodeSetSignerOneShot(job, nodeSet, desiredSignerResources) {
			return false, fmt.Errorf("Job %s/%s still matches undesired reservation claim %q", job.GetNamespace(), job.GetName(), claim)
		}
	}
	pods := &corev1.PodList{}
	if err := r.uncachedReader().List(ctx, pods, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if managedClaimOneShotName(pod.GetName(), claim) ||
			managedRetiredNodeSetSignerOneShot(pod, nodeSet, desiredSignerResources) ||
			managedRetiredNodeSetSignerReplica(pod, nodeSet, desiredSignerResources) {
			return false, fmt.Errorf("Pod %s/%s still matches undesired reservation claim %q", pod.GetNamespace(), pod.GetName(), claim)
		}
	}
	return true, nil
}

func managedRetiredNodeSetSignerOneShot(obj client.Object, nodeSet *appsv1.ChainNodeSet, desired map[string]struct{}) bool {
	resourceName, ok := managedSignerOneShotResourceName(obj.GetName())
	if !ok {
		return false
	}
	return managedRetiredNodeSetSignerResource(obj, nodeSet, desired, resourceName)
}

func managedRetiredNodeSetSignerReplica(pod *corev1.Pod, nodeSet *appsv1.ChainNodeSet, desired map[string]struct{}) bool {
	resourceName, ok := statefulSetReplicaResourceName(pod.GetName())
	if !ok {
		return false
	}
	return managedRetiredNodeSetSignerResource(pod, nodeSet, desired, resourceName)
}

func managedRetiredNodeSetSignerResource(obj client.Object, nodeSet *appsv1.ChainNodeSet, desired map[string]struct{}, resourceName string) bool {
	if _, current := desired[resourceName]; current {
		return false
	}
	if metav1.IsControlledBy(obj, nodeSet) {
		return deterministicNodeSetSignerResourceName(resourceName, nodeSet.GetName())
	}
	labels := obj.GetLabels()
	if ownerName := labels[controllers.LabelChainNodeSet]; ownerName != "" {
		return ownerName == nodeSet.GetName() && deterministicNodeSetSignerResourceName(resourceName, nodeSet.GetName())
	}
	if labels[controllers.LabelChainNode] != "" {
		return false
	}
	return deterministicNodeSetSignerResourceName(resourceName, nodeSet.GetName())
}

func managedSignerOneShotResourceName(name string) (string, bool) {
	lastIndex := -1
	for _, marker := range []string{"-import", "-pubkey"} {
		if index := strings.LastIndex(name, marker); index > lastIndex &&
			(index+len(marker) == len(name) || name[index+len(marker)] == '-') {
			lastIndex = index
		}
	}
	if lastIndex <= 0 {
		return "", false
	}
	return name[:lastIndex], true
}

func statefulSetReplicaResourceName(name string) (string, bool) {
	separator := strings.LastIndexByte(name, '-')
	if separator <= 0 || separator == len(name)-1 {
		return "", false
	}
	ordinal := name[separator+1:]
	n, err := strconv.Atoi(ordinal)
	if err != nil || n < 0 || strconv.Itoa(n) != ordinal {
		return "", false
	}
	return name[:separator], true
}

func deterministicNodeSetSignerResourceName(name, nodeSetName string) bool {
	return strings.HasPrefix(name, nodeSetName+"-") && strings.HasSuffix(name, "-signer")
}

func managedClaimOneShotName(name, claim string) bool {
	if !strings.HasPrefix(name, claim+"-") {
		return false
	}
	for _, marker := range []string{
		"-tmkms-generate-identity", "-tmkms-vault-upload", "-import", "-pubkey",
	} {
		if strings.HasSuffix(name, marker) || strings.Contains(name, marker+"-") {
			return true
		}
	}
	return false
}

func (r *Reconciler) finalizeConsensusKeyReservationOwner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	if !controllerutil.ContainsFinalizer(nodeSet, cosmosigner.ReservationOwnerFinalizer) {
		hasReservations, err := cosmosigner.HasConsensusKeyReservationsForOwner(ctx, r.uncachedReader(), nodeSet)
		if err != nil || !hasReservations {
			return !hasReservations, err
		}
	}
	pathsGone, err := cosmosigner.FinalizeConsensusKeySigningPaths(ctx, r.uncachedReader(), r.Client, nodeSet, nodeSet.GetNamespace())
	if err != nil || !pathsGone {
		return false, err
	}
	childrenGone, err := r.finalizeReservationOwnerChildren(ctx, nodeSet)
	if err != nil || !childrenGone {
		return false, err
	}
	orphanPodsGone, err := r.reservationOwnerPodsGone(ctx, nodeSet)
	if err != nil || !orphanPodsGone {
		return false, err
	}
	released, done, err := cosmosigner.ReleaseConsensusKeyReservations(ctx, r.uncachedReader(), r.Client, nodeSet)
	if err != nil || !done {
		return false, err
	}
	for _, name := range released {
		if r.recorder != nil {
			r.recorder.Eventf(nodeSet, corev1.EventTypeNormal, appsv1.ReasonConsensusKeyReservationReleased,
				"ConsensusKeyReservation %s released after signing-path teardown", name)
		}
	}
	return true, r.removeConsensusKeyReservationFinalizer(ctx, nodeSet)
}

func (r *Reconciler) finalizeReservationOwnerChildren(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	legacyNames := make(map[string]struct{}, len(nodeSet.Status.Nodes))
	for i := range nodeSet.Status.Nodes {
		if nodeSet.Status.Nodes[i].Name != "" {
			legacyNames[nodeSet.Status.Nodes[i].Name] = struct{}{}
		}
	}
	if err := r.collectOwnerReservationClaims(ctx, nodeSet, legacyNames); err != nil {
		return false, err
	}
	children := &appsv1.ChainNodeList{}
	if err := r.uncachedReader().List(ctx, children, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range children.Items {
		child := &children.Items[i]
		owner := metav1.GetControllerOf(child)
		if owner == nil || owner.UID != nodeSet.GetUID() {
			if _, recorded := legacyNames[child.GetName()]; recorded {
				return false, fmt.Errorf("ChainNode %s/%s is recorded in ChainNodeSet status but is not controlled by exact parent UID %q; refusing reservation release", child.GetNamespace(), child.GetName(), nodeSet.GetUID())
			}
			continue
		}
		if child.GetUID() == "" {
			return false, fmt.Errorf("owned ChainNode %s/%s has no UID; refusing an unguarded delete", child.GetNamespace(), child.GetName())
		}
		uid := child.GetUID()
		if child.GetDeletionTimestamp().IsZero() {
			foreground := metav1.DeletePropagationForeground
			if err := r.Delete(ctx, child, client.Preconditions{UID: &uid}, client.PropagationPolicy(foreground)); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		remaining := &appsv1.ChainNode{}
		if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(child), remaining); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

func (r *Reconciler) reservationOwnerPodsGone(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	knownPodNames := make(map[string]struct{}, len(nodeSet.Status.Nodes))
	for i := range nodeSet.Status.Nodes {
		if nodeSet.Status.Nodes[i].Name != "" {
			knownPodNames[nodeSet.Status.Nodes[i].Name] = struct{}{}
		}
	}
	if err := r.collectOwnerReservationClaims(ctx, nodeSet, knownPodNames); err != nil {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := r.uncachedReader().List(ctx, pods, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if _, known := knownPodNames[pod.GetName()]; known {
			return false, nil
		}
		if pod.GetLabels()[controllers.LabelChainNodeSet] == nodeSet.GetName() &&
			(pod.GetLabels()[controllers.LabelValidator] == controllers.StringValueTrue ||
				pod.GetLabels()[controllers.LabelCosmosignerTarget] != "") {
			return false, nil
		}
	}
	return true, nil
}

func (r *Reconciler) collectOwnerReservationClaims(ctx context.Context, nodeSet *appsv1.ChainNodeSet, claims map[string]struct{}) error {
	reservations := &appsv1.ConsensusKeyReservationList{}
	if err := r.uncachedReader().List(ctx, reservations); err != nil {
		return err
	}
	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.OwnerUID == nodeSet.GetUID() && reservation.Spec.OwnerKind == "ChainNodeSet" &&
			reservation.Spec.Namespace == nodeSet.GetNamespace() && reservation.Spec.OwnerName == nodeSet.GetName() &&
			reservation.Spec.Claim != "" {
			claims[reservation.Spec.Claim] = struct{}{}
		}
	}
	return nil
}

func (r *Reconciler) removeConsensusKeyReservationFinalizer(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	fresh := &appsv1.ChainNodeSet{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(nodeSet), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if fresh.GetUID() != nodeSet.GetUID() {
		return fmt.Errorf("ChainNodeSet %s/%s changed UID while removing reservation finalizer", nodeSet.GetNamespace(), nodeSet.GetName())
	}
	if !controllerutil.ContainsFinalizer(fresh, cosmosigner.ReservationOwnerFinalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(fresh, cosmosigner.ReservationOwnerFinalizer)
	return r.Update(ctx, fresh)
}
