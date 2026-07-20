package chainnodeset

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

type chainNodeSetRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f chainNodeSetRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReconcileRejectsRecoveredSignerLockMismatchWithWebhooksEnabled(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.App = appsv1.AppSpec{Image: "image", App: "appd", Version: ptr.To("1.0.0")}
	signer := resolveSingleSigner(t, nodeSet)
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	r := newValidatorTestReconciler(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nodeSet.Namespace}},
		nodeSet,
		source,
	)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	params, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	params.Replicas = 3
	params.ExpectedPublicKey = parsed.PubKey.Value
	liveConfig, err := params.ConfigYAML()
	require.NoError(t, err)
	configMap, err := params.ConfigMap(liveConfig)
	require.NoError(t, err)
	sts, err := params.StatefulSet(liveConfig)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, configMap, r.Scheme))
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), configMap))
	require.NoError(t, r.Create(context.Background(), sts))

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: nodeSet.Namespace,
		Name:      nodeSet.Name,
	}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "replicas are immutable")

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.Equal(t, int32(3), ptr.Deref(fresh.Spec.Replicas, 0))
}

func TestReconcileKeepsLocalValidatorWhenVaultImportFails(t *testing.T) {
	const namespace = "default"
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Status.GenesisInitGenerated = ptr.To(false)
	signer := resolveSingleSigner(t, nodeSet)
	signerStatus := nodeSet.EnsureCosmosignerStatus(signer.Name)
	signerStatus.Replicas = ptr.To(signer.Spec.GetReplicas())
	signerStatus.StateStorageSize = signer.Spec.GetStateStorageSize()
	signerStatus.StateStorageClassName = signer.Spec.StorageClassName
	previousVault := *nodeSet.Spec.Cosmosigner.Backend.Vault
	previousVault.KeyName = "previous-val-key"
	signerStatus.KeyImported = previousVault.ImportFingerprint(signer.SoftwareKeySecret, []byte("previous-validator-key"))

	localNodeSet := nodeSet.DeepCopy()
	localNodeSet.Spec.Cosmosigner = nil
	specBuilder := newValidatorTestReconciler(t)
	localValidator, err := specBuilder.getValidatorSpec(localNodeSet, "validators", 0, localNodeSet.Spec.Nodes[0].Validator)
	require.NoError(t, err)
	require.False(t, localValidator.Spec.RemoteSignerTarget)

	genesis := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      nodeSet.Spec.Genesis.GetConfigMapName(nodeSet.Status.ChainID),
		Namespace: namespace,
	}}
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	validatorKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	r := newValidatorTestReconciler(t, ns, nodeSet, genesis, token, validatorKey, localValidator)

	var importCreates int
	transport := chainNodeSetRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		statusCode := http.StatusNotFound
		body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		if req.Method == http.MethodPost {
			importCreates++
			statusCode = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","message":"forced import failure","code":500}`
		}
		return &http.Response{
			StatusCode: statusCode,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	restConfig := &rest.Config{Host: "https://kubernetes.invalid", Transport: transport}
	clientSet, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	r.ClientSet = clientSet
	r.RestConfig = restConfig

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: namespace,
		Name:      nodeSet.Name,
	}})
	require.Error(t, err)
	require.Equal(t, 1, importCreates, "the reconcile must reach the forced Vault import failure")

	fresh := &appsv1.ChainNode{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(localValidator), fresh))
	assert.False(t, fresh.Spec.RemoteSignerTarget, "a failed import must leave the validator on its local signing path")
	assert.NotContains(t, fresh.Labels, controllers.LabelCosmosignerTarget)
}

func TestReconcileImportsReplacementBeforeSignerTeardown(t *testing.T) {
	const namespace = "default"
	backend := appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}}
	nodeSet := cosmosignerValidatorNodeSet(backend)
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.App = appsv1.AppSpec{Image: "image", App: "appd", Version: ptr.To("1.0.0")}
	recordSignerRollout(t, nodeSet)
	oldSignerName := nodeSet.Status.Cosmosigners[0].Name

	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Cosmosigner = &appsv1.Cosmosigner{Backend: backend}
	replacement := resolveSingleSigner(t, nodeSet)
	replacementStatus := nodeSet.EnsureCosmosignerStatus(replacement.Name)
	replacementStatus.Replicas = ptr.To(replacement.Spec.GetReplicas())
	replacementStatus.StateStorageSize = replacement.Spec.GetStateStorageSize()
	replacementStatus.StateStorageClassName = replacement.Spec.StorageClassName
	replacementStatus.ServingGroup = replacement.ValidatorGroup

	oldSigner := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: oldSignerName, Namespace: namespace}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, oldSigner, testScheme(t)))
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: replacement.SoftwareKeySecret, Namespace: namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	r := newValidatorTestReconciler(t, ns, nodeSet, oldSigner, token, source)

	var importCreates int
	transport := chainNodeSetRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		statusCode := http.StatusNotFound
		body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		if req.Method == http.MethodPost {
			importCreates++
			statusCode = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","message":"forced import failure","code":500}`
		}
		return &http.Response{
			StatusCode: statusCode,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	restConfig := &rest.Config{Host: "https://kubernetes.invalid", Transport: transport}
	clientSet, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	r.ClientSet = clientSet
	r.RestConfig = restConfig

	request := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: namespace,
		Name:      nodeSet.Name,
	}}
	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, result.Requeue, "the replacement history must be persisted before migration work")
	require.Zero(t, importCreates)

	_, err = r.Reconcile(context.Background(), request)
	require.Error(t, err)
	require.Equal(t, 1, importCreates, "the reconcile must reach the forced replacement import failure")

	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: oldSignerName}, remaining),
		"a failed replacement import must leave the working signer intact")
}

func TestPrepareCosmosignerImportsBootstrapsCreateValidatorLocally(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.Nodes[0].Validator.CreateValidator = &appsv1.CreateValidatorConfig{}
	nodeSet.Status.GenesisInitGenerated = ptr.To(false)
	signer := resolveSingleSigner(t, nodeSet)
	signerStatus := nodeSet.EnsureCosmosignerStatus(signer.Name)
	signerStatus.Replicas = ptr.To(signer.Spec.GetReplicas())
	signerStatus.StateStorageSize = signer.Spec.GetStateStorageSize()
	signerStatus.StateStorageClassName = signer.Spec.StorageClassName
	r := newValidatorTestReconciler(t, nodeSet)

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, ready)
	require.NoError(t, r.ensureValidatorWithBlockedSignerTargets(context.Background(), nodeSet, blocked))

	validator := &appsv1.ChainNode{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      validatorNodeName(nodeSet, "validators", 0),
	}, validator))
	assert.False(t, validator.Spec.RemoteSignerTarget, "the validator must generate and register its key locally before Vault import")
	assert.NotContains(t, validator.Labels, controllers.LabelCosmosignerTarget)
}

