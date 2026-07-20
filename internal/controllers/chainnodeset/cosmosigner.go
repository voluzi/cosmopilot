package chainnodeset

import (
	"context"
	"fmt"
	"sort"
	"strings"

	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

func (r *Reconciler) initCosmosignerReplacementNames(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	desired := nodeSet.ResolveCosmosigners()
	desiredNames := make(map[string]struct{}, len(desired))
	for _, s := range desired {
		desiredNames[s.Name] = struct{}{}
	}

	changed := false
	for _, s := range desired {
		if nodeSet.GetCosmosignerStatus(s.Name) != nil {
			continue
		}
		for i := range nodeSet.Status.Cosmosigners {
			old := &nodeSet.Status.Cosmosigners[i]
			if _, stillDesired := desiredNames[old.Name]; stillDesired {
				continue
			}
			replacement, ok := desiredReplacementSigner(nodeSet, desired, old)
			if !ok || replacement.Name != s.Name {
				continue
			}
			st := nodeSet.EnsureCosmosignerStatus(s.Name)
			st.ResourceName = appsv1.CosmosignerStatusResourceName(old)
			st.AppliedDigest = old.AppliedDigest
			if st.AppliedDigest == "" {
				st.AppliedDigest = old.SigningDigest
			}
			st.PublicKey = old.PublicKey
			st.TargetGroups = append([]string(nil), old.TargetGroups...)
			st.SigningDigest = old.SigningDigest
			if old.AtEstablishment != nil {
				st.AtEstablishment = ptr.To(*old.AtEstablishment)
			}
			st.ServingIdentity = old.ServingIdentity
			st.ServingGroup = old.ServingGroup
			if old.LocalKeyEverServed != nil {
				st.LocalKeyEverServed = ptr.To(*old.LocalKeyEverServed)
			}
			// A missing replica lock must stay missing so initCosmosignerLocks recovers it from the live
			// StatefulSet: defaulting to the replacement spec here would "re-lock" the surviving raft
			// cluster to a membership it was never formed with.
			if old.Replicas != nil {
				st.Replicas = ptr.To(*old.Replicas)
			}
			st.StateStorageSize = old.StateStorageSize
			st.StateStorageClassName = old.StateStorageClassName
			st.KeyImported = old.KeyImported
			if st.AppliedDigest == s.Digest() {
				st.Migration = &appsv1.CosmosignerMigrationStatus{
					DesiredDigest:    s.Digest(),
					DesiredPublicKey: old.PublicKey,
					Phase:            appsv1.CosmosignerMigrationQuiescing,
				}
			}
			changed = true
			break
		}
	}
	if !changed {
		return false, nil
	}
	return true, r.Status().Update(ctx, nodeSet)
}

// reconcileSignerTeardown removes managed signers no longer produced by ResolveCosmosigners. A signer
// with no replacement must be fully torn down before children can return to local/tmKMS signing. A
// replacement inherits the stable resource name only after its migration has removed the StatefulSet
// and verified every pod is gone; the old status entry can then be dropped without deleting retained
// resources needed for recreation.
func (r *Reconciler) reconcileSignerTeardown(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	if group, ok := legacyPerInstanceCosmosignerGroup(nodeSet); ok {
		return false, fmt.Errorf("validator group %q still has legacy per-instance cosmosigners recorded in status; refusing to replace or delete those signing identities during an automatic operator upgrade", group)
	}

	desiredSigners := nodeSet.ResolveCosmosigners()
	desired := map[string]struct{}{}
	for _, s := range desiredSigners {
		desired[s.Name] = struct{}{}
	}

	// A signer StatefulSet owned by this ChainNodeSet but backed by no status entry and no desired
	// signer can only exist when the status was lost or restored incomplete: status entries are
	// recorded before any signer resource is created. Tearing it down blindly could remove a
	// validator's only signing path with no fallback preflight, while ignoring it would let the old
	// signer keep holding privval connections once children move to their fallback/replacement path.
	// Fail closed so the operator restores the status or removes the signer deliberately.
	knownResources := map[string]struct{}{}
	for _, s := range desiredSigners {
		knownResources[nodeSet.CosmosignerResourceName(s)] = struct{}{}
	}
	for i := range nodeSet.Status.Cosmosigners {
		knownResources[appsv1.CosmosignerStatusResourceName(&nodeSet.Status.Cosmosigners[i])] = struct{}{}
	}
	statefulSets := &k8sappsv1.StatefulSetList{}
	if err := r.List(ctx, statefulSets, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if !cosmosigner.IsOwnedSignerStatefulSet(sts, nodeSet) {
			continue
		}
		if _, ok := knownResources[sts.GetName()]; ok {
			continue
		}
		return false, fmt.Errorf("cosmosigner StatefulSet %q is owned by this ChainNodeSet but has no status entry (status lost or restored incomplete): cannot verify the signing identity it serves — restore .status.cosmosigners or delete the signer before reconciling", sts.GetName())
	}

	// Snapshot the recorded signer names: RemoveCosmosignerStatus mutates the slice we would iterate.
	var stale []string
	for _, st := range nodeSet.Status.Cosmosigners {
		if _, ok := desired[st.Name]; !ok {
			stale = append(stale, st.Name)
		}
	}

	allDone := true
	changed := false
	for _, name := range stale {
		st := nodeSet.GetCosmosignerStatus(name)
		if replacement, ok := desiredReplacementSigner(nodeSet, desiredSigners, st); ok {
			replacementStatus := nodeSet.GetCosmosignerStatus(replacement.Name)
			if replacementStatus == nil || replacementStatus.Migration == nil ||
				(replacementStatus.Migration.Phase != appsv1.CosmosignerMigrationRetargeting &&
					replacementStatus.Migration.Phase != appsv1.CosmosignerMigrationRecreating) {
				allDone = false
				continue
			}
			// The replacement has inherited this signer's stable resource name. Its migration already
			// removed the old StatefulSet and verified every pod is gone; keep ConfigMaps, Services and
			// retained PVCs for recreation under the replacement status entry.
			if appsv1.CosmosignerStatusResourceName(replacementStatus) == appsv1.CosmosignerStatusResourceName(st) {
				nodeSet.RemoveCosmosignerStatus(name)
				changed = true
				continue
			}
		}
		resourceName := appsv1.CosmosignerStatusResourceName(st)
		if err := cosmosigner.Undeploy(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), resourceName); err != nil {
			return false, err
		}
		// Teardown completion gates both the status-entry drop and the caller's child reconciliation.
		// Clearing the entry while the old raft cluster is still terminating would let a remove-and-
		// immediate-re-add bypass the replica guard and bind the surviving PVCs with stale membership.
		tornDown, err := cosmosigner.IsTornDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), resourceName)
		if err != nil {
			return false, err
		}
		if !tornDown {
			allDone = false
			continue
		}
		nodeSet.RemoveCosmosignerStatus(name)
		changed = true
	}

	if changed {
		// Tolerate a conflict: the drop is idempotent, so a concurrent update just defers it to the
		// next reconcile rather than spinning the workqueue with no progress.
		if err := r.Status().Update(ctx, nodeSet); err != nil && !errors.IsConflict(err) {
			return false, err
		}
	}
	return allDone, nil
}

func (r *Reconciler) signerTargetReferencesRemain(ctx context.Context, nodeSet *appsv1.ChainNodeSet, signerName string) (bool, error) {
	selector := client.MatchingLabels{
		controllers.LabelChainNodeSet:      nodeSet.GetName(),
		controllers.LabelCosmosignerTarget: signerName,
	}
	children := &appsv1.ChainNodeList{}
	if err := r.List(ctx, children, client.InNamespace(nodeSet.GetNamespace()), selector); err != nil {
		return false, err
	}
	if len(children.Items) > 0 {
		return true, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(nodeSet.GetNamespace()), selector); err != nil {
		return false, err
	}
	return len(pods.Items) > 0, nil
}

