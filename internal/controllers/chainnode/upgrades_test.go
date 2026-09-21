package chainnode

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func TestGetUpgrade(t *testing.T) {
	r := &Reconciler{}

	tests := []struct {
		name      string
		chainNode *appsv1.ChainNode
		height    int64
		want      *appsv1.Upgrade
	}{
		{
			name: "finds matching scheduled upgrade",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
						{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
						{Height: 300, Image: "myapp:v3", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			height: 200,
			want:   &appsv1.Upgrade{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
		},
		{
			name: "no matching upgrade",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
						{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			height: 150,
			want:   nil,
		},
		{
			name: "empty upgrades list",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{},
				},
			},
			height: 100,
			want:   nil,
		},
		{
			name: "finds scheduled upgrade at height",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			height: 100,
			want:   &appsv1.Upgrade{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
		},
		{
			name: "ignores completed upgrades",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeCompleted},
						{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			height: 100,
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.getUpgrade(tt.chainNode, tt.height)
			if tt.want == nil {
				if got != nil {
					t.Errorf("getUpgrade() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Errorf("getUpgrade() = nil, want %v", tt.want)
				return
			}
			if got.Height != tt.want.Height || got.Image != tt.want.Image || got.Status != tt.want.Status {
				t.Errorf("getUpgrade() = {Height: %d, Image: %s, Status: %s}, want {Height: %d, Image: %s, Status: %s}",
					got.Height, got.Image, got.Status, tt.want.Height, tt.want.Image, tt.want.Status)
			}
		})
	}
}

func TestCompleteUpgradePersistsStatusBeforeResettingVPA(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	node := upgradePersistenceTestNode()
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node).Build()
	tracking := &upgradePersistenceClient{Client: base}
	r := &Reconciler{Client: tracking, Scheme: scheme}
	current := &appsv1.ChainNode{}
	require.NoError(t, tracking.Get(t.Context(), client.ObjectKeyFromObject(node), current))

	require.NoError(t, r.completeUpgrade(t.Context(), current, &current.Status.Upgrades[0]))
	assert.Equal(t, []string{"status", "metadata"}, tracking.chainNodeWrites)

	stored := &appsv1.ChainNode{}
	require.NoError(t, tracking.Get(t.Context(), client.ObjectKeyFromObject(node), stored))
	assert.Equal(t, "v2", stored.Status.AppVersion)
	assert.Equal(t, appsv1.UpgradeCompleted, stored.Status.Upgrades[0].Status)
	assert.NotContains(t, stored.Annotations, controllers.AnnotationVPAResources)
	assert.NotEmpty(t, stored.Annotations[controllers.AnnotationVPALastCPUScale])
	assert.NotEmpty(t, stored.Annotations[controllers.AnnotationVPALastMemoryScale])
	stored.Status.LatestHeight = 99
	assert.Equal(t, "v2", stored.GetAppVersion())
}

func TestCompleteUpgradeDoesNotResetVPAWhenStatusPersistenceFails(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	node := upgradePersistenceTestNode()
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node).Build()
	tracking := &upgradePersistenceClient{Client: base, failStatus: true}
	r := &Reconciler{Client: tracking, Scheme: scheme}
	current := &appsv1.ChainNode{}
	require.NoError(t, tracking.Get(t.Context(), client.ObjectKeyFromObject(node), current))

	err := r.completeUpgrade(t.Context(), current, &current.Status.Upgrades[0])
	require.ErrorContains(t, err, "status persistence failed")
	assert.Equal(t, []string{"status"}, tracking.chainNodeWrites)
	assert.Contains(t, current.Annotations, controllers.AnnotationVPAResources)
	assert.NotContains(t, current.Annotations, controllers.AnnotationVPALastCPUScale)
	assert.NotContains(t, current.Annotations, controllers.AnnotationVPALastMemoryScale)
}

func upgradePersistenceTestNode() *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node", Namespace: "default", UID: "node-uid",
			Annotations: map[string]string{controllers.AnnotationVPAResources: `{}`},
		},
		Spec: appsv1.ChainNodeSpec{
			App: appsv1.AppSpec{Image: "app"},
			VPA: &appsv1.VerticalAutoscalingConfig{Enabled: true, ResetVpaAfterNodeUpgrade: true},
		},
		Status: appsv1.ChainNodeStatus{
			LatestHeight: 100,
			AppVersion:   "v1",
			Upgrades: []appsv1.Upgrade{{
				Height: 100, Image: "app:v2", Status: appsv1.UpgradeOnGoing,
			}},
		},
	}
}

type upgradePersistenceClient struct {
	client.Client
	chainNodeWrites []string
	failStatus      bool
}

func (c *upgradePersistenceClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*appsv1.ChainNode); ok {
		c.chainNodeWrites = append(c.chainNodeWrites, "metadata")
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *upgradePersistenceClient) Status() client.StatusWriter {
	return &upgradePersistenceStatusWriter{StatusWriter: c.Client.Status(), client: c}
}

type upgradePersistenceStatusWriter struct {
	client.StatusWriter
	client *upgradePersistenceClient
}

func (w *upgradePersistenceStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*appsv1.ChainNode); ok {
		w.client.chainNodeWrites = append(w.client.chainNodeWrites, "status")
		if w.client.failStatus {
			return errors.New("status persistence failed")
		}
	}
	return w.StatusWriter.Update(ctx, obj, opts...)
}

func TestAddOrUpdateUpgrade(t *testing.T) {
	tests := []struct {
		name          string
		upgrades      []appsv1.Upgrade
		upgrade       appsv1.Upgrade
		currentHeight int64
		wantLen       int
	}{
		{
			name:          "add new upgrade to empty list",
			upgrades:      []appsv1.Upgrade{},
			upgrade:       appsv1.Upgrade{Height: 100, Image: "myapp:v1"},
			currentHeight: 50,
			wantLen:       1,
		},
		{
			name: "add new upgrade to existing list",
			upgrades: []appsv1.Upgrade{
				{Height: 100, Image: "myapp:v1"},
			},
			upgrade:       appsv1.Upgrade{Height: 200, Image: "myapp:v2"},
			currentHeight: 50,
			wantLen:       2,
		},
		{
			name: "update existing upgrade with ImageMissing status",
			upgrades: []appsv1.Upgrade{
				{Height: 100, Image: "", Status: appsv1.UpgradeImageMissing},
			},
			upgrade:       appsv1.Upgrade{Height: 100, Image: "new:v1", Source: appsv1.OnChainUpgrade},
			currentHeight: 50,
			wantLen:       1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AddOrUpdateUpgrade(tt.upgrades, tt.upgrade, tt.currentHeight)
			if len(result) != tt.wantLen {
				t.Errorf("AddOrUpdateUpgrade() returned %d upgrades, want %d", len(result), tt.wantLen)
			}
		})
	}
}
