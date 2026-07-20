package chainnode

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEnsureValidatorConsensusKeyReservationStopsConflictingLocalSigner(t *testing.T) {
	const namespace, name = "default", "validator"

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", parsed.PubKey.Value)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: parsed.PubKey.Value, OwnerUID: "other-root",
			OwnerKind: "ChainNodeSet", Namespace: "other", OwnerName: "other-validator", Claim: "validator",
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: name,
			UID: chainNode.UID, Controller: ptr.To(true),
		}},
	}}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(keySecret, reservation, pod).Build()}

	_, err = r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode)
	if err == nil {
		t.Fatal("a local validator must not sign a key reserved by another controller root")
	}
	remaining := &corev1.Pod{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, remaining)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("conflicting validator pod must be deleted, got %v", err)
	}
}

func TestEnsureValidatorConsensusKeyReservationUsesChainNodeSetRoot(t *testing.T) {
	const namespace, name = "default", "nodes-validators-0"
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: "child-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "nodes",
				UID: "nodeset-uid", Controller: ptr.To(true),
			}},
		},
		Spec:   appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")}},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", parsed.PubKey.Value)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: parsed.PubKey.Value, OwnerUID: "nodeset-uid",
			OwnerKind: "ChainNodeSet", Namespace: namespace, OwnerName: "nodes", Claim: name,
		},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(keySecret, reservation).Build()}

	if _, err := r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode); err != nil {
		t.Fatalf("a ChainNodeSet child must share its parent root reservation: %v", err)
	}
}

func TestEnsureValidatorConsensusKeyReservationSkipsRemoteSignerTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "child-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator:          &appsv1.ValidatorConfig{},
			RemoteSignerTarget: true,
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	if _, err := r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode); err != nil {
		t.Fatalf("a remote signer target must rely on its parent reservation: %v", err)
	}
}

func TestEnsureValidatorConsensusKeyReservationUsesPendingTmKMSUploadKey(t *testing.T) {
	const namespace, name = "default", "validator"
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{
			PrivateKeySecret: ptr.To("validator-key"),
			TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{Hashicorp: &appsv1.TmKmsHashicorpProvider{
				Address: "https://vault:8200", Key: "validator", UploadGenerated: true,
			}}},
		}},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(keySecret).Build()}

	if _, err := r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode); err != nil {
		t.Fatal(err)
	}
	reservation := &appsv1.ConsensusKeyReservation{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: cosmosigner.ConsensusKeyReservationName("chain-1", parsed.PubKey.Value)}, reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.Spec.OwnerUID != chainNode.UID || reservation.Spec.Claim != name {
		t.Fatalf("unexpected reservation owner: %#v", reservation.Spec)
	}
}

func TestEnsureValidatorConsensusKeyReservationReusesVerifiedTmKMSIdentity(t *testing.T) {
	const (
		namespace = "default"
		name      = "validator"
		publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	)
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{
			Hashicorp: &appsv1.TmKmsHashicorpProvider{
				Address: "https://vault:8200",
				Key:     "validator",
				TokenSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
					Key:                  "token",
				},
			},
		}}}},
		Status: appsv1.ChainNodeStatus{
			ChainID: "chain-1",
			PubKey:  `{"key":"` + publicKey + `"}`,
		},
	}
	chainNode.Status.TmKMSReservationIdentity = chainNode.EffectiveSigningIdentity()
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("token")},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(token).Build()}

	recorded, err := r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode)
	if err != nil {
		t.Fatalf("verified tmKMS identity must not require another Cosmosigner pubkey pod: %v", err)
	}
	if recorded {
		t.Fatal("an unchanged verified tmKMS identity must not rewrite status")
	}
	reservation := &appsv1.ConsensusKeyReservation{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: cosmosigner.ConsensusKeyReservationName("chain-1", publicKey)}, reservation); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureValidatorConsensusKeyReservationClaimsActualKeyBeforeRejectingMalformedStatus(t *testing.T) {
	const namespace, name = "default", "validator"
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec:       appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")}},
		Status:     appsv1.ChainNodeStatus{ChainID: "chain-1", PubKey: "malformed"},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(keySecret).Build()}

	_, err = r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode)
	if err == nil {
		t.Fatal("malformed recorded validator status must fail closed")
	}
	reservation := &appsv1.ConsensusKeyReservation{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: cosmosigner.ConsensusKeyReservationName("chain-1", parsed.PubKey.Value)}, reservation); err != nil {
		t.Fatalf("the actual active key must still be reserved before returning the status error: %v", err)
	}
}

