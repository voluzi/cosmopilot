package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func cosmoGuardTestReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, autoscalingv2.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))

	return &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{CosmoGuardImage: "ghcr.io/voluzi/cosmoguard:4.0.0-rc.7"},
	}
}

func guardedChainNode(name string, child bool) *appsv1.ChainNode {
	labels := map[string]string{}
	if child {
		labels[controllers.LabelChainNodeSet] = "some-set"
	}
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels, UID: types.UID(name + "-uid")},
		Spec: appsv1.ChainNodeSpec{
			Config: &appsv1.Config{
				CosmoGuard: &appsv1.CosmoGuardConfig{
					Enable: true,
					Config: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "rules"},
						Key:                  "cosmoguard.yaml",
					},
				},
			},
		},
	}
}

// TestStandaloneGuardCreatesStatefulSetAndService verifies a standalone ChainNode gets a clustered
// guard StatefulSet + client Service + headless peer Service + encryption Secret, pointed at its
// internal Service (static upstream).
func TestStandaloneGuardCreatesStatefulSetAndService(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, sts))

	env := sts.Spec.Template.Spec.Containers[0].Env
	found := false
	for _, e := range env {
		if e.Name == "COSMOGUARD_NODE_HOST" {
			found = true
			// Upstream is the ready-gated main node Service, not the not-ready-publishing "-internal".
			assert.Equal(t, "node-0.ns.svc.cluster.local", e.Value)
		}
	}
	assert.True(t, found, "static upstream host must be injected")

	svc := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, svc))

	// Headless peer Service + encryption Secret provisioned for the olric cluster.
	peer := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-peer"}, peer))
	assert.Equal(t, corev1.ClusterIPNone, peer.Spec.ClusterIP)

	secret := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-cluster"}, secret))
	assert.NotEmpty(t, secret.Data["encryptionKey"])
}

// TestStandaloneGuardInheritsServiceAccount verifies the standalone guard runs under the node's
// configured ServiceAccount (the in-pod sidecar inherited it; the standalone guard must carry it so
// SA-bound pull secrets / workload identity still apply).
func TestStandaloneGuardInheritsServiceAccount(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.ServiceAccountName = ptr.To("node-sa")
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, sts))
	assert.Equal(t, "node-sa", sts.Spec.Template.Spec.ServiceAccountName)
}

// TestStandaloneGuardInheritsUserLabels verifies the guard pods carry the node's genuine user labels
// (so NetworkPolicies / monitoring cover them) but not cosmopilot-managed selector labels.
func TestStandaloneGuardInheritsUserLabels(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Labels["team"] = "payments"             // user label -> propagated
	cn.Labels[controllers.LabelChainID] = "c1" // managed selector -> stripped
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, sts))
	labels := sts.Spec.Template.Labels
	assert.Equal(t, "payments", labels["team"], "user label propagated to guard pods")
	assert.NotContains(t, labels, controllers.LabelChainID, "managed selector label must not reach guard pods")
}

// servingGuard returns a guard StatefulSet reporting a ready replica (so IsServing is true).
func servingGuard(name string) *k8sappsv1.StatefulSet {
	return &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Generation: 1},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
		Status:     k8sappsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1},
	}
}