func legacyPerInstanceCosmosignerGroup(nodeSet *appsv1.ChainNodeSet) (string, bool) {
	for i := range nodeSet.Spec.Nodes {
		group := &nodeSet.Spec.Nodes[i]
		if group.Validator == nil {
			continue
		}
		if nodeSet.HasLegacyPerInstanceCosmosignerStatus(group.Name) {
			return group.Name, true
		}
	}
	return "", false
}

// preflightCosmosigners fails the reconcile when any desired signer cannot be deployed, so children
// are not switched to a remote signer that will never come up. It is READ-ONLY (resolves params and
// reads Secrets; never applies resources). It runs before ensureValidator/ensureNodes stamp
// RemoteSignerTarget on the child ChainNodes, so a bad signer spec leaves the validators on their
// existing local signing path instead of dropping the local key. Genuinely-pending states (a
// validator key-generation flow that has not run yet) are NOT failures.
func (r *Reconciler) preflightCosmosigners(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	resourceNames := map[string]string{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		resourceName := nodeSet.CosmosignerResourceName(s)
		if previous, duplicate := resourceNames[resourceName]; duplicate {
			return fmt.Errorf("cosmosigners %q and %q resolve to the same stable resource name %q; complete or revert the previous placement migration", previous, s.Name, resourceName)
		}
		resourceNames[resourceName] = s.Name
		if err := r.requireGenesisSentrySecrets(ctx, nodeSet, s); err != nil {
			return err
		}
		// Resolving params verifies a software backend's key secret and the backend's referenced auth
		// Secrets (Vault token/certificate, GCP credentials) exist (unless a validator key-generation
		// flow is still pending — handled inside cosmosignerBackend).
		params, err := r.cosmosignerParams(ctx, nodeSet, s)
		if err != nil {
			return err
		}
		// Run the same deploy-time blockers ApplyOwned would hit when it creates the StatefulSet (name
		// collision, foreign/ambiguous raft-state PVCs), so a signer that can never be created does not
		// cause children to be retargeted to a remote signer that will never come up. Only an
		// uploadGenerated signer runs the one-shot <name>-import pod, so only it checks that name.
		usesImportPod := s.Spec.VaultUploadsGenerated(signerTargetInitializesGenesis(nodeSet, s))
		usesPubkeyPod := !s.Spec.UsesSoftwareBackend() && !usesImportPod
		if err := cosmosigner.PreflightDeployable(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), resourceName, s.Spec.GetReplicas(), usesImportPod, usesPubkeyPod); err != nil {
			return err
		}
		st := nodeSet.GetCosmosignerStatus(s.Name)
		if st != nil && st.AppliedDigest == "" && st.SigningDigest == "" && st.Replicas != nil && st.StateStorageSize != "" {
			if err := cosmosigner.ValidateRecoveredSigningIdentity(ctx, r.Client, nodeSet, params); err != nil {
				return err
			}
			if err := r.validateRecoveredSignerTargets(ctx, nodeSet, s, resourceName); err != nil {
				return err
			}
		}
		// The raft mTLS Secret (when set) is mounted at startup; a missing/incomplete one keeps every
		// signer pod from coming up. Verify it before children are retargeted, like the backend auth Secrets.
		if err := cosmosigner.RequireRaftTLSSecret(ctx, r.Client, nodeSet.GetNamespace(), s.Spec.RaftTLSSecret); err != nil {
			return err
		}
		// The signer StatefulSet and import pod run as the configured ServiceAccount; a missing one keeps
		// Kubernetes from starting them. Verify it before children are retargeted.
		if err := cosmosigner.RequireServiceAccount(ctx, r.Client, nodeSet.GetNamespace(), s.Spec.GetServiceAccountName()); err != nil {
			return err
		}
		// A Vault uploadGenerated signer imports the validator's own key; if that source secret is
		// missing and no controller flow will create it, the signer can never roll out, so fail before
		// children switch. But the source is only bootstrap material: once the import for the CURRENT
		// target/source has completed (matching KeyImported, mirroring maybeImportCosmosignerKey's
		// absent-source fast path), or the current signer digest has served, Vault already holds the key
		// and the missing Secret is fine. A changed digest must validate its import source before teardown.
		if s.Spec.VaultUploadsGenerated(signerTargetInitializesGenesis(nodeSet, s)) {
			if st != nil && st.SigningDigest != "" && s.Digest() == st.SigningDigest {
				continue
			}
			if st != nil && appsv1.ImportRecordMatchesTarget(st.KeyImported, s.Spec.Backend.Vault.ImportTargetFingerprint(s.SoftwareKeySecret)) {
				continue
			}
			if r.signerImportSourcePending(nodeSet, s) {
				continue
			}
			keyMaterial, err := r.secretKey(ctx, nodeSet.GetNamespace(), s.SoftwareKeySecret, privKeyFilename)
			if err != nil {
				return err
			}
			if len(keyMaterial) == 0 {
				return fmt.Errorf("cosmosigner %q Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", s.Name, s.SoftwareKeySecret, privKeyFilename)
			}
			if _, err := cometbft.LoadPrivKey(keyMaterial); err != nil {
				return fmt.Errorf("cosmosigner %q Vault uploadGenerated source secret %q contains an invalid %s: %w", s.Name, s.SoftwareKeySecret, privKeyFilename, err)
			}
		}
	}
	return nil
}

// validateRecoveredSignerTargets fails closed when a signer being recovered from live state (its
// recorded digests are gone) still serves pods whose group the current spec no longer targets.
// ValidateRecoveredSigningIdentity proves the live signer uses the desired backend, but not WHICH
// nodes it signs for: the cosmosigner-target label on live pods is the last applied truth, and a
// spec restored with different targets would otherwise rewrite children with none of the
// migration/fallback guards the missing status would have triggered.
func (r *Reconciler) validateRecoveredSignerTargets(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner, resourceName string) error {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(nodeSet.GetNamespace()), client.MatchingLabels{
		controllers.LabelChainNodeSet:      nodeSet.GetName(),
		controllers.LabelCosmosignerTarget: resourceName,
	}); err != nil {
		return err
	}
	desired := make(map[string]struct{}, len(s.TargetGroups))
	for _, group := range s.TargetGroups {
		desired[group] = struct{}{}
	}
	for i := range pods.Items {
		group := pods.Items[i].GetLabels()[controllers.LabelChainNodeSetGroup]
		if _, ok := desired[group]; !ok {
			return fmt.Errorf("cosmosigner %q is still serving pods of group %q, which the current spec no longer targets, and its recorded status was lost: refusing to adopt the live signer under the new targets — restore .status.cosmosigners or the previous target set", s.Name, group)
		}
	}
	return nil
}

