package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func TestGenerationChangedPredicateAllowsChainNodeSetDeletionTimestamp(t *testing.T) {
	p := GenerationChangedPredicate{}
	oldNodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "validators", Generation: 1}}
	newNodeSet := oldNodeSet.DeepCopy()
	now := metav1.Now()
	newNodeSet.DeletionTimestamp = &now

	require.True(t, p.Update(event.UpdateEvent{ObjectOld: oldNodeSet, ObjectNew: newNodeSet}))
}

func TestGenerationChangedPredicateAllowsChainNodeDeletionTimestamp(t *testing.T) {
	p := GenerationChangedPredicate{}
	oldNode := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Generation: 1}}
	newNode := oldNode.DeepCopy()
	now := metav1.Now()
	newNode.DeletionTimestamp = &now

	require.True(t, p.Update(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: newNode}))
}

func TestGenerationChangedPredicateAllowsConfigMapsWithTemporaryPodNames(t *testing.T) {
	p := GenerationChangedPredicate{}
	for _, name := range []string{"rules-data-init", "rules-genesis-init", "rules-write-file", "rules-config-generator"} {
		t.Run(name, func(t *testing.T) {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}}
			changed := cm.DeepCopy()
			changed.Data = map[string]string{"cosmoguard.yaml": "auth: {enable: true}"}
			require.True(t, p.Create(event.CreateEvent{Object: cm}))
			require.True(t, p.Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: changed}))
			require.True(t, p.Delete(event.DeleteEvent{Object: cm}))
		})
	}
}
