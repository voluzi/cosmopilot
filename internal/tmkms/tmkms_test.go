package tmkms

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/voluzi/cosmopilot/v3/pkg/images"
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
	want := images.DefaultVaultTokenRenewerImage
	if got := containers[0].Image; got != want {
		t.Fatalf("renewer image = %q, want %q", got, want)
	}
}

func TestImageOptionsPreserveDefaultsAndApplyOverrides(t *testing.T) {
	t.Run("tmkms", func(t *testing.T) {
		cfg := defaultConfig()
		WithImage("")(cfg)
		if got := cfg.Image; got != DefaultTmKmsImage {
			t.Fatalf("empty override image = %q, want %q", got, DefaultTmKmsImage)
		}

		WithImage("registry.example.com/tmkms@sha256:abcdef")(cfg)
		if got := cfg.Image; got != "registry.example.com/tmkms@sha256:abcdef" {
			t.Fatalf("configured image = %q", got)
		}
	})

	t.Run("vault-token-renewer", func(t *testing.T) {
		defaultProvider := NewHashicorpProvider(
			"chain", "https://vault.example.com", "key",
			&corev1.SecretKeySelector{Key: "token"}, nil, true, false,
			WithTokenRenewerImage(""),
		).(*HashicorpProvider)
		if got := defaultProvider.getContainers()[0].Image; got != images.DefaultVaultTokenRenewerImage {
			t.Fatalf("empty override renewer image = %q, want %q", got, images.DefaultVaultTokenRenewerImage)
		}

		provider := NewHashicorpProvider(
			"chain", "https://vault.example.com", "key",
			&corev1.SecretKeySelector{Key: "token"}, nil, true, false,
			WithTokenRenewerImage("registry.example.com/renewer@sha256:abcdef"),
		).(*HashicorpProvider)

		containers := provider.getContainers()
		if got := containers[0].Image; got != "registry.example.com/renewer@sha256:abcdef" {
			t.Fatalf("configured renewer image = %q", got)
		}
	})
}

func TestHelperPodsUseDefensivelyClonedImagePullSecrets(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "validator-uid",
	}}
	pullSecrets := []corev1.LocalObjectReference{{Name: "registry-creds"}}
	kms := New(nil, scheme, "validator-tmkms", owner, WithImagePullSecrets(pullSecrets))
	pullSecrets[0].Name = "mutated-input"
	if got := kms.Config.ImagePullSecrets[0].Name; got != "registry-creds" {
		t.Fatalf("config pull secret = %q, want cloned input", got)
	}

	identityPod, err := kms.identityGenerationPod()
	if err != nil {
		t.Fatal(err)
	}
	provider := NewHashicorpProvider(
		"chain", "https://vault.example.com", "key",
		&corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
			Key:                  "token",
		},
		nil, false, false,
	).(*HashicorpProvider)
	uploadPod, err := provider.uploadKeyPod(kms, "key-material")
	if err != nil {
		t.Fatal(err)
	}

	kms.Config.ImagePullSecrets[0].Name = "mutated-config"
	want := []corev1.LocalObjectReference{{Name: "registry-creds"}}
	if got := identityPod.Spec.ImagePullSecrets; !reflect.DeepEqual(got, want) {
		t.Fatalf("identity pod pull secrets = %#v, want %#v", got, want)
	}
	if got := uploadPod.Spec.ImagePullSecrets; !reflect.DeepEqual(got, want) {
		t.Fatalf("upload pod pull secrets = %#v, want %#v", got, want)
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
