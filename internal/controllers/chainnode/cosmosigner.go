package chainnode

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"

	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

// cosmosignerName is the base name for a standalone ChainNode's managed signer resources.
func cosmosignerName(chainNode *appsv1.ChainNode) string {
	return fmt.Sprintf("%s-signer", chainNode.GetName())
}

// cosmosignerTargetLabelValue returns the cosmosigner discovery-service selector label value this
// node's pod must carry, and whether it is a signer target at all. A standalone node with its own
// .spec.cosmosigner uses its own signer name; a ChainNodeSet-managed target (RemoteSignerTarget)
// reuses the value the nodeset controller stamped on the child ChainNode's metadata.
func cosmosignerTargetLabelValue(chainNode *appsv1.ChainNode) (string, bool) {
	if chainNode.UsesCosmosigner() {
		return cosmosignerName(chainNode), true
	}
	if chainNode.Spec.RemoteSignerTarget {
		if v := chainNode.Labels[controllers.LabelCosmosignerTarget]; v != "" {
			return v, true
		}
	}
	return "", false
}

// backfillCosmosignerLegacyStatus backfills the cosmosigner status invariants on nodes upgraded
// from versions predating them. It runs at the TOP of Reconcile — before configs/pod are touched —
// and reports whether a status write happened, in which case the caller must stop the current
// reconcile: the no-webhook validation must judge the fresh markers before the pod is switched to a
// potentially unverifiable remote-signer spec (which would drop the validator's local key mount).
//
//   - CosmosignerAtEstablishment: nil only on pre-marker chains (SetEstablishedChainID records it
//     atomically otherwise); backfilled conservatively — only a digest-proven identity is kept.
//   - CosmosignerServingIdentity: a digest recorded by a pre-field version proves a validator signer
//     served; backfilled regardless of the establishment marker so removal stays guarded.
func (r *Reconciler) backfillCosmosignerLegacyStatus(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if chainNode.Status.ChainID == "" {
		return false, nil
	}
	changed := false

	if chainNode.Status.CosmosignerSigningDigest != "" && chainNode.Status.CosmosignerServingIdentity == "" {
		if identity := chainNode.CosmosignerValidatorTargetedIdentity(); identity != "" {
			chainNode.Status.CosmosignerServingIdentity = identity
			changed = true
		}
	}

	if chainNode.Status.CosmosignerAtEstablishment == nil {
		identity := ""
		if chainNode.Status.CosmosignerSigningDigest != "" {
			identity = chainNode.CosmosignerValidatorTargetedIdentity()
		}
		chainNode.Status.CosmosignerAtEstablishment = ptr.To(identity)
		changed = true
	}
	if chainNode.Status.CosmosignerValidatorTargeted == nil && chainNode.Spec.Cosmosigner != nil &&
		(chainNode.Status.CosmosignerReplicas != nil || chainNode.Status.CosmosignerStateStorageSize != "" ||
			chainNode.Status.CosmosignerSigningDigest != "" || chainNode.Status.CosmosignerServingIdentity != "") {
		chainNode.Status.CosmosignerValidatorTargeted = ptr.To(chainNode.IsValidator())
		changed = true
	}

	if changed {
		return true, r.Status().Update(ctx, chainNode)
	}
	return false, nil
}

// ensureCosmosigner deploys (or tears down) a managed cosmosigner remote signer for a standalone
// ChainNode. It is a no-op until the chain ID is known. It returns wait=true while a removed
// signer's teardown is still in flight: the caller must NOT proceed to pod reconciliation then,
// or the node could be switched back to its local/tmKMS signing path while old signer pods (deletion
// is asynchronous) can still sign the same consensus key.
func (r *Reconciler) ensureCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) (wait bool, err error) {
	if chainNode.Spec.Cosmosigner == nil {
		if err := r.preflightCosmosignerFallback(ctx, chainNode); err != nil {
			return false, err
		}
		tornDown, err := r.undeployCosmosigner(ctx, chainNode)
		if err != nil {
			return false, err
		}
		return !tornDown, nil
	}
	if chainNode.Status.ChainID == "" {
		return false, nil
	}
	params, err := r.preflightCosmosigner(ctx, chainNode)
	if err != nil {
		return false, err
	}
	return r.ensureCosmosignerWithParams(ctx, chainNode, params)
}

