package chainnode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func TestPeerPodPredicate_Create(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "pod with chain-id label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-1",
					Labels: map[string]string{
						controllers.LabelChainID: "test-chain",
					},
				},
			},
			want: true,
		},
		{
			name: "pod without chain-id label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "unrelated-pod",
					Labels: map[string]string{},
				},
			},
			want: false,
		},
		{
			name: "pod with no labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "no-labels-pod",
				},
			},
			want: false,
		},
	}

	p := peerPodPredicate{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := p.Create(event.CreateEvent{Object: tt.pod})
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestPeerPodPredicate_Create_NilObject(t *testing.T) {
	p := peerPodPredicate{}
	result := p.Create(event.CreateEvent{Object: nil})
	assert.False(t, result)
}

func TestPeerPodPredicate_Update(t *testing.T) {
	p := peerPodPredicate{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				controllers.LabelChainID: "test-chain",
			},
		},
	}

	// Update events should always return false — only create/delete matter for UID tracking
	result := p.Update(event.UpdateEvent{
		ObjectOld: pod,
		ObjectNew: pod,
	})
	assert.False(t, result)
}

func TestPeerPodPredicate_Delete(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "pod with chain-id label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-1",
					Labels: map[string]string{
						controllers.LabelChainID: "test-chain",
					},
				},
			},
			want: true,
		},
		{
			name: "pod without chain-id label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "unrelated-pod",
					Labels: map[string]string{},
				},
			},
			want: false,
		},
	}

	p := peerPodPredicate{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := p.Delete(event.DeleteEvent{Object: tt.pod})
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestPeerPodPredicate_Delete_NilObject(t *testing.T) {
	p := peerPodPredicate{}
	result := p.Delete(event.DeleteEvent{Object: nil})
	assert.False(t, result)
}

func TestPeerPodPredicate_Generic(t *testing.T) {
	p := peerPodPredicate{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				controllers.LabelChainID: "test-chain",
			},
		},
	}

	result := p.Generic(event.GenericEvent{Object: pod})
	assert.False(t, result)
}
