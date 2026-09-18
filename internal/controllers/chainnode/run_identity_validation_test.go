package chainnode

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

func TestValidateNodeUtilsRunIdentityRecordsInvalidEvent(t *testing.T) {
	recorder := record.NewFakeRecorder(1)
	r := &Reconciler{recorder: recorder}
	node := &appsv1.ChainNode{Spec: appsv1.ChainNodeSpec{Config: &appsv1.Config{
		SecurityContext:    &corev1.SecurityContext{},
		PodSecurityContext: &corev1.PodSecurityContext{},
	}}}

	err := r.validateNodeUtilsRunIdentity(node)

	require.ErrorContains(t, err, "runAsUser")
	select {
	case event := <-recorder.Events:
		require.Contains(t, event, appsv1.ReasonInvalid)
		require.Contains(t, event, "runAsUser")
	default:
		t.Fatal("invalid run identity did not emit a Warning event")
	}
}
