package chainutils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/voluzi/cosmopilot/v2/internal/chainutils/sdkcmd"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

var (
	// httpClient is a shared HTTP client with reasonable timeout for genesis downloads
	// Note: Individual requests should use context for cancellation control
	httpClient = &http.Client{
		Timeout: 5 * time.Minute,
	}
)

func genesisPodRunningTimeout(extraValidators []*GenesisValidator) time.Duration {
	return time.Minute + time.Duration(len(extraValidators))*2*time.Minute
}

func ExtractChainIdFromGenesis(genesis string) (string, error) {
	var genesisJson map[string]interface{}
	if err := json.Unmarshal([]byte(genesis), &genesisJson); err != nil {
		return "", err
	}
	chainIDRaw, ok := genesisJson["chain_id"]
	if !ok {
		return "", fmt.Errorf("chain_id field not found in genesis")
	}
	chainID, ok := chainIDRaw.(string)
	if !ok {
		return "", fmt.Errorf("chain_id is not a string type")
	}
	return chainID, nil
}

func (a *App) NewGenesis(ctx context.Context,
	privkeySecret string,
	account *Account,
	nodeInfo *NodeInfo,
	params *Params,
	extraValidators []*GenesisValidator,
	initCommands ...*InitCommand,
) (string, error) {
	pod := a.buildGenesisPod(privkeySecret, account, nodeInfo, params, extraValidators, initCommands)

	if err := controllerutil.SetControllerReference(a.owner, pod, a.scheme); err != nil {
		return "", err
	}

	ph := k8s.NewPodHelper(a.client, a.restConfig, pod)

	// Delete the pod if it already exists
	logger := log.FromContext(ctx)
	if err := ph.Delete(ctx); err != nil {
		logger.V(1).Info("failed to delete existing genesis pod (may not exist)", "error", err)
	}

	// Delete the pod independently of the result
	defer func() {
		if err := ph.Delete(ctx); err != nil {
			logger.Error(err, "failed to cleanup genesis pod")
		}
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

	// Feed each extra validator's mnemonic into its own load-account container. Init containers
	// run sequentially, so we wait for each container to be running (blocked on stdin) before
	// attaching, in the same order they appear in the pod spec.
	for i, ev := range extraValidators {
		container := fmt.Sprintf("load-account-%d", i+1)
		if err := ph.WaitForInitContainerRunning(ctx, container, 5*time.Minute); err != nil {
			return "", err
		}
		var evInput bytes.Buffer
		evInput.WriteString(fmt.Sprintf("%s\n", ev.Account.Mnemonic))
		if _, _, err := ph.Attach(ctx, container, &evInput); err != nil {
			return "", err
		}
	}

	// Wait for the pod to be running
	if err := ph.WaitForPodRunning(ctx, genesisPodRunningTimeout(extraValidators)); err != nil {
		return "", err
	}

	genesis, _, err := ph.Exec(ctx, "busybox", []string{"cat", filepath.Join(defaultHome, defaultGenesisFile)})
	return genesis, err
}

// buildGenesisPod constructs the ephemeral pod that initializes a new genesis. The pod runs a
// sequence of init containers: chain init, consensus-key and account loading, genesis account and
// gentx generation for the owning validator and every extra genesis validator, and finally
// collect-gentxs. Init-container names are unique across the regular accounts and the extra
// validators so the resulting pod spec is always valid.
func (a *App) buildGenesisPod(
	privkeySecret string,
	account *Account,
	nodeInfo *NodeInfo,
	params *Params,
	extraValidators []*GenesisValidator,
	initCommands []*InitCommand,
) *corev1.Pod {
	var (
		dataVolumeMount = corev1.VolumeMount{
			Name:      "data",
			MountPath: defaultHome,
		}
		privKeyVolumeMount = corev1.VolumeMount{
			Name:      "priv-key",
			MountPath: "/secrets",
		}
		tempVolumeMount = corev1.VolumeMount{
			Name:      "temp",
			MountPath: "/temp",
		}
	)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-genesis-init", a.owner.GetName()),
			Namespace: a.owner.GetNamespace(),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: a.priorityClassName,
			Affinity:          a.Affinity,
			NodeSelector:      a.NodeSelector,
			Volumes: []corev1.Volume{
				{
					Name: dataVolumeMount.Name,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: tempVolumeMount.Name,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: privKeyVolumeMount.Name,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: privkeySecret,
						},
					},
				},
			},
			InitContainers: []corev1.Container{
				{
					Name:            "init-chain",
					Image:           a.image,
					ImagePullPolicy: a.pullPolicy,
					Command:         []string{a.binary},
					Args:            a.cmd.InitArgs(nodeInfo.Moniker, params.ChainID),
					VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
					SecurityContext: k8s.RestrictedSecurityContext(),
				},
				{
					Name:            "load-priv-key",
					Image:           "busybox",
					Command:         []string{"/bin/sh"},
					Args:            []string{"-c", "cp /secrets/priv_validator_key.json /home/app/config/priv_validator_key.json"},
					VolumeMounts:    []corev1.VolumeMount{dataVolumeMount, privKeyVolumeMount},
					SecurityContext: k8s.RestrictedSecurityContext(),
				},
				{
					Name:            "load-account",
					Image:           a.image,
					ImagePullPolicy: a.pullPolicy,
					Command:         []string{a.binary},
					Args:            a.cmd.RecoverAccountArgs(defaultAccountName),
					Stdin:           true,
					StdinOnce:       true,
					VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
					SecurityContext: k8s.RestrictedSecurityContext(),
				},
				{
					Name:            "add-validator-account",
					Image:           a.image,
					ImagePullPolicy: a.pullPolicy,
					Command:         []string{a.binary},
					Args:            a.cmd.AddGenesisAccountArgs(account.Address, params.Assets),
					VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
					SecurityContext: k8s.RestrictedSecurityContext(),
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "busybox",
					Image:           "busybox",
					Command:         []string{"cat"},
					Stdin:           true,
					VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
					SecurityContext: k8s.RestrictedSecurityContext(),
				},
			},
			TerminationGracePeriodSeconds: ptr.To[int64](0),
		},
	}

	// Add genesis parameter modifications (only when explicitly set)
	if params.UnbondingTime != "" {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:            "set-unbonding-time",
			Image:           "apteno/alpine-jq",
			Command:         []string{"sh", "-c"},
			Args:            []string{a.cmd.GenesisSetUnbondingTimeCmd(params.UnbondingTime, filepath.Join(defaultHome, defaultGenesisFile))},
			VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
			SecurityContext: k8s.RestrictedSecurityContext(),
		})
	}
	if params.VotingPeriod != "" {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:            "set-voting-period",
			Image:           "apteno/alpine-jq",
			Command:         []string{"sh", "-c"},
			Args:            []string{a.cmd.GenesisSetVotingPeriodCmd(params.VotingPeriod, filepath.Join(defaultHome, defaultGenesisFile))},
			VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
			SecurityContext: k8s.RestrictedSecurityContext(),
		})
	}
	if cmd := a.cmd.GenesisSetExpeditedVotingPeriodCmd(params.ExpeditedVotingPeriod, filepath.Join(defaultHome, defaultGenesisFile)); params.ExpeditedVotingPeriod != "" && cmd != "" {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:            "set-expedited-voting-period",
			Image:           "apteno/alpine-jq",
			Command:         []string{"sh", "-c"},
			Args:            []string{cmd},
			VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
			SecurityContext: k8s.RestrictedSecurityContext(),
		})
	}

	// Add additional accounts
	for i, acc := range params.Accounts {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:            fmt.Sprintf("add-account-%d", i),
			Image:           a.image,
			ImagePullPolicy: a.pullPolicy,
			Command:         []string{a.binary},
			Args:            a.cmd.AddGenesisAccountArgs(acc.Address, acc.Assets),
			VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
			SecurityContext: k8s.RestrictedSecurityContext(),
		})
	}

	// Add additional commands
	for i, cmd := range initCommands {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:            fmt.Sprintf("init-command-%d", i),
			Image:           cmd.Image,
			Command:         cmd.Command,
			Args:            cmd.Args,
			VolumeMounts:    []corev1.VolumeMount{dataVolumeMount, tempVolumeMount},
			Resources:       cmd.Resources,
			Env:             cmd.Env,
			SecurityContext: k8s.RestrictedSecurityContext(),
		})
	}

	// Add gentx container
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name:            "gentx",
		Image:           a.image,
		ImagePullPolicy: a.pullPolicy,
		Command:         []string{a.binary},
		Args: a.cmd.GenTxArgs(defaultAccountName, nodeInfo.Moniker, params.StakeAmount, params.ChainID,
			sdkcmd.WithArg(sdkcmd.CommissionMaxChangeRate, params.CommissionMaxChangeRate),
			sdkcmd.WithArg(sdkcmd.CommissionMaxRate, params.CommissionMaxRate),
			sdkcmd.WithArg(sdkcmd.CommissionRate, params.CommissionRate),
			sdkcmd.WithOptionalArg(sdkcmd.MinSelfDelegation, params.MinSelfDelegation),
			sdkcmd.WithOptionalArg(sdkcmd.Details, nodeInfo.Details),
			sdkcmd.WithOptionalArg(sdkcmd.Website, nodeInfo.Website),
			sdkcmd.WithOptionalArg(sdkcmd.Identity, nodeInfo.Identity),
		),
		VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
		SecurityContext: k8s.RestrictedSecurityContext(),
	})

	// Add the extra genesis validators. Each one reuses the same chain home: we swap in its
	// consensus key, recover its account into the keyring under a distinct name, add its account
	// to genesis and generate a gentx for it. Each gentx is written to a distinct file inside the
	// gentx directory (the default filename is derived from the shared node key, so without an
	// explicit output document later gentxs would overwrite earlier ones). collect-gentxs then
	// picks up every gentx at once.
	genTxDir := filepath.Join(defaultHome, defaultConfig, "gentx")
	for i, ev := range extraValidators {
		idx := i + 1
		accountName := fmt.Sprintf("validator-%d", idx)
		privKeyMount := corev1.VolumeMount{
			Name:      fmt.Sprintf("priv-key-%d", idx),
			MountPath: fmt.Sprintf("/secrets-%d", idx),
		}

		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: privKeyMount.Name,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: ev.PrivKeySecret,
				},
			},
		})

		pod.Spec.InitContainers = append(pod.Spec.InitContainers,
			corev1.Container{
				Name:            fmt.Sprintf("load-priv-key-%d", idx),
				Image:           "busybox",
				Command:         []string{"/bin/sh"},
				Args:            []string{"-c", fmt.Sprintf("cp %s/priv_validator_key.json /home/app/config/priv_validator_key.json", privKeyMount.MountPath)},
				VolumeMounts:    []corev1.VolumeMount{dataVolumeMount, privKeyMount},
				SecurityContext: k8s.RestrictedSecurityContext(),
			},
			corev1.Container{
				Name:            fmt.Sprintf("load-account-%d", idx),
				Image:           a.image,
				ImagePullPolicy: a.pullPolicy,
				Command:         []string{a.binary},
				Args:            a.cmd.RecoverAccountArgs(accountName),
				Stdin:           true,
				StdinOnce:       true,
				VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
				SecurityContext: k8s.RestrictedSecurityContext(),
			},
			corev1.Container{
				// Distinct from the regular "add-account-%d" containers above so the two index
				// spaces never produce colliding container names in the same pod.
				Name:            fmt.Sprintf("add-validator-account-%d", idx),
				Image:           a.image,
				ImagePullPolicy: a.pullPolicy,
				Command:         []string{a.binary},
				Args:            a.cmd.AddGenesisAccountArgs(ev.Account.Address, ev.Assets),
				VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
				SecurityContext: k8s.RestrictedSecurityContext(),
			},
			corev1.Container{
				Name:            fmt.Sprintf("gentx-%d", idx),
				Image:           a.image,
				ImagePullPolicy: a.pullPolicy,
				Command:         []string{a.binary},
				Args: a.cmd.GenTxArgs(accountName, ev.NodeInfo.Moniker, ev.StakeAmount, params.ChainID,
					sdkcmd.WithArg(sdkcmd.CommissionMaxChangeRate, params.CommissionMaxChangeRate),
					sdkcmd.WithArg(sdkcmd.CommissionMaxRate, params.CommissionMaxRate),
					sdkcmd.WithArg(sdkcmd.CommissionRate, params.CommissionRate),
					sdkcmd.WithArg(sdkcmd.OutputDocument, filepath.Join(genTxDir, fmt.Sprintf("gentx-%d.json", idx))),
					sdkcmd.WithOptionalArg(sdkcmd.MinSelfDelegation, params.MinSelfDelegation),
					sdkcmd.WithOptionalArg(sdkcmd.Details, ev.NodeInfo.Details),
					sdkcmd.WithOptionalArg(sdkcmd.Website, ev.NodeInfo.Website),
					sdkcmd.WithOptionalArg(sdkcmd.Identity, ev.NodeInfo.Identity),
				),
				VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
				SecurityContext: k8s.RestrictedSecurityContext(),
			},
		)
	}

	// Add collect-gentxs container
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
		Name:            "collect-gentxs",
		Image:           a.image,
		ImagePullPolicy: a.pullPolicy,
		Command:         []string{a.binary},
		Args:            a.cmd.CollectGenTxsArgs(),
		VolumeMounts:    []corev1.VolumeMount{dataVolumeMount},
		SecurityContext: k8s.RestrictedSecurityContext(),
	})

	return pod
}