func (r *Reconciler) reconcileCosmosignerMigrations(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	pending := false
	changed := false
	for _, s := range nodeSet.ResolveCosmosigners() {
		st := nodeSet.EnsureCosmosignerStatus(s.Name)
		desiredDigest := s.Digest()
		resourceName := nodeSet.CosmosignerResourceName(s)

		if st.AppliedDigest == "" {
			if st.SigningDigest != "" && st.SigningDigest != desiredDigest {
				return false, fmt.Errorf("cosmosigner %q applied public key was not recorded before the signing configuration changed; restore the previous configuration for one reconcile before migrating", s.Name)
			}
			rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), resourceName, s.Spec.GetReplicas())
			if err != nil {
				return false, err
			}
			if rolledOut {
				publicKey, err := r.cosmosignerPublicKey(ctx, nodeSet, s)
				if err != nil {
					return false, err
				}
				st.AppliedDigest = desiredDigest
				st.PublicKey = publicKey
				st.TargetGroups = sortedTargetGroups(s)
				changed = true
			}
			continue
		}

		if st.AppliedDigest == desiredDigest && st.Migration == nil {
			continue
		}
		if st.Migration == nil || st.Migration.DesiredDigest != desiredDigest {
			if st.PublicKey == "" {
				return false, fmt.Errorf("cosmosigner %q applied public key is missing; restore the previous configuration so it can be recorded before migrating", s.Name)
			}
			publicKey, err := r.cosmosignerPublicKey(ctx, nodeSet, s)
			if err != nil {
				return false, err
			}
			st.Migration = &appsv1.CosmosignerMigrationStatus{
				DesiredDigest:    desiredDigest,
				DesiredPublicKey: publicKey,
				Phase:            appsv1.CosmosignerMigrationQuiescing,
				ResetState:       publicKey != st.PublicKey,
			}
			changed = true
			pending = true
			continue
		}
		if st.Migration.Phase == appsv1.CosmosignerMigrationRetargeting ||
			st.Migration.Phase == appsv1.CosmosignerMigrationRollingOut {
			continue
		}

		ready, next, err := cosmosigner.ReconcileStatefulSetMigration(
			ctx, r.Client, nodeSet, nodeSet.GetNamespace(), resourceName, st.Migration.Phase, st.Migration.ResetState,
		)
		if err != nil {
			return false, err
		}
		if st.Migration.Phase == appsv1.CosmosignerMigrationResettingState &&
			next == appsv1.CosmosignerMigrationRecreating {
			next = appsv1.CosmosignerMigrationRetargeting
		}
		if next != st.Migration.Phase {
			st.Migration.Phase = next
			changed = true
		}
		if ready && st.Migration.Phase == appsv1.CosmosignerMigrationRecreating {
			st.Migration.Phase = appsv1.CosmosignerMigrationRollingOut
			changed = true
			pending = true
			continue
		}
		pending = pending || !ready || changed
	}
	if changed {
		if err := r.Status().Update(ctx, nodeSet); err != nil {
			return false, err
		}
	}
	return pending, nil
}

func hasRetargetingCosmosignerMigration(nodeSet *appsv1.ChainNodeSet) bool {
	for i := range nodeSet.Status.Cosmosigners {
		migration := nodeSet.Status.Cosmosigners[i].Migration
		if migration != nil && migration.Phase == appsv1.CosmosignerMigrationRetargeting {
			return true
		}
	}
	return false
}

func (r *Reconciler) reconcileCosmosignerRetargeting(ctx context.Context, nodeSet *appsv1.ChainNodeSet, blocked blockedSignerTargets) (bool, error) {
	for _, s := range nodeSet.ResolveCosmosigners() {
		st := nodeSet.GetCosmosignerStatus(s.Name)
		if st == nil || st.Migration == nil || st.Migration.Phase != appsv1.CosmosignerMigrationRetargeting {
			continue
		}
		if _, isBlocked := blocked[s.Name]; isBlocked {
			continue
		}
		gone, err := cosmosigner.DeleteDiscoveryService(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), nodeSet.CosmosignerResourceName(s))
		if err != nil || !gone {
			return false, err
		}
		endpointsGone, err := cosmosigner.DiscoveryEndpointsGone(ctx, r.Client, nodeSet.GetNamespace(), nodeSet.CosmosignerResourceName(s))
		if err != nil || !endpointsGone {
			return false, err
		}
	}

	if err := r.ensureValidatorWithBlockedSignerTargets(ctx, nodeSet, blocked); err != nil {
		return false, err
	}
	if err := r.ensureNodesWithBlockedSignerTargets(ctx, nodeSet, blocked); err != nil {
		return false, err
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(nodeSet.GetNamespace()), client.MatchingLabels{
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		group := pod.GetLabels()[controllers.LabelChainNodeSetGroup]
		want, targeted := signerNameForNodeWithBlockedTargets(nodeSet, group, blocked)
		got := pod.GetLabels()[controllers.LabelCosmosignerTarget]
		if (targeted && got != want) || (!targeted && got != "") {
			return false, nil
		}
	}

	changed := false
	for i := range nodeSet.Status.Cosmosigners {
		status := &nodeSet.Status.Cosmosigners[i]
		if _, isBlocked := blocked[status.Name]; isBlocked {
			continue
		}
		migration := status.Migration
		if migration != nil && migration.Phase == appsv1.CosmosignerMigrationRetargeting {
			migration.Phase = appsv1.CosmosignerMigrationRecreating
			changed = true
		}
	}
	if changed {
		if err := r.Status().Update(ctx, nodeSet); err != nil {
			return false, err
		}
		return false, nil
	}
	return !hasRetargetingCosmosignerMigration(nodeSet), nil
}

func (r *Reconciler) cosmosignerPublicKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) (string, error) {
	params, err := r.cosmosignerParams(ctx, nodeSet, s)
	if err != nil {
		return "", err
	}
	if params.Backend.Software != nil {
		return cosmosigner.PublicKeyFromSecret(ctx, r.Client, nodeSet.GetNamespace(), params.Backend.Software.SecretName)
	}
	if s.Spec.VaultUploadsGenerated(signerTargetInitializesGenesis(nodeSet, s)) && s.SoftwareKeySecret != "" {
		publicKey, err := cosmosigner.PublicKeyFromSecret(ctx, r.Client, nodeSet.GetNamespace(), s.SoftwareKeySecret)
		if err == nil {
			return publicKey, nil
		}
		status := nodeSet.GetCosmosignerStatus(s.Name)
		vault := s.Spec.Backend.Vault
		if !errors.IsNotFound(err) || status == nil || vault == nil ||
			!appsv1.ImportRecordMatchesTarget(status.KeyImported, vault.ImportTargetFingerprint(s.SoftwareKeySecret)) {
			return "", err
		}
	}
	if r.ClientSet == nil {
		return "", fmt.Errorf("cosmosigner %q public-key preflight requires a Kubernetes clientset", s.Name)
	}
	runner := cosmosigner.JobRunner{Client: r.ClientSet, Scheme: r.Scheme, Owner: nodeSet, Params: params}
	return runner.PublicKey(ctx)
}

