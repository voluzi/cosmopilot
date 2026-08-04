package chainutils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func testAppEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PLAIN", Value: "plain-value"},
		{
			Name: "FROM_SECRET",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "app-secret"},
				Key:                  "token",
			}},
		},
	}
}

func newTestAppWithEnv(t *testing.T, env []corev1.EnvVar) *App {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name:      "test-node",
		Namespace: "default",
		UID:       "test-uid",
	}}
	app, err := NewApp(nil, scheme, nil, owner, appsv1.V0_47, nil,
		WithBinary("appd"),
		WithImage("example/app:latest"),
		WithEnv(env),
	)
	require.NoError(t, err)
	return app
}

func requireContainer(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, container := range containers {
		if container.Name == name {
			return container
		}
	}
	require.FailNow(t, "container not found", name)
	return corev1.Container{}
}

func TestAppEnvPreservesOrderAndValueFromWithFreshCopies(t *testing.T) {
	source := testAppEnv()
	app := newTestAppWithEnv(t, source)

	source[0].Value = "mutated-source"
	source[1].ValueFrom.SecretKeyRef.Name = "mutated-secret"

	first := app.appEnv("app")
	require.Equal(t, testAppEnv(), first)
	first[0].Value = "mutated-build"
	first[1].ValueFrom.SecretKeyRef.Name = "mutated-build-secret"

	assert.Equal(t, testAppEnv(), app.appEnv("app"))
}

func TestAppEnvRetargetsContainerScopedResourceRefs(t *testing.T) {
	source := []corev1.EnvVar{{
		Name: "CPU_LIMIT",
		ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
			ContainerName: "appd",
			Resource:      "limits.cpu",
		}},
	}}
	app := newTestAppWithEnv(t, source)

	env := app.appEnv("init-chain")
	require.Equal(t, "init-chain", env[0].ValueFrom.ResourceFieldRef.ContainerName)
	assert.Equal(t, "appd", source[0].ValueFrom.ResourceFieldRef.ContainerName)
	assert.Equal(t, "appd", app.env[0].ValueFrom.ResourceFieldRef.ContainerName)
}

func TestBuildInitPodPropagatesAppEnvOnlyToTheChainApp(t *testing.T) {
	app := newTestAppWithEnv(t, testAppEnv())
	commandEnv := []corev1.EnvVar{{Name: "COMMAND_ONLY", Value: "command"}}
	command := &InitCommand{
		Image:   app.image,
		Command: []string{app.binary},
		Env:     commandEnv,
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data"}}

	first, err := app.BuildInitPod(pvc, nil, command)
	require.NoError(t, err)
	assert.Equal(t, testAppEnv(), requireContainer(t, first.Spec.InitContainers, "app").Env)
	assert.Equal(t, commandEnv, requireContainer(t, first.Spec.InitContainers, "init-command-0").Env)
	assert.Empty(t, requireContainer(t, first.Spec.Containers, "busybox").Env)

	first.Spec.InitContainers[0].Env[0].Value = "mutated"
	second, err := app.BuildInitPod(pvc, nil, command)
	require.NoError(t, err)
	assert.Equal(t, testAppEnv(), requireContainer(t, second.Spec.InitContainers, "app").Env)
}

func TestBuildConfigGeneratorPodPropagatesAppEnvOnlyToTheChainApp(t *testing.T) {
	app := newTestAppWithEnv(t, testAppEnv())

	pod := app.buildConfigGeneratorPod()

	assert.Equal(t, testAppEnv(), requireContainer(t, pod.Spec.InitContainers, "app").Env)
	assert.Empty(t, requireContainer(t, pod.Spec.Containers, "busybox").Env)
}

func TestBuildCreateValidatorPodPropagatesAppEnvToBothChainCLIContainers(t *testing.T) {
	app := newTestAppWithEnv(t, testAppEnv())

	pod := app.buildCreateValidatorPod(
		"pubkey",
		&NodeInfo{Moniker: "validator"},
		&Params{ChainID: "chain", StakeAmount: "1stake"},
		"tcp://node:26657",
	)

	assert.Equal(t, testAppEnv(), requireContainer(t, pod.Spec.InitContainers, "load-account").Env)
	assert.Equal(t, testAppEnv(), requireContainer(t, pod.Spec.Containers, "create-validator").Env)
}

func TestBuildGenesisPodPropagatesAppEnvOnlyToChainCLIContainers(t *testing.T) {
	app := newTestAppWithEnv(t, testAppEnv())
	commandEnv := []corev1.EnvVar{{Name: "COMMAND_ONLY", Value: "command"}}
	pod := app.buildGenesisPod(
		"owner-priv-key",
		&Account{Address: "owner-address"},
		&NodeInfo{Moniker: "owner"},
		&Params{
			ChainID:       "chain",
			Assets:        []string{"10stake"},
			StakeAmount:   "1stake",
			Accounts:      []AccountAssets{{Address: "extra-account", Assets: []string{"2stake"}}},
			UnbondingTime: "24h",
			VotingPeriod:  "1h",
		},
		[]*GenesisValidator{{
			PrivKeySecret: "extra-validator-key",
			Account:       &Account{Address: "extra-validator-address"},
			NodeInfo:      &NodeInfo{Moniker: "extra-validator"},
			StakeAmount:   "1stake",
			Assets:        []string{"10stake"},
		}},
		[]*InitCommand{{
			Image:   app.image,
			Command: []string{app.binary},
			Env:     commandEnv,
		}},
	)

	chainCLI := map[string]bool{
		"init-chain":              true,
		"load-account":            true,
		"add-validator-account":   true,
		"add-account-0":           true,
		"gentx":                   true,
		"load-account-1":          true,
		"add-validator-account-1": true,
		"gentx-1":                 true,
		"collect-gentxs":          true,
	}
	for _, container := range pod.Spec.InitContainers {
		if chainCLI[container.Name] {
			assert.Equal(t, testAppEnv(), container.Env, container.Name)
			continue
		}
		if container.Name == "init-command-0" {
			assert.Equal(t, commandEnv, container.Env)
			continue
		}
		assert.Empty(t, container.Env, container.Name)
	}
	assert.Empty(t, requireContainer(t, pod.Spec.Containers, "busybox").Env)
}
