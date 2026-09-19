package chainutils

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/k8s"
)

const validValidatorPubKey = `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"oWg2ISpLF405Jcm2vXV+2v4fnjodh6aafuIdeoW+rUw="}`

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

	err = app.CreateValidator(
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