func (r *Reconciler) preflightCosmosignerFallback(ctx context.Context, chainNode *appsv1.ChainNode) error {
	validatorTargeted := chainNode.Status.CosmosignerServingIdentity != "" ||
		ptr.Deref(chainNode.Status.CosmosignerValidatorTargeted, false) ||
		chainNode.Status.CosmosignerSigningDigest != ""
	if chainNode.Spec.Cosmosigner != nil {
		return nil
	}
	if !validatorTargeted {
		if targeted := chainNode.Status.CosmosignerValidatorTargeted; targeted != nil && !*targeted {
			return nil
		}
		sts := &k8sappsv1.StatefulSet{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: cosmosignerName(chainNode)}, sts); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if metav1.IsControlledBy(sts, chainNode) {
			return fmt.Errorf("cosmosigner cannot be removed: owned StatefulSet %q exists but its signing status is missing or incomplete; restore the signer status before teardown", sts.GetName())
		}
		return nil
	}
	if chainNode.Spec.Validator == nil {
		return fmt.Errorf("cosmosigner cannot be removed: the validator it served has no fallback signing path")
	}

	if t := chainNode.Spec.Validator.TmKMS; t != nil {
		hashicorp := t.Provider.Hashicorp
		if hashicorp == nil {
			return fmt.Errorf("cosmosigner cannot be removed: validator has no supported tmKMS provider configured")
		}
		if strings.TrimSpace(hashicorp.Address) == "" || strings.TrimSpace(hashicorp.Key) == "" {
			return fmt.Errorf("cosmosigner cannot be removed: tmKMS Hashicorp address and key are required")
		}
		if err := r.requireFallbackTmKMSSecret(ctx, chainNode.GetNamespace(), "tmKMS Vault token", hashicorp.TokenSecret); err != nil {
			return err
		}
		if hashicorp.CertificateSecret != nil {
			if err := r.requireFallbackTmKMSSecret(ctx, chainNode.GetNamespace(), "tmKMS Vault certificate", hashicorp.CertificateSecret); err != nil {
				return err
			}
		}
		if chainNode.ValidatorResolvesSigningIdentity(chainNode.Status.CosmosignerServingIdentity) {
			return nil
		}
		publicKey, err := r.fallbackTmKMSPublicKey(ctx, chainNode, hashicorp)
		if err != nil {
			return err
		}
		return requireMatchingFallbackPublicKey("cosmosigner", chainNode.Status.CosmosignerPublicKey, publicKey)
	}

	secretName := chainNode.Spec.Validator.GetPrivKeySecretName(chainNode)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: secretName}, secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner cannot be removed: local signing fallback secret %q not found", secretName)
		}
		return err
	}
	keyMaterial := secret.Data[PrivKeyFilename]
	if len(keyMaterial) == 0 {
		return fmt.Errorf("cosmosigner cannot be removed: local signing fallback secret %q is missing required key %q", secretName, PrivKeyFilename)
	}
	publicKey, err := cosmosigner.PublicKeyFromSecret(ctx, r.Client, chainNode.GetNamespace(), secretName)
	if err != nil {
		return fmt.Errorf("cosmosigner cannot be removed: local signing fallback secret %q contains an invalid %s", secretName, PrivKeyFilename)
	}
	return requireMatchingFallbackPublicKey("cosmosigner", chainNode.Status.CosmosignerPublicKey, publicKey)
}

func (r *Reconciler) fallbackTmKMSPublicKey(ctx context.Context, chainNode *appsv1.ChainNode, hashicorp *appsv1.TmKmsHashicorpProvider) (string, error) {
	if r.ClientSet == nil {
		return "", fmt.Errorf("cosmosigner fallback public-key preflight requires a Kubernetes clientset")
	}
	image := appsv1.DefaultCosmosignerImage
	if r.opts != nil && r.opts.CosmosignerImage != "" {
		image = r.opts.CosmosignerImage
	}
	runner := cosmosigner.JobRunner{
		Client: r.ClientSet,
		Scheme: r.Scheme,
		Owner:  chainNode,
		Params: cosmosigner.Params{
			Name:               cosmosignerName(chainNode),
			Namespace:          chainNode.GetNamespace(),
			Image:              image,
			ServiceAccountName: chainNode.Spec.Config.GetServiceAccountName(),
			Backend: cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{
				Address:               hashicorp.Address,
				KeyName:               hashicorp.Key,
				KeyVersion:            1,
				Mount:                 appsv1.DefaultCosmosignerVaultMount,
				TokenSecret:           hashicorp.TokenSecret,
				CertificateSecret:     hashicorp.CertificateSecret,
				SkipCertificateVerify: hashicorp.SkipCertificateVerify,
			}},
		},
	}
	return runner.PublicKey(ctx)
}

func requireMatchingFallbackPublicKey(signer string, recorded, fallback string) error {
	if recorded == "" {
		return fmt.Errorf("%s cannot be removed: its applied public key was not recorded; restore the previous signer configuration for one reconcile before removing it", signer)
	}
	if fallback != recorded {
		return fmt.Errorf("%s cannot be removed: fallback signing public key does not match the applied signer public key", signer)
	}
	return nil
}

func (r *Reconciler) requireFallbackTmKMSSecret(ctx context.Context, namespace, purpose string, selector *corev1.SecretKeySelector) error {
	if selector == nil || selector.Name == "" || selector.Key == "" {
		return fmt.Errorf("cosmosigner cannot be removed: %s secret selector must set both name and key", purpose)
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: selector.Name}, secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner cannot be removed: %s secret %q not found", purpose, selector.Name)
		}
		return err
	}
	if len(secret.Data[selector.Key]) == 0 {
		return fmt.Errorf("cosmosigner cannot be removed: %s secret %q is missing required key %q", purpose, selector.Name, selector.Key)
	}
	return nil
}

