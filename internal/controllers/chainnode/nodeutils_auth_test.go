package chainnode

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

const testShutdownToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func nodeUtilsAuthTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func nodeUtilsAuthTestNode() *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validator-0",
			Namespace: "test",
			UID:       "node-uid",
			Labels:    map[string]string{"team": "chain"},
		},
		Spec: appsv1.ChainNodeSpec{
			App:    appsv1.AppSpec{App: "chaind"},
			Config: &appsv1.Config{},
		},
	}
}

func ownedNodeUtilsSecret(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode, token string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: nodeUtilsShutdownSecretName(owner), Namespace: owner.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{nodeutils.ShutdownTokenSecretKey: []byte(token)},
	}
	require.NoError(t, controllerutil.SetControllerReference(owner, secret, scheme))
	return secret
}

func TestNodeUtilsShutdownSecretNameIsDeterministic(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	owner.Name = "foo"
	owner.UID = "foo-uid"

	const want = "node-utils.8399536f466ee5cb981fde0facdef088.b40492549584989451ca2c574f8cd118"
	assert.Equal(t, want, nodeUtilsShutdownSecretName(owner))
	assert.Equal(t, want, nodeUtilsShutdownSecretName(owner.DeepCopy()))
}

func TestNodeUtilsShutdownSecretNameChangesWithUID(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	recreated := owner.DeepCopy()
	recreated.UID = "replacement-uid"

	assert.NotEqual(t, nodeUtilsShutdownSecretName(owner), nodeUtilsShutdownSecretName(recreated))
}

func TestNodeUtilsShutdownSecretNameIsValidAndBounded(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	owner.Name = strings.Repeat("a", 63)
	owner.Namespace = strings.Repeat("b", 63)
	owner.UID = types.UID(strings.Repeat("c", 128))

	name := nodeUtilsShutdownSecretName(owner)
	assert.Len(t, name, 76)
	assert.Empty(t, validation.IsDNS1123Subdomain(name))
	assert.NotEmpty(t, validation.IsDNS1123Label(name))
}

func TestEnsureNodeUtilsShutdownSecretDoesNotCollideWithLegalChainNodeName(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	owner.Name = "foo"
	owner.UID = "foo-uid"
	collidingOwner := nodeUtilsAuthTestNode()
	collidingOwner.Name = "foo-node-utils"
	collidingOwner.UID = "foo-node-utils-uid"
	existingNodeKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: collidingOwner.Name, Namespace: owner.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{nodeKeyFilename: []byte("existing-node-key")},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(collidingOwner, existingNodeKey).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	token, err := r.ensureNodeUtilsShutdownSecret(context.Background(), owner)
	require.NoError(t, err)
	assert.True(t, nodeutils.ValidShutdownToken(token))

	authSecret := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: nodeUtilsShutdownSecretName(owner)}, authSecret))
	assert.True(t, metav1.IsControlledBy(authSecret, owner))
	assert.NotEqual(t, collidingOwner.Name, authSecret.Name)
	preservedNodeKey := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(existingNodeKey), preservedNodeKey))
	assert.Equal(t, existingNodeKey.Data, preservedNodeKey.Data)
}

type staleSecretCacheClient struct {
	client.Client
}

func (c *staleSecretCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestEnsureNodeUtilsShutdownSecretReturnsCreatedTokenWhenCacheIsStale(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	backing := fake.NewClientBuilder().WithScheme(scheme).Build()
	staleCache := &staleSecretCacheClient{Client: backing}
	r := &Reconciler{Client: staleCache, APIReader: backing, Scheme: scheme}

	createdToken, err := r.ensureNodeUtilsShutdownSecret(context.Background(), owner)
	require.NoError(t, err)
	assert.True(t, nodeutils.ValidShutdownToken(createdToken))

	cached := &corev1.Secret{}
	err = staleCache.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: nodeUtilsShutdownSecretName(owner)}, cached)
	require.True(t, apierrors.IsNotFound(err), "cached Get returned %v", err)
	preservedToken, err := r.ensureNodeUtilsShutdownSecret(context.Background(), owner)
	require.NoError(t, err)
	assert.Equal(t, createdToken, preservedToken)
}