// preflightRemovedSignerFallbacks verifies the validator signing path that will replace each stale
// signer before teardown makes that signer unavailable.
func (r *Reconciler) preflightRemovedSignerFallbacks(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	desiredSigners := nodeSet.ResolveCosmosigners()
	desired := map[string]struct{}{}
	for _, s := range desiredSigners {
		desired[s.Name] = struct{}{}
	}

	for i := range nodeSet.Status.Cosmosigners {
		st := &nodeSet.Status.Cosmosigners[i]
		if _, ok := desired[st.Name]; ok {
			continue
		}
		if st.ServingGroup == "" {
			if st.SigningDigest != "" {
				return fmt.Errorf("cosmosigner %q cannot be removed: its served validator group was not recorded; restore the previous signer configuration for one reconcile before removing it", st.Name)
			}
			continue
		}
		if _, replaced := desiredReplacementSigner(nodeSet, desiredSigners, st); replaced {
			continue
		}
		if st.ServingIdentity == "" && st.SigningDigest == "" {
			live, err := r.ownedSignerStatefulSetExists(ctx, nodeSet, appsv1.CosmosignerStatusResourceName(st))
			if err != nil {
				return err
			}
			if !live {
				continue
			}
		}

		var cfg *appsv1.NodeSetValidatorConfig
		if st.ServingGroup == appsv1.ReservedValidatorGroupName {
			cfg = nodeSet.Spec.Validator
		} else {
			for j := range nodeSet.Spec.Nodes {
				if nodeSet.Spec.Nodes[j].Name == st.ServingGroup {
					cfg = nodeSet.Spec.Nodes[j].Validator
					break
				}
			}
		}
		if cfg == nil {
			return fmt.Errorf("cosmosigner %q cannot be removed: validator group %q has no fallback signing path", st.Name, st.ServingGroup)
		}

		if cfg.TmKMS != nil {
			hashicorp := cfg.TmKMS.Provider.Hashicorp
			if hashicorp == nil {
				return fmt.Errorf("cosmosigner %q cannot be removed: validator group %q has no supported tmKMS provider configured", st.Name, st.ServingGroup)
			}
			if strings.TrimSpace(hashicorp.Address) == "" || strings.TrimSpace(hashicorp.Key) == "" {
				return fmt.Errorf("cosmosigner %q cannot be removed: validator group %q tmKMS Hashicorp address and key are required", st.Name, st.ServingGroup)
			}
			if err := r.requireTmKMSSecret(ctx, nodeSet.GetNamespace(), "tmKMS Vault token", hashicorp.TokenSecret); err != nil {
				return fmt.Errorf("cosmosigner %q cannot be removed: %w", st.Name, err)
			}
			if hashicorp.CertificateSecret != nil {
				if err := r.requireTmKMSSecret(ctx, nodeSet.GetNamespace(), "tmKMS Vault certificate", hashicorp.CertificateSecret); err != nil {
					return fmt.Errorf("cosmosigner %q cannot be removed: %w", st.Name, err)
				}
			}
			if nodeSet.ValidatorGroupResolvesSigningIdentity(st.ServingGroup, cfg, st.ServingIdentity) {
				continue
			}
			publicKey, err := r.fallbackTmKMSPublicKey(ctx, nodeSet, st, hashicorp, cfg.Config.GetServiceAccountName())
			if err != nil {
				return fmt.Errorf("cosmosigner %q cannot be removed: %w", st.Name, err)
			}
			if err := requireMatchingRemovedSignerPublicKey(st, publicKey); err != nil {
				return err
			}
			continue
		}

		validator := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
			Name:      validatorNodeName(nodeSet, st.ServingGroup, 0),
			Namespace: nodeSet.GetNamespace(),
		}}
		secretName := cfg.GetPrivKeySecretName(validator)
		keyMaterial, err := r.secretKey(ctx, nodeSet.GetNamespace(), secretName, privKeyFilename)
		if err != nil {
			return fmt.Errorf("preflight local signing fallback for cosmosigner %q: %w", st.Name, err)
		}
		if len(keyMaterial) == 0 {
			return fmt.Errorf("cosmosigner %q cannot be removed: local signing fallback secret %q is missing required key %q", st.Name, secretName, privKeyFilename)
		}
		publicKey, err := cosmosigner.PublicKeyFromSecret(ctx, r.Client, nodeSet.GetNamespace(), secretName)
		if err != nil {
			return fmt.Errorf("cosmosigner %q cannot be removed: local signing fallback secret %q contains an invalid %s", st.Name, secretName, privKeyFilename)
		}
		if err := requireMatchingRemovedSignerPublicKey(st, publicKey); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) fallbackTmKMSPublicKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, st *appsv1.CosmosignerStatus, hashicorp *appsv1.TmKmsHashicorpProvider, serviceAccountName string) (string, error) {
	if r.ClientSet == nil {
		return "", fmt.Errorf("fallback public-key preflight requires a Kubernetes clientset")
	}
	image := appsv1.DefaultCosmosignerImage
	if r.opts != nil && r.opts.CosmosignerImage != "" {
		image = r.opts.CosmosignerImage
	}
	runner := cosmosigner.JobRunner{
		Client: r.ClientSet,
		Scheme: r.Scheme,
		Owner:  nodeSet,
		Params: cosmosigner.Params{
			Name:               appsv1.CosmosignerStatusResourceName(st),
			Namespace:          nodeSet.GetNamespace(),
			Image:              image,
			ServiceAccountName: serviceAccountName,
			Backend: cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{
				Address:               hashicorp.Address,
				KeyName:               hashicorp.Key,
				Mount:                 appsv1.DefaultCosmosignerVaultMount,
				TokenSecret:           hashicorp.TokenSecret,
				CertificateSecret:     hashicorp.CertificateSecret,
				AutoRenewToken:        hashicorp.AutoRenewToken,
				SkipCertificateVerify: hashicorp.SkipCertificateVerify,
			}},
		},
	}
	return runner.PublicKey(ctx)
}

func requireMatchingRemovedSignerPublicKey(st *appsv1.CosmosignerStatus, fallback string) error {
	if st.PublicKey == "" {
		return fmt.Errorf("cosmosigner %q cannot be removed: its applied public key was not recorded; restore the previous signer configuration for one reconcile before removing it", st.Name)
	}
	if fallback != st.PublicKey {
		return fmt.Errorf("cosmosigner %q cannot be removed: fallback signing public key does not match the applied signer public key", st.Name)
	}
	return nil
}