func (r *Reconciler) ensureCosmosignerWithParams(ctx context.Context, chainNode *appsv1.ChainNode, params cosmosigner.Params) (wait bool, err error) {
	if recorded, err := r.initCosmosignerLocks(ctx, chainNode); err != nil {
		return false, err
	} else if err := validateRecordedCosmosignerLocks(chainNode); err != nil {
		return false, err
	} else if recorded {
		return true, nil
	}

	importPending, err := r.maybeImportCosmosignerKey(ctx, chainNode, params)
	if err != nil {
		return false, err
	}

	// Render once: the ConfigMap contents and the pod-template ROLLME hash come from the same render.
	configYAML, err := params.ConfigYAML()
	if err != nil {
		return false, err
	}
	configMap, err := params.ConfigMap(configYAML)
	if err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, configMap); err != nil {
		return false, err
	}

	if err := r.applyCosmosignerObject(ctx, chainNode, params.RaftService()); err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, params.DiscoveryService()); err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, params.NetworkPolicy()); err != nil {
		return false, err
	}

	// Do not roll out the signer until the node's generated key has been imported into Vault; an
	// already-running signer is scaled to zero so it cannot keep signing with the previously
	// imported key while a re-import is pending.
	if importPending {
		_, err := cosmosigner.ScaleDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), params.Name)
		return false, err
	}

	sts, err := params.StatefulSet(configYAML)
	if err != nil {
		return false, err
	}
	lifecycleDigest, err := params.LifecycleDigest(chainNode.CosmosignerSigningDigest())
	if err != nil {
		return false, err
	}
	cosmosigner.SetLifecycleDigest(sts, lifecycleDigest)
	if err := r.applyCosmosignerObject(ctx, chainNode, sts); err != nil {
		return false, err
	}

	// The replica/PVC-template locks are recorded before any resource is applied (above). Here we
	// record the signing digest + serving identity — only for a validator (a sentry key lives
	// out-of-band and stays add/remove/rotate-able) and only after the current generation is fully
	// rolled out (read from the live object — the freshly rendered sts carries no status), so a config
	// that never worked stays correctable. A digest recorded by a version predating the serving field
	// leaves it empty; backfill it under the same rolled-out proof so the removal guard covers those.
	needDigest := chainNode.Status.CosmosignerSigningDigest == "" && chainNode.IsValidator()
	needServing := chainNode.Status.CosmosignerServingIdentity == "" &&
		chainNode.Status.CosmosignerSigningDigest != "" && chainNode.IsValidator()
	if needDigest || needServing {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, chainNode.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return false, err
		}
		if rolledOut {
			if needDigest {
				chainNode.Status.CosmosignerSigningDigest = chainNode.CosmosignerSigningDigest()
			}
			// The serving identity lets the no-webhook path judge a later REMOVAL: it is admitted only
			// when the validator's own signing path still resolves this identity.
			chainNode.Status.CosmosignerServingIdentity = chainNode.CosmosignerValidatorTargetedIdentity()
			return false, r.Status().Update(ctx, chainNode)
		}
	}
	return false, nil
}