// TestAPIServiceName verifies ingress/gateway route targets, readiness-gated: a guarded node targets
// its own guard Service only once the guard is serving; otherwise (not serving, no individual guard,
// or disabled) it targets the raw node Service.
func TestAPIServiceName(t *testing.T) {
	ctx := context.Background()
	standalone := guardedChainNode("node-0", false)

	// Guarded but guard not yet serving -> raw (make-before-break).
	assert.Equal(t, "node-0", cosmoGuardTestReconciler(t).apiServiceName(ctx, standalone))
	// Guarded and serving -> guard.
	assert.Equal(t, "node-0-cosmoguard", cosmoGuardTestReconciler(t, servingGuard("node-0-cosmoguard")).apiServiceName(ctx, standalone))

	// Child with an individual ingress + serving guard -> its own guard.
	child := guardedChainNode("chain-fullnodes-0", true)
	child.Spec.Ingress = &appsv1.IngressConfig{Host: "0.rpc.example.com"}
	assert.Equal(t, "chain-fullnodes-0-cosmoguard", cosmoGuardTestReconciler(t, servingGuard("chain-fullnodes-0-cosmoguard")).apiServiceName(ctx, child))

	// Child without an individual ingress -> raw (fronted by the group guard).
	childNoIngress := guardedChainNode("chain-fullnodes-1", true)
	assert.Equal(t, "chain-fullnodes-1", cosmoGuardTestReconciler(t).apiServiceName(ctx, childNoIngress))

	// Disabled -> raw.
	unguarded := guardedChainNode("node-1", false)
	unguarded.Spec.Config.CosmoGuard.Enable = false
	assert.Equal(t, "node-1", cosmoGuardTestReconciler(t).apiServiceName(ctx, unguarded))
}

// TestStandaloneStickyFlipViaGrpcIngress verifies the sticky check inspects the separate "<node>-grpc"
// Ingress: for a gRPC-only exposure the base "<node>" Ingress carries no guard backend, so a guard
// that is momentarily not-serving during a rollout must still be recognized via the gRPC Ingress and
// keep its routes (rather than falling back to the raw node and bypassing CosmoGuard for gRPC).
func TestStandaloneStickyFlipViaGrpcIngress(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Ingress = &appsv1.IngressConfig{Host: "example.com", EnableGRPC: true}

	grpcIng := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{Name: "node-0-cosmoguard"},
							},
						}},
					},
				},
			}},
		},
	}

	// Guard not serving, but the gRPC Ingress already targets it -> sticky keeps routes on the guard.
	r := cosmoGuardTestReconciler(t, grpcIng)
	assert.Equal(t, "node-0-cosmoguard", r.apiServiceName(ctx, cn))
}

// guardIngress builds an Ingress named `name` whose single HTTP path points at `backend`.
func guardIngress(name, backend string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{Name: backend},
							},
						}},
					},
				},
			}},
		},
	}
}

// TestStandaloneRouteTargetsGuardChecksBothTypes verifies the sticky check inspects the old route type
// during an Ingress<->Gateway migration: a node whose Spec now points at Gateway but whose live guarded
// backend is still on the old Ingress is recognized as targeting the guard (so the flip stays sticky and
// the new routes aren't created on the raw Service before the old guarded ones are torn down).
func TestStandaloneRouteTargetsGuardChecksBothTypes(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Gateway = &appsv1.GatewayConfig{Host: "rpc.example.com"} // migrated to Gateway
	ing := guardIngress("node-0", "node-0-cosmoguard")               // old guarded Ingress still live

	r := cosmoGuardTestReconciler(t, ing)
	assert.True(t, r.standaloneRouteTargetsGuard(context.Background(), cn),
		"old guarded Ingress must keep the flip sticky even though Spec points at Gateway")
}

// TestFinalizeDefersUndeployWhileRouteTargetsGuard verifies the guard is not torn down while a live
// route still points at it (e.g. a Gateway migration whose routes could not be applied because the CRDs
// are missing, leaving the old guarded Ingress as a fallback), and is torn down once it no longer is.
func TestFinalizeDefersUndeployWhileRouteTargetsGuard(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, r.ensureCosmoGuard(ctx, cn))

	// Disable CosmoGuard, but a live Ingress still references the guard Service.
	cn.Spec.Config.CosmoGuard.Enable = false
	require.NoError(t, r.Create(ctx, guardIngress("node-0", "node-0-cosmoguard")))

	// Finalize must NOT delete the guard while that Ingress points at it.
	require.NoError(t, r.finalizeCosmoGuard(ctx, cn))
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &k8sappsv1.StatefulSet{}),
		"guard must survive while a live route still targets it")

	// Once the route no longer targets the guard, finalize tears it down.
	require.NoError(t, r.Delete(ctx, guardIngress("node-0", "node-0-cosmoguard")))
	require.NoError(t, r.finalizeCosmoGuard(ctx, cn))
	err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "guard torn down once no route references it")
}