func desiredReplacementSigner(nodeSet *appsv1.ChainNodeSet, desired []appsv1.ResolvedSigner, st *appsv1.CosmosignerStatus) (appsv1.ResolvedSigner, bool) {
	if st == nil {
		return appsv1.ResolvedSigner{}, false
	}
	for _, s := range desired {
		if st.ServingGroup == "" {
			if st.AtEstablishment != nil && *st.AtEstablishment != "" &&
				nodeSet.GenesisSentryEstablishmentIdentity(s) == *st.AtEstablishment {
				return s, true
			}
			if equalStringSet(st.TargetGroups, s.TargetGroups) {
				return s, true
			}
			continue
		}
		if s.ValidatorGroup != st.ServingGroup {
			continue
		}
		switch {
		case st.ServingIdentity != "" && s.ValidatorTargetedIdentity() == st.ServingIdentity:
			return s, true
		case st.ServingIdentity == "" && st.SigningDigest == "":
			return s, true
		}
	}
	return appsv1.ResolvedSigner{}, false
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedTargetGroups(s appsv1.ResolvedSigner) []string {
	groups := append([]string(nil), s.TargetGroups...)
	sort.Strings(groups)
	return groups
}

func (r *Reconciler) requireTmKMSSecret(ctx context.Context, namespace, purpose string, selector *corev1.SecretKeySelector) error {
	if selector == nil || selector.Name == "" || selector.Key == "" {
		return fmt.Errorf("%s secret selector must set both name and key", purpose)
	}
	data, err := r.secretKey(ctx, namespace, selector.Name, selector.Key)
	if err != nil {
		return fmt.Errorf("read %s secret %q: %w", purpose, selector.Name, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("%s secret %q is missing required key %q", purpose, selector.Name, selector.Key)
	}
	return nil
}

type blockedSignerTargets map[string]struct{}

// prepareCosmosignerImports completes every uploadGenerated import whose source key already exists.
// It runs before child reconciliation so an import failure cannot strand a validator after its local
// signing path has been replaced. ready is false while an existing signer is still scaling down for
// a safe re-import. Missing controller-generated source keys are left for the child bootstrap flow.
func (r *Reconciler) prepareCosmosignerImports(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (blockedSignerTargets, bool, error) {
	blocked := blockedSignerTargets{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		if s.Spec.UsesSoftwareBackend() && r.signerImportSourcePending(nodeSet, s) {
			keyMaterial, err := r.secretKey(ctx, nodeSet.GetNamespace(), s.SoftwareKeySecret, privKeyFilename)
			if err != nil {
				return nil, false, err
			}
			if len(keyMaterial) > 0 {
				if _, err := cometbft.LoadPrivKey(keyMaterial); err != nil {
					return nil, false, fmt.Errorf("cosmosigner %q software key secret %q contains an invalid %s: %w", s.Name, s.SoftwareKeySecret, privKeyFilename, err)
				}
			}
			live, err := r.ownedSignerStatefulSetExists(ctx, nodeSet, nodeSet.CosmosignerResourceName(s))
			if err != nil {
				return nil, false, err
			}
			if live {
				return nil, false, fmt.Errorf("cosmosigner %q has live state before its software key can be proven; refusing to switch child signing paths", s.Name)
			}
			blocked[s.Name] = struct{}{}
			continue
		}
		if !s.Spec.VaultUploadsGenerated(signerTargetInitializesGenesis(nodeSet, s)) {
			continue
		}
		if nodeSet.Status.ChainID == "" {
			exists, err := r.ownedSignerStatefulSetExists(ctx, nodeSet, nodeSet.CosmosignerResourceName(s))
			if err != nil {
				return nil, false, err
			}
			if exists {
				return nil, false, fmt.Errorf("cosmosigner %q has live state before its generated key import can be proven; refusing to switch child signing paths", s.Name)
			}
			blocked[s.Name] = struct{}{}
			continue
		}
		st := nodeSet.GetCosmosignerStatus(s.Name)
		if st != nil && st.SigningDigest != "" && s.Digest() == st.SigningDigest {
			continue
		}
		keyMaterial, err := r.secretKey(ctx, nodeSet.GetNamespace(), s.SoftwareKeySecret, privKeyFilename)
		if err != nil {
			return nil, false, err
		}
		if len(keyMaterial) == 0 {
			if st != nil && appsv1.ImportRecordMatchesTarget(st.KeyImported, s.Spec.Backend.Vault.ImportTargetFingerprint(s.SoftwareKeySecret)) {
				continue
			}
			if r.signerImportSourcePending(nodeSet, s) {
				exists, err := r.ownedSignerStatefulSetExists(ctx, nodeSet, nodeSet.CosmosignerResourceName(s))
				if err != nil {
					return nil, false, err
				}
				if exists {
					return nil, false, fmt.Errorf("cosmosigner %q has live state but its generated key import is unrecorded and the source key is unavailable; refusing to switch child signing paths", s.Name)
				}
				blocked[s.Name] = struct{}{}
				continue
			}
		} else if st != nil && st.KeyImported == s.Spec.Backend.Vault.ImportFingerprint(s.SoftwareKeySecret, keyMaterial) {
			continue
		}

		params, err := r.cosmosignerParams(ctx, nodeSet, s)
		if err != nil {
			return nil, false, err
		}
		pending, _, err := r.maybeImportCosmosignerKey(ctx, nodeSet, s, params)
		if err != nil {
			return nil, false, err
		}
		if pending {
			return nil, false, nil
		}
	}
	return blocked, true, nil
}

func (r *Reconciler) ownedSignerStatefulSetExists(ctx context.Context, nodeSet *appsv1.ChainNodeSet, name string) (bool, error) {
	sts := &k8sappsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return metav1.IsControlledBy(sts, nodeSet), nil
}

func (r *Reconciler) requireGenesisSentrySecrets(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) error {
	if nodeSet.Status.ChainID != "" || nodeSet.GenesisSentryEstablishmentIdentity(s) == "" {
		return nil
	}

	var match *appsv1.GenesisValidator
	find := func(init *appsv1.GenesisInitConfig) {
		if init == nil || match != nil {
			return
		}
		for i := range init.GenesisValidators {
			if init.GenesisValidators[i].PrivKeySecret == s.SoftwareKeySecret {
				match = &init.GenesisValidators[i]
				return
			}
		}
	}
	if nodeSet.Spec.Validator != nil {
		find(nodeSet.Spec.Validator.Init)
	}
	for i := range nodeSet.Spec.Nodes {
		group := &nodeSet.Spec.Nodes[i]
		if group.Validator != nil && group.GetInstances() > 0 {
			find(group.Validator.Init)
		}
	}
	if match == nil {
		return nil
	}

	required := []struct {
		name string
		key  string
		kind string
	}{
		{name: match.PrivKeySecret, key: privKeyFilename, kind: "private-key"},
		{name: match.AccountMnemonicSecret, key: mnemonicKey, kind: "account-mnemonic"},
	}
	for _, secret := range required {
		exists, err := r.secretHasKey(ctx, nodeSet.GetNamespace(), secret.name, secret.key)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("genesis-sentry %s secret %q is missing required key %q: init.genesisValidators entries are operator-provided and must exist before signer preflight", secret.kind, secret.name, secret.key)
		}
	}
	return nil
}

func (r *Reconciler) ensureCosmosignerWithBlockedTargets(ctx context.Context, nodeSet *appsv1.ChainNodeSet, blocked blockedSignerTargets) error {
	signers := nodeSet.ResolveCosmosigners()
	if len(signers) == 0 {
		return nil
	}

	// The signer config needs the chain ID; wait for genesis to be available.
	if nodeSet.Status.ChainID == "" {
		return nil
	}

	// Locks are normally recorded and validated in Reconcile before children are retargeted. Keep this
	// defensive initialization for direct callers, and enforce the recorded values before deployment.
	if changed, err := r.initCosmosignerLocks(ctx, nodeSet); err != nil {
		return err
	} else if err := validateRecordedCosmosignerLocks(nodeSet); err != nil {
		return err
	} else if changed {
		return nil
	}

	changed := false
	for _, s := range signers {
		if _, ok := blocked[s.Name]; ok {
			continue
		}
		c, err := r.reconcileSigner(ctx, nodeSet, s)
		if err != nil {
			return err
		}
		changed = changed || c
	}
	if changed {
		return r.Status().Update(ctx, nodeSet)
	}
	return nil
}

// prepareCosmosignerRollouts applies desired signers and waits for every non-bootstrap target to be
// ready before child reconciliation can publish its remote-signer path.
func (r *Reconciler) prepareCosmosignerRollouts(ctx context.Context, nodeSet *appsv1.ChainNodeSet, blocked blockedSignerTargets) (bool, error) {
	if err := r.ensureCosmosignerWithBlockedTargets(ctx, nodeSet, blocked); err != nil {
		return false, err
	}
	for _, s := range nodeSet.ResolveCosmosigners() {
		if _, ok := blocked[s.Name]; ok {
			continue
		}
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), nodeSet.CosmosignerResourceName(s), s.Spec.GetReplicas())
		if err != nil {
			return false, err
		}
		if !rolledOut {
			return false, nil
		}
	}
	return true, nil
}

