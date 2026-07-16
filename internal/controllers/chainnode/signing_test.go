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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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