// TestDisableAutoscalingRemovesHPA verifies the standalone guard deletes its HPA when autoscaling is
// turned off, so it stops driving the StatefulSet's replica count.
func TestDisableAutoscalingRemovesHPA(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Autoscaling = &appsv1.CosmoGuardAutoscalingConfig{Enable: true, MaxReplicas: 5}
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &autoscalingv2.HorizontalPodAutoscaler{}))

	cn.Spec.Config.CosmoGuard.Autoscaling.Enable = false
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &autoscalingv2.HorizontalPodAutoscaler{})
	assert.Error(t, err, "HPA should be removed when autoscaling is disabled")
}

// TestFinalizeTearsDownGuardWhenNodeBecomesChild verifies that moving a standalone guarded node into
// a ChainNodeSet removes its now-orphaned per-node guard on the next finalize.
func TestFinalizeTearsDownGuardWhenNodeBecomesChild(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &k8sappsv1.StatefulSet{}))

	// The node joins a ChainNodeSet; ensure no longer manages a guard and finalize tears the old one down.
	cn.Labels[controllers.LabelChainNodeSet] = "some-set"
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	require.NoError(t, r.finalizeCosmoGuard(context.Background(), cn))
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "standalone guard should be removed once the node is a ChainNodeSet child")
}

// TestChildWithIndividualIngressGetsGuard verifies a ChainNodeSet child that declares its own
// individual ingress gets a per-node guard, and its API routes target that guard — preserving the
// old sidecar behavior where individually-exposed nodes were guarded.
func TestChildWithIndividualIngressGetsGuard(t *testing.T) {
	cn := guardedChainNode("chain-fullnodes-0", true)
	cn.Spec.Ingress = &appsv1.IngressConfig{Host: "0.rpc.example.com"}
	r := cosmoGuardTestReconciler(t, cn)

	// The child manages its own guard (created here) even though it's a ChainNodeSet member.
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-0-cosmoguard"}, &k8sappsv1.StatefulSet{}))

	// Removing the individual ingress tears the per-node guard back down.
	cn.Spec.Ingress = nil
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	require.NoError(t, r.finalizeCosmoGuard(context.Background(), cn))
	assert.Equal(t, "chain-fullnodes-0", r.apiServiceName(context.Background(), cn))
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-0-cosmoguard"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "per-node guard should be removed once the individual ingress is gone")
}

// TestNodeSetChildSkipsStandaloneGuard verifies a ChainNodeSet child never creates its own guard
// (the group's guard, managed by the set, fronts it).
func TestNodeSetChildSkipsStandaloneGuard(t *testing.T) {
	cn := guardedChainNode("chain-fullnodes-0", true)
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	dep := &k8sappsv1.Deployment{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-0-cosmoguard"}, dep)
	assert.Error(t, err, "no standalone guard should be created for a nodeset child")
}

// TestDisableGuardUndeploys verifies disabling CosmoGuard removes the previously-created guard.
func TestDisableGuardUndeploys(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	// Confirm it was created first, then disable and reconcile again.
	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, sts))

	// Disable, then finalize (teardown runs after routes are retargeted, not in ensureCosmoGuard).
	cn.Spec.Config.CosmoGuard.Enable = false
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	require.NoError(t, r.finalizeCosmoGuard(context.Background(), cn))

	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "guard statefulset should be removed when disabled")
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-peer"}, &corev1.Service{})
	assert.Error(t, err, "peer service should be removed when disabled")
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-cluster"}, &corev1.Secret{})
	assert.Error(t, err, "encryption secret should be removed when disabled")
}
