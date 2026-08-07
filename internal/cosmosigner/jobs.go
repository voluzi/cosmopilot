package cosmosigner

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
)

const (
	jobActiveDeadlineSeconds int64 = 300
	// gcpImportActiveDeadlineSeconds is the budget for a Cloud KMS BYOK import, which waits for an
	// ImportJob's wrapping key to be generated (minutes for an HSM job) and then up to two more
	// minutes for the imported version to leave PENDING_IMPORT. The flow is idempotent, so an
	// overrunning pod is retried rather than lost — but a budget this size avoids churning through
	// deadline-exceeded pods for an import that was progressing normally.
	gcpImportActiveDeadlineSeconds int64 = 900
	// jobDeleteTimeout bounds waiting for a previous run's pod to finish terminating before
	// recreating it (pod deletion is asynchronous; an immediate Create races AlreadyExists).
	jobDeleteTimeout = time.Minute

	// importSourceVolume mounts the source priv_validator_key.json for `cosmosigner import`.
	importSourceVolume = "import-source"
	importSourceDir    = "/import"
	importSourceFile   = importSourceDir + "/priv_validator_key.json"

	// importJobSuffix names the one-shot `cosmosigner import` pod: <signer>-import.
	importJobSuffix        = "import"
	importTargetAnnotation = "cosmopilot.voluzi.com/cosmosigner-import-target"
	// pubkeyJobSuffix names the one-shot `cosmosigner pubkey` pod: <signer>-pubkey.
	pubkeyJobSuffix = "pubkey"
)

// jobWaitTimeout is the pod's own execution deadline (ActiveDeadlineSeconds — which only starts
// counting once the pod is scheduled) plus an allowance for scheduling latency, so the controller
// never gives up on a pod the kubelet would still have let finish.
func jobWaitTimeout(deadlineSeconds int64) time.Duration {
	return time.Duration(deadlineSeconds)*time.Second + 2*time.Minute
}

// JobRunner runs the one-shot cosmosigner key-management pods (pubkey, import). It needs the
// clientset for pod log scraping, mirroring the TmKMS identity/upload pattern.
type JobRunner struct {
	Client kubernetes.Interface
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
			{Name: "COSMOSIGNER_VAULT_KEY_VERSION", Value: strconv.Itoa(b.Vault.KeyVersion)},
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
		if b.Vault.SkipCertificateVerify {
			env = append(env, corev1.EnvVar{Name: "VAULT_SKIP_VERIFY", Value: "true"})
		}
		return env
	case b.GCP != nil:
		env := []corev1.EnvVar{{Name: "COSMOSIGNER_BACKEND", Value: backendGcpKms}}
		// A managed import has no key version until it runs. Emitting an empty one would look like a
		// deliberate (invalid) backend selection rather than an absent value.
		if b.GCP.KeyVersion != "" {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_GCP_KEY_VERSION", Value: b.GCP.KeyVersion})
		}
		if b.GCP.CredentialsSecret != nil {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_GCP_CREDENTIALS_FILE", Value: gcpCredsFile})
		}
		return env
	default:
		return nil
	}
}

func (b Backend) backendArgs() []string {
	if b.Vault != nil {
		return []string{"--vault-key-version", strconv.Itoa(b.Vault.KeyVersion)}
	}
	return nil
}

// importArgs is the full `cosmosigner import` invocation for this backend. Vault addresses its
// destination through the backend env (mount + key name); GCP KMS addresses the destination key ring
// and crypto key through FLAGS, which is a distinct set from the backend's own env — in particular
// --gcp-key-version is never passed, since the version is precisely what the import creates.
func (b Backend) importArgs() []string {
	args := []string{"import", "--from", importSourceFile}
	if b.GCP == nil || b.GCP.Import == nil {
		return args
	}
	i := b.GCP.Import
	return append(args,
		"--gcp-project", i.Project,
		"--gcp-location", i.Location,
		"--gcp-keyring", i.KeyRing,
		"--gcp-key", i.Key,
		"--gcp-protection", i.ProtectionLevel,
		"--gcp-import-job", i.ImportJob,
	)
	// The credentials file, when configured, arrives through COSMOSIGNER_GCP_CREDENTIALS_FILE in
	// backendEnv(); repeating it as a flag would be redundant.
}

