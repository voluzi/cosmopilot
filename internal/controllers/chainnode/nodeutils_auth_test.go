package chainnode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0", Namespace: "test", UID: "node-uid", ResourceVersion: "1", Labels: map[string]string{"team": "chain"}},
		Spec:       appsv1.ChainNodeSpec{App: appsv1.AppSpec{App: "chaind"}, Config: &appsv1.Config{}},
	}
}

func bindNodeUtilsCredential(owner *appsv1.ChainNode, name string, uid types.UID) {
	if owner.Annotations == nil {
		owner.Annotations = map[string]string{}
	}
	owner.Annotations[controllers.AnnotationNodeUtilsShutdownSecretName] = name
	owner.Annotations[controllers.AnnotationNodeUtilsShutdownSecretUID] = string(uid)
}

func ownedNodeUtilsSecret(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode, token string, uid types.UID) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: nodeUtilsShutdownSecretNameForToken(token), Namespace: owner.Namespace, UID: uid, Labels: WithChainNodeLabels(owner)},
		Immutable:  ptr.To(true), Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{nodeutils.ShutdownTokenSecretKey: []byte(token)},
	}
	require.NoError(t, controllerutil.SetControllerReference(owner, secret, scheme))
	return secret
}

type secretUIDClient struct {
	client.Client
	nextUID, secretCreates int
	chainNodePatchErr      error
}

func (c *secretUIDClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if secret, ok := obj.(*corev1.Secret); ok {
		c.nextUID++
		c.secretCreates++
		secret.UID = types.UID(fmt.Sprintf("secret-uid-%d", c.nextUID))
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *secretUIDClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*appsv1.ChainNode); ok && c.chainNodePatchErr != nil {
		return c.chainNodePatchErr
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type staleSecretCacheClient struct{ client.Client }

func (c *staleSecretCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type noSecretUIDClient struct{ client.Client }

func (c *noSecretUIDClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.Client.Create(ctx, obj, opts...)
}

func newNodeUtilsAuthReconciler(t *testing.T, owner *appsv1.ChainNode, objects ...client.Object) (*Reconciler, *secretUIDClient, client.Client) {
	t.Helper()
	scheme := nodeUtilsAuthTestScheme(t)
	all := append([]client.Object{owner}, objects...)
	backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).Build()
	writer := &secretUIDClient{Client: backing}
	return &Reconciler{Client: writer, APIReader: backing, Scheme: scheme}, writer, backing
}

func TestNodeUtilsShutdownSecretNameUsesTokenDigest(t *testing.T) {
	name := nodeUtilsShutdownSecretNameForToken(testShutdownToken)
	assert.Equal(t, "node-utils.0f007385b6f9d4b7eeb2748605afe1a9.84a0a3bfa3f014d09e2a784ce9e5cd1a", name)
	assert.Len(t, name, 76)
	assert.Empty(t, validation.IsDNS1123Subdomain(name))
	assert.NotEmpty(t, validation.IsDNS1123Label(name))
	assert.NotEqual(t, name, nodeUtilsShutdownSecretNameForToken("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"))
}

func TestEnsureNodeUtilsShutdownCredentialCreatesBindsAndReusesIdentity(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	r, writer, backing := newNodeUtilsAuthReconciler(t, owner)
	credential, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.NoError(t, err)
	assert.True(t, nodeutils.ValidShutdownToken(credential.token))
	assert.Equal(t, nodeUtilsShutdownSecretNameForToken(credential.token), credential.name)
	assert.NotEmpty(t, credential.uid)
	assert.Equal(t, credential.name, owner.Annotations[controllers.AnnotationNodeUtilsShutdownSecretName])
	assert.Equal(t, string(credential.uid), owner.Annotations[controllers.AnnotationNodeUtilsShutdownSecretUID])
	assert.NotEqual(t, "1", owner.ResourceVersion)
	created := &corev1.Secret{}
	require.NoError(t, backing.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: credential.name}, created))
	assert.Equal(t, credential.uid, created.UID)
	assert.Equal(t, ptr.To(true), created.Immutable)
	assert.True(t, metav1.IsControlledBy(created, owner))
	persistedOwner := &appsv1.ChainNode{}
	require.NoError(t, backing.Get(context.Background(), client.ObjectKeyFromObject(owner), persistedOwner))
	assert.Equal(t, credential.name, persistedOwner.Annotations[controllers.AnnotationNodeUtilsShutdownSecretName])
	assert.Equal(t, string(credential.uid), persistedOwner.Annotations[controllers.AnnotationNodeUtilsShutdownSecretUID])
	assert.Equal(t, owner.ResourceVersion, persistedOwner.ResourceVersion)
	reused, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.NoError(t, err)
	assert.Equal(t, credential, reused)
	assert.Equal(t, 1, writer.secretCreates)
}

