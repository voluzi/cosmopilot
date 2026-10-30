package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func TestNativeUploadDeniesBeforeArchiveOrStoreAccess(t *testing.T) {
	var storeRequests atomic.Int32
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storeRequests.Add(1)
		http.Error(w, "store must not be reached", 500)
	}))
	t.Cleanup(store.Close)
	t.Setenv("AWS_ENDPOINT_URL", store.URL)
	t.Setenv("AWS_ENDPOINT_URL_S3", store.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "invalid")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "invalid")
	for _, tc := range []struct {
		name, phase string
		ownerUID    string
		missing     bool
		cleanup     bool
		stopped     string
		deleting    bool
		wantErr     string
	}{
		{name: "stopped cleanup before pod selection", phase: "Succeeded", ownerUID: "owner", cleanup: true, stopped: "Failed", wantErr: "native authority denied"},
		{name: "acknowledged cleanup", phase: "Succeeded", ownerUID: "owner", cleanup: true, stopped: "Acknowledged", wantErr: "native authority denied"},
		{name: "abandoned cleanup", phase: "Succeeded", ownerUID: "owner", cleanup: true, stopped: "Abandoned", wantErr: "native authority denied"},
		{name: "deleting cleanup owner", phase: "Succeeded", ownerUID: "owner", cleanup: true, deleting: true, wantErr: "native authority denied"},
		{name: "terminal", phase: "Invalidated", ownerUID: "owner", wantErr: "native authority denied"},
		{name: "succeeded", phase: "Succeeded", ownerUID: "owner", wantErr: "native authority denied"},
		{name: "replaced owner", phase: "Uploading", ownerUID: "replacement", wantErr: "native authority denied"},
		{name: "missing attempt", ownerUID: "owner", wantErr: "native authority denied"},
		{name: "missing owner", missing: true, wantErr: "native authority GET denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority := appsv1.ChainNode{TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"}, ObjectMeta: metav1.ObjectMeta{Name: "authority", Namespace: "ns", UID: types.UID(tc.ownerUID)}, Status: appsv1.ChainNodeStatus{SnapshotExportAuthority: &appsv1.SnapshotExportAuthority{Version: 1, LastSequence: 1, Attempts: []appsv1.SnapshotExportAttempt{}}}}
			if tc.phase != "" {
				authority.Status.SnapshotExportAuthority.Attempts = append(authority.Status.SnapshotExportAuthority.Attempts, appsv1.SnapshotExportAttempt{ID: "attempt", Sequence: 1, Phase: tc.phase, CleanupState: tc.stopped})
			}
			if tc.deleting {
				now := metav1.Now()
				authority.DeletionTimestamp = &now
			}
			var authorityRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if version := r.URL.Query().Get("resourceVersion"); version != "" {
					t.Errorf("unexpected resourceVersion query: %q", version)
					http.Error(w, "authority reads must be uncached", http.StatusBadRequest)
					return
				}
				var body any
				switch r.URL.Path {
				case "/api":
					body = metav1.APIVersions{Versions: []string{"v1"}}
				case "/apis":
					body = metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: appsv1.GroupVersion.Group, Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: appsv1.GroupVersion.String(), Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: appsv1.GroupVersion.String(), Version: "v1"}}}}
				case "/apis/" + appsv1.GroupVersion.String():
					body = metav1.APIResourceList{GroupVersion: appsv1.GroupVersion.String(), APIResources: []metav1.APIResource{{Name: "chainnodes", SingularName: "chainnode", Namespaced: true, Kind: "ChainNode", Verbs: []string{"get"}}}}
				case "/apis/" + appsv1.GroupVersion.String() + "/namespaces/ns/chainnodes/authority":
					authorityRequests.Add(1)
					if tc.missing {
						w.WriteHeader(404)
						body = metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404}
					} else {
						body = authority
					}
				default:
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			t.Cleanup(server.Close)
			config := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(config, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n"), 0600))
			command := newNativeUploadCmd()
			if tc.cleanup {
				command = newNativeCleanupCmd()
			}
			command.SetArgs([]string{"--namespace", "ns", "--owner", "authority", "--owner-uid", "owner", "--attempt-id", "attempt", "--attempt-sequence", "1", "--pod-uid", "pod", "--kubeconfig", config})
			err := command.ExecuteContext(t.Context())
			require.Error(t, err)
			require.Positive(t, authorityRequests.Load())
			if tc.missing {
				require.ErrorContains(t, err, tc.wantErr)
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.EqualError(t, err, tc.wantErr)
			}
			require.False(t, strings.Contains(err.Error(), "stat directory"))
			require.Zero(t, storeRequests.Load())
		})
	}
}
