package cosmoguard

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func baseParams() Params {
	return Params{
		Name:      "chain-group-cosmoguard",
		Namespace: "ns",
		Image:     "ghcr.io/voluzi/cosmoguard:4.0.0-rc.7",
		Replicas:  2,
		ConfigMap: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "rules"},
			Key:                  "cosmoguard.yaml",
		},
		PeerServiceName:     PeerServiceName("chain-group-cosmoguard"),
		EncryptionKeySecret: EncryptionKeySecretName("chain-group-cosmoguard"),
		Labels: map[string]string{
			controllers.LabelChainNodeSet:      "chain",
			controllers.LabelChainNodeSetGroup: "group",
		},
	}
}

func envMap(env []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		m[e.Name] = e
	}
	return m
}

func TestStatefulSet_StaticUpstream(t *testing.T) {
	p := baseParams()
	p.UpstreamHost = "chain-0-internal.ns.svc.cluster.local"

	dep := p.StatefulSet()

	// Selector is the immutable instance labels only.
	assert.Equal(t, InstanceLabels(p.Name), dep.Spec.Selector.MatchLabels)
	// Routing labels are carried on the pod template so group Services can select the pods.
	assert.Equal(t, "group", dep.Spec.Template.Labels[controllers.LabelChainNodeSetGroup])
	assert.Equal(t, appName, dep.Spec.Template.Labels[labelAppName])

	require.Len(t, dep.Spec.Template.Spec.Containers, 1)
	c := dep.Spec.Template.Spec.Containers[0]

	env := envMap(c.Env)
	assert.Equal(t, "chain-0-internal.ns.svc.cluster.local", env["COSMOGUARD_NODE_HOST"].Value)
	assert.NotContains(t, env, "COSMOGUARD_DISCOVERY_HOST")
	assert.Equal(t, "true", env["COSMOGUARD_METRICS_ENABLE"].Value)

	// Read-only rootfs + config mounted as a whole volume (no subPath) for hot-reload.
	require.NotNil(t, c.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	require.Len(t, c.VolumeMounts, 1)
	assert.Empty(t, c.VolumeMounts[0].SubPath)
	assert.Equal(t, []string{"--config", "/config/cosmoguard.yaml"}, c.Args)

	// Replicas managed directly when no autoscaling.
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(2), *dep.Spec.Replicas)
}

func TestStatefulSet_DiscoveryUpstream(t *testing.T) {
	p := baseParams()
	p.DiscoveryHost = "chain-group-cosmoguard-upstream.ns.svc.cluster.local"

	env := envMap(p.StatefulSet().Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "chain-group-cosmoguard-upstream.ns.svc.cluster.local", env["COSMOGUARD_DISCOVERY_HOST"].Value)
	assert.Equal(t, "dns", env["COSMOGUARD_DISCOVERY_TYPE"].Value)
	assert.NotContains(t, env, "COSMOGUARD_NODE_HOST")
}

func TestStatefulSet_EVMAndDashboard(t *testing.T) {
	p := baseParams()
	p.UpstreamHost = "host"
	p.EvmEnabled = true
	p.Dashboard = &DashboardParams{
		Port:         8080,
		AuthUser:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "user"},
		AuthPassword: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "pass"},
	}

	c := p.StatefulSet().Spec.Template.Spec.Containers[0]
	env := envMap(c.Env)
	assert.Equal(t, "true", env["COSMOGUARD_ENABLE_EVM"].Value)
	assert.Equal(t, "true", env["COSMOGUARD_DASHBOARD_ENABLE"].Value)
	assert.Equal(t, "8080", env["COSMOGUARD_DASHBOARD_PORT"].Value)
	// Credentials come from a Secret, never inlined.
	require.NotNil(t, env["COSMOGUARD_DASHBOARD_AUTH_USER"].ValueFrom)
	assert.Equal(t, "user", env["COSMOGUARD_DASHBOARD_AUTH_USER"].ValueFrom.SecretKeyRef.Key)
	assert.Empty(t, env["COSMOGUARD_DASHBOARD_AUTH_USER"].Value)

	portNames := map[string]bool{}
	for _, prt := range c.Ports {
		portNames[prt.Name] = true
	}
	assert.True(t, portNames[controllers.CosmoGuardEvmRpcPortName])
	assert.True(t, portNames["dashboard"])
}

func TestStatefulSet_AutoscalingLeavesReplicasUnset(t *testing.T) {
	p := baseParams()
	p.UpstreamHost = "host"
	p.Autoscaling = &AutoscalingParams{MaxReplicas: 5, TargetCPU: ptr.To[int32](80)}

	dep := p.StatefulSet()
	assert.Nil(t, dep.Spec.Replicas, "HPA owns replicas; deployment must not set them")
}