func TestEnsureNodeUtilsShutdownCredentialRequiresCreatedSecretUID(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	scheme := nodeUtilsAuthTestScheme(t)
	backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	r := &Reconciler{Client: &noSecretUIDClient{Client: backing}, APIReader: backing, Scheme: scheme}

	_, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "API response has no UID")
	assert.Empty(t, owner.Annotations)
}

func TestEnsureNodeUtilsShutdownCredentialNeverAdoptsCreateCollision(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	forged := ownedNodeUtilsSecret(t, nodeUtilsAuthTestScheme(t), owner, testShutdownToken, "attacker-uid")
	r, writer, _ := newNodeUtilsAuthReconciler(t, owner, forged)
	r.shutdownTokenGenerator = func() (string, error) { return testShutdownToken, nil }
	_, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Empty(t, owner.Annotations)
	assert.Equal(t, 1, writer.secretCreates)
}

func TestEnsureNodeUtilsShutdownCredentialIgnoresForgedPredictableOwnerSecret(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	digest := sha256.Sum256([]byte(owner.Namespace + "\x00" + owner.Name + "\x00" + string(owner.UID)))
	hexDigest := fmt.Sprintf("%x", digest)
	forged := ownedNodeUtilsSecret(t, nodeUtilsAuthTestScheme(t), owner, testShutdownToken, "attacker-uid")
	forged.Name = "node-utils." + hexDigest[:32] + "." + hexDigest[32:]
	r, _, backing := newNodeUtilsAuthReconciler(t, owner, forged)
	credential, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.NoError(t, err)
	assert.NotEqual(t, forged.Name, credential.name)
	preserved := &corev1.Secret{}
	require.NoError(t, backing.Get(context.Background(), client.ObjectKeyFromObject(forged), preserved))
}

func TestEnsureNodeUtilsShutdownCredentialUsesUncachedReads(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	scheme := nodeUtilsAuthTestScheme(t)
	backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()
	writer := &secretUIDClient{Client: &staleSecretCacheClient{Client: backing}}
	r := &Reconciler{Client: writer, APIReader: backing, Scheme: scheme}
	created, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.NoError(t, err)
	reused, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.NoError(t, err)
	assert.Equal(t, created, reused)
	assert.Equal(t, 1, writer.secretCreates)
}

func TestEnsureNodeUtilsShutdownCredentialRotatesOnUIDSubstitution(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	r, writer, backing := newNodeUtilsAuthReconciler(t, owner)
	first, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.NoError(t, err)
	require.NoError(t, backing.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: first.name, Namespace: owner.Namespace}}))
	substitute := ownedNodeUtilsSecret(t, r.Scheme, owner, first.token, "substitute-uid")
	require.NoError(t, backing.Create(context.Background(), substitute))
	replacement, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
	require.NoError(t, err)
	assert.NotEqual(t, first.name, replacement.name)
	assert.Equal(t, 2, writer.secretCreates)
}