func (r *Reconciler) initCosmosignerLocks(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	// Record raft membership and PVC-template locks before creating a signer. Existing live state takes
	// precedence over the spec so status recovery cannot redefine an established raft cluster. Callers
	// validate the recorded values immediately and requeue before applying signer resources; when no
	// live signer exists, the first rollout is locked to the requested spec.
	if chainNode.Status.CosmosignerReplicas == nil || chainNode.Status.CosmosignerStateStorageSize == "" ||
		chainNode.Status.CosmosignerValidatorTargeted == nil {
		liveReplicas, liveSize, liveClass, foundReplicas, foundStorage, err := cosmosigner.ReadSignerLock(ctx, r.Client, chainNode, chainNode.GetNamespace(), cosmosignerName(chainNode))
		if err != nil {
			return false, err
		}
		if chainNode.Status.CosmosignerReplicas == nil {
			if foundReplicas {
				chainNode.Status.CosmosignerReplicas = ptr.To(liveReplicas)
			} else {
				chainNode.Status.CosmosignerReplicas = ptr.To(chainNode.Spec.Cosmosigner.GetReplicas())
			}
		}
		if chainNode.Status.CosmosignerStateStorageSize == "" {
			if foundStorage {
				chainNode.Status.CosmosignerStateStorageSize = liveSize
				chainNode.Status.CosmosignerStateStorageClassName = liveClass
			} else {
				chainNode.Status.CosmosignerStateStorageSize = chainNode.Spec.Cosmosigner.GetStateStorageSize()
				chainNode.Status.CosmosignerStateStorageClassName = chainNode.Spec.Cosmosigner.StorageClassName
			}
		}
		if chainNode.Status.CosmosignerValidatorTargeted == nil {
			chainNode.Status.CosmosignerValidatorTargeted = ptr.To(chainNode.IsValidator())
		}
		if err := r.Status().Update(ctx, chainNode); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func validateRecordedCosmosignerLocks(chainNode *appsv1.ChainNode) error {
	c := chainNode.Spec.Cosmosigner
	if c == nil {
		return nil
	}
	if replicas := chainNode.Status.CosmosignerReplicas; replicas != nil && *replicas != c.GetReplicas() {
		return fmt.Errorf("cosmosigner replicas are immutable after deployment: changing them does not migrate the raft membership and can break quorum")
	}
	if recorded := chainNode.Status.CosmosignerStateStorageSize; recorded != "" &&
		!appsv1.CosmosignerStateStorageEqual(recorded, chainNode.Status.CosmosignerStateStorageClassName, c.GetStateStorageSize(), c.StorageClassName) {
		return fmt.Errorf("cosmosigner state storage (size/class) is immutable after deployment: its raft state PVCs cannot be resized or moved — remove the signer and re-add it")
	}
	return nil
}

func (r *Reconciler) preflightCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Params, error) {
	// Preflight deployability BEFORE the immutable raft/PVC locks are recorded, so a signer that
	// cannot deploy yet (a missing/incomplete raft-TLS Secret, or a missing backend auth/software Secret
	// resolved inside cosmosignerParams) fails WITHOUT first trapping the operator into the
	// initially-chosen replica count / storage template — which the webhook would then refuse to change
	// even though no signer was ever created. The raft mTLS Secret is mounted at pod startup; the params
	// resolution verifies the backend Secrets. Neither depends on the recorded locks.
	if err := cosmosigner.RequireRaftTLSSecret(ctx, r.Client, chainNode.GetNamespace(), chainNode.Spec.Cosmosigner.RaftTLSSecret); err != nil {
		return cosmosigner.Params{}, err
	}
	params, err := r.cosmosignerParams(ctx, chainNode)
	if err != nil {
		return cosmosigner.Params{}, err
	}
	// The signer StatefulSet and import pod run as the configured ServiceAccount; a missing one keeps
	// Kubernetes from starting them.
	if err := cosmosigner.RequireServiceAccount(ctx, r.Client, chainNode.GetNamespace(), chainNode.Spec.Cosmosigner.GetServiceAccountName()); err != nil {
		return cosmosigner.Params{}, err
	}
	// Run the same deploy-time blockers ApplyOwned/runJob would hit (name collision, foreign/ambiguous
	// raft-state PVCs) BEFORE recording locks or importing into Vault — otherwise a foreign same-name
	// signer would persist locks (and, for uploadGenerated, mutate the Vault key) for a signer ApplyOwned
	// then refuses. Only an uploadGenerated signer runs the one-shot <name>-import pod.
	usesImportPod := chainNode.Spec.Cosmosigner.VaultUploadsGenerated(chainNode.ShouldInitGenesis())
	usesPubkeyPod := !chainNode.Spec.Cosmosigner.UsesSoftwareBackend() && !usesImportPod
	established := chainNode.Status.CosmosignerAppliedDigest != "" || chainNode.Status.CosmosignerSigningDigest != "" ||
		chainNode.Status.CosmosignerPublicKey != "" || chainNode.Status.CosmosignerServingIdentity != ""
	requireRetainedState := cosmosigner.RetainedStateRequired(established, chainNode.Status.CosmosignerMigration)
	replicas := chainNode.Spec.Cosmosigner.GetReplicas()
	if requireRetainedState && chainNode.Status.CosmosignerReplicas != nil {
		replicas = *chainNode.Status.CosmosignerReplicas
	}
	if err := cosmosigner.PreflightDeployable(ctx, r.Client, chainNode, chainNode.GetNamespace(), cosmosignerName(chainNode), replicas, usesImportPod, usesPubkeyPod, requireRetainedState); err != nil {
		return cosmosigner.Params{}, err
	}
	recovering := chainNode.Status.CosmosignerAppliedDigest == "" && chainNode.Status.CosmosignerSigningDigest == ""
	publicKey := ""
	if recovering {
		recovered, live, err := cosmosigner.RecoveredSigningPublicKey(ctx, r.Client, chainNode, params)
		if err != nil {
			return cosmosigner.Params{}, err
		}
		if live {
			publicKey = recovered
		}
	}
	if publicKey == "" {
		// Preflight the uploadGenerated import SOURCE (read-only) before locks/import: a terminally
		// missing source key would otherwise be found only inside maybeImportCosmosignerKey, after locks.
		if err := r.preflightCosmosignerImportSource(ctx, chainNode); err != nil {
			return cosmosigner.Params{}, err
		}
		publicKey, err = r.cosmosignerPublicKey(ctx, chainNode, params)
		if err != nil {
			return cosmosigner.Params{}, err
		}
	}
	if chainNode.IsValidator() {
		if recorded := chainNode.Status.PubKey; recorded != "" {
			onChain := cosmosigner.CanonicalSDKPublicKey(recorded)
			if onChain == "" {
				return cosmosigner.Params{}, fmt.Errorf("cosmosigner cannot verify the on-chain validator public key recorded in status")
			}
			if publicKey != onChain {
				return cosmosigner.Params{}, fmt.Errorf("cosmosigner public key does not match the on-chain validator public key recorded in status; Cosmopilot does not rotate validator consensus keys")
			}
		}
		if applied := chainNode.Status.CosmosignerPublicKey; applied != "" && publicKey != applied {
			return cosmosigner.Params{}, fmt.Errorf("cosmosigner cannot change a validator public key after rollout because the replacement would not inherit its slash-protection history")
		}
	}
	if err := cosmosigner.EnsureConsensusKeyReservation(ctx, r.Client, chainNode.Status.ChainID, publicKey, cosmosigner.ReservationHolder{
		UID: chainNode.GetUID(), Kind: "ChainNode", Namespace: chainNode.GetNamespace(), Name: chainNode.GetName(), Claim: standaloneCosmosignerReservationClaim(chainNode),
	}); err != nil {
		if stderrors.Is(err, cosmosigner.ErrConsensusKeyReservationConflict) {
			if _, scaleErr := cosmosigner.ScaleDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), params.Name); scaleErr != nil {
				err = fmt.Errorf("%w; failed to scale down conflicting cosmosigner %q: %v", err, params.Name, scaleErr)
			}
			if chainNode.IsValidator() {
				return cosmosigner.Params{}, r.quiesceValidatorOnReservationConflict(ctx, chainNode, err)
			}
		}
		return cosmosigner.Params{}, err
	}
	params.ExpectedPublicKey = publicKey
	return params, nil
}

func standaloneCosmosignerReservationClaim(chainNode *appsv1.ChainNode) string {
	if chainNode.IsValidator() {
		return chainNode.GetName()
	}
	return "signer-" + utils.Sha256(chainNode.CosmosignerSigningIdentity())
}

