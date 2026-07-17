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

// ValidateRecoveredSigningIdentity prevents status recovery from redefining the consensus key of an
// owned live signer. A missing StatefulSet is a first rollout only when no raft-state PVC survives;
// a live one must still use the backend identity described by params before the controller may import
// keys or rewrite signer resources.
func ValidateRecoveredSigningIdentity(ctx context.Context, c client.Client, owner client.Object, params Params) error {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: params.Namespace, Name: params.Name}, sts); err != nil {
		if errors.IsNotFound(err) {
			pvcs := &corev1.PersistentVolumeClaimList{}
			if err := c.List(ctx, pvcs, client.InNamespace(params.Namespace)); err != nil {
				return err
			}
			for i := range pvcs.Items {
				pvc := &pvcs.Items[i]
				if _, owned := ownedStatefulSetDataPVCOrdinal(pvc, owner, params.Name); owned || isAmbiguousLegacyDataPVC(pvc, params.Name) {
					return fmt.Errorf("cosmosigner %q has orphaned raft-state PVC %q but no StatefulSet; refusing to recover an unverifiable live signing identity", params.Name, pvc.GetName())
				}
			}
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return foreignObjectErr("StatefulSet", params.Name)
	}

	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: params.Namespace, Name: params.Name}, configMap); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner %q has live state but its ConfigMap is missing; refusing to recover an unverifiable live signing identity", params.Name)
		}
		return err
	}
	if !metav1.IsControlledBy(configMap, owner) {
		return foreignObjectErr("ConfigMap", params.Name)
	}
	liveYAML, ok := configMap.Data[configFileName]
	if !ok || liveYAML == "" {
		return fmt.Errorf("cosmosigner %q has live state but its ConfigMap has no %s; refusing to recover an unverifiable live signing identity", params.Name, configFileName)
	}
	liveConfigHash, ok := signerConfigHash(sts)
	if !ok || liveConfigHash != configDataHash(configMap.Data) {
		return fmt.Errorf("cosmosigner %q live StatefulSet %s does not match its ConfigMap; refusing to recover a torn signing configuration", params.Name, configHashEnv)
	}
	liveConfig := &Config{}
	if err := yaml.Unmarshal([]byte(liveYAML), liveConfig); err != nil {
		return fmt.Errorf("cosmosigner %q has live state but its ConfigMap is invalid; refusing to recover an unverifiable live signing identity: %w", params.Name, err)
	}
	if !recoveredBackendMatches(liveConfig.Backend, params.Backend, sts) {
		return fmt.Errorf("cosmosigner %q live signing identity does not match the desired spec; refusing to overwrite the recovered signer", params.Name)
	}
	return nil
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
			live.Vault.KeyName == want.Vault.KeyName
	case desired.GCP != nil:
		return live.GCP != nil && want.GCP != nil && live.GCP.KeyVersion == want.GCP.KeyVersion
	default:
		return false
	}
}
