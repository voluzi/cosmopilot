package chainnode

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalRequeueResultForDeferredPodRecreation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		period    string
		deferred  bool
		wantDelay time.Duration
	}{
		{name: "long period is capped", period: "2h", deferred: true, wantDelay: 15 * time.Second},
		{name: "zero period still retries", period: "0s", deferred: true, wantDelay: 15 * time.Second},
		{name: "short period stays short", period: "3s", deferred: true, wantDelay: 3 * time.Second},
		{name: "normal long period unchanged", period: "2h", wantDelay: 2 * time.Hour},
		{name: "normal zero period unchanged", period: "0s", wantDelay: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{Spec: appsv1.ChainNodeSpec{Config: &appsv1.Config{ReconcilePeriod: &tt.period}}}
			if tt.deferred {
				node.Status.Conditions = []metav1.Condition{{Type: appsv1.ConditionPodRecreationDeferred,
					Status: metav1.ConditionTrue, Reason: appsv1.ReasonDisruptionLockBusy, LastTransitionTime: metav1.Now()}}
			}
			require.Equal(t, tt.wantDelay, normalRequeueResult(node).RequeueAfter)
		})
	}
}