func (r *Reconciler) reconcileSigningConfigs(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if recorded, err := r.ensureValidatorConsensusKeyReservation(ctx, chainNode); err != nil {
		return false, err
	} else if recorded {
		return true, nil
	}
	if chainNode.Spec.Cosmosigner == nil {
		if !chainNode.Spec.RemoteSignerTarget {
			if err := r.ensureTmKMSConfig(ctx, chainNode); err != nil {
				return false, err
			}
		}
		return r.ensureCosmosigner(ctx, chainNode)
	}
	if chainNode.Status.ChainID == "" {
		return false, nil
	}
	params, err := r.preflightCosmosigner(ctx, chainNode)
	if err != nil {
		return false, err
	}
	if recorded, err := r.initCosmosignerLocks(ctx, chainNode); err != nil {
		return false, err
	} else if err := validateRecordedCosmosignerLocks(chainNode); err != nil {
		return false, err
	} else if recorded {
		return true, nil
	}
	if pending, err := r.reconcileCosmosignerMigration(ctx, chainNode, params); err != nil || pending {
		return pending, err
	}
	if importPending, err := r.maybeImportCosmosignerKey(ctx, chainNode, params); err != nil || importPending {
		return importPending, err
	}
	wait, err := r.ensureCosmosignerWithParams(ctx, chainNode, params)
	if err != nil || wait {
		return wait, err
	}
	rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, chainNode.GetNamespace(), params.Name, params.Replicas)
	if err != nil {
		return false, err
	}
	if !rolledOut {
		return true, nil
	}
	_, err = r.recordCosmosignerAppliedState(ctx, chainNode, params)
	return false, err
}

func (r *Reconciler) reconcileCosmosignerMigration(ctx context.Context, chainNode *appsv1.ChainNode, params cosmosigner.Params) (bool, error) {
	signingDigest := chainNode.CosmosignerSigningDigest()
	desiredDigest, err := params.LifecycleDigest(signingDigest)
	if err != nil {
		return false, err
	}
	if chainNode.Status.CosmosignerAppliedDigest == "" {
		if chainNode.Status.CosmosignerSigningDigest != "" && chainNode.Status.CosmosignerSigningDigest != signingDigest {
			return false, fmt.Errorf("cosmosigner applied public key was not recorded before the signing configuration changed; restore the previous configuration for one reconcile before migrating")
		}
		liveDigest, found, err := cosmosigner.ReadLifecycleDigest(ctx, r.Client, chainNode.GetNamespace(), params.Name)
		if err != nil {
			return false, err
		}
		if !found {
			rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, chainNode.GetNamespace(), params.Name, params.Replicas)
			if err != nil || !rolledOut {
				return false, err
			}
			liveDigest = "legacy:" + signingDigest
		}
		chainNode.Status.CosmosignerAppliedDigest = liveDigest
		chainNode.Status.CosmosignerPublicKey = params.ExpectedPublicKey
		return true, r.Status().Update(ctx, chainNode)
	}

	if chainNode.Status.CosmosignerAppliedDigest == desiredDigest &&
		chainNode.Status.CosmosignerPublicKey == params.ExpectedPublicKey && chainNode.Status.CosmosignerMigration == nil {
		return false, nil
	}

	migration := chainNode.Status.CosmosignerMigration
	if migration == nil || migration.DesiredDigest != desiredDigest || migration.DesiredPublicKey != params.ExpectedPublicKey {
		if chainNode.Status.CosmosignerPublicKey == "" {
			return false, fmt.Errorf("cosmosigner applied public key is missing; restore the previous configuration so it can be recorded before migrating")
		}
		chainNode.Status.CosmosignerMigration = &appsv1.CosmosignerMigrationStatus{
			DesiredDigest:    desiredDigest,
			DesiredPublicKey: params.ExpectedPublicKey,
			Phase:            appsv1.CosmosignerMigrationQuiescing,
			ResetState:       params.ExpectedPublicKey != chainNode.Status.CosmosignerPublicKey,
		}
		return true, r.Status().Update(ctx, chainNode)
	}
	if migration.Phase == appsv1.CosmosignerMigrationRollingOut {
		return false, nil
	}

	ready, next, err := cosmosigner.ReconcileStatefulSetMigration(
		ctx, r.Client, chainNode, chainNode.GetNamespace(), params.Name, migration.Phase, migration.ResetState,
	)
	if err != nil {
		return false, err
	}
	if next != migration.Phase {
		migration.Phase = next
		return true, r.Status().Update(ctx, chainNode)
	}
	if ready && migration.Phase == appsv1.CosmosignerMigrationRecreating {
		migration.Phase = appsv1.CosmosignerMigrationRollingOut
		return true, r.Status().Update(ctx, chainNode)
	}
	return !ready, nil
}

func (r *Reconciler) recordCosmosignerAppliedState(ctx context.Context, chainNode *appsv1.ChainNode, params cosmosigner.Params) (bool, error) {
	signingDigest := chainNode.CosmosignerSigningDigest()
	desiredDigest, err := params.LifecycleDigest(signingDigest)
	if err != nil {
		return false, err
	}
	if chainNode.Status.CosmosignerAppliedDigest == desiredDigest && chainNode.Status.CosmosignerPublicKey == params.ExpectedPublicKey && chainNode.Status.CosmosignerMigration == nil {
		return false, nil
	}
	publicKey := params.ExpectedPublicKey
	if migration := chainNode.Status.CosmosignerMigration; migration != nil && migration.DesiredDigest == desiredDigest {
		publicKey = migration.DesiredPublicKey
	}
	chainNode.Status.CosmosignerAppliedDigest = desiredDigest
	chainNode.Status.CosmosignerPublicKey = publicKey
	chainNode.Status.CosmosignerMigration = nil
	if chainNode.IsValidator() {
		chainNode.Status.CosmosignerSigningDigest = signingDigest
		chainNode.Status.CosmosignerServingIdentity = chainNode.CosmosignerValidatorTargetedIdentity()
	}
	return true, r.Status().Update(ctx, chainNode)
}

