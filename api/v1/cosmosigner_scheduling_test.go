package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestWebhooksValidateCosmosignerScheduling(t *testing.T) {
	nodeAffinity := func(preferred bool, req corev1.NodeSelectorRequirement) *corev1.Affinity {
		term := corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{req}}
		a := &corev1.NodeAffinity{}
		if preferred {
			a.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.PreferredSchedulingTerm{{Weight: 1, Preference: term}}
		} else {
			a.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{term}}
		}
		return &corev1.Affinity{NodeAffinity: a}
	}
	nodeFieldAffinity := func(req corev1.NodeSelectorRequirement) *corev1.Affinity {
		a := nodeAffinity(false, req)
		term := &a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]
		term.MatchFields = term.MatchExpressions
		term.MatchExpressions = nil
		return a
	}
	podAffinity := func(anti, preferred bool, term corev1.PodAffinityTerm) *corev1.Affinity {
		a := &corev1.PodAffinity{}
		if preferred {
			a.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.WeightedPodAffinityTerm{{Weight: 1, PodAffinityTerm: term}}
		} else {
			a.RequiredDuringSchedulingIgnoredDuringExecution = []corev1.PodAffinityTerm{term}
		}
		if anti {
			return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution:  a.RequiredDuringSchedulingIgnoredDuringExecution,
				PreferredDuringSchedulingIgnoredDuringExecution: a.PreferredDuringSchedulingIgnoredDuringExecution,
			}}
		}
		return &corev1.Affinity{PodAffinity: a}
	}

	type schedulingCase struct {
		name         string
		nodeSelector map[string]string
		affinity     *corev1.Affinity
		field        string
	}
	cases := []schedulingCase{
		{name: "unset"},
		{name: "valid node selector", nodeSelector: map[string]string{"example.com/pool": "validators", "empty": ""}},
		{name: "invalid node selector key", nodeSelector: map[string]string{"bad key": "validators"}, field: "nodeSelector"},
		{name: "invalid node selector value", nodeSelector: map[string]string{"pool": "my pool"}, field: "nodeSelector"},
		{name: "valid node affinity", affinity: nodeAffinity(false, corev1.NodeSelectorRequirement{Key: "pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"validators"}})},
		{name: "valid numeric node affinity", affinity: nodeAffinity(true, corev1.NodeSelectorRequirement{Key: "generation", Operator: corev1.NodeSelectorOpGt, Values: []string{"2"}})},
		{name: "valid absent node label", affinity: nodeAffinity(false, corev1.NodeSelectorRequirement{Key: "maintenance", Operator: corev1.NodeSelectorOpDoesNotExist})},
		{name: "invalid required node affinity key", affinity: nodeAffinity(false, corev1.NodeSelectorRequirement{Key: "bad key", Operator: corev1.NodeSelectorOpExists}), field: "affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key"},
		{name: "invalid preferred node affinity operator", affinity: nodeAffinity(true, corev1.NodeSelectorRequirement{Key: "pool", Operator: "Unknown"}), field: "affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].preference.matchExpressions[0].operator"},
		{name: "invalid node affinity value", affinity: nodeAffinity(false, corev1.NodeSelectorRequirement{Key: "pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"my pool"}}), field: "matchExpressions[0].values"},
		{name: "missing node affinity values", affinity: nodeAffinity(false, corev1.NodeSelectorRequirement{Key: "pool", Operator: corev1.NodeSelectorOpIn}), field: "matchExpressions[0].values"},
		{name: "invalid numeric node affinity", affinity: nodeAffinity(true, corev1.NodeSelectorRequirement{Key: "generation", Operator: corev1.NodeSelectorOpGt, Values: []string{"two"}}), field: "matchExpressions[0].values"},
		{name: "valid node field selector", affinity: nodeFieldAffinity(corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}})},
		{name: "invalid node field operator", affinity: nodeFieldAffinity(corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: "Unknown"}), field: "matchFields[0].operator"},
		{name: "node affinity weight out of range", affinity: func() *corev1.Affinity {
			a := nodeAffinity(true, corev1.NodeSelectorRequirement{Key: "pool", Operator: corev1.NodeSelectorOpExists})
			a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight = 0
			return a
		}(), field: "affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight"},
		{name: "pod anti-affinity weight out of range", affinity: func() *corev1.Affinity {
			a := podAffinity(true, true, corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname"})
			a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight = 101
			return a
		}(), field: "affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight"},
		{name: "empty node field key", affinity: nodeFieldAffinity(corev1.NodeSelectorRequirement{Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}}), field: "matchFields[0].key"},
	}
	for _, anti := range []bool{false, true} {
		for _, preferred := range []bool{false, true} {
			name := "pod affinity"
			if anti {
				name = "pod anti-affinity"
			}
			if preferred {
				name += " preferred"
			} else {
				name += " required"
			}
			for _, tc := range []struct {
				name  string
				term  corev1.PodAffinityTerm
				field string
			}{
				{name: "valid", term: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cosmosigner"}}}},
				{name: "invalid selector value", term: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "bad value"}}}, field: "labelSelector"},
				{name: "invalid selector operator", term: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Unknown"}}}}, field: "labelSelector.matchExpressions[0].operator"},
				{name: "invalid namespace selector", term: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"bad key": "team"}}}, field: "namespaceSelector"},
				{name: "invalid match label key", term: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{}, MatchLabelKeys: []string{"bad key"}}, field: "matchLabelKeys[0]"},
				{name: "invalid mismatch label key", term: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{}, MismatchLabelKeys: []string{"bad key"}}, field: "mismatchLabelKeys[0]"},
				{name: "empty topology key", term: corev1.PodAffinityTerm{}, field: "topologyKey"},
				{name: "invalid topology key", term: corev1.PodAffinityTerm{TopologyKey: "bad key"}, field: "topologyKey"},
			} {
				cases = append(cases, schedulingCase{name: name + "/" + tc.name, affinity: podAffinity(anti, preferred, tc.term), field: tc.field})
			}
		}
	}

	for _, placement := range []string{"ChainNode", "ChainNodeSet top-level", "ChainNodeSet group"} {
		for _, tc := range cases {
			t.Run(placement+"/"+tc.name, func(t *testing.T) {
				signer := &Cosmosigner{NodeSelector: tc.nodeSelector, Affinity: tc.affinity, Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("signer-key")}}}
				app := AppSpec{Image: "ghcr.io/example/app", App: "appd"}
				genesis := &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")}
				var createErr, updateErr error
				path := ".spec.cosmosigner"
				if placement == "ChainNode" {
					node := &ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "sentry"}, Spec: ChainNodeSpec{App: app, Genesis: genesis, Cosmosigner: signer}}
					old := node.DeepCopy()
					old.Spec.Cosmosigner.NodeSelector = nil
					old.Spec.Cosmosigner.Affinity = nil
					_, createErr = node.ValidateCreate(context.Background(), node)
					_, updateErr = node.ValidateUpdate(context.Background(), old, node)
				} else {
					set := &ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain"}, Spec: ChainNodeSetSpec{App: app, Genesis: genesis, Nodes: []NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}}}}
					if placement == "ChainNodeSet group" {
						set.Spec.Nodes[0].Cosmosigner = signer
						path = ".spec.nodes[0].cosmosigner"
					} else {
						signer.NodeGroups = []string{"sentries"}
						set.Spec.Cosmosigner = signer
					}
					old := set.DeepCopy()
					for _, s := range old.ResolveCosmosigners() {
						s.Spec.NodeSelector = nil
						s.Spec.Affinity = nil
					}
					_, createErr = set.ValidateCreate(context.Background(), set)
					_, updateErr = set.ValidateUpdate(context.Background(), old, set)
				}
				for _, err := range []error{createErr, updateErr} {
					if tc.field == "" {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, tc.field)
						require.ErrorContains(t, err, path)
					}
				}
			})
		}
	}
}