func TestStatefulSet_ClusterConfig(t *testing.T) {
	p := baseParams()
	p.UpstreamHost = "host"
	sts := p.StatefulSet()

	assert.Equal(t, PeerServiceName(p.Name), sts.Spec.ServiceName)
	assert.Equal(t, appsv1.ParallelPodManagement, sts.Spec.PodManagementPolicy)
	assert.Empty(t, sts.Spec.VolumeClaimTemplates, "cosmoguard is in-memory; no PVCs")

	env := envMap(sts.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "true", env["COSMOGUARD_CLUSTER_ENABLE"].Value)
	assert.Equal(t, "dns", env["COSMOGUARD_CLUSTER_DISCOVERY_MODE"].Value)
	assert.Equal(t, "chain-group-cosmoguard-peer.ns.svc.cluster.local", env["COSMOGUARD_CLUSTER_DISCOVERY_DNS_HOST"].Value)
	require.NotNil(t, env["COSMOGUARD_CLUSTER_BIND_ADDR"].ValueFrom)
	assert.Equal(t, "status.podIP", env["COSMOGUARD_CLUSTER_BIND_ADDR"].ValueFrom.FieldRef.FieldPath)
	require.NotNil(t, env["COSMOGUARD_CLUSTER_ENCRYPTION_KEY"].ValueFrom)
	assert.Equal(t, EncryptionKeySecretName(p.Name), env["COSMOGUARD_CLUSTER_ENCRYPTION_KEY"].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, EncryptionKeySecretKey, env["COSMOGUARD_CLUSTER_ENCRYPTION_KEY"].ValueFrom.SecretKeyRef.Key)
}

func TestPeerService(t *testing.T) {
	svc := baseParams().PeerService()
	assert.Equal(t, PeerServiceName("chain-group-cosmoguard"), svc.Name)
	assert.Equal(t, corev1.ClusterIPNone, svc.Spec.ClusterIP)
	assert.True(t, svc.Spec.PublishNotReadyAddresses)

	var tcp, udp bool
	for _, sp := range svc.Spec.Ports {
		if sp.Port == clusterGossipPort && sp.Protocol == corev1.ProtocolTCP {
			tcp = true
		}
		if sp.Port == clusterGossipPort && sp.Protocol == corev1.ProtocolUDP {
			udp = true
		}
	}
	assert.True(t, tcp, "gossip TCP port exposed")
	assert.True(t, udp, "gossip UDP port exposed")
}

func TestSelectsGuard(t *testing.T) {
	assert.True(t, SelectsGuard(InstanceLabels("chain-group-cosmoguard")))
	assert.True(t, SelectsGuard(AppLabel()))
	assert.False(t, SelectsGuard(map[string]string{"nodeset": "chain", "group": "fullnodes"}))
	assert.False(t, SelectsGuard(nil))
}

func TestGenerateEncryptionKey(t *testing.T) {
	k, err := GenerateEncryptionKey()
	require.NoError(t, err)
	b, err := base64.StdEncoding.DecodeString(k)
	require.NoError(t, err)
	assert.Len(t, b, 32, "olric key must be 32 bytes")
}

func TestService_Ports(t *testing.T) {
	p := baseParams()
	p.UpstreamHost = "host"
	p.EvmEnabled = true

	svc := p.Service()
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, InstanceLabels(p.Name), svc.Spec.Selector)

	byName := map[string]corev1.ServicePort{}
	for _, sp := range svc.Spec.Ports {
		byName[sp.Name] = sp
	}
	// Public port numbers preserved; target the guard listener ports.
	assert.Equal(t, int32(chainutils.RpcPort), byName[chainutils.RpcPortName].Port)
	assert.Equal(t, int32(controllers.CosmoGuardRpcPort), byName[chainutils.RpcPortName].TargetPort.IntVal)
	assert.Equal(t, int32(controllers.EvmRpcPort), byName[controllers.EvmRpcPortName].Port)
	assert.Equal(t, int32(controllers.CosmoGuardEvmRpcPort), byName[controllers.EvmRpcPortName].TargetPort.IntVal)
}

func TestHPA(t *testing.T) {
	p := baseParams()
	p.UpstreamHost = "host"
	assert.Nil(t, p.HPA(), "no HPA without autoscaling")

	p.Autoscaling = &AutoscalingParams{
		MinReplicas:  ptr.To[int32](2),
		MaxReplicas:  8,
		TargetCPU:    ptr.To[int32](75),
		TargetMemory: ptr.To[int32](70),
	}
	hpa := p.HPA()
	require.NotNil(t, hpa)
	assert.Equal(t, p.Name, hpa.Spec.ScaleTargetRef.Name)
	assert.Equal(t, int32(8), hpa.Spec.MaxReplicas)
	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas)
	assert.Len(t, hpa.Spec.Metrics, 2)
}