func TestEnsureNodeUtilsShutdownSecretCreatesStableOwnedToken(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}

	first, err := r.ensureNodeUtilsShutdownSecret(context.Background(), owner)
	require.NoError(t, err)
	key := client.ObjectKey{Namespace: owner.Namespace, Name: nodeUtilsShutdownSecretName(owner)}
	created := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), key, created))
	require.Equal(t, corev1.SecretTypeOpaque, created.Type)
	assert.True(t, metav1.IsControlledBy(created, owner))
	assert.Equal(t, WithChainNodeLabels(owner), created.Labels)
	assert.Equal(t, first, string(created.Data[nodeutils.ShutdownTokenSecretKey]))
	assert.True(t, nodeutils.ValidShutdownToken(first))

	preservedToken, err := r.ensureNodeUtilsShutdownSecret(context.Background(), owner)
	require.NoError(t, err)
	preserved := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), key, preserved))
	assert.Equal(t, first, preservedToken)
	assert.Equal(t, first, string(preserved.Data[nodeutils.ShutdownTokenSecretKey]))
}

func TestEnsureNodeUtilsShutdownSecretRefusesUnsafeExistingSecret(t *testing.T) {
	tests := []struct {
		name   string
		secret func(*testing.T, *runtime.Scheme, *appsv1.ChainNode) *corev1.Secret
	}{
		{
			name: "unowned",
			secret: func(_ *testing.T, _ *runtime.Scheme, owner *appsv1.ChainNode) *corev1.Secret {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nodeUtilsShutdownSecretName(owner), Namespace: owner.Namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{nodeutils.ShutdownTokenSecretKey: []byte(testShutdownToken)}}
			},
		},
		{
			name: "foreign owner",
			secret: func(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode) *corev1.Secret {
				foreign := owner.DeepCopy()
				foreign.Name = "foreign"
				foreign.UID = "foreign-uid"
				return ownedNodeUtilsSecret(t, scheme, foreign, testShutdownToken)
			},
		},
		{
			name: "wrong type",
			secret: func(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode) *corev1.Secret {
				secret := ownedNodeUtilsSecret(t, scheme, owner, testShutdownToken)
				secret.Type = corev1.SecretTypeTLS
				return secret
			},
		},
		{
			name: "missing token",
			secret: func(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode) *corev1.Secret {
				secret := ownedNodeUtilsSecret(t, scheme, owner, testShutdownToken)
				secret.Data = nil
				return secret
			},
		},
		{
			name: "malformed token",
			secret: func(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode) *corev1.Secret {
				return ownedNodeUtilsSecret(t, scheme, owner, "not-a-token")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := nodeUtilsAuthTestScheme(t)
			owner := nodeUtilsAuthTestNode()
			secret := tt.secret(t, scheme, owner)
			secret.Name = nodeUtilsShutdownSecretName(owner)
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
			r := &Reconciler{Client: base, Scheme: scheme}

			_, err := r.ensureNodeUtilsShutdownSecret(context.Background(), owner)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "node-utils shutdown Secret")
		})
	}
}

