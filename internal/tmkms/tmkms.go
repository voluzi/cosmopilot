package tmkms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v4/internal/k8s"
	"github.com/voluzi/cosmopilot/v4/pkg/utils"
)

type KMS struct {
	Name   string
	Owner  client.Object
	Client kubernetes.Interface
	Scheme *runtime.Scheme
	Config *Config
}

func New(client kubernetes.Interface, scheme *runtime.Scheme, name string, owner client.Object, opts ...Option) *KMS {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}
	return &KMS{
		Name:   name,
		Owner:  owner,
		Client: client,
		Scheme: scheme,
		Config: cfg,
	}
}

func (kms *KMS) DeployConfig(ctx context.Context) error {
	if kms.Config.Attribution == nil {
		return fmt.Errorf("tmKMS %q has no resource attribution configured", kms.Name)
	}
	if err := kms.ensureIdentityKey(ctx); err != nil {
		return err
	}

	if err := kms.ensureConfigMap(ctx); err != nil {
		return err
	}

	if kms.Config.PersistState {
		return kms.ensurePVC(ctx)
	}
	return nil
}

// UndeployConfig removes the ConfigMap and identity Secret this KMS created. Same-name objects that
// belong to someone else are left in place, and the signing state PVC is always kept.
func (kms *KMS) UndeployConfig(ctx context.Context) error {
	if kms.Config.Attribution == nil {
		return fmt.Errorf("tmKMS %q has no resource attribution configured", kms.Name)
	}
	var configMapErr error
	if err := kms.deleteConfigMap(ctx); err != nil {
		configMapErr = fmt.Errorf("delete tmKMS ConfigMap %q: %w", kms.Name, err)
	}

	var secretErr error
	if err := kms.deleteIdentitySecret(ctx); err != nil {
		secretErr = fmt.Errorf("delete tmKMS Secret %q: %w", kms.Name, err)
	}
	return errors.Join(configMapErr, secretErr)
}

func (kms *KMS) deleteConfigMap(ctx context.Context) error {
	cms := kms.Client.CoreV1().ConfigMaps(kms.Owner.GetNamespace())
	cm, err := cms.Get(ctx, kms.Name, metav1.GetOptions{})
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	owned, err := kms.controlledByOwnerOrPredecessor(cm)
	if err != nil || !owned {
		return err
	}
	return ignoreGoneOrReplaced(cms.Delete(ctx, kms.Name, deleteExactly(cm.GetUID())))
}

func (kms *KMS) deleteIdentitySecret(ctx context.Context) error {
	secrets := kms.Client.CoreV1().Secrets(kms.Owner.GetNamespace())
	secret, err := secrets.Get(ctx, kms.Name, metav1.GetOptions{})
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	owned, err := kms.claims(ctx, secret, ClassIdentity, hasIdentitySecretShape(secret))
	if err != nil || !owned {
		return err
	}
	return ignoreGoneOrReplaced(secrets.Delete(ctx, kms.Name, deleteExactly(secret.GetUID())))
}

func deleteExactly(uid types.UID) metav1.DeleteOptions {
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
}

// ignoreGoneOrReplaced treats a missing object, or one replaced after it was read (UID precondition
// conflict), as nothing left to delete.
func ignoreGoneOrReplaced(err error) error {
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

func (kms *KMS) getConfigToml() (string, error) {
	return utils.TomlEncode(kms.Config)
}

func (kms *KMS) getConfigHash() string {
	cfg, _ := kms.getConfigToml()
	return utils.Sha256(cfg)
}

func (kms *KMS) ensureIdentityKey(ctx context.Context) error {
	secrets := kms.Client.CoreV1().Secrets(kms.Owner.GetNamespace())
	secret, err := secrets.Get(ctx, kms.Name, metav1.GetOptions{})
	if err == nil {
		owned, err := kms.claims(ctx, secret, ClassIdentity, hasIdentitySecretShape(secret))
		if err != nil {
			return err
		}
		if !owned {
			return kms.notClaimedError("Secret", ClassIdentity)
		}
		if _, ok := secret.Data[identityKeyName]; !ok {
			return fmt.Errorf("tmKMS Secret %q has no %s key", kms.Name, identityKeyName)
		}
		if kms.stamp(secret, ClassIdentity) {
			_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		}
		return err
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	key, err := kms.generateKmsIdentityKey(ctx)
	if err != nil {
		return err
	}
	spec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kms.Name,
			Namespace: kms.Owner.GetNamespace(),
		},
		Immutable: ptr.To(true),
		Data:      map[string][]byte{identityKeyName: []byte(key)},
	}
	kms.stamp(spec, ClassIdentity)
	_, err = secrets.Create(ctx, spec, metav1.CreateOptions{})
	return err
}