func TestPrepareCosmosignerImportsBootstrapsSoftwareKeyLocally(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.Nodes[0].Validator.PrivateKeySecret = nil
	nodeSet.Spec.Nodes[0].Validator.CreateValidator = &appsv1.CreateValidatorConfig{}
	signer := resolveSingleSigner(t, nodeSet)
	signerStatus := nodeSet.EnsureCosmosignerStatus(signer.Name)
	signerStatus.Replicas = ptr.To(signer.Spec.GetReplicas())
	signerStatus.StateStorageSize = signer.Spec.GetStateStorageSize()
	signerStatus.StateStorageClassName = signer.Spec.StorageClassName
	r := newValidatorTestReconciler(t, nodeSet)

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, ready)
	require.Contains(t, blocked, signer.Name)
	rolloutsReady, err := r.prepareCosmosignerRollouts(context.Background(), nodeSet, blocked)
	require.NoError(t, err)
	require.True(t, rolloutsReady)
	err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: signer.Name}, &k8sappsv1.StatefulSet{})
	require.True(t, apierrors.IsNotFound(err), "a bootstrap-blocked signer must not be applied before its key exists")
	require.NoError(t, r.ensureValidatorWithBlockedSignerTargets(context.Background(), nodeSet, blocked))

	validator := &appsv1.ChainNode{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: "default",
		Name:      validatorNodeName(nodeSet, "validators", 0),
	}, validator))
	assert.False(t, validator.Spec.RemoteSignerTarget, "the validator must generate its software key locally before signer rollout")
}

func TestPrepareCosmosignerImportsKeepsBlockedSecondaryValidatorsRemote(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.Nodes[0].Instances = ptr.To(2)
	nodeSet.Spec.Nodes[0].Validator.PrivateKeySecret = nil
	nodeSet.Spec.Nodes[0].Validator.CreateValidator = &appsv1.CreateValidatorConfig{}
	signer := resolveSingleSigner(t, nodeSet)
	r := newValidatorTestReconciler(t, nodeSet)

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, ready)
	require.Contains(t, blocked, signer.Name)

	cfg := deriveGroupValidatorConfig(nodeSet, "validators", 1, 2, nodeSet.Spec.Nodes[0].Validator)

	// While instance 0 has not handed over (secondariesDiscoverable=false), a secondary stays a remote
	// target — so it never mounts the shared consensus key — but is withheld from the signer's
	// discovery Service, so the signer cannot sign through it while instance 0 still signs locally.
	blockedSecondary, err := r.getValidatorSpecWithBlockedSignerTargets(nodeSet, "validators", 1, cfg, blocked, false)
	require.NoError(t, err)
	require.True(t, blockedSecondary.Spec.RemoteSignerTarget)
	require.NotContains(t, blockedSecondary.Labels, controllers.LabelCosmosignerTarget)

	// Once instance 0 has handed over, the secondary joins the discovery Service.
	discoverableSecondary, err := r.getValidatorSpecWithBlockedSignerTargets(nodeSet, "validators", 1, cfg, blocked, true)
	require.NoError(t, err)
	require.True(t, discoverableSecondary.Spec.RemoteSignerTarget)
	require.Equal(t, signer.Name, discoverableSecondary.Labels[controllers.LabelCosmosignerTarget])

	// Even once the signer is no longer blocked (empty blocked set, so instance 0 has flipped this
	// pass), a secondary is STILL withheld from the discovery Service until instance 0's handover is
	// confirmed complete (secondariesDiscoverable=false) — this is the residual in-place-patch window.
	unblockedSecondary, err := r.getValidatorSpecWithBlockedSignerTargets(nodeSet, "validators", 1, cfg, nil, false)
	require.NoError(t, err)
	require.True(t, unblockedSecondary.Spec.RemoteSignerTarget)
	require.NotContains(t, unblockedSecondary.Labels, controllers.LabelCosmosignerTarget)
}

// TestCosmosignerSecondariesDiscoverableGatesOnInstanceZeroHandover verifies the handover gate that
// closes the multi-instance bootstrap double-sign window: secondaries join the signer's discovery
// Service only once instance 0's live pod carries the discovery label — proof its local-key pod was
// recreated away — and stay eligible thereafter (latch) even if instance 0's pod later restarts.
func TestCosmosignerSecondariesDiscoverableGatesOnInstanceZeroHandover(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.Nodes[0].Instances = ptr.To(3)
	signerName, targeted := signerNameForNode(nodeSet, "validators")
	require.True(t, targeted)
	instanceZero := validatorNodeName(nodeSet, "validators", 0)

	// Instance 0 has no pod yet (still bootstrapping): secondaries are not discoverable.
	r := newValidatorTestReconciler(t, nodeSet)
	discoverable, err := r.cosmosignerSecondariesDiscoverable(context.Background(), nodeSet, "validators", signerName, 3)
	require.NoError(t, err)
	require.False(t, discoverable)

	// Instance 0's pod exists but carries no discovery label (its local key is still live): still not
	// discoverable — this is the window the gate closes.
	localPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: instanceZero, Namespace: nodeSet.Namespace}}
	r = newValidatorTestReconciler(t, nodeSet, localPod)
	discoverable, err = r.cosmosignerSecondariesDiscoverable(context.Background(), nodeSet, "validators", signerName, 3)
	require.NoError(t, err)
	require.False(t, discoverable)

	// Instance 0's live pod carries the discovery label (its local-key pod was recreated away):
	// handover complete, secondaries may join.
	remotePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: instanceZero, Namespace: nodeSet.Namespace,
		Labels: map[string]string{controllers.LabelCosmosignerTarget: signerName}}}
	r = newValidatorTestReconciler(t, nodeSet, remotePod)
	discoverable, err = r.cosmosignerSecondariesDiscoverable(context.Background(), nodeSet, "validators", signerName, 3)
	require.NoError(t, err)
	require.True(t, discoverable)

	// Latch: instance 0's pod is momentarily absent (restarting post-handover) but a secondary already
	// joined the discovery Service — the group stays discoverable so the signer keeps those endpoints.
	labeledSecondary := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: validatorNodeName(nodeSet, "validators", 1), Namespace: nodeSet.Namespace,
		Labels: map[string]string{controllers.LabelCosmosignerTarget: signerName}}}
	r = newValidatorTestReconciler(t, nodeSet, labeledSecondary)
	discoverable, err = r.cosmosignerSecondariesDiscoverable(context.Background(), nodeSet, "validators", signerName, 3)
	require.NoError(t, err)
	require.True(t, discoverable)
}

func TestPrepareCosmosignerImportsKeepsExistingSoftwareKeyLocalUntilPubKeyRecorded(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.Nodes[0].Validator.PrivateKeySecret = nil
	nodeSet.Spec.Nodes[0].Validator.CreateValidator = &appsv1.CreateValidatorConfig{}
	signer := resolveSingleSigner(t, nodeSet)
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	r := newValidatorTestReconciler(t, nodeSet, source)

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, ready)
	require.Contains(t, blocked, signer.Name)

	validator, err := r.getValidatorSpecWithBlockedSignerTargets(nodeSet, "validators", 0, nodeSet.Spec.Nodes[0].Validator, blocked, true)
	require.NoError(t, err)
	require.False(t, validator.Spec.RemoteSignerTarget)
}

func TestPrepareCosmosignerImportsBlocksOnlyThePendingSigner(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators"},
				Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
					Address: "https://vault.example:8200", KeyName: "validator-key",
					TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
					UploadGenerated: true,
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}}},
				{Name: "sentries", Instances: ptr.To(1), Cosmosigner: &appsv1.Cosmosigner{Backend: cosmosignerVaultBackend()}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, ready)

	validator, err := r.getValidatorSpecWithBlockedSignerTargets(nodeSet, "validators", 0, nodeSet.Spec.Nodes[0].Validator, blocked, true)
	require.NoError(t, err)
	assert.False(t, validator.Spec.RemoteSignerTarget)

	sentry, err := r.getNodeSpecWithBlockedSignerTargets(nodeSet, nodeSet.Spec.Nodes[1], 0, blocked)
	require.NoError(t, err)
	assert.True(t, sentry.Spec.RemoteSignerTarget, "a ready signer must remain active while another signer waits for bootstrap material")
	assert.Equal(t, "test-nodeset-sentries-signer", sentry.Labels[controllers.LabelCosmosignerTarget])
}