// initCosmosignerLocks persists a status entry and the raft-membership/PVC-template locks for every
// desired signer, plus the at-establishment marker for a genesis-registered software sentry whose entry
// is first created after establishment. It reports whether anything was written. It is invoked from
// Reconcile BEFORE ensureValidator/ensureNodes retarget children, so the entries and locks exist before
// a child is switched to a signer whose StatefulSet is created later in the same reconcile — otherwise a
// retargeted validator would point at a signer that does not yet exist. The caller requeues when it
// returns true so validateForReconcile runs against the fresh locks before any signer resource is
// applied (a legacy/status-lost signer with a live 1-replica cluster must not record the live lock and
// then apply a replicas:3 change in the same pass). Recording an entry also lets reconcileSignerTeardown
// see the signer. No-op once everything is recorded; safe to call repeatedly.
func (r *Reconciler) initCosmosignerLocks(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	if nodeSet.Status.ChainID == "" {
		return false, nil
	}
	changed := false
	for _, s := range nodeSet.ResolveCosmosigners() {
		st := nodeSet.EnsureCosmosignerStatus(s.Name)
		if s.TargetsValidator() && st.SigningDigest == "" && st.ServingGroup == "" {
			st.ServingGroup = s.ValidatorGroup
			changed = true
		}
		usesLocalKey := nodeSet.SignerUsesLocalValidatorKey(s)
		if s.TargetsValidator() && (st.LocalKeyEverServed == nil || (usesLocalKey && !*st.LocalKeyEverServed)) {
			st.LocalKeyEverServed = ptr.To(true)
			changed = true
		}
		if !s.TargetsValidator() && st.AtEstablishment == nil {
			id := nodeSet.GenesisSentryEstablishmentIdentity(s)
			st.AtEstablishment = &id
			changed = true
		}
		if st.Replicas == nil || st.StateStorageSize == "" {
			if err := r.initSignerLock(ctx, nodeSet, s, st); err != nil {
				return false, err
			}
			changed = true
		}
	}
	if changed {
		return true, r.Status().Update(ctx, nodeSet)
	}
	return false, nil
}

// initSignerLock initialises a signer's Replicas/StateStorageSize/ClassName from the live signer
// state owned by this controller, falling back to the spec when no signer state exists (a true first
// rollout). Anchoring on the live state prevents an in-flight roll-out (failed first reconcile, or an
// already-deployed signer whose status was lost) from being "re-locked" to a different replica count
// or PVC template than the one the raft cluster was actually formed with. It mutates st in memory;
// the caller persists it.
func (r *Reconciler) initSignerLock(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner, st *appsv1.CosmosignerStatus) error {
	liveReplicas, liveSize, liveClass, foundReplicas, foundStorage, err := cosmosigner.ReadSignerLock(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), nodeSet.CosmosignerResourceName(s))
	if err != nil {
		return err
	}
	if st.Replicas == nil {
		if foundReplicas {
			st.Replicas = ptr.To(liveReplicas)
		} else {
			st.Replicas = ptr.To(s.Spec.GetReplicas())
		}
	}
	if st.StateStorageSize == "" {
		if foundStorage {
			st.StateStorageSize = liveSize
			st.StateStorageClassName = liveClass
		} else {
			st.StateStorageSize = s.Spec.GetStateStorageSize()
			st.StateStorageClassName = s.Spec.StorageClassName
		}
	}
	return nil
}

// reconcileSigner deploys one resolved signer and records its persisted status invariants, mutating
// nodeSet.Status in memory. It returns whether a status field changed (so the caller batches a single
// status write for all signers). Nothing is persisted here directly except through that batched write.
func (r *Reconciler) reconcileSigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) (bool, error) {
	params, err := r.cosmosignerParams(ctx, nodeSet, s)
	if err != nil {
		return false, err
	}

	// The status entry was persisted before any resource creation (so teardown
	// can always discover this signer from status, even across a crash).
	changed := false
	st := nodeSet.EnsureCosmosignerStatus(s.Name)

	// Import a locally-generated key into Vault when requested (one-shot, once-only). Returns pending
	// when the validator has not produced its key yet.
	importPending, importChanged, err := r.maybeImportCosmosignerKey(ctx, nodeSet, s, params)
	if err != nil {
		return false, err
	}
	changed = changed || importChanged

	// Render once: the ConfigMap contents and the pod-template ROLLME hash come from the same render.
	configYAML, err := params.ConfigYAML()
	if err != nil {
		return false, err
	}
	configMap, err := params.ConfigMap(configYAML)
	if err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, configMap); err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, params.RaftService()); err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, params.DiscoveryService()); err != nil {
		return false, err
	}

	// Do not roll out the signer until the validator's generated key has been imported into Vault,
	// otherwise the signer would come up against an empty/stale transit key while the validator
	// registers the local pubkey. An ALREADY-RUNNING signer is scaled to zero for the same reason:
	// after a source/target change it would keep signing with the previously imported key while the
	// re-import is pending. A later reconcile completes the import and (re)deploys the signer.
	if importPending {
		if _, err := cosmosigner.ScaleDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), params.Name); err != nil {
			return false, err
		}
		return changed, nil
	}

	sts, err := params.StatefulSet(configYAML)
	if err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, sts); err != nil {
		return false, err
	}

	// Persist lifecycle identity only after the desired generation is fully rolled out. AppliedDigest
	// and PublicKey cover every signer; SigningDigest and serving fields retain their validator-only
	// admission semantics.
	needServing := (s.TargetsValidator() && st.SigningDigest == "") ||
		(st.SigningDigest != "" && st.ServingIdentity == "" && s.TargetsValidator())
	needApplied := st.AppliedDigest != s.Digest() || st.PublicKey == "" || st.Migration != nil
	if needServing || needApplied {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return false, err
		}
		if rolledOut {
			if needApplied {
				publicKey := ""
				if st.Migration != nil && st.Migration.DesiredDigest == s.Digest() {
					publicKey = st.Migration.DesiredPublicKey
				}
				if publicKey == "" {
					publicKey, err = r.cosmosignerPublicKey(ctx, nodeSet, s)
					if err != nil {
						return false, err
					}
				}
				st.AppliedDigest = s.Digest()
				st.PublicKey = publicKey
				st.TargetGroups = sortedTargetGroups(s)
				st.Migration = nil
				if s.TargetsValidator() {
					st.SigningDigest = s.Digest()
					st.ServingIdentity = s.ValidatorTargetedIdentity()
					st.ServingGroup = s.ValidatorGroup
				}
				changed = true
			}
			if needServing {
				st.SigningDigest = s.Digest()
				st.ServingIdentity = s.ValidatorTargetedIdentity()
				st.ServingGroup = s.ValidatorGroup
				changed = true
			}
		}
	}
	return changed, nil
}

// cosmosignerParams resolves the builder parameters for one signer, ensuring the software key secret
// exists when the software backend is used.
func (r *Reconciler) cosmosignerParams(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) (cosmosigner.Params, error) {
	c := s.Spec

	// Signer pods/resources must never inherit internal selector labels (group/global Service
	// selectors, P2P peer discovery, resource-cleanup selectors) — see
	// controllers.CosmosignerReservedSelectorLabels. The generated global ingress/gateway membership
	// label names are per-nodeset and appended below.
	exclude := controllers.CosmosignerReservedSelectorLabels()
	for _, ingress := range nodeSet.Spec.Ingresses {
		exclude = append(exclude, ingress.GetName(nodeSet))
	}
	for _, gw := range nodeSet.Spec.GatewayRoutes {
		exclude = append(exclude, gw.GetName(nodeSet))
	}
	labels := utils.ExcludeMapKeys(WithChainNodeSetLabels(nodeSet, map[string]string{
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}), exclude...)

	backend, err := r.cosmosignerBackend(ctx, nodeSet, s)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:               nodeSet.CosmosignerResourceName(s),
		Namespace:          nodeSet.GetNamespace(),
		OwnerUID:           nodeSet.GetUID(),
		ChainID:            nodeSet.Status.ChainID,
		Image:              c.GetImage(r.opts.CosmosignerImage),
		Replicas:           c.GetReplicas(),
		LogLevel:           c.GetLogLevel(),
		StateStorageSize:   c.GetStateStorageSize(),
		StorageClassName:   c.StorageClassName,
		Resources:          c.GetResources(),
		RaftTLSSecret:      c.RaftTLSSecret,
		ServiceAccountName: c.GetServiceAccountName(),
		Backend:            backend,
		Labels:             labels,
		TargetSelector: map[string]string{
			controllers.LabelChainNodeSet:      nodeSet.GetName(),
			controllers.LabelCosmosignerTarget: nodeSet.CosmosignerResourceName(s),
		},
	}, nil
}