func TestBuildNodeUtilsInitContainerUsesReservedSecretEnvironment(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	owner.Spec.Config.NodeUtilsEnv = []corev1.EnvVar{
		{Name: "CUSTOM", Value: "kept"},
		{Name: nodeutils.ShutdownTokenEnvironmentVariable, Value: "user-override"},
	}
	r := &Reconciler{opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}

	container := r.buildNodeUtilsInitContainer(owner)

	var tokenEnv *corev1.EnvVar
	tokenEnvCount := 0
	for i := range container.Env {
		env := &container.Env[i]
		if env.Name == nodeutils.ShutdownTokenEnvironmentVariable {
			tokenEnv = env
			tokenEnvCount++
		}
	}
	require.Equal(t, 1, tokenEnvCount)
	require.NotNil(t, tokenEnv)
	assert.Empty(t, tokenEnv.Value)
	require.NotNil(t, tokenEnv.ValueFrom)
	require.NotNil(t, tokenEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, nodeUtilsShutdownSecretName(owner), tokenEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, nodeutils.ShutdownTokenSecretKey, tokenEnv.ValueFrom.SecretKeyRef.Key)
	assert.NotContains(t, container.Args, testShutdownToken)
	assert.Contains(t, container.Env, corev1.EnvVar{Name: "CUSTOM", Value: "kept"})

	discovery := r.buildCosmosignerDiscoveryInitContainer(owner, "signer")
	for _, env := range discovery.Env {
		assert.NotEqual(t, nodeutils.ShutdownTokenEnvironmentVariable, env.Name)
	}
}

func TestGetPodSpecTracksShutdownTokenWithoutExposingIt(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	secret := ownedNodeUtilsSecret(t, scheme, owner, testShutdownToken)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, config).Build()
	r := &Reconciler{
		Client: base,
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
	}

	first, err := r.getPodSpec(context.Background(), owner, "config-hash")
	require.NoError(t, err)
	setNodeUtilsShutdownTokenHash(first, testShutdownToken)
	firstHash := first.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash]
	assert.Len(t, firstHash, 64)
	assert.NotEqual(t, testShutdownToken, firstHash)
	assert.NotContains(t, firstHash, testShutdownToken)

	replacement, err := nodeutils.GenerateShutdownToken()
	require.NoError(t, err)
	secret.Data[nodeutils.ShutdownTokenSecretKey] = []byte(replacement)
	require.NoError(t, base.Update(context.Background(), secret))
	second, err := r.getPodSpec(context.Background(), owner, "config-hash")
	require.NoError(t, err)
	setNodeUtilsShutdownTokenHash(second, replacement)
	assert.NotEqual(t, firstHash, second.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash])
	assert.True(t, nodeUtilsShutdownTokenChanged(first, second))
	assert.False(t, nodeUtilsShutdownTokenChanged(second, second.DeepCopy()))
}

type fakeNodeUtilsShutdownClient struct {
	called bool
	err    error
}

func (c *fakeNodeUtilsShutdownClient) ShutdownNodeUtilsServer(context.Context) error {
	c.called = true
	return c.err
}

func TestStopNodeUtilsContainerLoadsOwnedToken(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	token := testShutdownToken
	secret := ownedNodeUtilsSecret(t, scheme, owner, token)
	shutdown := &fakeNodeUtilsShutdownClient{}
	var gotHost, gotToken string
	backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := &Reconciler{
		Client:    &staleSecretCacheClient{Client: backing},
		APIReader: backing,
		Scheme:    scheme,
		shutdownClientFactory: func(host, credential string) nodeUtilsShutdownClient {
			gotHost, gotToken = host, credential
			return shutdown
		},
	}

	require.NoError(t, r.stopNodeUtilsContainer(context.Background(), owner))
	assert.True(t, shutdown.called)
	assert.Equal(t, owner.GetNodeFQDN(), gotHost)
	assert.Equal(t, token, gotToken)
}