func TestPrepareCosmosignerImportsBootstrapsGenesisValidatorLocally(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{
			Validator: &appsv1.NodeSetValidatorConfig{
				Init: &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1stake"}, StakeAmount: "1stake"},
			},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: "https://vault.example:8200", KeyName: "val-key",
				TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
				UploadGenerated: true,
			}}},
		},
	}
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.EnsureCosmosignerStatus(signer.Name).SigningDigest = "stale-status-without-chain-id"
	r := newValidatorTestReconciler(t, nodeSet)

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, ready)

	validator, err := r.getValidatorSpecWithBlockedSignerTargets(nodeSet, validatorGroupName, 0, nodeSet.Spec.Validator, blocked, true)
	require.NoError(t, err)
	assert.False(t, validator.Spec.RemoteSignerTarget, "the init validator must generate genesis and its key locally")
	assert.NotContains(t, validator.Labels, controllers.LabelCosmosignerTarget)
}

func TestPrepareCosmosignerImportsDoesNotLocalizeAnExistingSigner(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.Nodes[0].Validator.CreateValidator = &appsv1.CreateValidatorConfig{}
	r := newValidatorTestReconciler(t, nodeSet)
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-signer", Namespace: "default"}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), sts))

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source key is unavailable")
	assert.False(t, ready, "an existing signer with incomplete import status must stop child reconciliation")
	assert.Empty(t, blocked)

	validator, err := r.getValidatorSpecWithBlockedSignerTargets(nodeSet, "validators", 0, nodeSet.Spec.Nodes[0].Validator, blocked, true)
	require.NoError(t, err)
	assert.True(t, validator.Spec.RemoteSignerTarget, "status recovery must not switch a remote validator back to local signing")
}

func TestPrepareCosmosignerImportsStopsWhileSignerScalesDown(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	nodeSet.UID = types.UID("nodeset-uid")
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.EnsureCosmosignerStatus(signer.Name).KeyImported = "stale-import"
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	r := newValidatorTestReconciler(t, nodeSet, token, source)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: signer.Name, Namespace: "default"},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), sts))

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, ready)
	assert.Empty(t, blocked)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: signer.Name}, fresh))
	require.NotNil(t, fresh.Spec.Replicas)
	assert.Zero(t, *fresh.Spec.Replicas, "the old signer must quiesce before the import and child retargeting proceed")
}

func TestPrepareCosmosignerImportsReimportsForVaultTargetMigration(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "old-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	nodeSet.UID = types.UID("nodeset-uid")
	oldSigner := resolveSingleSigner(t, nodeSet)
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	status := nodeSet.EnsureCosmosignerStatus(oldSigner.Name)
	status.SigningDigest = oldSigner.Digest()
	status.KeyImported = oldSigner.Spec.Backend.Vault.ImportFingerprint(oldSigner.SoftwareKeySecret, key)
	nodeSet.Spec.Cosmosigner.Backend.Vault.KeyName = "new-key"
	newSigner := resolveSingleSigner(t, nodeSet)
	require.NotEqual(t, status.SigningDigest, newSigner.Digest())

	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: newSigner.SoftwareKeySecret, Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: newSigner.Name, Namespace: "default"},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	r := newValidatorTestReconciler(t, nodeSet, token, source)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), sts))

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, ready)
	assert.Empty(t, blocked)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.NotNil(t, fresh.Spec.Replicas)
	assert.Zero(t, *fresh.Spec.Replicas)
}

func TestReconcileValidatesVaultMigrationSourceBeforeQuiescing(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address: "https://vault.example:8200", KeyName: "old-key", UploadGenerated: true,
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.App = appsv1.AppSpec{Image: "image", App: "appd", Version: ptr.To("1.0.0")}
	oldSigner := resolveSingleSigner(t, nodeSet)
	oldImport := oldSigner.Spec.Backend.Vault.ImportFingerprint(oldSigner.SoftwareKeySecret, []byte("old-key-material"))
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Cosmosigner.Backend.Vault.KeyName = "new-key"
	newSigner := resolveSingleSigner(t, nodeSet)
	status := nodeSet.GetCosmosignerStatus(newSigner.Name)
	status.StateStorageSize = newSigner.Spec.GetStateStorageSize()
	status.StateStorageClassName = newSigner.Spec.StorageClassName
	status.Migration = &appsv1.CosmosignerMigrationStatus{
		DesiredDigest:    newSigner.Digest(),
		DesiredPublicKey: status.PublicKey,
		Phase:            appsv1.CosmosignerMigrationQuiescing,
	}
	status.KeyImported = oldImport

	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: newSigner.SoftwareKeySecret, Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: []byte("malformed-key")},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: newSigner.Name, Namespace: "default"},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	r := newValidatorTestReconciler(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		nodeSet, token, source,
	)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), sts))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: nodeSet.Name}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.Equal(t, int32(1), ptr.Deref(fresh.Spec.Replicas, 0))
}

func TestPrepareCosmosignerImportsRejectsMalformedKeyBeforeScaleDown(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	nodeSet.UID = types.UID("nodeset-uid")
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.EnsureCosmosignerStatus(signer.Name).KeyImported = "stale-import"
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: []byte("malformed-key")},
	}
	r := newValidatorTestReconciler(t, nodeSet, token, source)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: signer.Name, Namespace: "default"},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), sts))

	_, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
	assert.False(t, ready)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: signer.Name}, fresh))
	require.NotNil(t, fresh.Spec.Replicas)
	assert.Equal(t, int32(1), *fresh.Spec.Replicas, "a malformed source key must not quiesce the working signer")
}

func TestPrepareCosmosignerRolloutsKeepsLocalValidatorUntilReady(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	signer := resolveSingleSigner(t, nodeSet)
	signerStatus := nodeSet.EnsureCosmosignerStatus(signer.Name)
	signerStatus.Replicas = ptr.To(signer.Spec.GetReplicas())
	signerStatus.StateStorageSize = signer.Spec.GetStateStorageSize()
	signerStatus.StateStorageClassName = signer.Spec.StorageClassName
	signerStatus.ServingGroup = signer.ValidatorGroup

	localNodeSet := nodeSet.DeepCopy()
	localNodeSet.Spec.Cosmosigner = nil
	localValidator, err := newValidatorTestReconciler(t).getValidatorSpec(localNodeSet, "validators", 0, localNodeSet.Spec.Nodes[0].Validator)
	require.NoError(t, err)
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	r := newValidatorTestReconciler(t, nodeSet, keySecret, localValidator)

	ready, err := r.prepareCosmosignerRollouts(context.Background(), nodeSet, nil)
	require.NoError(t, err)
	assert.False(t, ready, "a newly applied signer must block child retargeting until rollout")

	fresh := &appsv1.ChainNode{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(localValidator), fresh))
	assert.False(t, fresh.Spec.RemoteSignerTarget)
	assert.NotContains(t, fresh.Labels, controllers.LabelCosmosignerTarget)
}