func (r *Reconciler) cosmosignerPublicKey(ctx context.Context, chainNode *appsv1.ChainNode, params cosmosigner.Params) (string, error) {
	if params.Backend.Software != nil {
		return cosmosigner.PublicKeyFromSecret(ctx, r.Client, chainNode.GetNamespace(), params.Backend.Software.SecretName)
	}
	if chainNode.Spec.Cosmosigner.VaultUploadsGenerated(chainNode.ShouldInitGenesis()) && chainNode.Spec.Validator != nil {
		sourceSecret := chainNode.Spec.Validator.GetPrivKeySecretName(chainNode)
		publicKey, err := cosmosigner.PublicKeyFromSecret(ctx, r.Client, chainNode.GetNamespace(), sourceSecret)
		if err == nil {
			return publicKey, nil
		}
		vault := chainNode.Spec.Cosmosigner.Backend.Vault
		if !errors.IsNotFound(err) || vault == nil ||
			!vault.ImportRecordMatchesTarget(chainNode.Status.CosmosignerKeyImported, sourceSecret) {
			return "", err
		}
	}
	if r.ClientSet == nil {
		return "", fmt.Errorf("cosmosigner public-key preflight requires a Kubernetes clientset")
	}
	runner := cosmosigner.JobRunner{Client: r.ClientSet, Scheme: r.Scheme, Owner: chainNode, Params: params}
	return runner.PublicKey(ctx)
}

func (r *Reconciler) cosmosignerParams(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Params, error) {
	c := chainNode.Spec.Cosmosigner
	name := cosmosignerName(chainNode)

	// Exclude the internal selector labels (group/global Service selectors, P2P peer discovery,
	// cleanup selectors) so signer resources can never be selected as node Services or peers —
	// see controllers.CosmosignerReservedSelectorLabels. The ChainNodeSet label is excluded too:
	// every ChainNodeSet global Service selector requires `nodeset=<name>`, so dropping that one
	// key breaks any global-route selector match regardless of which per-nodeset route-membership
	// labels (whose names are dynamic and unknowable here) were inherited.
	exclude := append(controllers.CosmosignerReservedSelectorLabels(), controllers.LabelChainNodeSet)
	labels := utils.ExcludeMapKeys(WithChainNodeLabels(chainNode, map[string]string{
		controllers.LabelChainNode: chainNode.GetName(),
	}), exclude...)

	backend, err := r.cosmosignerBackend(ctx, chainNode)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:               name,
		Namespace:          chainNode.GetNamespace(),
		OwnerUID:           chainNode.GetUID(),
		ChainID:            chainNode.Status.ChainID,
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
		// The chain-node label disambiguates from a same-named ChainNodeSet's target pods, which
		// carry the same cosmosigner-target value (`<name>-signer`) but never the chain-node label.
		TargetSelector: map[string]string{
			controllers.LabelChainNode:         chainNode.GetName(),
			controllers.LabelCosmosignerTarget: name,
		},
	}, nil
}

// cosmosignerBackend translates the CRD backend into the builder backend. The software backend
// references the node's own priv-key secret (created by its genesis/create-validator flow) or an
// explicit secret; no key is generated here, so the signer always signs with the node's registered
// consensus key.
func (r *Reconciler) cosmosignerBackend(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Backend, error) {
	c := chainNode.Spec.Cosmosigner
	switch {
	case c.UsesVaultBackend():
		v := c.Backend.Vault
		// The Vault token authenticates every signing call and the optional CA certificate is mounted at
		// startup; a missing/mistyped Secret would recreate the validator pod without its local key for a
		// signer that can never reach Vault. Verify them before the backend is allowed to deploy.
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, chainNode.GetNamespace(), "Vault token", v.TokenSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, chainNode.GetNamespace(), "Vault certificate", v.CertificateSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		return cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{
			Address:           v.Address,
			KeyName:           v.KeyName,
			KeyVersion:        v.GetKeyVersion(),
			Mount:             v.GetVaultMount(),
			Namespace:         ptr.Deref(v.Namespace, ""),
			TokenSecret:       v.TokenSecret,
			CertificateSecret: v.CertificateSecret,
		}}, nil
	case c.UsesGcpKmsBackend():
		g := c.Backend.GcpKMS
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, chainNode.GetNamespace(), "GCP credentials", g.CredentialsSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		return cosmosigner.Backend{GCP: &cosmosigner.GcpBackend{
			KeyVersion:        g.KeyVersion,
			CredentialsSecret: g.CredentialsSecret,
		}}, nil
	default:
		secretName := r.cosmosignerSoftwareSecretName(chainNode)
		// Preflight the key secret whenever no controller flow will (re)create it, instead of rolling
		// out signer pods stuck on a missing Secret mount:
		//   - sentry mode (non-validator): the key is registered out-of-band and always user-supplied;
		//   - an external-genesis validator: its key is user-supplied too (RequiresPrivKey only
		//     generates keys for init/createValidator validators);
		//   - an init/createValidator validator whose key flow already COMPLETED (Status.PubKey set):
		//     RequiresPrivKey no longer regenerates the secret, so a deleted Secret stays deleted.
		// A pending init/createValidator flow may omit the Secret or key field because ensureSigningKey
		// creates/fills it. If key bytes already exist, validate them now: the key flow reuses them.
		keyFlowPending := chainNode.Status.PubKey == "" &&
			(chainNode.ShouldInitGenesis() || (chainNode.IsValidator() && chainNode.Spec.Validator.CreateValidator != nil))
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: secretName}, secret); err != nil {
			if errors.IsNotFound(err) && keyFlowPending {
				return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil
			}
			if errors.IsNotFound(err) {
				return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q not found: provide the consensus key registered on-chain — refusing to roll out a signer with no key", secretName)
			}
			return cosmosigner.Backend{}, err
		}
		keyMaterial, keyExists := secret.Data[PrivKeyFilename]
		if !keyExists && keyFlowPending {
			return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil
		}
		if len(keyMaterial) == 0 {
			return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q has no %s: provide the registered consensus key", secretName, PrivKeyFilename)
		}
		if _, err := cometbft.LoadPrivKey(keyMaterial); err != nil {
			return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q contains an invalid %s: %w", secretName, PrivKeyFilename, err)
		}
		return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil
	}
}