func (kms *KMS) generateKmsIdentityKey(ctx context.Context) (string, error) {
	pod, err := kms.identityGenerationPod()
	if err != nil {
		return "", err
	}

	if err := kms.replaceHelperPod(ctx, pod.GetName()); err != nil {
		return "", err
	}

	ph := k8s.NewPodHelper(kms.Client, nil, pod)
	if err := ph.Create(ctx); err != nil {
		return "", err
	}
	// Delete the pod independently of the result
	defer func() { _ = ph.DeleteWithUIDPrecondition(ctx) }()

	// Wait for the pod to finish
	if err := ph.WaitForPodSucceeded(ctx, time.Minute); err != nil {
		return "", err
	}

	// Grab identity key file content
	out, err := ph.GetLogs(ctx, "busybox")
	return strings.TrimSpace(out), err
}

func (kms *KMS) identityGenerationPod() (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-generate-identity", kms.Name),
			Namespace: kms.Owner.GetNamespace(),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:   corev1.RestartPolicyNever,
			SecurityContext: k8s.RestrictedPodSecurityContext(),
			ImagePullSecrets: append([]corev1.LocalObjectReference(nil),
				kms.Config.ImagePullSecrets...),
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			InitContainers: []corev1.Container{
				{
					Name:            tmkmsAppName,
					Image:           kms.Config.Image,
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: k8s.RestrictedSecurityContext(),
					Command:         []string{"/bin/sh", "-c", "tmkms init /data/tmkms"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: "/data/tmkms",
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "busybox",
					Image:           kms.Config.Image,
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: k8s.RestrictedSecurityContext(),
					Command:         []string{"cat", "/data/tmkms/secrets/" + identityKeyName},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: "/data/tmkms",
						},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(kms.Owner, pod, kms.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

func (kms *KMS) ensureConfigMap(ctx context.Context) error {
	config, err := kms.getConfigToml()
	if err != nil {
		return err
	}

	spec := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kms.Name,
			Namespace: kms.Owner.GetNamespace(),
		},
		Data: map[string]string{configFileName: config},
	}
	if err := controllerutil.SetControllerReference(kms.Owner, spec, kms.Scheme); err != nil {
		return err
	}

	cms := kms.Client.CoreV1().ConfigMaps(kms.Owner.GetNamespace())
	cm, err := cms.Get(ctx, kms.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, err = cms.Create(ctx, spec, metav1.CreateOptions{})
		}
		return err
	}
	if !metav1.IsControlledBy(cm, kms.Owner) {
		predecessor, err := kms.controlledByOwnerOrPredecessor(cm)
		if err != nil {
			return err
		}
		if !predecessor {
			return notOwnedError("ConfigMap", kms.Name)
		}
		// Left by a deleted owner of the same name and still waiting for garbage collection.
		if err := ignoreGoneOrReplaced(cms.Delete(ctx, kms.Name, deleteExactly(cm.GetUID()))); err != nil {
			return err
		}
		_, err = cms.Create(ctx, spec, metav1.CreateOptions{})
		return err
	}

	// Update when config changes
	if cm.Data[configFileName] != config {
		cm.Data[configFileName] = config
		_, err = kms.Client.CoreV1().ConfigMaps(kms.Owner.GetNamespace()).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

func (kms *KMS) ensurePVC(ctx context.Context) error {
	storageSize, err := resource.ParseQuantity(tmkmsPvcSize)
	if err != nil {
		return err
	}

	spec := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kms.Name,
			Namespace: kms.Owner.GetNamespace(),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}

	pvcs := kms.Client.CoreV1().PersistentVolumeClaims(kms.Owner.GetNamespace())
	pvc, err := pvcs.Get(ctx, kms.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			kms.stamp(spec, ClassState)
			_, err = pvcs.Create(ctx, spec, metav1.CreateOptions{})
		}
		return err
	}
	owned, err := kms.claims(ctx, pvc, ClassState, hasStatePVCShape(pvc))
	if err != nil {
		return err
	}
	if !owned {
		return kms.notClaimedError("PersistentVolumeClaim", ClassState)
	}
	if isBlockVolume(pvc) {
		return fmt.Errorf("tmKMS PersistentVolumeClaim %q is a block volume; TmKMS needs a filesystem volume for its state", kms.Name)
	}
	if kms.stamp(pvc, ClassState) {
		_, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{})
	}
	return err
}

