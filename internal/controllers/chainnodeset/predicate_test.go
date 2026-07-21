package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func TestGenerationChangedPredicateAllowsChainNodeSetDeletionTimestamp(t *testing.T) {
	p := GenerationChangedPredicate{}
	oldNodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "validators", Generation: 1}}
	newNodeSet := oldNodeSet.DeepCopy()
	now := metav1.Now()
	newNodeSet.DeletionTimestamp = &now

	require.True(t, p.Update(event.UpdateEvent{ObjectOld: oldNodeSet, ObjectNew: newNodeSet}))
}
