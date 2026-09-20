package chainutils

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v3/internal/chainutils/sdkcmd"
	"github.com/voluzi/cosmopilot/v3/internal/k8s"
)

type createValidatorResultReader interface {
	WaitForPodSucceeded(context.Context, time.Duration) error
	GetLogs(context.Context, string) (string, error)
}

type createValidatorBroadcastResult struct {
	TxHash    string `json:"txhash"`
	Code      uint32 `json:"code"`
	Codespace string `json:"codespace"`
	RawLog    string `json:"raw_log"`
}

func waitForCreateValidatorResult(ctx context.Context, reader createValidatorResultReader) (string, error) {
	if err := reader.WaitForPodSucceeded(ctx, time.Minute); err != nil {
		return "", err
	}
	logs, err := reader.GetLogs(ctx, "create-validator")
	if err != nil {
		return "", fmt.Errorf("reading create-validator output: %w", err)
	}
	result, err := parseCreateValidatorBroadcastResult(logs)
	if err != nil {
		return "", err
	}
	return result.TxHash, nil
}

func parseCreateValidatorBroadcastResult(output string) (*createValidatorBroadcastResult, error) {
	var result createValidatorBroadcastResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return nil, fmt.Errorf("decoding create-validator output: %w", err)
	}
	if result.Code != 0 {
		if result.TxHash == "" {
			return nil, fmt.Errorf("create-validator CheckTx rejected with code %d, codespace %s: %s", result.Code, result.Codespace, result.RawLog)
		}
		return nil, fmt.Errorf("create-validator CheckTx rejected for transaction %s with code %d, codespace %s: %s", result.TxHash, result.Code, result.Codespace, result.RawLog)
	}
	if result.TxHash == "" {
		return nil, fmt.Errorf("create-validator transaction hash is required")
	}
	hash, err := hex.DecodeString(result.TxHash)
	if err != nil {
		return nil, fmt.Errorf("decode create-validator transaction hash: %w", err)
	}
	if len(hash) != 32 {
		return nil, fmt.Errorf("create-validator transaction hash must be 32 bytes, got %d", len(hash))
	}
	return &result, nil
}

func (a *App) buildCreateValidatorPod(
	pubKey string,
	nodeInfo *NodeInfo,
	params *Params,
	node string,
) (*corev1.Pod, error) {
	validatorFile := filepath.Join(defaultHome, "validator.json")
	command, err := a.cmd.CreateValidatorCommand(
		validatorFile,
		defaultAccountName,
		pubKey,
		nodeInfo.Moniker,
		params.StakeAmount,
		params.ChainID,
		params.GasPrices,
		sdkcmd.WithArg(sdkcmd.CommissionMaxChangeRate, params.CommissionMaxChangeRate),
		sdkcmd.WithArg(sdkcmd.CommissionMaxRate, params.CommissionMaxRate),
		sdkcmd.WithArg(sdkcmd.CommissionRate, params.CommissionRate),
		sdkcmd.WithOptionalArg(sdkcmd.MinSelfDelegation, params.MinSelfDelegation),
		sdkcmd.WithOptionalArg(sdkcmd.Details, nodeInfo.Details),
		sdkcmd.WithOptionalArg(sdkcmd.Website, nodeInfo.Website),
		sdkcmd.WithOptionalArg(sdkcmd.Identity, nodeInfo.Identity),
		sdkcmd.WithArg(sdkcmd.Node, node),
		sdkcmd.WithArg("output", "json"),
		sdkcmd.WithArg("broadcast-mode", "sync"),
	)
	if err != nil {
		return nil, fmt.Errorf("building create-validator command: %w", err)
	}

	var (
		dataVolumeMount = corev1.VolumeMount{
			Name:      "data",
			MountPath: defaultHome,
		}
	)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-create-validator", a.owner.GetName()),
			Namespace: a.owner.GetNamespace(),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: a.priorityClassName,
			Affinity:          a.Affinity,
			NodeSelector:      a.NodeSelector,
			ImagePullSecrets:  a.appImagePullSecrets(),
			SecurityContext:   k8s.RestrictedPodSecurityContext(),
			Volumes: []corev1.Volume{
				{
					Name: dataVolumeMount.Name,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			InitContainers: []corev1.Container{
				{
					Name:            "load-account",
					Image:           a.image,
					ImagePullPolicy: a.pullPolicy,
					Command:         []string{a.binary},
					Args:            a.cmd.RecoverAccountArgs(defaultAccountName),
					Env:             a.appEnv(),
					Stdin:           true,
					StdinOnce:       true,
					VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
					SecurityContext: k8s.RestrictedSecurityContext(),
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "create-validator",
					Image:           a.image,
					ImagePullPolicy: a.pullPolicy,
					Command:         []string{a.binary},
					Args:            command.Args,
					Env:             a.appEnv(),
					VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
					SecurityContext: k8s.RestrictedSecurityContext(),
				},
			},
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			// Kubelet reaps the pod after 5 min even if cosmopilot dies mid-call
			// (SIGKILL prevents `defer ph.Delete` from running).
			ActiveDeadlineSeconds: ptr.To[int64](300),
		},
	}
	if len(command.ValidatorJSON) > 0 {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:            "write-validator-json",
			Image:           a.utilityImageRef(),
			Command:         []string{"/bin/sh", "-c"},
			Args:            []string{`printf '%s' "$1" | base64 -d > "$2"`, "write-validator-json", base64.StdEncoding.EncodeToString(command.ValidatorJSON), validatorFile},
			VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
			SecurityContext: k8s.RestrictedSecurityContext(),
		})
	}
	return pod, nil
}

func (a *App) CreateValidator(
	ctx context.Context,
	pubKey string,
	account *Account,
	nodeInfo *NodeInfo,
	params *Params,
	node string,
) (string, error) {
	pod, err := a.buildCreateValidatorPod(pubKey, nodeInfo, params, node)
	if err != nil {
		return "", err
	}

	if err := controllerutil.SetControllerReference(a.owner, pod, a.scheme); err != nil {
		return "", err
	}

	ph := k8s.NewPodHelper(a.client, a.restConfig, pod)

	// Delete the pod if it already exists
	_ = ph.Delete(ctx)

	// Cleanup must survive request cancellation so the helper cannot be stranded.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = ph.Delete(cleanupCtx)
	}()

	// Create the pod
	if err := ph.Create(ctx); err != nil {
		return "", err
	}

	// Wait for load-account container to be running
	if err := ph.WaitForInitContainerRunning(ctx, "load-account", time.Minute); err != nil {
		return "", err
	}

	// Attach to load-account container to insert mnemonic
	var input bytes.Buffer
	input.WriteString(fmt.Sprintf("%s\n", account.Mnemonic))
	if _, _, err := ph.Attach(ctx, "load-account", &input); err != nil {
		return "", err
	}

	// Wait for the pod to be completed
	return waitForCreateValidatorResult(ctx, ph)
}
