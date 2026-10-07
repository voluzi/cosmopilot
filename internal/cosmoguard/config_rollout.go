package cosmoguard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"runtime/debug"

	guardconfig "github.com/voluzi/cosmoguard/v5/pkg/cosmoguard"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

var cosmoGuardModuleVersion = func() string {
	info, _ := debug.ReadBuildInfo()
	if info != nil {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/voluzi/cosmoguard/v5" {
				return dep.Version
			}
		}
	}
	return ""
}()

// ApplyStatefulSet classifies file changes using CosmoGuard's reload policy against the same
// live object ApplyOwned updates, so a conflicting write cannot advance the rollout baseline.
func ApplyStatefulSet(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, p Params, desired *appsv1.StatefulSet) error {
	return applyOwned(ctx, c, scheme, owner, desired, func(existing client.Object) error {
		var live *appsv1.StatefulSet
		if existing != nil {
			live = existing.(*appsv1.StatefulSet)
		}
		return p.prepareConfigRollout(ctx, c, desired, live)
	})
}

func (p Params) prepareConfigRollout(ctx context.Context, c client.Client, desired, live *appsv1.StatefulSet) error {
	keys := []string{controllers.AnnotationCosmoGuardRestartFingerprint, controllers.AnnotationCosmoGuardConfigDigest, controllers.AnnotationCosmoGuardFingerprintScheme}
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	for _, key := range keys {
		delete(desired.Annotations, key)
		if live != nil {
			if value, ok := live.Annotations[key]; ok {
				desired.Annotations[key] = value
			}
		}
	}
	desired.Spec.Template.Annotations = maps.Clone(desired.Spec.Template.Annotations)
	// The marker must survive rendering; removing it would request a second rollout.
	delete(desired.Spec.Template.Annotations, controllers.AnnotationCosmoGuardRestart)
	if live != nil {
		if value, ok := live.Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart]; ok {
			if desired.Spec.Template.Annotations == nil {
				desired.Spec.Template.Annotations = map[string]string{}
			}
			desired.Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart] = value
		}
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.ConfigMap.Name}, cm); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	raw, ok := cm.BinaryData[p.ConfigMap.Key]
	if value, exists := cm.Data[p.ConfigMap.Key]; exists {
		raw, ok = []byte(value), true
	}
	if !ok {
		return nil
	}

	env, err := guardEnvironment(ctx, c, desired)
	if err != nil {
		return err
	}
	if env == nil {
		return nil
	}
	key := []byte(env["COSMOGUARD_CLUSTER_ENCRYPTION_KEY"])
	digest := keyedConfigValue(key, raw)
	// Include the module version because fingerprints from different releases are not comparable.
	// The keyed scheme identifier also rebaselines encryption-key rotation without a file rollout.
	scheme := cosmoGuardModuleVersion + ":" + keyedConfigValue(key, []byte("config-rollout-v1"))
	previousFingerprint := desired.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint]
	previousDigest := desired.Annotations[controllers.AnnotationCosmoGuardConfigDigest]
	comparable := live != nil && previousFingerprint != "" && previousDigest != "" && desired.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme] == scheme
	cfg, err := guardconfig.ParseConfig(raw, func(name string) (string, bool) { value, ok := env[name]; return value, ok })
	if err != nil {
		// Parser errors can contain config values, including credentials.
		log.FromContext(ctx).Info("cosmoguard configuration cannot be parsed; skipping config rollout", "configMap", cm.Name)
		return nil
	}
	fingerprint, err := guardconfig.RestartFingerprint(cfg)
	if err != nil {
		return err
	}
	value := keyedConfigValue(key, []byte(fingerprint))
	if comparable && previousDigest != digest && previousFingerprint != value {
		if desired.Spec.Template.Annotations == nil {
			desired.Spec.Template.Annotations = map[string]string{}
		}
		desired.Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart] = keyedConfigValue(key, []byte(string(cm.UID)+":"+cm.ResourceVersion+":"+value))
	}
	desired.Annotations[controllers.AnnotationCosmoGuardConfigDigest] = digest
	desired.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint] = value
	desired.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme] = scheme
	return nil
}

func keyedConfigValue(key, value []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil))
}

// Resolve the rendered environment instead of maintaining another list of deployment overrides.
func guardEnvironment(ctx context.Context, c client.Client, sts *appsv1.StatefulSet) (map[string]string, error) {
	env := map[string]string{}
	for _, variable := range sts.Spec.Template.Spec.Containers[0].Env {
		if variable.ValueFrom == nil {
			env[variable.Name] = variable.Value
			continue
		}
		if ref := variable.ValueFrom.SecretKeyRef; ref != nil {
			secret := &corev1.Secret{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: sts.Namespace, Name: ref.Name}, secret); err != nil {
				if errors.IsNotFound(err) {
					if ref.Optional != nil && *ref.Optional {
						continue
					}
					return nil, nil
				}
				return nil, err
			}
			value, ok := secret.Data[ref.Key]
			if !ok {
				if ref.Optional != nil && *ref.Optional {
					continue
				}
				return nil, nil
			}
			env[variable.Name] = string(value)
		} else if variable.ValueFrom.FieldRef != nil {
			// A pod's bind address is constant during its lifetime; pod churn must not alter the baseline.
			env[variable.Name] = "192.0.2.1"
		}
	}
	return env, nil
}