func (kms *KMS) GetVolumes() []corev1.Volume {
	// Build volumes list with providers volumes
	volumes := []corev1.Volume{
		{
			Name: "tmkms-identity",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: kms.Name,
				},
			},
		},
		{
			Name: "tmkms-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: kms.Name,
					},
				},
			},
		},
	}

	if kms.Config.PersistState {
		volumes = append(volumes, corev1.Volume{
			Name: "tmkms-data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: kms.Name,
				},
			},
		})
	} else {
		volumes = append(volumes, corev1.Volume{
			Name: "tmkms-data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	for provider := range kms.Config.Providers {
		for _, p := range kms.Config.Providers[provider] {
			volumes = append(volumes, p.getVolumes()...)
		}
	}

	return volumes
}

func (kms *KMS) GetContainersSpec() []corev1.Container {
	// Build volume mounts list with provider volume mounts
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "tmkms-data",
			MountPath: "/data",
		},
		{
			Name:      "tmkms-identity",
			MountPath: "/data/" + identityKeyName,
			SubPath:   identityKeyName,
		},
		{
			Name:      "tmkms-config",
			MountPath: "/data/" + configFileName,
			SubPath:   configFileName,
		},
	}
	for provider := range kms.Config.Providers {
		for _, p := range kms.Config.Providers[provider] {
			volumeMounts = append(volumeMounts, p.getVolumeMounts()...)
		}
	}

	containers := []corev1.Container{
		{
			Name:            tmkmsAppName,
			Image:           kms.Config.Image,
			ImagePullPolicy: corev1.PullAlways,
			SecurityContext: k8s.RestrictedSecurityContext(),
			Args:            []string{"start", "-c", "/data/" + configFileName},
			VolumeMounts:    volumeMounts,
			Env: []corev1.EnvVar{
				{
					Name:  "ROLLME",
					Value: kms.getConfigHash(),
				},
			},
			Resources: kms.Config.Resources,
			StartupProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/tmkms_active",
						Port: intstr.IntOrString{
							Type:   intstr.Int,
							IntVal: 8000,
						},
						Scheme: "HTTP",
					},
				},
				// Startup failure after 1h (not likely to happen)
				FailureThreshold: 720,
				PeriodSeconds:    5,
				TimeoutSeconds:   5,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/tmkms_active",
						Port: intstr.IntOrString{
							Type:   intstr.Int,
							IntVal: 8000,
						},
						Scheme: "HTTP",
					},
				},
				FailureThreshold: 2,
				PeriodSeconds:    2,
				TimeoutSeconds:   5,
			},
		},
	}

	for provider := range kms.Config.Providers {
		for _, p := range kms.Config.Providers[provider] {
			containers = append(containers, p.getContainers()...)
		}
	}

	return containers
}