func TestEnsureNodeUtilsShutdownCredentialFailsClosedForBoundInvalidSecret(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*appsv1.ChainNode, *corev1.Secret)
	}{
		{name: "partial binding", mutate: func(owner *appsv1.ChainNode, _ *corev1.Secret) {
			owner.Annotations = map[string]string{controllers.AnnotationNodeUtilsShutdownSecretName: nodeUtilsShutdownSecretNameForToken(testShutdownToken)}
		}},
		{name: "malformed name", mutate: func(owner *appsv1.ChainNode, _ *corev1.Secret) {
			bindNodeUtilsCredential(owner, "predictable-secret", "secret-uid")
		}},
		{name: "wrong type", mutate: func(_ *appsv1.ChainNode, secret *corev1.Secret) { secret.Type = corev1.SecretTypeTLS }},
		{name: "unowned", mutate: func(_ *appsv1.ChainNode, secret *corev1.Secret) { secret.OwnerReferences = nil }},
		{name: "mutable", mutate: func(_ *appsv1.ChainNode, secret *corev1.Secret) { secret.Immutable = nil }},
		{name: "terminating", mutate: func(_ *appsv1.ChainNode, secret *corev1.Secret) {
			now := metav1.Now()
			secret.DeletionTimestamp = &now
			secret.Finalizers = []string{"test"}
		}},
		{name: "modified token", mutate: func(_ *appsv1.ChainNode, secret *corev1.Secret) {
			secret.Data[nodeutils.ShutdownTokenSecretKey] = []byte("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE")
		}},
		{name: "missing token", mutate: func(_ *appsv1.ChainNode, secret *corev1.Secret) { secret.Data = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner := nodeUtilsAuthTestNode()
			secret := ownedNodeUtilsSecret(t, nodeUtilsAuthTestScheme(t), owner, testShutdownToken, "secret-uid")
			bindNodeUtilsCredential(owner, secret.Name, secret.UID)
			tt.mutate(owner, secret)
			var objects []client.Object
			if tt.name != "partial binding" && tt.name != "malformed name" {
				objects = append(objects, secret)
			}
			r, writer, _ := newNodeUtilsAuthReconciler(t, owner, objects...)
			_, err := r.ensureNodeUtilsShutdownCredential(context.Background(), owner)
			require.Error(t, err)
			assert.Zero(t, writer.secretCreates)
		})
	}
}

func TestEnsureNodeUtilsShutdownCredentialRejectsStaleCallerBeforeCreate(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*appsv1.ChainNode)
	}{
		{name: "recreated object", mutate: func(current *appsv1.ChainNode) { current.UID = "replacement-node-uid" }},
		{name: "advanced resource version", mutate: func(current *appsv1.ChainNode) { current.ResourceVersion = "2" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stale := nodeUtilsAuthTestNode()
			current := stale.DeepCopy()
			tt.mutate(current)
			r, writer, _ := newNodeUtilsAuthReconciler(t, current)
			_, err := r.ensureNodeUtilsShutdownCredential(context.Background(), stale)
			require.Error(t, err)
			assert.Zero(t, writer.secretCreates)
		})
	}
}

func TestEnsurePodDoesNotCreatePodWhenCredentialBindingPatchFails(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	r, writer, backing := newNodeUtilsAuthReconciler(t, owner)
	writer.chainNodePatchErr = apierrors.NewConflict(schema.GroupResource{Group: appsv1.GroupVersion.Group, Resource: "chainnodes"}, owner.Name, errors.New("test conflict"))
	err := r.ensurePod(context.Background(), nil, owner, "config-hash")
	require.Error(t, err)
	pods := &corev1.PodList{}
	require.NoError(t, backing.List(context.Background(), pods))
	assert.Empty(t, pods.Items)
}