func TestInitializeLegacySignerServiceNamesUsesOwnedServices(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes:         []appsv1.NodeGroupSpec{{Name: "fullnodes-signer", Instances: ptr.To(1)}},
			Ingresses:     []appsv1.GlobalIngressConfig{{Name: "rpc-signer"}},
			GatewayRoutes: []appsv1.GlobalGatewayConfig{{Name: "grpc-signer-privval"}},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	ownedService := func(name, scope string) *corev1.Service {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: map[string]string{controllers.LabelScope: scope},
		}}
		require.NoError(t, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme))
		return svc
	}
	legacyNames := []string{
		"test-nodeset-fullnodes-signer",
		"test-nodeset-global-grpc-signer-privval",
		"test-nodeset-global-rpc-signer",
	}
	for i, name := range legacyNames {
		scope := scopeGlobal
		if i == 0 {
			scope = scopeGroup
		}
		require.NoError(t, r.Create(context.Background(), ownedService(name, scope)))
	}
	// Owned and correctly scoped is insufficient: this stale name is not derived by the current spec.
	require.NoError(t, r.Create(context.Background(), ownedService("test-nodeset-unused-signer", scopeGroup)))
	require.NoError(t, r.Create(context.Background(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "test-nodeset-rpc-signer", Namespace: "default",
	}}))
	require.NoError(t, r.Create(context.Background(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "test-nodeset-fullnodes", Namespace: "default",
	}}))

	initialized, err := r.initializeLegacySignerServiceNames(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, initialized)
	assert.True(t, nodeSet.Status.LegacySignerServiceNamesInitialized)
	assert.Equal(t, legacyNames, nodeSet.Status.LegacySignerServiceNames)

	initialized, err = r.initializeLegacySignerServiceNames(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, initialized)
}

func TestEnsureServiceRefusesForeignOwner(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	desired, err := r.getServiceSpec(nodeSet, appsv1.NodeGroupSpec{Name: "fullnodes"})
	require.NoError(t, err)
	require.NoError(t, r.Create(context.Background(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: desired.Name, Namespace: desired.Namespace,
	}}))

	err = r.ensureService(context.Background(), desired)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "managed by another owner")
}

// TestValidateForReconcileHonorsExistingStatus verifies the controller's no-webhook validation
// path validates an already-persisted ChainNodeSet against its own current status, so Validate can
// observe Status.ChainID (genesis already exists). A running ChainNodeSet that adds a
// createValidator group with no .spec.genesis is accepted, even though the same spec is rejected as
// a fresh create.
func TestValidateForReconcileHonorsExistingStatus(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig}},
				{Name: "joiners", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			},
		},
	}

	// With the existing status visible, the running configuration is valid.
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)

	// The recorded status is what makes it valid: the same spec validated as a fresh create is
	// rejected because there is no existing genesis to consume.
	fresh := nodeSet.DeepCopy()
	fresh.Status = appsv1.ChainNodeSetStatus{}
	_, err = fresh.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required")
}

// TestValidateForReconcileRejectsUnsafeDroppedGenesis verifies the no-webhook path enforces the
// status-gated genesis invariant without an old spec. A running chain (chainID set) whose current
// spec has a non-init validator group but no .spec.genesis and no genesis-initializing validator has
// no derivable <chainID>-genesis to consume, so it is rejected — rather than validated against a
// copy of itself, which previously masked the missing genesis.
func TestValidateForReconcileRejectsUnsafeDroppedGenesis(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "joiners", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required")
}

func TestValidateForReconcileRejectsUnsafeGenesisInitMutation(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	base := func(instances int, validators []appsv1.ChainNodeSetValidatorStatus) *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(instances),
				Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig},
			}}},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet", Validators: validators},
		}
	}

	t.Run("matching status is accepted", func(t *testing.T) {
		nodeSet := base(2, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		_, err := validateForReconcile(nodeSet)
		assert.NoError(t, err)
	})

	t.Run("scale up is rejected", func(t *testing.T) {
		nodeSet := base(3, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		_, err := validateForReconcile(nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be added with webhooks disabled")
	})

	t.Run("scale down is rejected", func(t *testing.T) {
		nodeSet := base(1, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		_, err := validateForReconcile(nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be removed or converted")
	})

	t.Run("rename is rejected", func(t *testing.T) {
		nodeSet := base(2, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		nodeSet.Spec.Nodes[0].Name = "renamed"
		_, err := validateForReconcile(nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be removed or converted")
	})
}

// TestValidateForReconcileAllowsLegacyEmptyValidatorStatus verifies that a pre-existing ChainNodeSet
// upgraded into this controller version — genesis already created (chainID set) but .status.validators
// not yet populated (the field is new) — is not rejected on the no-webhook path. Rejecting it would
// strand the running chain, since validation runs before ensureValidator can backfill the slice.
func TestValidateForReconcileAllowsLegacyEmptyValidatorStatus(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig},
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)
}

// TestValidateForReconcileRejectsGenesisSigningMaterialChange verifies that, on the no-webhook path,
// changing the resolved signing material of a recorded genesis validator is rejected — its consensus
// key is part of the immutable genesis validator set and cannot change after genesis.
func TestValidateForReconcileRejectsGenesisSigningMaterialChange(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig},
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	// Record the genesis validator with the fingerprint of its current signing material, exactly as
	// ensureValidator would.
	cfg := deriveGroupValidatorConfig(nodeSet, "validators", 0, 1, nodeSet.Spec.Nodes[0].Validator)
	nodeSet.Status.Validators = []appsv1.ChainNodeSetValidatorStatus{{
		Name:             "test-nodeset-validators-0",
		Group:            "validators",
		Init:             true,
		SigningKeyDigest: cfg.GenesisSigningFingerprint("test-nodeset-validators-0-priv-key"),
	}}

	// Unchanged signing material is accepted.
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)

	// Rotating the validator's private-key secret after genesis is rejected.
	nodeSet.Spec.Nodes[0].Validator.PrivateKeySecret = ptr.To("rotated-priv-key")
	_, err = validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signing material")
}

// TestValidateForReconcileRejectsRemovingGenesisInitValidator verifies that, on the no-webhook path,
// removing or converting a recorded genesis-initializing validator is rejected even when the desired
// init set is empty (e.g. switching the group to createValidator and supplying an external genesis).
// Its voting power remains in the immutable genesis, so deleting the ChainNode can halt the chain.
func TestValidateForReconcileRejectsRemovingGenesisInitValidator(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			},
		},
	}
	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be removed or converted")
}

// TestValidateForReconcileRejectsAddingInitToExternalChain verifies that, on the no-webhook path, adding
// a genesis-initializing validator to a running chain whose recorded validators are all non-init (e.g.
// an external-genesis chain with createValidator validators) is rejected: the immutable genesis was
// already created without it.
func TestValidateForReconcileRejectsAddingInitToExternalChain(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-joiners-0", Group: "joiners"}, // recorded createValidator, not init
			},
		},
	}
	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be added with webhooks disabled")
}