func (r *Reconciler) cosmosignerSoftwareSecretName(chainNode *appsv1.ChainNode) string {
	// A validator always signs with its own registered key (the webhook forbids an explicit
	// software override in that case). Only a non-validator sentry uses the explicit secret.
	if chainNode.IsValidator() {
		return r.cosmosignerNodeKeySecret(chainNode)
	}
	if s := chainNode.Spec.Cosmosigner.Backend.Software.PrivateKeySecret; s != nil && *s != "" {
		return *s
	}
	return r.cosmosignerNodeKeySecret(chainNode)
}

// cosmosignerNodeKeySecret resolves the node's own consensus key secret: the validator's resolved
// private-key secret when the node is a validator, otherwise the default name.
func (r *Reconciler) cosmosignerNodeKeySecret(chainNode *appsv1.ChainNode) string {
	if chainNode.IsValidator() {
		return chainNode.Spec.Validator.GetPrivKeySecretName(chainNode)
	}
	return fmt.Sprintf("%s-priv-key", chainNode.GetName())
}

// cosmosignerImportSourcePending reports whether the source key Secret for a Vault upload may still
// be created by this controller. Explicit privateKeySecret values are user-supplied and are never
// generated; after a validator pubkey is recorded, the init/createValidator key flow is complete and
// a missing Secret is an error rather than a pending condition.
func (r *Reconciler) cosmosignerImportSourcePending(chainNode *appsv1.ChainNode) bool {
	if chainNode.Spec.Validator == nil {
		return false
	}
	if chainNode.Spec.Validator.PrivateKeySecret != nil && *chainNode.Spec.Validator.PrivateKeySecret != "" {
		return false
	}
	if chainNode.Status.PubKey != "" {
		return false
	}
	return chainNode.Spec.Validator.Init != nil || chainNode.Spec.Validator.CreateValidator != nil
}

// preflightCosmosignerImportSource errors when a Vault uploadGenerated signer's source key Secret is
// TERMINALLY missing — read-only, so it can run before the raft/PVC locks are recorded and before any
// import mutates Vault. It mirrors maybeImportCosmosignerKey's terminal-missing path: a completed import
// for the current target (status record matches) or a genuinely pending key-generation flow is fine; only a
// source that no controller flow will create is an error.
func (r *Reconciler) preflightCosmosignerImportSource(ctx context.Context, chainNode *appsv1.ChainNode) error {
	c := chainNode.Spec.Cosmosigner
	if !c.VaultUploadsGenerated(chainNode.ShouldInitGenesis()) ||
		(chainNode.Status.CosmosignerSigningDigest != "" && chainNode.CosmosignerSigningDigest() == chainNode.Status.CosmosignerSigningDigest) {
		return nil
	}
	sourceSecret := r.cosmosignerNodeKeySecret(chainNode)
	if c.Backend.Vault.ImportRecordMatchesTarget(chainNode.Status.CosmosignerKeyImported, sourceSecret) {
		return nil
	}
	if r.cosmosignerImportSourcePending(chainNode) {
		return nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: sourceSecret}, secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", sourceSecret, PrivKeyFilename)
		}
		return err
	}
	if len(secret.Data[PrivKeyFilename]) == 0 {
		return fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", sourceSecret, PrivKeyFilename)
	}
	if _, err := cometbft.LoadPrivKey(secret.Data[PrivKeyFilename]); err != nil {
		return fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q contains an invalid %s: %w", sourceSecret, PrivKeyFilename, err)
	}
	return nil
}

