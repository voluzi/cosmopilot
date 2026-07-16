package cosmosigner

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v2/internal/k8s"
)

const (
	jobActiveDeadlineSeconds int64 = 300
	// jobWaitTimeout is the pod's own execution deadline (ActiveDeadlineSeconds — which only starts
	// counting once the pod is scheduled) plus an allowance for scheduling latency, so the
	// controller never gives up on a pod the kubelet would still have let finish.
	jobWaitTimeout = time.Duration(jobActiveDeadlineSeconds)*time.Second + 2*time.Minute
	// jobDeleteTimeout bounds waiting for a previous run's pod to finish terminating before
	// recreating it (pod deletion is asynchronous; an immediate Create races AlreadyExists).
	jobDeleteTimeout = time.Minute

	// importSourceVolume mounts the source priv_validator_key.json for `cosmosigner import`.
	importSourceVolume = "import-source"
	importSourceDir    = "/import"
	importSourceFile   = importSourceDir + "/priv_validator_key.json"

	// importJobSuffix names the one-shot `cosmosigner import` pod: <signer>-import.
	importJobSuffix = "import"
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
			// Same service account as the signer pods: it may carry the imagePullSecrets or identity
			// bindings the cosmosigner image needs, without which this one-shot pod could never start.
			ServiceAccountName: j.Params.ServiceAccountName,
			Volumes:            volumes,
			Containers: []corev1.Container{
				{
					Name:            containerName,
					Image:           j.Params.Image,
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

	// A pre-existing pod with this name is only ours to delete when this owner controls it — a
	// same-named signer owner's (or unrelated) pod must not be touched. The delete carries a UID
	// precondition so a pod recreated by another owner between the check and the delete is never
	// removed (the apiserver rejects the mismatched UID).
	if existing, err := j.Client.CoreV1().Pods(j.Params.Namespace).Get(ctx, pod.GetName(), metav1.GetOptions{}); err == nil {
		if !metav1.IsControlledBy(existing, j.Owner) {
			return "", fmt.Errorf("pod %q already exists and is managed by another owner; rename the ChainNode/ChainNodeSet to avoid the name collision", pod.GetName())
		}
		uid := existing.GetUID()
		if err := j.Client.CoreV1().Pods(j.Params.Namespace).Delete(ctx, pod.GetName(), metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		}); err != nil {
			switch {
			case errors.IsNotFound(err):
				// Already gone; continue to the name-delete wait below.
			case errors.IsConflict(err):
				// The pod at this name is no longer the UID whose owner we checked. Waiting by name
				// would now wait on a replacement pod that may belong to another owner, so surface the
				// collision instead of timing out against the wrong object.
				return "", fmt.Errorf("pod %q changed while deleting the previous run; refusing to wait on an unverified replacement", pod.GetName())
			default:
				return "", err
			}
		}
	} else if !errors.IsNotFound(err) {
		return "", err
	}

	ph := k8s.NewPodHelper(j.Client, nil, pod)
	// Wait for the (asynchronously) deleted previous pod to actually go away — an immediate Create
	// would race AlreadyExists.
	if err := ph.WaitForPodDeleted(ctx, jobDeleteTimeout); err != nil {
		return "", fmt.Errorf("waiting for previous %s pod to terminate: %w", nameSuffix, err)
	}

	if err := ph.Create(ctx); err != nil {
		return "", err
	}
	// Cleanup is only deferred once THIS controller created the pod, so a failed Create (e.g.
	// AlreadyExists from a concurrent same-named owner) never deletes a pod that is not ours.
	defer func() { _ = ph.Delete(ctx) }()

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
	_, err := j.runJob(ctx, importJobSuffix, []string{"import", "--from", importSourceFile}, volumes, mounts)
	return err
}
