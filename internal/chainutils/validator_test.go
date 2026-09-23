package chainutils

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/k8s"
)

const validValidatorPubKey = `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"oWg2ISpLF405Jcm2vXV+2v4fnjodh6aafuIdeoW+rUw="}`

type fakeCreateValidatorResultReader struct {
	waitErr  error
	logs     string
	logsErr  error
	logsRead int
}

func (f *fakeCreateValidatorResultReader) WaitForPodSucceeded(context.Context, time.Duration) error {
	return f.waitErr
}

func (f *fakeCreateValidatorResultReader) GetLogs(context.Context, string) (string, error) {
	f.logsRead++
	return f.logs, f.logsErr
}

func TestWaitForCreateValidatorResultRejectsCheckTxFailure(t *testing.T) {
	t.Parallel()

	reader := &fakeCreateValidatorResultReader{
		logs: `{"code":5,"codespace":"sdk","raw_log":"insufficient fees"}`,
	}

	_, err := waitForCreateValidatorResult(t.Context(), reader)
	require.Error(t, err)
	assert.ErrorContains(t, err, "CheckTx rejected")
	assert.ErrorContains(t, err, "code 5")
	assert.ErrorContains(t, err, "codespace sdk")
	assert.ErrorContains(t, err, "insufficient fees")
}

func TestParseCreateValidatorBroadcastResult(t *testing.T) {
	t.Parallel()

	validHash := strings.Repeat("ab", 32)
	tests := []struct {
		name       string
		output     string
		wantHash   string
		wantErrors []string
	}{
		{
			name:     "accepted transaction",
			output:   `{"height":"0","txhash":"` + validHash + `","codespace":"","code":0,"data":"","raw_log":"[]"}`,
			wantHash: validHash,
		},
		{
			name:     "accepted transaction after automatic gas estimate",
			output:   "gas estimate: 302357\n" + `{"height":"0","txhash":"` + validHash + `","codespace":"","code":0,"data":"","raw_log":"[]"}`,
			wantHash: validHash,
		},
		{
			name:       "missing transaction hash",
			output:     `{"code":0}`,
			wantErrors: []string{"transaction hash", "required"},
		},
		{
			name:       "malformed transaction hash",
			output:     `{"txhash":"1234","code":0}`,
			wantErrors: []string{"transaction hash", "32 bytes"},
		},
		{
			name:       "unrelated JSON",
			output:     `{"status":"ok"}`,
			wantErrors: []string{"transaction hash", "required"},
		},
		{
			name:       "empty output",
			output:     "",
			wantErrors: []string{"decoding create-validator output"},
		},
		{
			name:       "malformed JSON",
			output:     `{"txhash":`,
			wantErrors: []string{"decoding create-validator output"},
		},
		{
			name:       "rejected transaction retains chain error without hash",
			output:     `{"code":7,"codespace":"sdk","raw_log":"invalid sequence"}`,
			wantErrors: []string{"CheckTx rejected", "code 7", "codespace sdk", "invalid sequence"},
		},
		{
			name:       "rejected transaction includes hash",
			output:     `{"txhash":"` + validHash + `","code":8,"codespace":"staking","raw_log":"validator exists"}`,
			wantErrors: []string{"CheckTx rejected", validHash, "code 8", "codespace staking", "validator exists"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := parseCreateValidatorBroadcastResult(tt.output)
			if len(tt.wantErrors) > 0 {
				require.Error(t, err)
				for _, want := range tt.wantErrors {
					assert.ErrorContains(t, err, want)
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantHash, result.TxHash)
		})
	}
}

func TestWaitForCreateValidatorResultStopsBeforeReadingLogsWhenPodFails(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("pod failed")
	reader := &fakeCreateValidatorResultReader{waitErr: wantErr}

	_, err := waitForCreateValidatorResult(t.Context(), reader)

	require.ErrorIs(t, err, wantErr)
	assert.Zero(t, reader.logsRead)
}

func TestWaitForCreateValidatorResultReportsLogReadFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("stream unavailable")
	reader := &fakeCreateValidatorResultReader{logsErr: wantErr}

	_, err := waitForCreateValidatorResult(t.Context(), reader)

	require.ErrorIs(t, err, wantErr)
	assert.ErrorContains(t, err, "reading create-validator output")
	assert.Equal(t, 1, reader.logsRead)
}

func TestWaitForCreateValidatorResultReturnsAcceptedHash(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("ab", 32)
	reader := &fakeCreateValidatorResultReader{logs: `{"txhash":"` + hash + `","code":0}`}

	got, err := waitForCreateValidatorResult(t.Context(), reader)

	require.NoError(t, err)
	assert.Equal(t, hash, got)
	assert.Equal(t, 1, reader.logsRead)
}

func TestBuildCreateValidatorPodSDKVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sdkVersion appsv1.SdkVersion
		modern     bool
	}{
		{name: "v0.45", sdkVersion: appsv1.V0_45},
		{name: "v0.47", sdkVersion: appsv1.V0_47},
		{name: "v0.50", sdkVersion: appsv1.V0_50, modern: true},
		{name: "v0.53", sdkVersion: appsv1.V0_53, modern: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			app, err := NewApp(nil, scheme, nil, &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
				Name: "node", Namespace: "default", UID: "node-uid",
			}}, tt.sdkVersion, nil, WithBinary("appd"), WithImage("example/app:v1"))
			require.NoError(t, err)

			pod, err := app.buildCreateValidatorPod(
				validValidatorPubKey,
				"",
				&NodeInfo{Moniker: "validator"},
				&Params{
					ChainID:                 "chain-1",
					StakeAmount:             "1000stake",
					CommissionMaxChangeRate: "0.01",
					CommissionMaxRate:       "0.20",
					CommissionRate:          "0.10",
					GasPrices:               "0.025stake",
				},
				"tcp://node:26657",
			)
			require.NoError(t, err)

			args := requireContainer(t, pod.Spec.Containers, "create-validator").Args
			assertCommandArg(t, args, "--gas", "auto")
			assertCommandArg(t, args, "--gas-adjustment", "1.5")
			assertCommandArg(t, args, "--output", "json")
			assertCommandArg(t, args, "--broadcast-mode", "sync")
			if !tt.modern {
				assert.Equal(t, "--amount", args[3])
				for _, container := range pod.Spec.InitContainers {
					assert.NotEqual(t, "write-validator-json", container.Name)
				}
				return
			}

			assert.Equal(t, "/home/app/validator.json", args[3])
			assert.NotContains(t, args, "--amount")
			assert.NotContains(t, args, "--pubkey")
			assert.NotContains(t, args, "--moniker")
			requireContainer(t, pod.Spec.InitContainers, "write-validator-json")
		})
	}
}

func assertCommandArg(t *testing.T, args []string, key, value string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key {
			assert.Equal(t, value, args[i+1])
			return
		}
	}
	t.Errorf("argument %q not found in %v", key, args)
}

func TestBuildCreateValidatorPodMaterializesModernValidatorJSON(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	app, err := NewApp(nil, scheme, nil, &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "node", Namespace: "default", UID: "node-uid",
	}}, appsv1.V0_53, nil,
		WithBinary("appd"),
		WithImage("example/app:v1"),
		WithImagePullPolicy(corev1.PullAlways),
		WithEnv(testAppEnv()),
		WithUtilityImage("example/utility:v2"),
		WithImagePullSecrets([]corev1.LocalObjectReference{{Name: "registry-creds"}}),
	)
	require.NoError(t, err)
	details := "quotes: \"hello\"\nbacktick: ` dollar: $ expansion: $(NAME) unicode: Olá"
	minimum := "7"
	pod, err := app.buildCreateValidatorPod(
		validValidatorPubKey,
		"",
		&NodeInfo{Moniker: "validator", Details: &details},
		&Params{
			ChainID:                 "chain-1",
			StakeAmount:             "1000stake",
			CommissionMaxChangeRate: "0.01",
			CommissionMaxRate:       "0.20",
			CommissionRate:          "0.10",
			MinSelfDelegation:       &minimum,
			GasPrices:               "0.025stake",
		},
		"tcp://node:26657",
	)
	require.NoError(t, err)

	require.Len(t, pod.Spec.InitContainers, 2)
	loadAccount := pod.Spec.InitContainers[0]
	writer := pod.Spec.InitContainers[1]
	createValidator := requireContainer(t, pod.Spec.Containers, "create-validator")
	assert.Equal(t, "load-account", loadAccount.Name)
	assert.Equal(t, "write-validator-json", writer.Name)
	assert.Equal(t, "example/utility:v2", writer.Image)
	assert.Equal(t, []string{"/bin/sh", "-c"}, writer.Command)
	require.Len(t, writer.Args, 4)
	assert.Equal(t, `printf '%s' "$1" | base64 -d > "$2"`, writer.Args[0])
	assert.Equal(t, "write-validator-json", writer.Args[1])
	assert.Equal(t, createValidator.Args[3], writer.Args[3])
	assert.Empty(t, writer.Env)
	assert.Equal(t, []string{"appd"}, createValidator.Command)
	assert.Equal(t, corev1.PullAlways, createValidator.ImagePullPolicy)
	assert.Equal(t, testAppEnv(), loadAccount.Env)
	assert.Equal(t, testAppEnv(), createValidator.Env)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "registry-creds"}}, pod.Spec.ImagePullSecrets)
	assert.Equal(t, int64(300), *pod.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.FSGroup)
	assert.Equal(t, int64(k8s.NonRootUID), *pod.Spec.SecurityContext.FSGroup)
	for _, container := range []corev1.Container{loadAccount, writer, createValidator} {
		assert.Contains(t, container.VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: defaultHome}, container.Name)
	}

	writtenPath := filepath.Join(t.TempDir(), "validator.json")
	writerArgs := append([]string(nil), writer.Args...)
	writerArgs[3] = writtenPath
	processArgs := append(append([]string(nil), writer.Command[1:]...), writerArgs...)
	output, err := exec.Command(writer.Command[0], processArgs...).CombinedOutput()
	require.NoError(t, err, string(output))
	written, err := os.ReadFile(writtenPath)
	require.NoError(t, err)
	want, err := base64.StdEncoding.DecodeString(writer.Args[2])
	require.NoError(t, err)
	assert.Equal(t, want, written)
	assert.Contains(t, string(written), `$(NAME)`)
	assert.Contains(t, string(written), "Olá")
}

func TestCreateValidatorRejectsInvalidModernPubKeyBeforeKubernetesCall(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	app, err := NewApp(nil, scheme, nil, &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "node", Namespace: "default", UID: "node-uid",
	}}, appsv1.V0_53, nil, WithBinary("appd"), WithImage("example/app:v1"))
	require.NoError(t, err)

	_, err = app.CreateValidator(
		t.Context(),
		"not-json",
		&Account{Mnemonic: "unused"},
		&NodeInfo{Moniker: "validator"},
		&Params{ChainID: "chain-1", StakeAmount: "1000stake", GasPrices: "0.025stake"},
		"tcp://node:26657",
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "building create-validator command")
	assert.ErrorContains(t, err, "decode validator public key")
}