func (a *App) LoadGenesisFromConfigMap(ctx context.Context, configMapName string) (string, error) {
	cm, err := a.client.CoreV1().ConfigMaps(a.owner.GetNamespace()).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	genesis, ok := cm.Data[GenesisFilename]
	if !ok {
		return "", fmt.Errorf("%q not found in specified configmap", GenesisFilename)
	}
	return genesis, nil
}

func RetrieveGenesisFromURL(ctx context.Context, url string, sha *string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	response, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	genesis := string(body)

	if sha != nil {
		hash := utils.Sha256(genesis)
		if hash != *sha {
			return "", fmt.Errorf("genesis SHA256 mismatch: expected %s, got %s", *sha, hash)
		}
	}

	return genesis, nil
}

func RetrieveGenesisFromNodeRPC(ctx context.Context, url string, sha *string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	out := struct {
		Result struct {
			Genesis json.RawMessage `json:"genesis"`
		} `json:"result"`
	}{}

	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}

	b, err := json.MarshalIndent(out.Result.Genesis, "", "  ")
	if err != nil {
		return "", err
	}

	genesis := string(b) + "\n"

	if sha != nil {
		hash := utils.Sha256(genesis)
		if hash != *sha {
			return "", fmt.Errorf("genesis SHA256 mismatch: expected %s, got %s", *sha, hash)
		}
	}

	return genesis, nil
}