// TestValidateForReconcileEmptyStatusGenesisSourceMarker verifies the empty-.status.validators case is
// gated by the recorded genesis source: adding init validators is rejected when the genesis was imported
// externally (GenesisInitGenerated=false), but allowed when it was init-generated or the source is
// unknown (nil, a pre-marker legacy chain whose validator slice gets backfilled).
func TestValidateForReconcileEmptyStatusGenesisSourceMarker(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	mk := func(src *bool) *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Nodes: []appsv1.NodeGroupSpec{
					{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig}},
				},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet", GenesisInitGenerated: src},
		}
	}

	// External genesis source: adding an init validator is rejected.
	_, err := validateForReconcile(mk(ptr.To(false)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "imported an external genesis")

	// Init-generated source: allowed (this is the running init chain).
	_, err = validateForReconcile(mk(ptr.To(true)))
	require.NoError(t, err)

	// Unknown source (legacy, pre-marker): allowed so ensureValidator can backfill the slice.
	_, err = validateForReconcile(mk(nil))
	require.NoError(t, err)
}

// TestValidateForReconcileAllowsPureCreateValidatorChain verifies that a ChainNodeSet consuming an
// external genesis with only createValidator validators is not falsely rejected: its recorded
// validators are not genesis-initializing (not Init-flagged), so they are not genesis-protected.
func TestValidateForReconcileAllowsPureCreateValidatorChain(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "joiners",
				Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-joiners-0", Group: "joiners"},
				{Name: "test-nodeset-joiners-1", Group: "joiners"},
			},
		},
	}
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)
}

// cosmosignerVaultBackend builds a pre-provisioned (uploadGenerated=false) Vault backend.
func cosmosignerVaultBackend() appsv1.CosmosignerBackend {
	return appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:     "https://vault.example:8200",
		KeyName:     "val-key",
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}}
}

// recordSignerRollout records the sole signer of a ChainNodeSet as rolled out and serving, mirroring
// what reconcileSigner persists into the per-signer status entry after a successful rollout.
func recordSignerRollout(t *testing.T, ns *appsv1.ChainNodeSet) {
	t.Helper()
	s := resolveSingleSigner(t, ns)
	st := ns.EnsureCosmosignerStatus(s.Name)
	st.Replicas = ptr.To(s.Spec.GetReplicas())
	st.AppliedDigest = s.Digest()
	st.PublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if s.TargetsValidator() {
		st.SigningDigest = s.Digest()
		st.ServingIdentity = s.ValidatorTargetedIdentity()
		st.ServingGroup = s.ValidatorGroup
	}
}

// TestValidateForReconcileRejectsSentryReplicaChange verifies the no-webhook path enforces raft
// replica immutability for a sentry-mode signer, which never records a signing digest. The recorded
// per-signer status Replicas is what makes a later replica change rejectable.
func TestValidateForReconcileRejectsSentryReplicaChange(t *testing.T) {
	mk := func(replicas int32) *appsv1.ChainNodeSet {
		ns := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Cosmosigner: &appsv1.Cosmosigner{
					NodeGroups:              []string{"fullnodes"},
					Replicas:                ptr.To(replicas),
					UnsafeAllowInsecureRaft: true,
					Backend:                 cosmosignerVaultBackend(),
				},
				Nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(3)}},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
		}
		ns.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: "test-nodeset-signer", Replicas: ptr.To(int32(3))}}
		return ns
	}

	// Same replica count as recorded: accepted.
	_, err := validateForReconcile(mk(3))
	require.NoError(t, err)

	// Changed replica count: rejected — the raft membership cannot be migrated.
	_, err = validateForReconcile(mk(5))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replicas are immutable")
}

func TestValidateForReconcileRejectsSignerAdditionToEstablishedMultiInstanceValidator(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{},
				}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}

	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-instance validator group")
}

func TestValidateForReconcileRejectsSentryRetargetToEstablishedMultiInstanceValidator(t *testing.T) {
	old := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"sentries"},
				Backend:    cosmosignerVaultBackend(),
			},
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "sentries", Instances: ptr.To(1)},
				{Name: "validators", Instances: ptr.To(2), Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}}},
			},
		},
	}
	old.SetEstablishedChainID("test-localnet")
	oldSigner := resolveSingleSigner(t, old)
	oldStatus := old.EnsureCosmosignerStatus(oldSigner.Name)
	oldStatus.AppliedDigest = oldSigner.Digest()
	oldStatus.PublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	old.Status.Validators = []appsv1.ChainNodeSetValidatorStatus{
		{Name: "test-nodeset-validators-0", Group: "validators", PubKey: "validator-0"},
		{Name: "test-nodeset-validators-1", Group: "validators", PubKey: "validator-1"},
	}
	updated := old.DeepCopy()
	updated.Spec.Cosmosigner.NodeGroups = []string{"validators"}

	_, err := validateForReconcile(updated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-instance validator group")
}

// cosmosignerValidatorNodeSet builds an established ChainNodeSet whose validator group is targeted by a
// cosmosigner with the given backend. SetEstablishedChainID records the establishing at-establishment
// marker for the signer (mirroring the controller), so a signer present at establishment is
// distinguishable from a post-establishment addition.
func cosmosignerValidatorNodeSet(backend appsv1.CosmosignerBackend) *appsv1.ChainNodeSet {
	ns := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators"},
				Backend:    backend,
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{},
	}
	ns.SetEstablishedChainID("test-localnet")
	return ns
}

// TestValidateForReconcileAllowsFirstSignerRollout verifies the no-webhook path admits a
// validator-targeted signer present at establishment that has not yet recorded a rollout digest —
// including a pre-provisioned Vault backend. Blocking it here would deadlock the first rollout (the
// at-establishment marker matches the signer identity, so it is not a post-establishment addition).
func TestValidateForReconcileAllowsFirstSignerRollout(t *testing.T) {
	// Pre-provisioned Vault key, no recorded digest (first rollout in progress): accepted.
	_, err := validateForReconcile(cosmosignerValidatorNodeSet(cosmosignerVaultBackend()))
	require.NoError(t, err)

	// Software backend, no recorded digest: accepted.
	_, err = validateForReconcile(cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}}))
	require.NoError(t, err)
}

// TestValidateForReconcileAllowsRecordedSignerKeyMigration verifies a recorded public key admits a
// backend/key migration and controlled fallback removal.
func TestValidateForReconcileAllowsRecordedSignerKeyMigration(t *testing.T) {
	recorded := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, recorded)

	// Unchanged config: accepted.
	_, err := validateForReconcile(recorded)
	require.NoError(t, err)

	// Changed Vault key on the live signer: admitted for break-before-make migration.
	changed := recorded.DeepCopy()
	changed.Spec.Cosmosigner.Backend.Vault.KeyName = "different-key"
	_, err = validateForReconcile(changed)
	require.NoError(t, err)

	// Removing the Vault signer is admitted; the controller stops it before publishing fallback.
	removed := recorded.DeepCopy()
	removed.Spec.Cosmosigner = nil
	_, err = validateForReconcile(removed)
	require.NoError(t, err)
}

func TestValidateForReconcileAllowsProvenMultiInstancePlacementReplacement(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	nodeSet.Spec.Nodes[0].Instances = ptr.To(3)
	recordSignerRollout(t, nodeSet)

	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Cosmosigner = &appsv1.Cosmosigner{Backend: cosmosignerVaultBackend()}

	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)
}

// TestValidateForReconcileAllowsRecordedValidatorSigner verifies that once a validator-targeted
// signer's digest is recorded (it rolled out and served), the same spec passes and a pre-provisioned
// backend is no longer refused — the recorded digest proves the key is the one in effect.
func TestValidateForReconcileAllowsRecordedValidatorSigner(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, nodeSet)

	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)
}