// cosmosignerBackend translates a signer's CRD backend into the builder backend. Key material is never
// generated here: the software backend references either an explicit secret or the targeted validator
// instance's own key secret (produced by its genesis/create-validator flow), so the signer always
// signs with the exact consensus key registered on-chain.
func (r *Reconciler) cosmosignerBackend(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) (cosmosigner.Backend, error) {
	c := s.Spec
	switch {
	case c.UsesSoftwareBackend():
		secretName := s.SoftwareKeySecret
		if secretName == "" {
			return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software backend has no resolvable private-key secret")
		}
		// Preflight the key secret whenever no controller flow will (re)create it, instead of rolling
		// out signer pods stuck on a missing Secret mount:
		//   - sentry mode (no validator target): the key is registered on-chain out-of-band and always
		//     user-supplied (pre-provision it before genesis is fixed — a key minted here could never
		//     be in the validator set);
		//   - a targeted EXTERNAL-GENESIS validator: its explicit privateKeySecret is user-supplied
		//     too — only init/createValidator validator flows generate their own key secret;
		//   - a targeted init/createValidator validator whose key flow already COMPLETED (its pubkey
		//     is recorded in status): the key secret is never regenerated, so a deleted Secret stays
		//     deleted.
		// A pending key-generation flow may omit the Secret or key field because the child ChainNode
		// creates/fills it. If key bytes already exist, validate them now: the child reuses them.
		keyFlowPending := false
		if s.ValidatorGroup != "" {
			// `generates` says whether the targeted validator generates its own key (genesis init or
			// createValidator, used once the chain exists); `groupInitializes` is the genesis-init case:
			// ensureValidator generates that key during genesis bootstrap, so BEFORE the chain ID exists
			// it is still pending (ensureValidator has not run yet).
			generates := false
			groupInitializes := false
			if s.ValidatorGroup == appsv1.ReservedValidatorGroupName {
				v := nodeSet.Spec.Validator
				generates = v != nil && (v.Init != nil || v.CreateValidator != nil)
				groupInitializes = v != nil && v.Init != nil
			} else {
				for _, g := range nodeSet.Spec.Nodes {
					if g.Name == s.ValidatorGroup && g.Validator != nil {
						generates = g.Validator.Init != nil || g.Validator.CreateValidator != nil
						groupInitializes = g.Validator.Init != nil
					}
				}
			}
			switch {
			case groupInitializes && nodeSet.Status.ChainID == "":
				// Pre-genesis: ensureValidator has not yet created this init group's key.
				keyFlowPending = true
			case generates:
				keyFlowPending = true
				vname := validatorNodeName(nodeSet, s.ValidatorGroup, 0)
				for _, v := range nodeSet.Status.Validators {
					if v.Name == vname && v.PubKey != "" {
						// The targeted validator already registered its key; the flow will not run again.
						keyFlowPending = false
					}
				}
			}
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: secretName}, secret); err != nil {
			if errors.IsNotFound(err) && keyFlowPending {
				return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil
			}
			if errors.IsNotFound(err) {
				return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q not found: provide the consensus key registered on-chain — refusing to roll out a signer with no key", secretName)
			}
			return cosmosigner.Backend{}, err
		}
		keyMaterial, keyExists := secret.Data[privKeyFilename]
		if !keyExists && keyFlowPending {
			return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil
		}
		if len(keyMaterial) == 0 {
			return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q has no %s: provide the registered consensus key", secretName, privKeyFilename)
		}
		if _, err := cometbft.LoadPrivKey(keyMaterial); err != nil {
			return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q contains an invalid %s: %w", secretName, privKeyFilename, err)
		}
		return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil

	case c.UsesVaultBackend():
		v := c.Backend.Vault
		// The Vault token authenticates every signing call and the optional CA certificate is mounted at
		// startup; a missing Secret would roll out a signer that can never reach Vault. Verify them before
		// deploy (and, via preflightCosmosigners, before children are retargeted).
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, nodeSet.GetNamespace(), "Vault token", v.TokenSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, nodeSet.GetNamespace(), "Vault certificate", v.CertificateSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		return cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{
			Address:           v.Address,
			KeyName:           v.KeyName,
			Mount:             v.GetVaultMount(),
			Namespace:         ptr.Deref(v.Namespace, ""),
			TokenSecret:       v.TokenSecret,
			CertificateSecret: v.CertificateSecret,
			AutoRenewToken:    v.AutoRenewToken,
		}}, nil

	case c.UsesGcpKmsBackend():
		g := c.Backend.GcpKMS
		// The GCP credentials Secret (when set — omitted for Workload Identity) is mounted at startup.
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, nodeSet.GetNamespace(), "GCP credentials", g.CredentialsSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		return cosmosigner.Backend{GCP: &cosmosigner.GcpBackend{
			KeyVersion:        g.KeyVersion,
			CredentialsSecret: g.CredentialsSecret,
		}}, nil
	}
	return cosmosigner.Backend{}, fmt.Errorf("cosmosigner has no backend configured")
}