func TestReconcileSigningConfigsPreflightsCosmosignerBeforeRemovingTmKMS(t *testing.T) {
	const namespace, name = "default", "validator"

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{},
			Cosmosigner: &appsv1.Cosmosigner{
				ServiceAccountName: ptr.To("missing-signer-sa"),
				Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
					Address: "https://vault:8200", KeyName: "validator-key",
					TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecret.Name}, Key: "token"},
				}},
			},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}

	var deleteRequests atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deleteRequests.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    req,
		}, nil
	})
	clientSet, err := kubernetes.NewForConfig(&rest.Config{Host: "https://kubernetes.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(tokenSecret).Build(),
		ClientSet: clientSet,
		Scheme:    scheme,
		opts:      &controllers.ControllerRunOptions{},
	}

	_, err = r.reconcileSigningConfigs(context.Background(), chainNode)
	if err == nil {
		t.Fatal("missing signer ServiceAccount must fail preflight")
	}
	if got := deleteRequests.Load(); got != 0 {
		t.Fatalf("failed signer preflight issued %d tmKMS delete requests, want 0", got)
	}
}

func TestReconcileSigningConfigsImportsCosmosignerBeforeRemovingTmKMS(t *testing.T) {
	const namespace, name = "default", "validator"

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8sappsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To(keySecret.Name)},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: "https://vault:8200", KeyName: "validator-key",
				TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecret.Name}, Key: "token"},
				UploadGenerated: true,
			}}},
		},
		Status: appsv1.ChainNodeStatus{
			ChainID:                      "chain-1",
			CosmosignerReplicas:          ptr.To(int32(1)),
			CosmosignerStateStorageSize:  "1Gi",
			CosmosignerValidatorTargeted: ptr.To(true),
		},
	}

	var deleteRequests atomic.Int32
	var importCreates atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		statusCode := http.StatusNotFound
		body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		switch req.Method {
		case http.MethodDelete:
			deleteRequests.Add(1)
		case http.MethodPost:
			importCreates.Add(1)
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
	clientSet, err := kubernetes.NewForConfig(&rest.Config{Host: "https://kubernetes.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(tokenSecret, keySecret).Build(),
		ClientSet: clientSet,
		Scheme:    scheme,
		recorder:  record.NewFakeRecorder(10),
		opts:      &controllers.ControllerRunOptions{},
	}

	_, err = r.reconcileSigningConfigs(context.Background(), chainNode)
	if err == nil {
		t.Fatal("forced Vault import failure must stop the signing-config migration")
	}
	if got := importCreates.Load(); got != 1 {
		t.Fatalf("Vault import creates = %d, want 1 (reconcile error: %v)", got, err)
	}
	if got := deleteRequests.Load(); got != 0 {
		t.Fatalf("failed Vault import issued %d tmKMS delete requests, want 0", got)
	}
}

func TestReconcileSigningConfigsWaitsForCosmosignerRollout(t *testing.T) {
	const namespace, name = "default", "sentry"
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8sappsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "sentry-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{
			Replicas: ptr.To(int32(1)),
			Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(keySecret.Name)},
			},
		}},
		Status: appsv1.ChainNodeStatus{
			ChainID:                      "chain-1",
			CosmosignerReplicas:          ptr.To(int32(1)),
			CosmosignerStateStorageSize:  "1Gi",
			CosmosignerValidatorTargeted: ptr.To(false),
		},
	}
	var deleteRequests atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deleteRequests.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)),
			Request:    req,
		}, nil
	})
	clientSet, err := kubernetes.NewForConfig(&rest.Config{Host: "https://kubernetes.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&appsv1.ChainNode{}, &k8sappsv1.StatefulSet{}).
			WithObjects(chainNode, keySecret).Build(),
		ClientSet: clientSet,
		Scheme:    scheme,
		recorder:  record.NewFakeRecorder(10),
		opts:      &controllers.ControllerRunOptions{},
	}

	pending, err := r.reconcileSigningConfigs(context.Background(), chainNode)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("a newly applied cosmosigner must keep the node on its existing signing path until rollout")
	}
	if got := deleteRequests.Load(); got != 0 {
		t.Fatalf("pending cosmosigner rollout issued %d tmKMS delete requests, want 0", got)
	}

	sts := &k8sappsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: cosmosignerName(chainNode)}, sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.ObservedGeneration = sts.Generation
	sts.Status.UpdatedReplicas = 1
	sts.Status.ReadyReplicas = 1
	if err := r.Status().Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}

	pending, err = r.reconcileSigningConfigs(context.Background(), chainNode)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("lifecycle recovery must requeue before the node signing-config transition")
	}

	pending, err = r.reconcileSigningConfigs(context.Background(), chainNode)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("a recovered, fully rolled-out cosmosigner must allow the node signing-config transition on the next reconcile")
	}
	if got := deleteRequests.Load(); got != 0 {
		t.Fatalf("signing preparation issued %d tmKMS delete requests before the node pod transition, want 0", got)
	}
}