// TestValidateForReconcilePostEstablishmentSignerAddition verifies the write-once at-establishment
// marker: both an establishing signer and a signer added later are admitted. Cosmopilot controls the
// handoff; the user remains responsible for the selected on-chain key.
func TestValidateForReconcilePostEstablishmentSignerAddition(t *testing.T) {
	// Established WITH the signer (entry AtEstablishment == identity): admitted.
	establishing := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	_, err := validateForReconcile(establishing)
	require.NoError(t, err)

	// Established, then a pre-provisioned Vault signer added later: admitted.
	added := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	added.Status.Cosmosigners = nil
	_, err = validateForReconcile(added)
	require.NoError(t, err)

	// The same late addition with uploadGenerated on an external-genesis target is also admitted.
	importing := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	importing.Status.Cosmosigners = nil
	_, err = validateForReconcile(importing)
	require.NoError(t, err)
}

// TestSetEstablishedChainIDRecordsMarkerAtomically verifies the chain ID and the per-signer
// at-establishment identity marker are recorded in the same status mutation, closing the window in
// which a chain is established but the marker is nil (during which an unverifiable signer addition
// could slip past the no-webhook guard and be blessed by a late marker write).
func TestSetEstablishedChainIDRecordsMarkerAtomically(t *testing.T) {
	// Established WITH a signer: the establishing identity is recorded in the signer's status entry.
	withSigner := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	withSigner.Status = appsv1.ChainNodeSetStatus{}
	withSigner.SetEstablishedChainID("test-localnet")
	s := resolveSingleSigner(t, withSigner)
	require.Len(t, withSigner.Status.Cosmosigners, 1)
	require.NotNil(t, withSigner.Status.Cosmosigners[0].AtEstablishment)
	assert.Equal(t, s.ValidatorTargetedIdentity(), *withSigner.Status.Cosmosigners[0].AtEstablishment)
	require.NotNil(t, withSigner.Status.Cosmosigners[0].LocalKeyEverServed)
	assert.False(t, *withSigner.Status.Cosmosigners[0].LocalKeyEverServed)
	assert.Equal(t, "test-localnet", withSigner.Status.ChainID)

	unknownHistory := withSigner.DeepCopy()
	unknownHistory.Status.Cosmosigners[0].LocalKeyEverServed = nil
	unknownHistory.SetEstablishedChainID("test-localnet")
	assert.Nil(t, unknownHistory.Status.Cosmosigners[0].LocalKeyEverServed,
		"an already-established chain must not backfill unknown key-serving history from the current spec")

	// Established WITHOUT a signer: no signer lifecycle status is recorded.
	noSigner := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	noSigner.Spec.Cosmosigner = nil
	noSigner.Status = appsv1.ChainNodeSetStatus{}
	noSigner.SetEstablishedChainID("test-localnet")
	assert.Empty(t, noSigner.Status.Cosmosigners)

	// Empty chain ID: nothing is recorded (chain not established yet).
	pending := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	pending.Status = appsv1.ChainNodeSetStatus{}
	pending.SetEstablishedChainID("")
	assert.Empty(t, pending.Status.Cosmosigners)
	assert.Empty(t, pending.Status.ChainID)
}

// TestValidateForReconcileSignerRemoval verifies rolled-out validator signers can be removed through
// the controlled break-before-make fallback path regardless of backend.
func TestValidateForReconcileSignerRemoval(t *testing.T) {
	// Vault-backed signer served; removal is admitted and the user owns the fallback key choice.
	vaultServed := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, vaultServed)
	vaultServed.Spec.Cosmosigner = nil
	_, err := validateForReconcile(vaultServed)
	require.NoError(t, err)

	// Software-backed signer served with the validator's own key; removal keeps the same identity.
	softwareServed := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	recordSignerRollout(t, softwareServed)
	softwareServed.Spec.Cosmosigner = nil
	_, err = validateForReconcile(softwareServed)
	require.NoError(t, err)

	// A multi-instance validator group targeted by one signer is one validator with redundant
	// endpoints. Removing the signer would restore per-instance createValidator/local-key behavior
	// and expand it into multiple validator identities, even though instance 0 resolves the same key.
	multiInstance := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{
					CreateValidator: &appsv1.CreateValidatorConfig{},
				},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}}},
			}},
		},
	}
	multiInstance.SetEstablishedChainID("test-localnet")
	recordSignerRollout(t, multiInstance)
	multiInstance.Spec.Nodes[0].Cosmosigner = nil
	_, err = validateForReconcile(multiInstance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-instance")

	// A different fallback key is admitted; Cosmopilot controls the stop/start sequence and the user
	// remains responsible for the on-chain key.
	otherResolves := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	recordSignerRollout(t, otherResolves)
	otherResolves.Spec.Cosmosigner = nil
	// The served group now points at a different explicit key, while an unrelated group references
	// the served identity's secret.
	otherResolves.Spec.Nodes[0].Validator.PrivateKeySecret = ptr.To("rotated-away-key")
	otherResolves.Spec.Nodes = append(otherResolves.Spec.Nodes, appsv1.NodeGroupSpec{
		Name:      "other",
		Instances: ptr.To(1),
		Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
	})
	_, err = validateForReconcile(otherResolves)
	require.NoError(t, err)

	// No serving identity recorded (the signer never rolled out): removal is free.
	neverServed := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	neverServed.Status.Cosmosigners = nil
	neverServed.Spec.Cosmosigner = nil
	_, err = validateForReconcile(neverServed)
	require.NoError(t, err)
}

func TestValidateForReconcileRejectsPreRolloutMigratedSignerRemoval(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:             "test-nodeset-signer",
		Replicas:         ptr.To(int32(1)),
		StateStorageSize: "1Gi",
		ServingGroup:     "validators",
	}}
	nodeSet.Spec.Cosmosigner = nil

	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rollout identity has not been recorded")
}

// TestValidateForReconcileSentryRetargetToValidator verifies that a sentry-mode signer records "" in
// its at-establishment marker (its key identity is deliberately excluded), and a later retarget is
// admitted as a controlled migration once the applied public key is recorded.
func TestValidateForReconcileSentryRetargetToValidator(t *testing.T) {
	// Established with a sentry signer over fullnodes (pre-provisioned Vault key).
	sentry := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"fullnodes"},
				Backend:    cosmosignerVaultBackend(),
			},
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "fullnodes", Instances: ptr.To(3)},
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{},
	}
	sentry.SetEstablishedChainID("test-localnet")
	recordSignerRollout(t, sentry)
	require.Len(t, sentry.Status.Cosmosigners, 1)
	require.NotNil(t, sentry.Status.Cosmosigners[0].AtEstablishment)
	assert.Empty(t, *sentry.Status.Cosmosigners[0].AtEstablishment, "a sentry signer must record an empty validator-targeted identity")

	// Sentry configuration itself validates fine.
	_, err := validateForReconcile(sentry)
	require.NoError(t, err)

	// Retargeting the same key onto the validator group is admitted for break-before-make migration.
	retargeted := sentry.DeepCopy()
	retargeted.Spec.Cosmosigner.NodeGroups = []string{"validators"}
	_, err = validateForReconcile(retargeted)
	require.NoError(t, err)
}