func TestStopNodeUtilsContainerNeverFallsBackWithoutOwnedToken(t *testing.T) {
	tests := []struct {
		name   string
		secret func(*testing.T, *runtime.Scheme, *appsv1.ChainNode) client.Object
	}{
		{name: "missing secret"},
		{
			name: "unowned secret",
			secret: func(_ *testing.T, _ *runtime.Scheme, owner *appsv1.ChainNode) client.Object {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: nodeUtilsShutdownSecretName(owner), Namespace: owner.Namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{nodeutils.ShutdownTokenSecretKey: []byte(testShutdownToken)}}
			},
		},
		{
			name: "malformed token",
			secret: func(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode) client.Object {
				return ownedNodeUtilsSecret(t, scheme, owner, "bad")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := nodeUtilsAuthTestScheme(t)
			owner := nodeUtilsAuthTestNode()
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.secret != nil {
				builder = builder.WithObjects(tt.secret(t, scheme, owner))
			}
			factoryCalled := false
			r := &Reconciler{
				Client: builder.Build(),
				Scheme: scheme,
				shutdownClientFactory: func(string, string) nodeUtilsShutdownClient {
					factoryCalled = true
					return &fakeNodeUtilsShutdownClient{}
				},
			}

			err := r.stopNodeUtilsContainer(context.Background(), owner)

			require.Error(t, err)
			assert.False(t, factoryCalled)
		})
	}
}

func TestEnsurePodReconcilesShutdownSecretBeforeBuildingPod(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: nodeUtilsShutdownSecretName(owner), Namespace: owner.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{nodeutils.ShutdownTokenSecretKey: []byte(testShutdownToken)},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build(), Scheme: scheme}

	err := r.ensurePod(context.Background(), nil, owner, "config-hash")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "node-utils shutdown Secret")
}

func TestTokenDriftHonorsDisruptionAllowanceForHealthyCurrentPod(t *testing.T) {
	ctx := context.Background()
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	owner.Spec.Validator = &appsv1.ValidatorConfig{}
	owner.Spec.Config.HaltHeight = ptr.To[int64](1)
	owner.Status.ChainID = "chain-a"
	owner.Status.LatestHeight = 1
	owner.Status.Phase = appsv1.PhaseChainNodeRunning
	secret := ownedNodeUtilsSecret(t, scheme, owner, testShutdownToken)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, config).Build()
	specReconciler := &Reconciler{
		Client: specClient,
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
	}
	desired, err := specReconciler.getPodSpec(ctx, owner, "config-hash")
	require.NoError(t, err)
	setNodeUtilsShutdownTokenHash(desired, testShutdownToken)
	current := desired.DeepCopy()
	current.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash] = "old-token-hash"
	current.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: owner.Spec.App.App, Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: nodeUtilsContainerName, Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	unavailablePeer := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-1", Namespace: owner.Namespace, Labels: current.Labels},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(owner).
		WithObjects(owner, current, unavailablePeer, secret, config).
		Build()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := "false"
		if req.URL.Path == "/latest_height" {
			body = "1"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	defer func() { http.DefaultTransport = originalTransport }()
	deletes := 0
	httpClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deletes++
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Success"}`)),
		}, nil
	})}
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"}, httpClient)
	require.NoError(t, err)
	r := &Reconciler{
		Client:          base,
		ClientSet:       clientSet,
		Scheme:          scheme,
		recorder:        record.NewFakeRecorder(10),
		disruptionLocks: newLockManager(),
		opts: &controllers.ControllerRunOptions{
			DisruptionCheckEnabled:   true,
			DisruptionMaxUnavailable: 1,
		},
		shutdownClientFactory: func(string, string) nodeUtilsShutdownClient {
			return &fakeNodeUtilsShutdownClient{}
		},
	}

	require.NoError(t, r.ensurePod(ctx, nil, owner, "config-hash"))
	assert.Zero(t, deletes, "healthy current Pod must remain while a peer exhausts disruption allowance")

	require.NoError(t, base.Delete(ctx, unavailablePeer))
	require.NoError(t, r.ensurePod(ctx, nil, owner, "config-hash"))
	assert.Equal(t, 1, deletes, "replacement must proceed once disruption allowance is available")

	freshOwner := &appsv1.ChainNode{}
	require.NoError(t, base.Get(ctx, types.NamespacedName{Namespace: owner.Namespace, Name: owner.Name}, freshOwner))
	assert.Equal(t, appsv1.PhaseChainNodeStopped, freshOwner.Status.Phase)
}
