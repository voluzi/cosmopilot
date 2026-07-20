package cosmosigner

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RecoveredSigningPublicKey validates an owned live signer's immutable runtime configuration and
// returns the canonical public key pinned in that configuration. A missing StatefulSet is a first
// rollout only when no raft-state PVC survives. Controllers use the returned key before writing a
// reservation so lost status cannot make a restored spec reserve a different key first.
func RecoveredSigningPublicKey(ctx context.Context, c client.Client, owner client.Object, params Params) (string, bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: params.Namespace, Name: params.Name}, sts); err != nil {
		if errors.IsNotFound(err) {
			pvcs := &corev1.PersistentVolumeClaimList{}
			if err := c.List(ctx, pvcs, client.InNamespace(params.Namespace)); err != nil {
				return "", false, err
			}
			for i := range pvcs.Items {
				pvc := &pvcs.Items[i]
				if _, owned := ownedStatefulSetDataPVCOrdinal(pvc, owner, params.Name); owned || isAmbiguousLegacyDataPVC(pvc, params.Name) {
					return "", false, fmt.Errorf("cosmosigner %q has orphaned raft-state PVC %q but no StatefulSet; refusing to recover an unverifiable live signing identity", params.Name, pvc.GetName())
				}
			}
			return "", false, nil
		}
		return "", false, err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return "", false, foreignObjectErr("StatefulSet", params.Name)
	}

	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: params.Namespace, Name: params.Name}, configMap); err != nil {
		if errors.IsNotFound(err) {
			return "", false, fmt.Errorf("cosmosigner %q has live state but its ConfigMap is missing; refusing to recover an unverifiable live signing identity", params.Name)
		}
		return "", false, err
	}
	if !metav1.IsControlledBy(configMap, owner) {
		return "", false, foreignObjectErr("ConfigMap", params.Name)
	}
	liveYAML, ok := configMap.Data[configFileName]
	if !ok || liveYAML == "" {
		return "", false, fmt.Errorf("cosmosigner %q has live state but its ConfigMap has no %s; refusing to recover an unverifiable live signing identity", params.Name, configFileName)
	}
	liveConfigHash, ok := signerConfigHash(sts)
	if !ok || liveConfigHash != configDataHash(configMap.Data) {
		return "", false, fmt.Errorf("cosmosigner %q live StatefulSet %s does not match its ConfigMap; refusing to recover a torn signing configuration", params.Name, configHashEnv)
	}
	liveConfig := &Config{}
	if err := yaml.Unmarshal([]byte(liveYAML), liveConfig); err != nil {
		return "", false, fmt.Errorf("cosmosigner %q has live state but its ConfigMap is invalid; refusing to recover an unverifiable live signing identity: %w", params.Name, err)
	}
	publicKey := liveConfig.ExpectedPublicKey
	if !recoveredBackendMatches(liveConfig.Backend, params.Backend, sts) || validateCanonicalPublicKey(publicKey) != nil {
		return "", false, fmt.Errorf("cosmosigner %q live signing identity does not match the desired spec; refusing to overwrite the recovered signer", params.Name)
	}
	if params.ExpectedPublicKey != "" && params.ExpectedPublicKey != publicKey {
		return "", false, fmt.Errorf("cosmosigner %q live signing identity does not match the desired spec; refusing to overwrite the recovered signer", params.Name)
	}
	return publicKey, true, nil
}

// ValidateRecoveredSigningIdentity prevents status recovery from redefining the consensus key of an
// owned live signer.
func ValidateRecoveredSigningIdentity(ctx context.Context, c client.Client, owner client.Object, params Params) error {
	_, _, err := RecoveredSigningPublicKey(ctx, c, owner, params)
	return err
}

func signerConfigHash(sts *appsv1.StatefulSet) (string, bool) {
	for _, container := range sts.Spec.Template.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		for _, env := range container.Env {
			if env.Name == configHashEnv && env.Value != "" {
				return env.Value, true
			}
		}
	}
	return "", false
}

func recoveredBackendMatches(live BackendConfig, desired Backend, sts *appsv1.StatefulSet) bool {
	want := desired.backendConfig()
	if live.Type != want.Type {
		return false
	}
	switch {
	case desired.Software != nil:
		if live.KeyFile != want.KeyFile {
			return false
		}
		for _, volume := range sts.Spec.Template.Spec.Volumes {
			if volume.Name == softwareVolume && volume.Secret != nil {
				return volume.Secret.SecretName == desired.Software.SecretName
			}
		}
		return false
	case desired.Vault != nil:
		return live.Vault != nil && want.Vault != nil &&
			live.Vault.Address == want.Vault.Address &&
			live.Vault.Namespace == want.Vault.Namespace &&
			live.Vault.Mount == want.Vault.Mount &&
			live.Vault.KeyName == want.Vault.KeyName &&
			live.Vault.KeyVersion == want.Vault.KeyVersion
	case desired.GCP != nil:
		return live.GCP != nil && want.GCP != nil && live.GCP.KeyVersion == want.GCP.KeyVersion
	default:
		return false
	}
}