// TestValidateForReconcileAllowsServedGroupConversionMigration verifies validator targeting is part
// of the lifecycle digest, so converting a served group becomes an explicit migration.
func TestValidateForReconcileAllowsServedGroupConversionMigration(t *testing.T) {
	served := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, served)

	// Unchanged: accepted.
	_, err := validateForReconcile(served)
	require.NoError(t, err)

	// Convert the served group into a regular node group: validator targeting changes the digest and
	// the recorded public key admits a controlled migration.
	converted := served.DeepCopy()
	converted.Spec.Nodes[0].Validator = nil
	require.NotEqual(t, served.Status.Cosmosigners[0].SigningDigest, resolveSingleSigner(t, converted).Digest())
	_, err = validateForReconcile(converted)
	require.NoError(t, err)
}

// TestValidateForReconcileAllowsServedGroupSwapMigration verifies moving validator status between
// targeted groups is represented as a migration and admitted once the public key is recorded.
func TestValidateForReconcileAllowsServedGroupSwapMigration(t *testing.T) {
	served := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	// Target two groups: the served validator group and a plain group.
	served.Spec.Cosmosigner.NodeGroups = []string{"validators", "others"}
	served.Spec.Nodes = append(served.Spec.Nodes, appsv1.NodeGroupSpec{Name: "others", Instances: ptr.To(1)})
	recordSignerRollout(t, served)

	// Unchanged: accepted.
	_, err := validateForReconcile(served)
	require.NoError(t, err)

	// Swap validator-ness: "validators" becomes a plain group and "others" gains a validator.
	swapped := served.DeepCopy()
	swapped.Spec.Nodes[0].Validator = nil
	swapped.Spec.Nodes[1].Validator = &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("other-key")}
	require.NotEqual(t, served.Status.Cosmosigners[0].SigningDigest, resolveSingleSigner(t, swapped).Digest())
	_, err = validateForReconcile(swapped)
	require.NoError(t, err)
}

// TestValidateForReconcileRejectsLegacyDigestDemotion verifies the no-webhook guard rejects demoting
// the served validator group of a LEGACY-digest signer (SigningDigest recorded, serving fields empty —
// an upgrade from a status shape predating them) into a regular/sentry group. Without the serving
// fields the precise group+instance check cannot run, so a signer that no longer targets any validator
// while its digest still matches is unverifiable and rejected. Kept targeting a validator, it passes so
// the controller can backfill the serving identity.
func TestValidateForReconcileRejectsLegacyDigestDemotion(t *testing.T) {
	// Legacy status: digest recorded, but no serving identity/group/instance.
	served := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	served.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:             "test-nodeset-signer",
		Replicas:         ptr.To(int32(1)),
		StateStorageSize: "1Gi",
		SigningDigest:    resolveSingleSigner(t, served).Digest(),
	}}

	// Unchanged (still targets the validator): admitted — the controller backfills serving fields.
	_, err := validateForReconcile(served)
	require.NoError(t, err)

	// Demoting changes the new lifecycle digest, but legacy status has no applied public key, so the
	// migration is rejected until the old configuration is restored and backfilled.
	demoted := served.DeepCopy()
	demoted.Spec.Nodes[0].Validator = nil
	require.NotEqual(t, served.Status.Cosmosigners[0].SigningDigest, resolveSingleSigner(t, demoted).Digest())
	_, err = validateForReconcile(demoted)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "applied public key")
}

// TestValidateForReconcileRejectsLegacyMultiTargetDigestSwap verifies that a LEGACY-digest signer
// (SigningDigest recorded, serving fields empty) that targets MULTIPLE groups is rejected even when its
// digest is unchanged: the digest hashes the backend identity, replica count and target-group NAMES —
// not which targeted group is the validator — so a no-webhook edit could have swapped validator-ness
// between the targets (e.g. served group a -> b) with the digest intact, which status alone cannot
// disprove. Only a single-target legacy signer (no sibling to swap with) is admitted for backfill.
func TestValidateForReconcileRejectsLegacyMultiTargetDigestSwap(t *testing.T) {
	ns := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators", "others"},
				Backend:    cosmosignerVaultBackend(),
			},
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")}},
				{Name: "others", Instances: ptr.To(1)},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	// Legacy status: digest recorded, no serving identity/group/instance.
	ns.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:             "test-nodeset-signer",
		Replicas:         ptr.To(int32(1)),
		StateStorageSize: "1Gi",
		SigningDigest:    resolveSingleSigner(t, ns).Digest(),
	}}

	// Unchanged, but multi-target legacy: unverifiable -> rejected.
	_, err := validateForReconcile(ns)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validator target cannot be verified from status alone")
}

// genesisSentryNodeSet builds an ESTABLISHED ChainNodeSet whose legacy validator registers `genesisKey`
// in init.genesisValidators, with a sentry-mode software signer (over a non-validator group) using
// `sentryKey`. SetEstablishedChainID records the sentry signer's at-establishment identity — its genesis
// key when sentryKey is registered in genesis, else the empty (unprotected) marker.
func genesisSentryNodeSet(genesisKey, sentryKey string) *appsv1.ChainNodeSet {
	ns := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			App: appsv1.AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Validator: &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
				ChainID:     "test-localnet",
				Assets:      []string{"1stake"},
				StakeAmount: "1stake",
				GenesisValidators: []appsv1.GenesisValidator{{
					PrivKeySecret:         genesisKey,
					AccountMnemonicSecret: "mn",
					Moniker:               "preserved",
					Assets:                []string{"1stake"},
					StakeAmount:           "1stake",
				}},
			}},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:        "sentries",
				Instances:   ptr.To(3),
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(sentryKey)}}},
			}},
		},
	}
	ns.SetEstablishedChainID("test-localnet")
	return ns
}