// importSourceVolume mounts the source consensus key for `cosmosigner import`. Only
// priv_validator_key.json is projected: the referenced Secret may hold unrelated material (e.g. a
// validator's account mnemonic in a shared Secret) that the import pod has no business reading.
func importSourceMount(sourceSecret string) ([]corev1.Volume, []corev1.VolumeMount) {
	return []corev1.Volume{{
			Name: importSourceVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: sourceSecret,
					Items:      []corev1.KeyToPath{{Key: "priv_validator_key.json", Path: "priv_validator_key.json"}},
				},
			},
		}}, []corev1.VolumeMount{
			{Name: importSourceVolume, ReadOnly: true, MountPath: importSourceDir},
		}
}

// buildPod renders a one-shot cosmosigner pod. It is pure so the rendered command line, mounts and
// security posture can be asserted without a cluster.
func (j JobRunner) buildPod(nameSuffix string, args []string, extraVolumes []corev1.Volume, extraMounts []corev1.VolumeMount, deadline int64) *corev1.Pod {
	args = append(args, j.Params.Backend.backendArgs()...)
	volumes := append(j.Params.Backend.volumes(), extraVolumes...)
	mounts := append(j.Params.Backend.volumeMounts(), extraMounts...)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", j.Params.Name, nameSuffix),
			Namespace: j.Params.Namespace,
			Labels:    j.Params.Labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Kubelet reaps the pod even if cosmopilot dies mid-call before the deferred delete runs.
			ActiveDeadlineSeconds: ptr.To(deadline),
			SecurityContext:       k8s.RestrictedPodSecurityContext(),
			// Same service account as the signer pods: it may carry the imagePullSecrets or identity
			// bindings the cosmosigner image needs, without which this one-shot pod could never start.
			ServiceAccountName: j.Params.ServiceAccountName,
			ImagePullSecrets:   j.Params.ImagePullSecrets,
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
}

// buildImportPod renders the one-shot `cosmosigner import` pod for this backend.
func (j JobRunner) buildImportPod(sourceSecret string) *corev1.Pod {
	volumes, mounts := importSourceMount(sourceSecret)
	pod := j.buildPod(importJobSuffix, j.Params.Backend.importArgs(), volumes, mounts, j.importDeadline())
	if j.Params.Backend.GCP != nil && j.Params.Backend.GCP.Import != nil {
		pod.Annotations = map[string]string{importTargetAnnotation: j.Params.Backend.GCP.Import.CryptoKeyName()}
	}
	return pod
}

// buildPubkeyPod renders the one-shot `cosmosigner pubkey` pod. keyVersion pins a GCP KMS crypto key
// version explicitly; it is empty for every other backend (and for a pre-provisioned GCP signer,
// whose version already comes from the backend env).
func (j JobRunner) buildPubkeyPod(keyVersion string) *corev1.Pod {
	args := []string{"pubkey"}
	if keyVersion != "" {
		args = append(args, "--gcp-key-version", keyVersion)
	}
	return j.buildPod(pubkeyJobSuffix, args, nil, nil, jobActiveDeadlineSeconds)
}

// importDeadline is the pod-side execution budget for an import. A GCP KMS import waits for an
// ImportJob to be generated (minutes for HSM) and then up to two more minutes for the imported
// version to leave PENDING_IMPORT, which does not fit the default one-shot budget.
func (j JobRunner) importDeadline() int64 {
	if j.Params.Backend.GCP != nil && j.Params.Backend.GCP.Import != nil {
		return gcpImportActiveDeadlineSeconds
	}
	return jobActiveDeadlineSeconds
}

