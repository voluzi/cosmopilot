package cosmosigner

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v2/internal/k8s"
)

const (
	jobActiveDeadlineSeconds int64 = 300
	// jobWaitTimeout matches the pod's own ActiveDeadlineSeconds so a slow image pull or Vault
	// round-trip is not killed by the controller before the kubelet would have given up anyway.
	jobWaitTimeout = time.Duration(jobActiveDeadlineSeconds) * time.Second
	// jobDeleteTimeout bounds waiting for a previous run's pod to finish terminating before
	// recreating it (pod deletion is asynchronous; an immediate Create races AlreadyExists).
	jobDeleteTimeout = time.Minute

	// importSourceVolume mounts the source priv_validator_key.json for `cosmosigner import`.
	importSourceVolume = "import-source"
	importSourceDir    = "/import"
	importSourceFile   = importSourceDir + "/priv_validator_key.json"
)

// JobRunner runs the one-shot cosmosigner key-management pods (pubkey, import). It needs the
// clientset for pod log scraping, mirroring the TmKMS identity/upload pattern.
type JobRunner struct {
	Client *kubernetes.Clientset
	Scheme *runtime.Scheme
	Owner  metav1.Object
	Params Params
}

// backendEnv returns the COSMOSIGNER_* environment variables that configure the backend for the
// one-shot commands (which read the backend from env/flags, not the config file).
func (b Backend) backendEnv() []corev1.EnvVar {
	switch {
	case b.Software != nil:
		return []corev1.EnvVar{
			{Name: "COSMOSIGNER_BACKEND", Value: backendSoftware},
			{Name: "COSMOSIGNER_KEY_FILE", Value: softwareKeyFile},
		}
	case b.Vault != nil:
		env := []corev1.EnvVar{
			{Name: "COSMOSIGNER_BACKEND", Value: backendVault},
			{Name: "COSMOSIGNER_VAULT_ADDR", Value: b.Vault.Address},
			{Name: "COSMOSIGNER_VAULT_TOKEN_FILE", Value: vaultTokenFile},
			{Name: "COSMOSIGNER_VAULT_KEY", Value: b.Vault.KeyName},
		}
		if b.Vault.Mount != "" {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_VAULT_MOUNT", Value: b.Vault.Mount})
		}
		if b.Vault.Namespace != "" {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_VAULT_NAMESPACE", Value: b.Vault.Namespace})
		}
		if b.Vault.CertificateSecret != nil {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_VAULT_CA_CERT", Value: vaultCaFile})
		}
		return env
	case b.GCP != nil:
		env := []corev1.EnvVar{
			{Name: "COSMOSIGNER_BACKEND", Value: backendGcpKms},
			{Name: "COSMOSIGNER_GCP_KEY_VERSION", Value: b.GCP.KeyVersion},
		}
		if b.GCP.CredentialsSecret != nil {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_GCP_CREDENTIALS_FILE", Value: gcpCredsFile})
		}
		return env
	default:
		return nil
	}
}

// runJob creates a one-shot pod, waits for it to succeed, returns its logs and always cleans up.
func (j JobRunner) runJob(ctx context.Context, nameSuffix string, args []string, extraVolumes []corev1.Volume, extraMounts []corev1.VolumeMount) (string, error) {
	volumes := append(j.Params.Backend.volumes(), extraVolumes...)
	mounts := append(j.Params.Backend.volumeMounts(), extraMounts...)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", j.Params.Name, nameSuffix),
			Namespace: j.Params.Namespace,
			Labels:    j.Params.Labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Kubelet reaps the pod even if cosmopilot dies mid-call before the deferred delete runs.
			ActiveDeadlineSeconds: ptr.To(jobActiveDeadlineSeconds),
			SecurityContext:       k8s.RestrictedPodSecurityContext(),
			Volumes:               volumes,
			Containers: []corev1.Container{
				{
					Name:            containerName,
					Image:           j.Params.Image,
					ImagePullPolicy: corev1.PullAlways,
					SecurityContext: k8s.RestrictedSecurityContext(),
					Args:            args,
					Env:             j.Params.Backend.backendEnv(),
					VolumeMounts:    mounts,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(j.Owner, pod, j.Scheme); err != nil {
		return "", err
	}

	ph := k8s.NewPodHelper(j.Client, nil, pod)
	// Delete any pod left over from a previous attempt and wait for it to actually go away —
	// deletion is asynchronous and an immediate Create would race AlreadyExists.
	_ = ph.Delete(ctx)
	if err := ph.WaitForPodDeleted(ctx, jobDeleteTimeout); err != nil {
		return "", fmt.Errorf("waiting for previous %s pod to terminate: %w", nameSuffix, err)
	}
	defer func() { _ = ph.Delete(ctx) }()

	if err := ph.Create(ctx); err != nil {
		return "", err
	}
	if err := ph.WaitForPodSucceeded(ctx, jobWaitTimeout); err != nil {
		return "", err
	}
	return ph.GetLogs(ctx, containerName)
}

// ImportKey runs `cosmosigner import` to import an existing priv_validator_key.json (held in
// sourceSecret) into the configured backend. Only meaningful for the Vault backend.
func (j JobRunner) ImportKey(ctx context.Context, sourceSecret string) error {
	volumes := []corev1.Volume{
		{
			Name: importSourceVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: sourceSecret},
			},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: importSourceVolume, ReadOnly: true, MountPath: importSourceFile, SubPath: "priv_validator_key.json"},
	}
	_, err := j.runJob(ctx, "import", []string{"import", "--from", importSourceFile}, volumes, mounts)
	return err
}