// maybeImportCosmosignerKey imports the node's generated consensus key into Vault once, when
// uploadGenerated is set.
func (r *Reconciler) maybeImportCosmosignerKey(ctx context.Context, chainNode *appsv1.ChainNode, params cosmosigner.Params) (bool, error) {
	c := chainNode.Spec.Cosmosigner
	// uploadGenerated is auto-defaulted for genesis-init validators (their consensus key is always
	// generated locally, so it must be imported), matching the documented tmKMS-parity behavior.
	if !c.VaultUploadsGenerated(chainNode.ShouldInitGenesis()) {
		return false, nil
	}

	sourceSecret := r.cosmosignerNodeKeySecret(chainNode)

	// A matching served digest proves the current Vault target already holds the serving key. A signer
	// migration has a different desired digest and must import into its new target before rollout.
	if chainNode.Status.CosmosignerSigningDigest != "" && chainNode.CosmosignerSigningDigest() == chainNode.Status.CosmosignerSigningDigest {
		return false, nil
	}

	// Fetch the source key material first: the fingerprint hashes the actual bytes (not just the
	// secret name), so an in-place update of the source Secret re-imports rather than being skipped.
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: sourceSecret}, secret); err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	keyMaterial := secret.Data[PrivKeyFilename]
	if len(keyMaterial) == 0 {
		// No source material available. A completed import for the CURRENT Vault target and source
		// (the status record's target half matches) stays valid: Vault holds the registered key and the
		// bootstrap Secret is only needed at import time, so a Secret deleted after that import must
		// NOT re-mark the import pending (which would scale the signer to zero). A record from a
		// DIFFERENT target/source proves nothing about this spec.
		if c.Backend.Vault.ImportRecordMatchesTarget(chainNode.Status.CosmosignerKeyImported, sourceSecret) {
			return false, nil
		}
		// Wait only while a controller-owned key-generation flow is genuinely pending. For an explicit
		// external-genesis key (or a completed init/createValidator flow), nothing will create this
		// source later; surfacing an error avoids silently scaling the signer to zero forever.
		if r.cosmosignerImportSourcePending(chainNode) {
			return true, nil
		}
		return false, fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", sourceSecret, PrivKeyFilename)
	}
	if _, err := cometbft.LoadPrivKey(keyMaterial); err != nil {
		return false, fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q contains an invalid %s: %w", sourceSecret, PrivKeyFilename, err)
	}

	// Fingerprint the Vault target, the resolved source secret name, AND the key material, so changing
	// any of them re-imports rather than leaving the annotation set. Shared with the ChainNodeSet
	// controller so both import protocols stay in lockstep.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret, keyMaterial)
	if chainNode.Status.CosmosignerKeyImported == want {
		return false, nil
	}
	if c.Backend.Vault.ImportRecordMatches(chainNode.Status.CosmosignerKeyImported, sourceSecret, keyMaterial) {
		if err := r.markCosmosignerKeyImported(ctx, chainNode, want); err != nil {
			return false, err
		}
		return false, nil
	}
	if c.Backend.Vault.ImportRecordMatchesTarget(chainNode.Status.CosmosignerKeyImported, sourceSecret) {
		return false, fmt.Errorf("cosmosigner Vault uploadGenerated source key changed after import; Vault cannot overwrite an existing transit key — migrate to a new Vault keyName")
	}

	// Quiesce any already-running signer BEFORE the synchronous re-import, so it cannot keep
	// signing with the previously imported key while the new one lands. Scale-down is
	// asynchronous — until every signer pod is gone the import stays pending (retried next
	// reconcile), which also keeps the caller from re-applying the StatefulSet at full replicas.
	quiesced, err := cosmosigner.ScaleDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), params.Name)
	if err != nil {
		return false, err
	}
	if !quiesced {
		return true, nil
	}

	runner := cosmosigner.JobRunner{Client: r.ClientSet, Scheme: r.Scheme, Owner: chainNode, Params: params}
	if err := runner.ImportKey(ctx, sourceSecret); err != nil {
		r.recorder.Event(chainNode, corev1.EventTypeWarning, appsv1.ReasonUploadFailure,
			controllers.FormatErrorEvent("failed to import cosmosigner key to Vault", err))
		return false, err
	}

	// Mark imported, tolerating a concurrent update: the import command is idempotent (it verifies
	// the stored pubkey), so a conflict retry must not fail the reconcile after a successful import.
	if err := r.markCosmosignerKeyImported(ctx, chainNode, want); err != nil {
		return false, err
	}
	return false, nil
}

// markCosmosignerKeyImported records the import proof in controller-owned status, retrying conflicts
// so a successful import is never followed by a failed reconcile.
func (r *Reconciler) markCosmosignerKeyImported(ctx context.Context, chainNode *appsv1.ChainNode, value string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &appsv1.ChainNode{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(chainNode), fresh); err != nil {
			return err
		}
		fresh.Status.CosmosignerKeyImported = value
		if err := r.Status().Update(ctx, fresh); err != nil {
			return err
		}
		chainNode.ResourceVersion = fresh.ResourceVersion
		chainNode.Status = fresh.Status
		return nil
	})
}

// undeployCosmosigner removes managed signer resources this ChainNode owns, reporting whether
// teardown is COMPLETE (StatefulSet and PVCs gone). Callers must not switch the node's signing path
// back to local/tmKMS until it is.
func (r *Reconciler) undeployCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	name := cosmosignerName(chainNode)
	if err := cosmosigner.Undeploy(ctx, r.Client, chainNode, chainNode.GetNamespace(), name); err != nil {
		return false, err
	}

	tornDown, err := cosmosigner.IsTornDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), name)
	if err != nil || !tornDown {
		return false, err
	}
	if chainNode.Status.CosmosignerReplicas == nil && chainNode.Status.CosmosignerSigningDigest == "" &&
		chainNode.Status.CosmosignerKeyImported == "" &&
		chainNode.Status.CosmosignerValidatorTargeted == nil {
		return true, nil
	}
	// Clear the recorded signer invariants only once the StatefulSet AND its PVCs are actually gone.
	// Undeploy just *requests* deletion (it is asynchronous): clearing while the old raft cluster is
	// still terminating would let a remove-and-immediate-re-add bypass the replica guard and bind the
	// surviving PVCs, inheriting stale raft membership.
	chainNode.Status.CosmosignerReplicas = nil
	chainNode.Status.CosmosignerValidatorTargeted = nil
	chainNode.Status.CosmosignerStateStorageSize = ""
	chainNode.Status.CosmosignerStateStorageClassName = nil
	chainNode.Status.CosmosignerSigningDigest = ""
	chainNode.Status.CosmosignerAppliedDigest = ""
	chainNode.Status.CosmosignerPublicKey = ""
	chainNode.Status.CosmosignerMigration = nil
	chainNode.Status.CosmosignerKeyImported = ""
	chainNode.Status.CosmosignerServingIdentity = ""
	// Tolerate a conflict: the signer is already gone and this clear is idempotent, so a concurrent
	// update just defers it to the next reconcile rather than spinning the workqueue with no progress.
	if err := r.Status().Update(ctx, chainNode); err != nil && !errors.IsConflict(err) {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, chainNode *appsv1.ChainNode, obj client.Object) error {
	return cosmosigner.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, obj)
}
