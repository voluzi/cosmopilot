package tmkms

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUndeployConfigDeletesSecretWhenConfigMapIsMissing(t *testing.T) {
	var secretDeletes atomic.Int32
	kms := testKMSForCleanup(t, func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/secrets/") {
			secretDeletes.Add(1)
			return cleanupResponse(req, http.StatusOK, `{}`), nil
		}
		return cleanupResponse(req, http.StatusNotFound, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`), nil
	})

	if err := kms.UndeployConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := secretDeletes.Load(); got != 1 {
		t.Fatalf("tmKMS identity Secret delete requests = %d, want 1", got)
	}
}

func TestUndeployConfigAttemptsSecretDeleteAfterConfigMapError(t *testing.T) {
	var secretDeletes atomic.Int32
	kms := testKMSForCleanup(t, func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/secrets/") {
			secretDeletes.Add(1)
			return cleanupResponse(req, http.StatusOK, `{}`), nil
		}
		return cleanupResponse(req, http.StatusInternalServerError, `{"kind":"Status","status":"Failure","reason":"InternalError","message":"forced ConfigMap delete failure","code":500}`), nil
	})

	err := kms.UndeployConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "forced ConfigMap delete failure") {
		t.Fatalf("UndeployConfig() error = %v, want forced ConfigMap delete failure", err)
	}
	if got := secretDeletes.Load(); got != 1 {
		t.Fatalf("tmKMS identity Secret delete requests = %d, want 1", got)
	}
}

func TestHashicorpProviderUsesPinnedVaultTokenRenewerImage(t *testing.T) {
	provider := HashicorpProvider{
		Adapter:        &HashicorpAdapter{},
		TokenSecret:    &corev1.SecretKeySelector{Key: "token"},
		AutoRenewToken: true,
	}

	containers := provider.getContainers()
	if len(containers) != 1 {
		t.Fatalf("renewer containers = %d, want 1", len(containers))
	}
	want := "ghcr.io/voluzi/vault-renewer:1.0.0@sha256:55532cbf4c7a7c5038e1b7cf759fa5748216075719afde776226a4025cb8e579"
	if got := containers[0].Image; got != want {
		t.Fatalf("renewer image = %q, want %q", got, want)
	}
}

func testKMSForCleanup(t *testing.T, transport roundTripperFunc) *KMS {
	t.Helper()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: "https://kubernetes.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
	return New(client, runtime.NewScheme(), "validator-tmkms", owner)
}

func cleanupResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}