func TestReconcileSigningConfigsKeepsTmKMSForChainNodeSetSignerTarget(t *testing.T) {
	const namespace, name = "default", "validator"
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8sappsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{controllers.LabelCosmosignerTarget: "nodeset-signer"},
		},
		Spec: appsv1.ChainNodeSpec{
			Validator:          &appsv1.ValidatorConfig{},
			RemoteSignerTarget: true,
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}

	var deleteRequests atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deleteRequests.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)),
			Request:    req,
		}, nil
	})
	clientSet, err := kubernetes.NewForConfig(&rest.Config{Host: "https://kubernetes.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
		ClientSet: clientSet,
		Scheme:    scheme,
		opts:      &controllers.ControllerRunOptions{},
	}

	pending, err := r.reconcileSigningConfigs(context.Background(), chainNode)
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("a ChainNodeSet-managed target has no standalone signer transition to wait for")
	}
	if got := deleteRequests.Load(); got != 0 {
		t.Fatalf("remote-signer child issued %d tmKMS delete requests before its pod transition, want 0", got)
	}
}

func TestCanCleanupTmKMSConfig(t *testing.T) {
	const namespace, name = "default", "validator"
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	tmKMSName := name + "-tmkms"

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{name: "no replacement pod", want: false},
		{
			name: "old pod still references tmKMS",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: corev1.PodSpec{Volumes: []corev1.Volume{
					{Name: "tmkms-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: tmKMSName},
					}}},
				}},
			},
			want: false,
		},
		{
			name: "old pod still references tmKMS identity",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: corev1.PodSpec{Volumes: []corev1.Volume{
					{Name: "tmkms-identity", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: tmKMSName,
					}}},
				}},
			},
			want: false,
		},
		{
			name: "replacement pod uses cosmosigner",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: "config"}}},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.pod != nil {
				builder = builder.WithObjects(tc.pod)
			}
			r := &Reconciler{Client: builder.Build()}

			got, err := r.canCleanupTmKMSConfig(context.Background(), chainNode)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("canCleanupTmKMSConfig() = %t, want %t", got, tc.want)
			}
		})
	}
}
