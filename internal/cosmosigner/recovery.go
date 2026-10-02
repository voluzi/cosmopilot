package cosmosigner

import (
	"context"
	stderrors "errors"
	"fmt"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrRecoveredIdentityMismatch distinguishes a refused desired identity from untrustworthy live state.
var ErrRecoveredIdentityMismatch = stderrors.New("recovered cosmosigner identity mismatch")

// RecoveredSigningPublicKey validates an owned live signer's immutable runtime configuration and
// returns the canonical public key pinned in that configuration. A missing StatefulSet is a first
// rollout only when no raft-state PVC survives. Controllers use the returned key before writing a
// reservation so lost status cannot make a restored spec reserve a different key first.
func RecoveredSigningPublicKey(ctx context.Context, c client.Client, owner client.Object, params Params) (string, bool, error) {
	sts, liveConfig, err := liveSigningConfig(ctx, c, owner, params.Namespace, params.Name)
	if err != nil {
		return "", false, err
	}
	if liveConfig == nil {
		return "", false, nil
	}
	publicKey := liveConfig.ExpectedPublicKey
	if !recoveredBackendMatches(liveConfig.Backend, params.Backend, sts) ||
		(params.ExpectedPublicKey != "" && params.ExpectedPublicKey != publicKey) {
		return "", false, fmt.Errorf("%w: cosmosigner %q live signing identity does not match the desired spec; refusing to overwrite the recovered signer", ErrRecoveredIdentityMismatch, params.Name)
	}
	return publicKey, true, nil
}

// LiveSigningPublicKey reads an owned, coherent runtime pin without trusting the desired backend.
// It cannot authorize deployment; controllers must also prove the on-chain key and reserve it.
func LiveSigningPublicKey(ctx context.Context, c client.Reader, owner client.Object, namespace, name string) (string, bool, error) {
	_, config, err := liveSigningConfig(ctx, c, owner, namespace, name)
	if err != nil {
		return "", false, err
	}
	if config == nil {
		return "", false, nil
	}
	return config.ExpectedPublicKey, true, nil
}

func liveSigningConfig(ctx context.Context, c client.Reader, owner client.Object, namespace, name string) (*appsv1.StatefulSet, *Config, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			pvcs := &corev1.PersistentVolumeClaimList{}
			if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
				return nil, nil, err
			}
			for i := range pvcs.Items {
				pvc := &pvcs.Items[i]
				if _, owned := ownedStatefulSetDataPVCOrdinal(pvc, owner, name); owned || isAmbiguousLegacyDataPVC(pvc, name) {
					return nil, nil, fmt.Errorf("cosmosigner %q has orphaned raft-state PVC %q but no StatefulSet; refusing to recover an unverifiable live signing identity", name, pvc.GetName())
				}
			}
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return nil, nil, foreignObjectErr("StatefulSet", name)
	}

	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, configMap); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("cosmosigner %q has live state but its ConfigMap is missing; refusing to recover an unverifiable live signing identity", name)
		}
		return nil, nil, err
	}
	if !metav1.IsControlledBy(configMap, owner) {
		return nil, nil, foreignObjectErr("ConfigMap", name)
	}
	liveYAML, ok := configMap.Data[configFileName]
	if !ok || liveYAML == "" {
		return nil, nil, fmt.Errorf("cosmosigner %q has live state but its ConfigMap has no %s; refusing to recover an unverifiable live signing identity", name, configFileName)
	}
	liveConfigHash, ok := signerConfigHash(sts)
	if !ok || liveConfigHash != configDataHash(configMap.Data) {
		return nil, nil, fmt.Errorf("cosmosigner %q live StatefulSet %s does not match its ConfigMap; refusing to recover a torn signing configuration", name, configHashEnv)
	}
	liveConfig := &Config{}
	if err := yaml.Unmarshal([]byte(liveYAML), liveConfig); err != nil {
		return nil, nil, fmt.Errorf("cosmosigner %q has live state but its ConfigMap is invalid; refusing to recover an unverifiable live signing identity: %w", name, err)
	}
	if sts.GetAnnotations()[retainedStateLostAnnotation] == "true" {
		return nil, nil, fmt.Errorf("cosmosigner %q retained state is lost; refusing to trust its live signing identity", name)
	}
	if err := validateCanonicalPublicKey(liveConfig.ExpectedPublicKey); err != nil {
		return nil, nil, fmt.Errorf("cosmosigner %q live signing identity has an invalid expected_public_key: %w", name, err)
	}
	pinned := 0
	for _, container := range sts.Spec.Template.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		for i, arg := range container.Args {
			if arg != "--expected-public-key" {
				continue
			}
			if i+1 >= len(container.Args) || container.Args[i+1] != liveConfig.ExpectedPublicKey {
				return nil, nil, fmt.Errorf("cosmosigner %q live signing identity argument does not match its ConfigMap", name)
			}
			pinned++
		}
	}
	if pinned != 1 {
		return nil, nil, fmt.Errorf("cosmosigner %q live signing identity is not pinned by exactly one expected-public-key argument", name)
	}
	return sts, liveConfig, nil
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