func TestBuildNodeUtilsInitContainerUsesBoundCredentialEnvironment(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	owner.Spec.Config.NodeUtilsEnv = []corev1.EnvVar{{Name: "CUSTOM", Value: "kept"}, {Name: nodeutils.ShutdownTokenEnvironmentVariable, Value: "user-token"}, {Name: nodeutils.ExpectedShutdownTokenHashEnvironmentVariable, Value: "user-hash"}}
	r := &Reconciler{opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	secretName := nodeUtilsShutdownSecretNameForToken(testShutdownToken)
	container := r.buildNodeUtilsInitContainer(owner, secretName)
	assert.Contains(t, container.Env, corev1.EnvVar{Name: "CUSTOM", Value: "kept"})
	tokenEnv := requireSingleEnv(t, container.Env, nodeutils.ShutdownTokenEnvironmentVariable)
	require.NotNil(t, tokenEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, secretName, tokenEnv.ValueFrom.SecretKeyRef.Name)
	hashEnv := requireSingleEnv(t, container.Env, nodeutils.ExpectedShutdownTokenHashEnvironmentVariable)
	require.NotNil(t, hashEnv.ValueFrom.FieldRef)
	assert.Equal(t, "metadata.annotations['"+controllers.AnnotationNodeUtilsShutdownTokenHash+"']", hashEnv.ValueFrom.FieldRef.FieldPath)
}

func requireSingleEnv(t *testing.T, env []corev1.EnvVar, name string) corev1.EnvVar {
	t.Helper()
	var matches []corev1.EnvVar
	for _, item := range env {
		if item.Name == name {
			matches = append(matches, item)
		}
	}
	require.Len(t, matches, 1)
	return matches[0]
}

func TestPodCredentialStampContainsOnlyTrustedIdentityAndHash(t *testing.T) {
	pod := &corev1.Pod{}
	credential := nodeUtilsShutdownCredential{name: "node-utils.name", uid: "secret-uid", token: testShutdownToken}
	stampNodeUtilsShutdownCredential(pod, credential)
	assert.Equal(t, credential.name, pod.Annotations[controllers.AnnotationNodeUtilsShutdownSecretName])
	assert.Equal(t, string(credential.uid), pod.Annotations[controllers.AnnotationNodeUtilsShutdownSecretUID])
	assert.Equal(t, nodeutils.ShutdownTokenHash(testShutdownToken), pod.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash])
	for _, value := range pod.Annotations {
		assert.NotContains(t, value, testShutdownToken)
	}
}

type fakeNodeUtilsShutdownClient struct {
	called bool
	err    error
}

func (c *fakeNodeUtilsShutdownClient) ShutdownNodeUtilsServer(context.Context) error {
	c.called = true
	return c.err
}

func podWithShutdownCredential(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNode, credential nodeUtilsShutdownCredential) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: nodeUtilsContainerName, Env: []corev1.EnvVar{
			{Name: nodeutils.ShutdownTokenEnvironmentVariable, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: credential.name}, Key: nodeutils.ShutdownTokenSecretKey}}},
			{Name: nodeutils.ExpectedShutdownTokenHashEnvironmentVariable, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations['" + controllers.AnnotationNodeUtilsShutdownTokenHash + "']"}}},
		}}}},
	}
	stampNodeUtilsShutdownCredential(pod, credential)
	require.NoError(t, controllerutil.SetControllerReference(owner, pod, scheme))
	return pod
}

func TestStopNodeUtilsContainerUsesLivePodsStampedCredentialDuringRotation(t *testing.T) {
	owner := nodeUtilsAuthTestNode()
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "old-secret-uid", token: testShutdownToken}
	secret := ownedNodeUtilsSecret(t, nodeUtilsAuthTestScheme(t), owner, credential.token, credential.uid)
	pod := podWithShutdownCredential(t, nodeUtilsAuthTestScheme(t), owner, credential)
	bindNodeUtilsCredential(owner, "node-utils.11111111111111111111111111111111.11111111111111111111111111111111", "new-secret-uid")
	r, _, backing := newNodeUtilsAuthReconciler(t, owner, secret, pod)
	shutdown := &fakeNodeUtilsShutdownClient{}
	var gotToken string
	r.shutdownClientFactory = func(_ string, token string) nodeUtilsShutdownClient { gotToken = token; return shutdown }
	r.Client = &staleSecretCacheClient{Client: backing}
	require.NoError(t, r.stopNodeUtilsContainer(context.Background(), owner))
	assert.True(t, shutdown.called)
	assert.Equal(t, credential.token, gotToken)
}

