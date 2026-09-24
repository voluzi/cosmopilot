package chainutils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/chainutils/sdkcmd"
	"github.com/voluzi/cosmopilot/v3/pkg/images"
	"github.com/voluzi/cosmopilot/v3/pkg/utils"
)

func newHelperImageTestApp(t *testing.T) *App {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	app, err := NewApp(nil, scheme, nil, &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "node", Namespace: "default", UID: "node-uid",
	}}, appsv1.V0_50, []sdkcmd.Option{sdkcmd.WithGenesisSubcommand(true)},
		WithBinary("appd"),
		WithImage("registry.example.com/chain/app:v1"),
	)
	require.NoError(t, err)
	return app
}

func assertPodImagesVersioned(t *testing.T, pod *corev1.Pod) {
	t.Helper()

	containers := append([]corev1.Container{}, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)
	for _, container := range containers {
		_, reference := utils.SplitImageRef(container.Image)
		assert.NotEmptyf(t, reference, "%s image %q has no tag or digest", container.Name, container.Image)
		assert.NotEqualf(t, "latest", reference, "%s image must not use latest", container.Name)
		assert.NotEqual(t, "busybox", container.Image, container.Name)
		assert.NotEqual(t, "apteno/alpine-jq", container.Image, container.Name)
	}
}

func TestGeneratedHelperPodsUseVersionedImages(t *testing.T) {
	app := newHelperImageTestApp(t)
	pods, utilityContainers := buildHelperTestPods(t, app)

	for name, pod := range pods {
		t.Run(name, func(t *testing.T) {
			assertPodImagesVersioned(t, pod)
			containers := append([]corev1.Container{}, pod.Spec.InitContainers...)
			containers = append(containers, pod.Spec.Containers...)
			for _, containerName := range utilityContainers[name] {
				container := requireContainer(t, containers, containerName)
				assert.Equal(t, images.DefaultUtilityImage, container.Image)
				assert.Empty(t, container.ImagePullPolicy, "a pinned utility image keeps the unset pull policy")
			}
		})
	}
}

// Every utility container is pulled Always when the utility image moves, and only those: app-image
// containers keep the app's pull policy.
func TestGeneratedHelperPodsPullAMovingUtilityImageAlways(t *testing.T) {
	app := newHelperImageTestApp(t)
	WithUtilityImage("registry.example.com/tools:edge")(app)
	pods, utilityContainers := buildHelperTestPods(t, app)

	for name, pod := range pods {
		t.Run(name, func(t *testing.T) {
			utility := map[string]bool{}
			for _, containerName := range utilityContainers[name] {
				utility[containerName] = true
			}
			containers := append([]corev1.Container{}, pod.Spec.InitContainers...)
			containers = append(containers, pod.Spec.Containers...)
			for _, container := range containers {
				if utility[container.Name] {
					assert.Equal(t, corev1.PullAlways, container.ImagePullPolicy, container.Name)
				} else {
					assert.NotEqual(t, "registry.example.com/tools:edge", container.Image, container.Name)
					assert.NotEqual(t, corev1.PullAlways, container.ImagePullPolicy, container.Name)
				}
			}
		})
	}
}

func buildHelperTestPods(t *testing.T, app *App) (map[string]*corev1.Pod, map[string][]string) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "node-data"}}
	params := &Params{
		ChainID:                 "chain",
		Assets:                  []string{"10stake"},
		StakeAmount:             "1stake",
		UnbondingTime:           "24h",
		VotingPeriod:            "1h",
		ExpeditedVotingPeriod:   "30m",
		CommissionMaxChangeRate: "0.01",
		CommissionMaxRate:       "0.2",
		CommissionRate:          "0.1",
		GasPrices:               "0.01stake",
	}
	extraValidators := []*GenesisValidator{{
		PrivKeySecret: "validator-key",
		Account:       &Account{Address: "validator-address"},
		NodeInfo:      &NodeInfo{Moniker: "validator"},
		StakeAmount:   "1stake",
		Assets:        []string{"10stake"},
	}}

	initPod, err := app.BuildInitPod(pvc, nil, &InitCommand{
		Image: "registry.example.com/custom/init:v1",
	})
	require.NoError(t, err)
	validatorPod, err := app.buildCreateValidatorPod(
		validValidatorPubKey, "", &NodeInfo{Moniker: "validator"}, params, "tcp://node:26657",
	)
	require.NoError(t, err)

	pods := map[string]*corev1.Pod{
		"config": app.buildConfigGeneratorPod(),
		"data":   initPod,
		"genesis": app.buildGenesisPod(
			"owner-key",
			&Account{Address: "owner-address"},
			&NodeInfo{Moniker: "owner"},
			params,
			extraValidators,
			[]*InitCommand{{Image: "registry.example.com/custom/genesis-init:v1"}},
		),
		"validator": validatorPod,
	}
	utilityContainers := map[string][]string{
		"config":    {"busybox"},
		"data":      {"busybox"},
		"genesis":   {"load-priv-key", "busybox", "set-unbonding-time", "set-voting-period", "set-expedited-voting-period", "load-priv-key-1"},
		"validator": {"write-validator-json"},
	}
	return pods, utilityContainers
}

func TestUtilityImageOptionSupportsDefaultTagAndDigestOverrides(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{name: "default", want: images.DefaultUtilityImage},
		{name: "empty uses default", image: "", want: images.DefaultUtilityImage},
		{name: "registry port and tag", image: "registry.example.com:5000/tools:custom", want: "registry.example.com:5000/tools:custom"},
		{name: "digest", image: "registry.example.com/tools@sha256:abcdef", want: "registry.example.com/tools@sha256:abcdef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newHelperImageTestApp(t)
			WithUtilityImage(tt.image)(app)
			assert.Equal(t, tt.want, requireContainer(t, app.buildConfigGeneratorPod().Spec.Containers, "busybox").Image)
		})
	}
}

func TestAppPodsCopyImagePullSecrets(t *testing.T) {
	pullSecrets := []corev1.LocalObjectReference{{Name: "registry-creds"}}
	app := newHelperImageTestApp(t)
	WithImagePullSecrets(pullSecrets)(app)
	pullSecrets[0].Name = "mutated-source"

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "node-data"}}
	params := &Params{ChainID: "chain", Assets: []string{"10stake"}, StakeAmount: "1stake", GasPrices: "0.01stake"}
	initPod, err := app.BuildInitPod(pvc, nil)
	require.NoError(t, err)
	validatorPod, err := app.buildCreateValidatorPod(validValidatorPubKey, "", &NodeInfo{Moniker: "validator"}, params, "tcp://node:26657")
	require.NoError(t, err)
	pods := map[string]*corev1.Pod{
		"config":    app.buildConfigGeneratorPod(),
		"data":      initPod,
		"genesis":   app.buildGenesisPod("owner-key", &Account{Address: "owner"}, &NodeInfo{Moniker: "owner"}, params, nil, nil),
		"validator": validatorPod,
	}

	want := []corev1.LocalObjectReference{{Name: "registry-creds"}}
	for name, pod := range pods {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, pod.Spec.ImagePullSecrets)
		})
	}

	require.NotEmpty(t, pods["config"].Spec.ImagePullSecrets)
	pods["config"].Spec.ImagePullSecrets[0].Name = "mutated-pod"
	assert.Equal(t, want, app.buildConfigGeneratorPod().Spec.ImagePullSecrets)
}