func TestValidateForReconcileProtectsGenesisSentryKey(t *testing.T) {
	base := genesisSentryNodeSet("genesis-sentry-key", "genesis-sentry-key")
	recordSignerRollout(t, base)

	// Unchanged: accepted.
	_, err := validateForReconcile(base)
	require.NoError(t, err)

	// The immutable genesis key must retain a managed signing path.
	changed := base.DeepCopy()
	changed.Spec.Nodes[0].Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("other-key")
	_, err = validateForReconcile(changed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on-chain consensus key")

	// Removing the signer would orphan the immutable genesis key.
	removed := base.DeepCopy()
	removed.Spec.Nodes[0].Cosmosigner = nil
	_, err = validateForReconcile(removed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on-chain consensus key")

	// A sentry whose key is NOT in genesis stays rotatable and removable.
	free := genesisSentryNodeSet("some-genesis-key", "free-key")
	recordSignerRollout(t, free)
	freeChanged := free.DeepCopy()
	freeChanged.Spec.Nodes[0].Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("free-key-2")
	_, err = validateForReconcile(freeChanged)
	require.NoError(t, err)

	freeRemoved := free.DeepCopy()
	freeRemoved.Spec.Nodes[0].Cosmosigner = nil
	_, err = validateForReconcile(freeRemoved)
	require.NoError(t, err)
}

// TestValidateForReconcileRejectsPreDigestValidatorDemotion verifies the pre-digest window is guarded:
// a validator-targeted signer present at establishment (AtEstablishment recorded) but not yet rolled out
// (no SigningDigest) cannot have its validator dropped while the same pre-provisioned Vault signer is
// kept — that would demote the node to a sentry and strip the validator's signing path before the
// digest/serving guards ever apply.
func TestValidateForReconcileRejectsPreDigestValidatorDemotion(t *testing.T) {
	// Validator signer present at establishment (marker recorded), not yet rolled out (no digest).
	ns := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	// The fixture must record the establishment marker + served group, or the guard would be a no-op and
	// the test would pass for the wrong reason.
	require.Len(t, ns.Status.Cosmosigners, 1)
	require.NotNil(t, ns.Status.Cosmosigners[0].AtEstablishment)
	require.NotEmpty(t, *ns.Status.Cosmosigners[0].AtEstablishment)
	require.Equal(t, "validators", ns.Status.Cosmosigners[0].ServingGroup)
	require.Empty(t, ns.Status.Cosmosigners[0].SigningDigest, "must be pre-digest")

	_, err := validateForReconcile(ns)
	require.NoError(t, err)

	// Demote: the targeted group loses its validator while the same pre-provisioned Vault signer stays.
	demoted := ns.DeepCopy()
	demoted.Spec.Nodes[0].Validator = nil
	_, err = validateForReconcile(demoted)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must keep serving that exact validator/genesis key")
}

// TestValidateForReconcileRejectsPreDigestSiblingSwap verifies the pre-digest guard pins the SERVED
// group: a signer targeting multiple groups cannot move validator-ness from the originally served group
// to a sibling (keeping the same backend identity) before its rollout digest is recorded, which would
// otherwise pass the identity check while stripping the original validator's signing path.
func TestValidateForReconcileRejectsPreDigestSiblingSwap(t *testing.T) {
	ns := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators", "others"},
				Backend:    cosmosignerVaultBackend(),
			},
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")}},
				{Name: "others", Instances: ptr.To(1)},
			},
		},
	}
	ns.SetEstablishedChainID("test-localnet")
	require.Equal(t, "validators", ns.Status.Cosmosigners[0].ServingGroup)

	// Pre-digest, unchanged: accepted.
	_, err := validateForReconcile(ns)
	require.NoError(t, err)

	// Swap validator-ness to the sibling group (same Vault identity), still pre-digest: rejected.
	swapped := ns.DeepCopy()
	swapped.Spec.Nodes[0].Validator = nil
	swapped.Spec.Nodes[1].Validator = &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("other-priv-key")}
	_, err = validateForReconcile(swapped)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sibling-group swap")
}

// TestValidateForReconcileRejectsLegacyPreDigestValidator verifies a LEGACY establishment marker with
// no served group is treated as unverifiable for validator-ness: even a single-target validator signer
// is rejected, because a top-level signer can retarget .spec.cosmosigner.nodeGroups from [a] to [b]
// while keeping the same status entry, cardinality and identity — cardinality cannot tell a retarget
// from an unchanged config. A genesis SENTRY with no served group stays admitted (via its genesis-key
// identity). This shape only occurs on an intermediate pre-release status.
func TestValidateForReconcileRejectsLegacyPreDigestValidator(t *testing.T) {
	// Single-target validator signer, marker recorded but served group cleared (legacy shape): rejected.
	ns := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	ns.Status.Cosmosigners[0].ServingGroup = ""
	_, err := validateForReconcile(ns)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on-chain consensus key")

	// Lifecycle/public-key evidence proves a signer existed, but without the served group it cannot
	// prove which validator the signer protected. The same legacy shape must remain fail-closed.
	recovered := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recovered.Status.Cosmosigners[0].ServingGroup = ""
	recovered.Status.Cosmosigners[0].AppliedDigest = "legacy-lifecycle"
	recovered.Status.Cosmosigners[0].PublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	_, err = validateForReconcile(recovered)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on-chain consensus key")

	// A genesis sentry (no served group by design) with the same legacy-empty ServingGroup is still
	// admitted, because its key identity is verifiable against the immutable genesis set.
	sentry := genesisSentryNodeSet("genesis-sentry-key", "genesis-sentry-key")
	require.Empty(t, sentry.Status.Cosmosigners[0].ServingGroup)
	_, err = validateForReconcile(sentry)
	require.NoError(t, err)
}

// TestSignerImportSourcePendingGenesisInitExplicitKey reproduces the e2e drop-in Vault signer setup: a
// genesis-initializing legacy validator with an EXPLICIT privateKeySecret, fronted by a top-level Vault
// uploadGenerated signer. The key is GENERATED into that explicit secret during bootstrap, so the import
// source must be PENDING pre-genesis (else the every-pass preflight demands the secret before
// ensureValidator creates it and wedges the chain at height 0), and terminal once the pubkey is recorded.
func TestSignerImportSourcePendingGenesisInitExplicitKey(t *testing.T) {
	mk := func() *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Validator: &appsv1.NodeSetValidatorConfig{
					PrivateKeySecret: ptr.To("shared-key"),
					Init:             &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1stake"}, StakeAmount: "1stake"},
				},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
					Address: "https://vault:8200", KeyName: "k",
					TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
					UploadGenerated: true,
				}}},
			},
		}
	}
	r := &Reconciler{}

	// Pre-genesis: the generated key does not exist yet, so the import source is pending (skip the demand).
	pre := mk()
	signers := pre.ResolveCosmosigners()
	require.Len(t, signers, 1)
	require.True(t, r.signerImportSourcePending(pre, signers[0]), "genesis-init explicit key must be pending pre-genesis")

	// Post-genesis, pubkey not yet recorded: still pending — the generating validator records its key
	// itself, and an explicit privateKeySecret does not make it user-supplied.
	mid := mk()
	mid.Status.ChainID = "test-localnet"
	require.True(t, r.signerImportSourcePending(mid, mid.ResolveCosmosigners()[0]), "generating validator is pending until pubkey")

	// Post-genesis, pubkey recorded: terminal (the generated key now exists, so the source is required).
	done := mk()
	done.Status.ChainID = "test-localnet"
	done.Status.Validators = []appsv1.ChainNodeSetValidatorStatus{{Name: validatorNodeName(done, appsv1.ReservedValidatorGroupName, 0), PubKey: "pk"}}
	require.False(t, r.signerImportSourcePending(done, done.ResolveCosmosigners()[0]), "terminal once the pubkey is recorded")
}

// TestSignerImportSourcePendingCreateValidatorExplicitKey verifies a createValidator validator with an
// explicit privateKeySecret stays pending until its pubkey is recorded: the child ChainNode generates
// the key into that secret when it runs, so demanding it earlier would block a valid first-time config.
func TestSignerImportSourcePendingCreateValidatorExplicitKey(t *testing.T) {
	mk := func() *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Validator: &appsv1.NodeSetValidatorConfig{
					PrivateKeySecret: ptr.To("cv-key"),
					CreateValidator:  &appsv1.CreateValidatorConfig{},
				},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
					Address: "https://vault:8200", KeyName: "k",
					TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
					UploadGenerated: true,
				}}},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
		}
	}
	r := &Reconciler{}

	// Pubkey not yet recorded: pending (the child will generate the key into the explicit secret).
	pre := mk()
	require.True(t, r.signerImportSourcePending(pre, pre.ResolveCosmosigners()[0]), "createValidator explicit key must be pending until pubkey")

	// Pubkey recorded: terminal.
	done := mk()
	done.Status.Validators = []appsv1.ChainNodeSetValidatorStatus{{Name: validatorNodeName(done, appsv1.ReservedValidatorGroupName, 0), PubKey: "pk"}}
	require.False(t, r.signerImportSourcePending(done, done.ResolveCosmosigners()[0]), "terminal once the pubkey is recorded")
}
