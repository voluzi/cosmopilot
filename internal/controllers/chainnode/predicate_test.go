package chainnode

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestGenerationChangedPredicateIgnoresCosmosignerJobPods(t *testing.T) {
	p := GenerationChangedPredicate{}

	for _, suffix := range []string{"pubkey", "import"} {
		t.Run(suffix, func(t *testing.T) {
			oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-signer-" + suffix}}
			newPod := oldPod.DeepCopy()
			newPod.ResourceVersion = "2"

			require.False(t, p.Create(event.CreateEvent{Object: newPod}))
			require.False(t, p.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
			require.False(t, p.Delete(event.DeleteEvent{Object: newPod}))
		})
	}
}

func TestGenerationChangedPredicateKeepsMainPodsEndingInJobSuffixes(t *testing.T) {
	p := GenerationChangedPredicate{}

	for _, suffix := range []string{"pubkey", "import"} {
		t.Run(suffix, func(t *testing.T) {
			oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-" + suffix}}
			newPod := oldPod.DeepCopy()
			newPod.ResourceVersion = "2"

			require.True(t, p.Create(event.CreateEvent{Object: newPod}))
			require.True(t, p.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
			require.True(t, p.Delete(event.DeleteEvent{Object: newPod}))
		})
	}
}
