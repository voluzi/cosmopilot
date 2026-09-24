package chainnodeset

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/controllers"
)

const (
	seedTestNodeID  = "0123456789abcdef0123456789abcdef01234567"
	seedTestNodeID2 = "89abcdef0123456789abcdef0123456789abcdef"
)

func TestSeedExternalAddressesOnlyWhenEverySeedHasOne(t *testing.T) {
	complete := []string{seedTestNodeID + "@1.2.3.4:26656", seedTestNodeID2 + "@5.6.7.8:26656"}
	assert.Equal(t, []string{"1.2.3.4:26656", "5.6.7.8:26656"}, seedExternalAddresses(complete))

	// A seed still waiting for its address must not shift the others onto the wrong ordinal or
	// leave a later ordinal without an entry.
	assert.Nil(t, seedExternalAddresses([]string{"", complete[1]}))
	assert.Nil(t, seedExternalAddresses([]string{complete[0], ""}))
	assert.Nil(t, seedExternalAddresses(nil))
}

func TestSeedStatefulSetAdvertisesOneAddressPerOrdinal(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	r := &Reconciler{Scheme: scheme, opts: &controllers.ControllerRunOptions{}}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"},
		Spec:       appsv1.ChainNodeSetSpec{Cosmoseed: &appsv1.CosmoseedConfig{Instances: ptr.To(2)}},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "chain-1"},
	}
	externalAddress := func(addresses []string) string {
		sts, err := r.getStatefulSet(nodeSet, "hash", seedExternalAddresses(addresses))
		require.NoError(t, err)
		for _, env := range sts.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "EXTERNAL_ADDRESS" {
				return env.Value
			}
		}
		t.Fatal("EXTERNAL_ADDRESS not set")
		return ""
	}

	assert.Equal(t, "1.2.3.4:26656,5.6.7.8:26656",
		externalAddress([]string{seedTestNodeID + "@1.2.3.4:26656", seedTestNodeID2 + "@5.6.7.8:26656"}))
	assert.Empty(t, externalAddress([]string{"", seedTestNodeID2 + "@5.6.7.8:26656"}))
}

func TestDialableSeedPeersDropsPeersCosmoseedCannotDial(t *testing.T) {
	peers := appsv1.PeerList{
		{ID: seedTestNodeID, Address: "node-0", Port: ptr.To(26656)},
		{ID: "", Address: "node-1"},
		{ID: "not-hex", Address: "node-2"},
		{ID: seedTestNodeID[:38], Address: "node-3"},
		{ID: seedTestNodeID2, Address: " "},
		{ID: seedTestNodeID2, Address: "node-5", Port: ptr.To(0)},
		{ID: seedTestNodeID2, Address: "node-6", Port: ptr.To(70000)},
		{ID: seedTestNodeID2, Address: "public.example.com", Port: ptr.To(443)},
	}
	assert.Equal(t,
		seedTestNodeID+"@node-0:26656,"+seedTestNodeID2+"@public.example.com:443",
		dialableSeedPeers(peers).String())
}

// End to end through the ConfigMap the seeds read: a peer Service of a node that has not reported its
// ID yet must not reach cosmoseed's seeds.
func TestCosmoseedConfigSkipsUndialablePeers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	peerService := func(name, nodeID string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{
			controllers.LabelChainID: "chain-1",
			controllers.LabelPeer:    controllers.StringValueTrue,
			controllers.LabelNodeID:  nodeID,
		}}}
	}
	r := &Reconciler{
		Client: fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(
			peerService("ready-node", seedTestNodeID),
			peerService("new-node", ""),
		).Build(),
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"},
		Spec:       appsv1.ChainNodeSetSpec{Cosmoseed: &appsv1.CosmoseedConfig{Instances: ptr.To(1)}},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "chain-1"},
	}

	_, configMap, err := r.getCosmoseedConfigMap(context.Background(), nodeSet)
	require.NoError(t, err)
	var cfg struct {
		Seeds string `yaml:"seeds"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(configMap.Data[cosmoseedConfigFileName]), &cfg))
	assert.Equal(t, seedTestNodeID+"@ready-node:26656", cfg.Seeds)
	assert.False(t, strings.Contains(cfg.Seeds, "new-node"))
}