func TestStopNodeUtilsContainerRejectsSubstitutionBeforeHTTP(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Pod, *corev1.Secret)
	}{
		{name: "secret uid mismatch", mutate: func(_ *corev1.Pod, secret *corev1.Secret) { secret.UID = "replacement-uid" }},
		{name: "missing pod binding", mutate: func(pod *corev1.Pod, _ *corev1.Secret) {
			delete(pod.Annotations, controllers.AnnotationNodeUtilsShutdownSecretName)
		}},
		{name: "token hash mismatch", mutate: func(pod *corev1.Pod, _ *corev1.Secret) {
			pod.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash] = strings.Repeat("0", 64)
		}},
		{name: "secret ref mismatch", mutate: func(pod *corev1.Pod, _ *corev1.Secret) {
			pod.Spec.InitContainers[0].Env[0].ValueFrom.SecretKeyRef.Name = "other-secret"
		}},
		{name: "modified token same uid", mutate: func(_ *corev1.Pod, secret *corev1.Secret) {
			secret.Data[nodeutils.ShutdownTokenSecretKey] = []byte("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE")
		}},
		{name: "mutable secret", mutate: func(_ *corev1.Pod, secret *corev1.Secret) { secret.Immutable = nil }},
		{name: "unowned secret", mutate: func(_ *corev1.Pod, secret *corev1.Secret) { secret.OwnerReferences = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner := nodeUtilsAuthTestNode()
			credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
			secret := ownedNodeUtilsSecret(t, nodeUtilsAuthTestScheme(t), owner, credential.token, credential.uid)
			pod := podWithShutdownCredential(t, nodeUtilsAuthTestScheme(t), owner, credential)
			tt.mutate(pod, secret)
			r, _, _ := newNodeUtilsAuthReconciler(t, owner, secret, pod)
			factoryCalled := false
			r.shutdownClientFactory = func(string, string) nodeUtilsShutdownClient {
				factoryCalled = true
				return &fakeNodeUtilsShutdownClient{}
			}
			err := r.stopNodeUtilsContainer(context.Background(), owner)
			require.Error(t, err)
			assert.False(t, factoryCalled)
		})
	}
}

func TestTokenDriftHonorsDisruptionAllowanceForHealthyCurrentPod(t *testing.T) {
	ctx := context.Background()
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	owner.Spec.Validator = &appsv1.ValidatorConfig{}
	owner.Spec.Config.HaltHeight = ptr.To[int64](1)
	owner.Status.ChainID, owner.Status.LatestHeight, owner.Status.Phase = "chain-a", 1, appsv1.PhaseChainNodeRunning
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(owner, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, owner, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	desired, err := specReconciler.getPodSpec(ctx, owner, "config-hash", credential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(desired, credential)
	current := desired.DeepCopy()
	current.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash] = "old-token-hash"
	current.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: owner.Spec.App.App, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}, InitContainerStatuses: []corev1.ContainerStatus{{Name: nodeUtilsContainerName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}
	unavailablePeer := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-1", Namespace: owner.Namespace, Labels: current.Labels}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(owner).WithObjects(owner, current, unavailablePeer, secret, config).Build()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := "false"
		if req.URL.Path == "/latest_height" {
			body = "1"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	defer func() { http.DefaultTransport = originalTransport }()
	deletes := 0
	httpClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deletes++
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))}, nil
	})}
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"}, httpClient)
	require.NoError(t, err)
	r := &Reconciler{Client: base, APIReader: base, ClientSet: clientSet, Scheme: scheme, recorder: record.NewFakeRecorder(10), disruptionLocks: newLockManager(), opts: &controllers.ControllerRunOptions{DisruptionCheckEnabled: true, DisruptionMaxUnavailable: 1}, shutdownClientFactory: func(string, string) nodeUtilsShutdownClient { return &fakeNodeUtilsShutdownClient{} }}
	require.NoError(t, r.ensurePod(ctx, nil, owner, "config-hash"))
	assert.Zero(t, deletes)
	require.NoError(t, base.Delete(ctx, unavailablePeer))
	require.NoError(t, r.ensurePod(ctx, nil, owner, "config-hash"))
	assert.Equal(t, 1, deletes)
}