// runJob creates a one-shot pod, waits for it to succeed and returns its logs. When retainSucceeded
// is true, an existing owned pod that already carries a result is resumed instead of replaced, and a
// freshly succeeded pod is retained so the caller can durably persist its result before cleanup.
func (j JobRunner) runJob(ctx context.Context, pod *corev1.Pod, nameSuffix string, deadline int64, retainSucceeded bool) (string, error) {
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
		if retainSucceeded && existing.Annotations[importTargetAnnotation] == pod.Annotations[importTargetAnnotation] {
			// Only a same-owner pod from the CURRENT destination is recovery evidence. A pod from a
			// previous destination (or one with no matching annotation) must not be re-read — its old
			// logs would wedge the new destination migration — so we fall through to delete+recreate.
			existingHelper := k8s.NewPodHelper(j.Client, nil, existing)
			switch existing.Status.Phase {
			case corev1.PodSucceeded:
				// A succeeded pod is the record of a version Cloud KMS accepted, whatever its logs say:
				// re-running the import would create a second version, so the logs are returned as-is and
				// the caller reports an unusable result rather than importing again.
				return existingHelper.GetLogs(ctx, containerName)
			case corev1.PodFailed:
				// A Failed pod may still have created the version before a later verification step failed,
				// and then its logs are the only record of it. But only logs that actually NAME a usable
				// version of the current destination are that record: a pod that died before importing
				// (and logs that can no longer be read) would otherwise be re-read verbatim on every
				// reconcile, failing the same way forever with no way out. Nothing recoverable means
				// nothing to lose, so fall through to delete+recreate and retry the import — re-importing
				// the same source bytes only ever adds another version of the same consensus identity.
				if logs, logErr := existingHelper.GetLogs(ctx, containerName); logErr == nil && j.recoverableImportLogs(logs) {
					return logs, nil
				}
			default:
				// Never replace an in-flight GCP import: Cloud KMS may already have accepted the wrapped
				// key even though the Pod has not reached a terminal phase. Wait for this exact pod, and
				// on timeout surface the wait error unless the pod already reported a usable version.
				if err := existingHelper.WaitForPodSucceeded(ctx, jobWaitTimeout(deadline)); err != nil {
					if logs, logErr := existingHelper.GetLogs(ctx, containerName); logErr == nil && j.recoverableImportLogs(logs) {
						return logs, nil
					}
					return "", err
				}
				return existingHelper.GetLogs(ctx, containerName)
			}
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
	// AlreadyExists from a concurrent same-named owner) never deletes a pod that is not ours. Managed
	// GCP imports retain a succeeded pod until the created key version is durably persisted.
	if !retainSucceeded {
		defer func() { _ = ph.Delete(ctx) }()
	}

	// Re-read once before starting the watch. Besides avoiding a needless watch when a very short job
	// has already completed, this closes the create→watch race where the terminal update lands before
	// the watch is established (and makes the protocol deterministic with the client-go fake tracker).
	created, err := j.Client.CoreV1().Pods(j.Params.Namespace).Get(ctx, pod.GetName(), metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if created.Status.Phase != corev1.PodSucceeded {
		if err := ph.WaitForPodSucceeded(ctx, jobWaitTimeout(deadline)); err != nil {
			return "", err
		}
	}
	return ph.GetLogs(ctx, containerName)
}

// recoverableImportLogs reports whether the output of an import pod that did NOT succeed still names
// a crypto key version of the current destination — i.e. whether re-running the import would risk
// creating a second version of an already-imported consensus key. It applies the same containment
// check the caller does (ParseImportedKeyVersion), so evidence accepted here is evidence the caller
// can act on.
func (j JobRunner) recoverableImportLogs(logs string) bool {
	if j.Params.Backend.GCP == nil || j.Params.Backend.GCP.Import == nil {
		return false
	}
	_, err := ParseImportedKeyVersion(logs, j.Params.Backend.GCP.Import.CryptoKeyName())
	return err == nil
}

// ImportKey runs `cosmosigner import` to import an existing priv_validator_key.json (held in
// sourceSecret) into the configured backend. For a GCP KMS managed import it returns the exact
// crypto key version the import created, verified to live under the configured destination key; for
// Vault (whose destination is the named transit key itself) it returns "".
//
// A successful pod is NOT proof of a durably usable key: the CLI exits zero when Cloud KMS is still
// finalizing the version. Callers must persist the returned version and then read the public key
// back from it before treating the import as complete.
func (j JobRunner) ImportKey(ctx context.Context, sourceSecret string) (string, error) {
	pod := j.buildImportPod(sourceSecret)
	retainSucceeded := j.Params.Backend.GCP != nil && j.Params.Backend.GCP.Import != nil
	out, err := j.runJob(ctx, pod, importJobSuffix, j.importDeadline(), retainSucceeded)
	if err != nil {
		return "", err
	}
	if j.Params.Backend.GCP == nil || j.Params.Backend.GCP.Import == nil {
		return "", nil
	}
	return ParseImportedKeyVersion(out, j.Params.Backend.GCP.Import.CryptoKeyName())
}

// importPodName is the one-shot `cosmosigner import` pod of this signer.
func (j JobRunner) importPodName() string {
	return fmt.Sprintf("%s-%s", j.Params.Name, importJobSuffix)
}

// CleanupImportPod deletes the one-shot import pod once the caller has durably persisted its result.
// Cleanup is deliberately NOT deferred inside ImportKey: a Cloud KMS import creates a new crypto key
// version on every invocation, so between the pod succeeding and the controller recording the version
// it created, that pod's logs are the ONLY record of it — deleting it earlier turns a crash into a
// second imported version. A pod that is already gone, or one another owner now holds (the UID
// precondition rejects it), is left alone.
func (j JobRunner) CleanupImportPod(ctx context.Context) error {
	pods := j.Client.CoreV1().Pods(j.Params.Namespace)
	existing, err := pods.Get(ctx, j.importPodName(), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(existing, j.Owner) {
		return nil
	}
	uid := existing.GetUID()
	err = pods.Delete(ctx, existing.GetName(), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
	if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
		return err
	}
	return nil
}

// PublicKey runs `cosmosigner pubkey` against the configured backend and returns the canonical
// base64 Ed25519 public key. Controllers use it to decide whether a migration may retain raft state.
func (j JobRunner) PublicKey(ctx context.Context) (string, error) {
	return j.publicKey(ctx, "")
}

// PublicKeyAtVersion reads the public key of one exact Cloud KMS crypto key version. The managed
// import verifies its result through this rather than PublicKey: the backend must be proven to serve
// the version that was just created, not whichever version it would otherwise resolve.
func (j JobRunner) PublicKeyAtVersion(ctx context.Context, keyVersion string) (string, error) {
	if keyVersion == "" {
		return "", fmt.Errorf("cosmosigner pubkey requires a key version")
	}
	return j.publicKey(ctx, keyVersion)
}

func (j JobRunner) publicKey(ctx context.Context, keyVersion string) (string, error) {
	out, err := j.runJob(ctx, j.buildPubkeyPod(keyVersion), pubkeyJobSuffix, jobActiveDeadlineSeconds, false)
	if err != nil {
		return "", err
	}
	return ParsePublicKeyOutput(out)
}

// ParseImportedKeyVersion extracts the crypto key version `cosmosigner import` reports and requires
// it to belong to cryptoKeyName. Anything else — a truncated resource name, a non-numeric version, or
// a version under a different crypto key — would retarget the validator at an unintended identity, so
// it is rejected rather than persisted.
func ParseImportedKeyVersion(out, cryptoKeyName string) (string, error) {
	const prefix = "imported key version:"
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		version := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		suffix, ok := strings.CutPrefix(version, cryptoKeyName+"/cryptoKeyVersions/")
		if !ok || cryptoKeyName == "" {
			return "", fmt.Errorf("cosmosigner imported key version %q is not a version of %q; refusing to retarget the validator", version, cryptoKeyName)
		}
		if suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			return "", fmt.Errorf("cosmosigner imported key version %q does not end in a numeric version", version)
		}
		return version, nil
	}
	return "", fmt.Errorf("cosmosigner import output did not contain %q", prefix)
}

// ParsePublicKeyOutput extracts and validates the stable public-key line emitted by cosmosigner.
func ParsePublicKeyOutput(out string) (string, error) {
	const prefix = "pubkey (base64):"
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		encoded := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode cosmosigner public key: %w", err)
		}
		if len(raw) != 32 {
			return "", fmt.Errorf("cosmosigner public key is %d bytes, want 32", len(raw))
		}
		return base64.StdEncoding.EncodeToString(raw), nil
	}
	return "", fmt.Errorf("cosmosigner pubkey output did not contain %q", prefix)
}

// PublicKeyFromSecret reads and validates a CometBFT priv_validator_key.json Secret and returns its
// canonical base64 consensus public key.
func PublicKeyFromSecret(ctx context.Context, c client.Client, namespace, name string) (string, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return "", err
	}
	key, err := cometbft.LoadPrivKey(secret.Data["priv_validator_key.json"])
	if err != nil {
		return "", fmt.Errorf("read cosmosigner public key from Secret %q: %w", name, err)
	}
	raw, err := base64.StdEncoding.DecodeString(key.PubKey.Value)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("read cosmosigner public key from Secret %q: invalid Ed25519 public key", name)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
