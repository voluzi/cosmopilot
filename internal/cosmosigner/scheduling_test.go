package cosmosigner

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSchedulingUnsetLifecycleDigest(t *testing.T) {
	p := testParams()
	got, err := p.LifecycleDigest("signing-digest")
	require.NoError(t, err)
	require.Equal(t, "73c536e656812302bf05e77bc5a8f5a75663ab8baf99d92856affc096e208f95", got)
	yaml, err := p.ConfigYAML()
	require.NoError(t, err)
	sts, err := p.StatefulSet(yaml)
	require.NoError(t, err)
	encoded, err := json.Marshal(sts.Spec.Template.Spec)
	require.NoError(t, err)
	var spec map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &spec))
	require.NotContains(t, spec, "nodeSelector")
	require.NotContains(t, spec, "affinity")
}

func TestSignerSchedulingOnlyReachesStatefulSet(t *testing.T) {
	p := testParams()
	p.NodeSelector = map[string]string{"pool": "signers"}
	p.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/instance": "mychain-signer"}}, TopologyKey: "kubernetes.io/hostname",
	}}}}
	yaml, err := p.ConfigYAML()
	require.NoError(t, err)
	sts, err := p.StatefulSet(yaml)
	require.NoError(t, err)
	require.Equal(t, p.NodeSelector, sts.Spec.Template.Spec.NodeSelector)
	require.Equal(t, p.Affinity, sts.Spec.Template.Spec.Affinity)
	runner := JobRunner{Params: p}
	for _, pod := range []*corev1.Pod{runner.buildImportPod("source"), runner.buildPubkeyPod("")} {
		require.Nil(t, pod.Spec.NodeSelector)
		require.Nil(t, pod.Spec.Affinity)
	}
}