func TestTokenDriftRecreatesWaitingPodBeforeNodeUtilsProbe(t *testing.T) {
	ctx := context.Background()
	scheme := nodeUtilsAuthTestScheme(t)
	owner := nodeUtilsAuthTestNode()
	oldCredential := nodeUtilsShutdownCredential{
		name:  nodeUtilsShutdownSecretNameForToken(testShutdownToken),
		uid:   "old-secret-uid",
		token: testShutdownToken,
	}
	bindNodeUtilsCredential(owner, oldCredential.name, oldCredential.uid)
	owner.Annotations[controllers.AnnotationPvcSnapshotInProgress] = "true"
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	current, err := specReconciler.getPodSpec(ctx, owner, "config-hash", oldCredential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(current, oldCredential)
	current.Status = corev1.PodStatus{
		Phase: corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: nodeUtilsContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError",
			}},
		}},
	}

	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(owner).WithObjects(owner, current, config).Build()
	writer := &secretUIDClient{Client: backing}
	newToken := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	newSecretName := nodeUtilsShutdownSecretNameForToken(newToken)

	var (
		createdMu  sync.Mutex
		createdPod *corev1.Pod
		deletes    atomic.Int32
	)
	jsonResponse := func(status int, value any) (*http.Response, error) {
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}, nil
	}
	kubeHTTPClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodDelete:
			deletes.Add(1)
			return jsonResponse(http.StatusOK, &metav1.Status{Status: metav1.StatusSuccess})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pods/"+owner.Name):
			return jsonResponse(http.StatusNotFound, &metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
		case req.Method == http.MethodPost:
			created := &corev1.Pod{}
			if err := json.NewDecoder(req.Body).Decode(created); err != nil {
				return nil, err
			}
			created.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
			created.ResourceVersion = "1"
			createdMu.Lock()
			createdPod = created.DeepCopy()
			createdMu.Unlock()
			return jsonResponse(http.StatusCreated, created)
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pods"):
			createdMu.Lock()
			created := createdPod.DeepCopy()
			createdMu.Unlock()
			started := true
			created.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: owner.Spec.App.App, Started: &started}}}
			if req.URL.Query().Get("watch") != "" {
				added, err := json.Marshal(map[string]any{"type": "ADDED", "object": created})
				if err != nil {
					return nil, err
				}
				bookmark, err := json.Marshal(map[string]any{"type": "BOOKMARK", "object": &corev1.Pod{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
					ObjectMeta: metav1.ObjectMeta{
						ResourceVersion: "1",
						Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
					},
				}})
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(added) + "\n" + string(bookmark) + "\n"))}, nil
			}
			return jsonResponse(http.StatusOK, &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.Pod{*created}})
		default:
			return nil, fmt.Errorf("unexpected Kubernetes request: %s %s", req.Method, req.URL.String())
		}
	})}
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{
		Host: "https://kubernetes.invalid",
		ContentConfig: rest.ContentConfig{
			AcceptContentTypes: runtime.ContentTypeJSON,
			ContentType:        runtime.ContentTypeJSON,
		},
	}, kubeHTTPClient)
	require.NoError(t, err)

	var nodeUtilsRequests atomic.Int32
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		nodeUtilsRequests.Add(1)
		return nil, fmt.Errorf("unexpected node-utils request: %s", req.URL.String())
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	r := &Reconciler{
		Client:                 writer,
		APIReader:              backing,
		ClientSet:              clientSet,
		Scheme:                 scheme,
		recorder:               record.NewFakeRecorder(10),
		disruptionLocks:        newLockManager(),
		opts:                   &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test", DisruptionCheckEnabled: true, DisruptionMaxUnavailable: 0},
		shutdownTokenGenerator: func() (string, error) { return newToken, nil },
	}

	require.NoError(t, r.ensurePod(ctx, nil, owner, "config-hash"))
	assert.Equal(t, int32(1), deletes.Load())
	assert.Zero(t, nodeUtilsRequests.Load())
	createdMu.Lock()
	replacement := createdPod.DeepCopy()
	createdMu.Unlock()
	require.NotNil(t, replacement)
	assert.Equal(t, newSecretName, replacement.Annotations[controllers.AnnotationNodeUtilsShutdownSecretName])
	assert.Equal(t, "secret-uid-1", replacement.Annotations[controllers.AnnotationNodeUtilsShutdownSecretUID])
	assert.Equal(t, nodeutils.ShutdownTokenHash(newToken), replacement.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash])
	var nodeUtilsContainer *corev1.Container
	for i := range replacement.Spec.InitContainers {
		if replacement.Spec.InitContainers[i].Name == nodeUtilsContainerName {
			nodeUtilsContainer = &replacement.Spec.InitContainers[i]
			break
		}
	}
	require.NotNil(t, nodeUtilsContainer)
	tokenEnv := requireSingleEnv(t, nodeUtilsContainer.Env, nodeutils.ShutdownTokenEnvironmentVariable)
	require.NotNil(t, tokenEnv.ValueFrom)
	require.NotNil(t, tokenEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, newSecretName, tokenEnv.ValueFrom.SecretKeyRef.Name)
}
