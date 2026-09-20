package k8s

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestDownloadGenesisWaitsForPreviousPodDeletionBeforeCreate(t *testing.T) {
	requests := make([]string, 0, 3)
	var deleteGrace, podGrace *int64
	httpClient := &http.Client{Transport: pvcRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method)
		status := http.StatusOK
		body := `{"kind":"Status","apiVersion":"v1","status":"Success"}`
		switch req.Method {
		case http.MethodDelete:
			options := &metav1.DeleteOptions{}
			if err := json.NewDecoder(req.Body).Decode(options); err != nil {
				return nil, err
			}
			deleteGrace = options.GracePeriodSeconds
		case http.MethodGet:
			status = http.StatusNotFound
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		case http.MethodPost:
			pod := &corev1.Pod{}
			if err := json.NewDecoder(req.Body).Decode(pod); err != nil {
				return nil, err
			}
			podGrace = pod.Spec.TerminationGracePeriodSeconds
			status = http.StatusConflict
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","code":409}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	config := &rest.Config{Host: "https://kubernetes.invalid", ContentConfig: rest.ContentConfig{ContentType: "application/json"}}
	clientSet, err := kubernetes.NewForConfigAndClient(config, httpClient)
	require.NoError(t, err)
	helper := NewPvcHelper(clientSet, config, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "default",
	}}, "", nil)

	err = helper.DownloadGenesis(t.Context(), "https://example.invalid/genesis.json", "config/genesis.json", nil, "", nil, nil)
	require.Error(t, err)
	assert.Equal(t, []string{http.MethodDelete, http.MethodGet, http.MethodPost}, requests)
	require.NotNil(t, deleteGrace)
	assert.Positive(t, *deleteGrace)
	require.NotNil(t, podGrace)
	assert.Equal(t, *deleteGrace, *podGrace)
}

type pvcRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f pvcRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