// maybeImportCosmosignerKey imports the targeted validator's generated consensus key into Vault once,
// when uploadGenerated is set. The source is the validator instance's own priv-key secret (created by
// its genesis/create-validator flow), so Vault holds exactly the key registered on-chain. The import
// fingerprint is tracked per signer in its CosmosignerStatus.KeyImported; this mutates nodeSet.Status
// in memory and returns whether it changed (the caller batches the status write). importPending is
// true while the signer must not be rolled out yet.
func (r *Reconciler) maybeImportCosmosignerKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner, params cosmosigner.Params) (importPending, changed bool, err error) {
	c := s.Spec
	// uploadGenerated is auto-defaulted for genesis-init targets (their consensus key is always
	// generated locally, so it must be imported), matching the documented tmKMS-parity behavior.
	if !c.VaultUploadsGenerated(signerTargetInitializesGenesis(nodeSet, s)) {
		return false, false, nil
	}

	sourceSecret := s.SoftwareKeySecret
	if sourceSecret == "" {
		// The webhook rejects uploadGenerated without a validator target; guard defensively.
		return false, false, fmt.Errorf("cosmosigner uploadGenerated requires a targeted validator whose key can be imported")
	}

	st := nodeSet.EnsureCosmosignerStatus(s.Name)

	// A matching served digest proves the current Vault target already holds the serving key. A signer
	// migration has a different desired digest and must import into its new target before rollout.
	if st.SigningDigest != "" && s.Digest() == st.SigningDigest {
		return false, false, nil
	}

	// The key is produced by the validator's genesis/create-validator flow; wait for it rather than
	// generating (and thereby diverging from) a different key. The import is still pending until it
	// exists, so the caller must not roll out the signer yet.
	keyMaterial, err := r.secretKey(ctx, nodeSet.GetNamespace(), sourceSecret, privKeyFilename)
	if err != nil {
		return false, false, err
	}
	if len(keyMaterial) == 0 {
		// No source material available. A completed import for the CURRENT Vault target and source (the
		// recorded target half matches) stays valid: Vault holds the registered key and the bootstrap
		// Secret is only needed at import time, so a Secret deleted after that import must NOT re-mark
		// the import pending (which would scale the signer to zero). A record from a DIFFERENT
		// target/source proves nothing about this spec.
		if appsv1.ImportRecordMatchesTarget(st.KeyImported, c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
			return false, false, nil
		}
		// Wait only while a controller-owned key-generation flow is genuinely pending. For an explicit
		// external-genesis key (or a generated key whose pubkey is already recorded), nothing will create
		// this source later; surfacing an error avoids silently scaling the signer to zero forever.
		if r.signerImportSourcePending(nodeSet, s) {
			return true, false, nil
		}
		return false, false, fmt.Errorf("cosmosigner %q Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", s.Name, sourceSecret, privKeyFilename)
	}
	if _, err := cometbft.LoadPrivKey(keyMaterial); err != nil {
		return false, false, fmt.Errorf("cosmosigner %q Vault uploadGenerated source secret %q contains an invalid %s: %w", s.Name, sourceSecret, privKeyFilename, err)
	}

	// The record fingerprints the Vault target, the resolved source secret name, AND the key material,
	// so changing the target (key name/mount/address/namespace), the source secret, or its bytes (an
	// in-place update during bootstrap) re-imports instead of leaving the signer pointed at a stale
	// transit key.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret, keyMaterial)
	if st.KeyImported == want {
		return false, false, nil
	}

	// Quiesce any already-running signer BEFORE the (synchronous) re-import: on a source/target change
	// it would otherwise keep signing with the previously imported key while the new one is being
	// imported. Scale-down is asynchronous — until every signer pod is actually gone the import stays
	// pending (retried on a later reconcile), which also prevents the caller from re-applying the
	// StatefulSet at full replicas and cancelling the scale-down.
	quiesced, err := cosmosigner.ScaleDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), params.Name)
	if err != nil {
		return false, false, err
	}
	if !quiesced {
		return true, false, nil
	}

	runner := cosmosigner.JobRunner{
		Client: r.ClientSet,
		Scheme: r.Scheme,
		Owner:  nodeSet,
		Params: params,
	}
	if err := runner.ImportKey(ctx, sourceSecret); err != nil {
		r.recorder.Event(nodeSet, corev1.EventTypeWarning, appsv1.ReasonUploadFailure,
			controllers.FormatErrorEvent("failed to import cosmosigner key to Vault", err))
		return false, false, err
	}

	// Persist the import fingerprint IMMEDIATELY — before the caller rolls out the StatefulSet — so a
	// later status-write conflict cannot leave a live signer without its KeyImported record (the next
	// reconcile would then ScaleDown and re-import, taking a correct live signer offline even though
	// Vault already holds the key). On failure we abort before the StatefulSet is applied (no live
	// signer to disrupt); the next reconcile re-imports (ImportKey is idempotent — it verifies the
	// stored pubkey) and re-persists. r.Status().Update refreshes nodeSet's resourceVersion on success,
	// so the caller's later batched status write stays consistent. Mirrors the standalone path.
	st.KeyImported = want
	if err := r.Status().Update(ctx, nodeSet); err != nil {
		return false, false, err
	}
	return false, false, nil
}

// signerNameForNode returns the name of the managed signer that targets a group's nodes, and
// whether one does. A signer fronts every pod of its target groups: sentry fan-out, and every
// instance of the validator group it serves (redundant signing endpoints of one identity). The
// returned name is stamped as the node's LabelCosmosignerTarget so the signer's discovery Service
// selects exactly those pods.
func signerNameForNode(nodeSet *appsv1.ChainNodeSet, group string) (string, bool) {
	return signerNameForNodeWithBlockedTargets(nodeSet, group, nil)
}

func signerNameForNodeWithBlockedTargets(nodeSet *appsv1.ChainNodeSet, group string, blocked blockedSignerTargets) (string, bool) {
	for _, s := range nodeSet.ResolveCosmosigners() {
		if _, ok := blocked[s.Name]; ok {
			continue
		}
		for _, t := range s.TargetGroups {
			if t == group {
				return nodeSet.CosmosignerResourceName(s), true
			}
		}
	}
	return "", false
}

// signerImportSourcePending reports whether the source key Secret for a Vault upload may still be
// created by this controller. Explicit privateKeySecret values are user-supplied and are never
// generated; after the target validator pubkey is recorded, the init/createValidator key flow is
// complete and a missing Secret is an error rather than a pending condition.
func (r *Reconciler) signerImportSourcePending(nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) bool {
	if s.ValidatorGroup == "" {
		return false
	}

	cfg := nodeSet.Spec.Validator
	if s.ValidatorGroup != appsv1.ReservedValidatorGroupName {
		cfg = nil
		for i := range nodeSet.Spec.Nodes {
			g := &nodeSet.Spec.Nodes[i]
			if g.Name == s.ValidatorGroup && g.Validator != nil {
				cfg = g.Validator
				break
			}
		}
	}
	if cfg == nil {
		return false
	}
	name := validatorNodeName(nodeSet, s.ValidatorGroup, 0)
	pubKeyRecorded := func() bool {
		for _, v := range nodeSet.Status.Validators {
			if v.Name == name && v.PubKey != "" {
				return true
			}
		}
		return false
	}
	// PRE-GENESIS: a genesis-init validator's key is created during bootstrap (ensureValidator, into
	// its PrivateKeySecret — explicit or default), and does not exist yet while the chain ID is empty.
	// So it is pending — even with an explicit key name (the key is generated INTO it). This must
	// precede the explicit-key check below, which would otherwise demand a not-yet-created key.
	if cfg.Init != nil && nodeSet.Status.ChainID == "" {
		return true
	}
	// POST-GENESIS (and non-init): a validator that GENERATES its own key — Init or createValidator —
	// is pending until its pubkey is recorded, because the child ChainNode creates that key WHEN IT
	// RUNS, whether into an explicit or a default-named Secret (an explicit privateKeySecret does not
	// make a createValidator source user-supplied). A validator that does NOT generate is terminal, so
	// its source Secret must already exist: an external-genesis validator's user-supplied key —
	// failing fast rather than waiting forever for a pubkey the child can never record while its
	// Secret is missing.
	if cfg.Init == nil && cfg.CreateValidator == nil {
		return false
	}
	return !pubKeyRecorded()
}

// signerTargetInitializesGenesis reports whether the validator a signer targets initializes a new
// genesis (its consensus key is generated locally and must be imported).
func signerTargetInitializesGenesis(nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) bool {
	switch {
	case s.ValidatorGroup == "":
		return false
	case s.ValidatorGroup == appsv1.ReservedValidatorGroupName:
		return nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil
	default:
		for _, g := range nodeSet.Spec.Nodes {
			if g.Name == s.ValidatorGroup && g.Validator != nil && g.Validator.Init != nil {
				return true
			}
		}
	}
	return false
}

// secretHasKey reports whether the named secret exists and contains a non-empty value for key.
func (r *Reconciler) secretHasKey(ctx context.Context, namespace, name, key string) (bool, error) {
	data, err := r.secretKey(ctx, namespace, name, key)
	if err != nil {
		return false, err
	}
	return len(data) > 0, nil
}

// secretKey returns the value stored under key in the named secret, or nil when the secret does not
// exist yet (so callers can treat a missing secret as "not ready" rather than an error).
func (r *Reconciler) secretKey(ctx context.Context, namespace, name, key string) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return secret.Data[key], nil
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, nodeSet *appsv1.ChainNodeSet, obj client.Object) error {
	return cosmosigner.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, obj)
}
