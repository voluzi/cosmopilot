package chainnode

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmoguard"
)

// guardPriorityClassName returns the scheduling priority for a standalone guard: the validators'
// class for a validator node, the nodes' class otherwise.
func (r *Reconciler) guardPriorityClassName(chainNode *appsv1.ChainNode) string {
	if chainNode.IsValidator() {
		return r.opts.GetValidatorsPriorityClassName()
	}
	return r.opts.GetNodesPriorityClassName()
}

// apiServiceName returns the Service that a node's ingress/gateway API routes (RPC/LCD/gRPC/EVM)
// should target: the node's own CosmoGuard Service when it manages one (a standalone node, or a
// ChainNodeSet child with an individual ingress/gateway) AND that guard is serving, otherwise the
// raw node Service. Gating on readiness keeps routes on the raw node until the guard has ready
// endpoints (make-before-break), so enabling CosmoGuard doesn't rewrite a live route to a guard with
// zero endpoints. A child fronted only by its group's guard never targets "<child>-cosmoguard".
func (r *Reconciler) apiServiceName(ctx context.Context, chainNode *appsv1.ChainNode) string {
	if chainNode.UseInternal() {
		return fmt.Sprintf("%s-internal", chainNode.GetName())
	}
	if r.standaloneGuardManaged(chainNode) {
		serving, err := cosmoguard.IsServing(ctx, r.Client, chainNode.GetNamespace(), chainNode.CosmoGuardName())
		// Sticky: flip on first serving, and keep the route on the guard through transient rollout
		// un-readiness once it already points there (checked against the live route backend).
		if (err == nil && serving) || r.standaloneRouteTargetsGuard(ctx, chainNode) {
			return chainNode.CosmoGuardName()
		}
	}
	return chainNode.GetName()
}

// standaloneRouteTargetsGuard reports whether this node's live ingress/gateway route already points
// at its CosmoGuard Service, used to keep the route on the guard during transient guard rollouts.
func (r *Reconciler) standaloneRouteTargetsGuard(ctx context.Context, chainNode *appsv1.ChainNode) bool {
	guard := chainNode.CosmoGuardName()

	if chainNode.Spec.Gateway != nil {
		routes := &gwapiv1.HTTPRouteList{}
		if err := r.List(ctx, routes, client.InNamespace(chainNode.GetNamespace())); err == nil {
			for i := range routes.Items {
				rt := &routes.Items[i]
				if !metav1.IsControlledBy(rt, chainNode) {
					continue
				}
				for _, rule := range rt.Spec.Rules {
					for _, br := range rule.BackendRefs {
						if string(br.Name) == guard {
							return true
						}
					}
				}
			}
		}
		// gRPC-only routes use a GRPCRoute, so a gRPC-exposed guard must be recognized here too.
		grpcRoutes := &gwapiv1.GRPCRouteList{}
		if err := r.List(ctx, grpcRoutes, client.InNamespace(chainNode.GetNamespace())); err == nil {
			for i := range grpcRoutes.Items {
				rt := &grpcRoutes.Items[i]
				if !metav1.IsControlledBy(rt, chainNode) {
					continue
				}
				for _, rule := range rt.Spec.Rules {
					for _, br := range rule.BackendRefs {
						if string(br.Name) == guard {
							return true
						}
					}
				}
			}
		}
		return false
	}

	// A gRPC-only Ingress lives in a separate "<node>-grpc" Ingress (getGrpcIngressSpec); the base
	// "<node>" Ingress can carry no guard backend at all in that case. Inspect both so the sticky check
	// still keeps a gRPC-exposed guard through a transient rollout.
	for _, name := range []string{chainNode.GetName(), fmt.Sprintf("%s-grpc", chainNode.GetName())} {
		ing := &networkingv1.Ingress{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: name}, ing); err != nil {
			continue
		}
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service != nil && p.Backend.Service.Name == guard {
					return true
				}
			}
		}
	}
	return false
}

// ensureCosmoGuardSecret creates the olric gossip encryption Secret for a standalone guard if it
// does not exist yet. It never overwrites an existing Secret — the key must stay stable for the life
// of the cluster.
func (r *Reconciler) ensureCosmoGuardSecret(ctx context.Context, chainNode *appsv1.ChainNode, name string) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: name}, secret)
	if err == nil {
		// Refuse a same-named Secret we don't own: the guard would consume a foreign (possibly stale
		// or keyless) Secret, and Undeploy would never clean it up.
		if !metav1.IsControlledBy(secret, chainNode) {
			return fmt.Errorf("cosmoguard secret %q exists but is not owned by this ChainNode; refusing to use it", name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	key, err := cosmoguard.GenerateEncryptionKey()
	if err != nil {
		return err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: chainNode.GetNamespace()},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{cosmoguard.EncryptionKeySecretKey: []byte(key)},
	}
	if err := controllerutil.SetControllerReference(chainNode, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// cosmoGuardParams builds the CosmoGuard render parameters for a standalone ChainNode. The guard
// fronts this single node via its ready-gated main Service, so client API traffic is never forwarded
// to a node pod that is starting, upgrading, or stopped for snapshotting.
func (r *Reconciler) cosmoGuardParams(chainNode *appsv1.ChainNode) cosmoguard.Params {
	cfg := chainNode.Spec.Config

	name := chainNode.CosmoGuardName()
	p := cosmoguard.Params{
		Name:      name,
		Namespace: chainNode.GetNamespace(),
		Image:     cfg.GetCosmoGuardImage(r.opts.CosmoGuardImage),
		Replicas:  cfg.GetCosmoGuardReplicas(),
		// The main node Service publishes only ready endpoints (unlike "-internal", which publishes
		// not-ready addresses for peers/signer); client API traffic must go through the ready-gated one.
		UpstreamHost:        fmt.Sprintf("%s.%s.svc.cluster.local", chainNode.GetName(), chainNode.GetNamespace()),
		EvmEnabled:          cfg.IsEvmEnabled(),
		ConfigMap:           cfg.GetCosmoGuardConfig(),
		Resources:           cfg.GetCosmoGuardResources(),
		PeerServiceName:     cosmoguard.PeerServiceName(name),
		EncryptionKeySecret: cosmoguard.EncryptionKeySecretName(name),
		ImagePullSecrets:    cfg.ImagePullSecrets,
		// Match the node's scheduling priority so the guard isn't preempted while the node it protects
		// keeps running (which would drop guarded API traffic).
		PriorityClassName: r.guardPriorityClassName(chainNode),
		// Place the guard where the node runs (dedicated/tainted pools).
		NodeSelector: chainNode.Spec.NodeSelector,
		Affinity:     chainNode.Spec.Affinity,
		// Run under the node's ServiceAccount so SA-bound pull secrets / workload identity still apply,
		// as they did for the in-pod sidecar.
		ServiceAccountName: cfg.GetServiceAccountName(),
		// Carry the node's pod annotations (mesh/Vault injection, admission markers) plus the mirrored
		// safe-to-evict setting, as the in-pod sidecar did by sharing the node pod.
		PodAnnotations: cosmoguard.GuardPodAnnotations(cfg.PodAnnotations, cfg.SafeToEvict),
	}

	if cfg.CosmoGuardAutoscalingEnabled() {
		as := cfg.GetCosmoGuardAutoscaling()
		// Resolve resources and targets together so the HPA always has a positive request to measure.
		resources, targetCPU, targetMemory := cfg.GetCosmoGuardAutoscalingTargets()
		p.Resources = resources
		p.Autoscaling = &cosmoguard.AutoscalingParams{
			MinReplicas:  as.MinReplicas,
			MaxReplicas:  as.MaxReplicas,
			TargetCPU:    targetCPU,
			TargetMemory: targetMemory,
		}
	}

	if cfg.CosmoGuardDashboardEnabled() {
		d := cfg.GetCosmoGuardDashboard()
		dp := &cosmoguard.DashboardParams{Port: cfg.GetCosmoGuardDashboardPort()}
		if d.BasicAuth != nil {
			dp.AuthUser = &d.BasicAuth.Username
			dp.AuthPassword = &d.BasicAuth.Password
		}
		if d.Ingress != nil {
			dp.Ingress = &cosmoguard.DashboardIngressParams{
				Host:             d.Ingress.Host,
				IngressClassName: d.Ingress.IngressClassName,
				Annotations:      d.Ingress.Annotations,
				TLSSecretName:    d.Ingress.TLSSecretName,
			}
		}
		p.Dashboard = dp
	}

	return p
}

// standaloneGuardManaged reports whether this ChainNode should have its own standalone CosmoGuard.
//
// A ChainNodeSet child is normally fronted by its group's guard (managed by the ChainNodeSet
// controller), so it does not get a per-node guard — EXCEPT when it declares its own individual
// ingress/gateway. Those are per-instance routes to one specific node, which the shared group guard
// (a single deployment that load-balances the whole group via discovery) cannot target. To keep such
// per-node endpoints guarded (as the in-pod sidecar did), the child gets its own single-node guard.
func (r *Reconciler) standaloneGuardManaged(chainNode *appsv1.ChainNode) bool {
	if !chainNode.Spec.Config.CosmoGuardEnabled() {
		return false
	}
	if _, isChild := chainNode.Labels[controllers.LabelChainNodeSet]; isChild {
		return chainNode.Spec.Ingress != nil || chainNode.Spec.Gateway != nil
	}
	return true
}

// finalizeCosmoGuard tears down a standalone guard the node no longer uses (CosmoGuard disabled, or
// the node was moved into a ChainNodeSet). It runs AFTER ingress/gateway routes are reconciled, so
// routes have already been retargeted to the raw node Service before the guard Service is removed —
// avoiding a window where a live route points at a deleted backend (make-before-break on teardown).
func (r *Reconciler) finalizeCosmoGuard(ctx context.Context, chainNode *appsv1.ChainNode) error {
	if r.standaloneGuardManaged(chainNode) {
		return nil
	}
	return cosmoguard.Undeploy(ctx, r.Client, chainNode, chainNode.GetNamespace(), chainNode.CosmoGuardName())
}

// ensureCosmoGuard reconciles the standalone CosmoGuard deployment for a ChainNode. It only
// creates/updates resources; teardown (disabled, or the node became a ChainNodeSet child) is handled
// by finalizeCosmoGuard after routes are retargeted.
func (r *Reconciler) ensureCosmoGuard(ctx context.Context, chainNode *appsv1.ChainNode) error {
	logger := log.FromContext(ctx)

	if !r.standaloneGuardManaged(chainNode) {
		return nil
	}

	if chainNode.Spec.Config.GetCosmoGuardConfig() == nil {
		logger.Info("cosmoguard enabled without a config ConfigMap; skipping")
		return nil
	}

	params := r.cosmoGuardParams(chainNode)

	if err := r.ensureCosmoGuardSecret(ctx, chainNode, cosmoguard.EncryptionKeySecretName(chainNode.CosmoGuardName())); err != nil {
		return fmt.Errorf("failed to ensure cosmoguard secret for %s: %w", chainNode.GetName(), err)
	}
	if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, params.PeerService()); err != nil {
		return fmt.Errorf("failed to apply cosmoguard peer service for %s: %w", chainNode.GetName(), err)
	}
	if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, params.StatefulSet()); err != nil {
		return fmt.Errorf("failed to apply cosmoguard statefulset for %s: %w", chainNode.GetName(), err)
	}
	if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, params.Service()); err != nil {
		return fmt.Errorf("failed to apply cosmoguard service for %s: %w", chainNode.GetName(), err)
	}
	if pdb := params.PDB(); pdb != nil {
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, pdb); err != nil {
			return fmt.Errorf("failed to apply cosmoguard pdb for %s: %w", chainNode.GetName(), err)
		}
	} else {
		stale := &policyv1.PodDisruptionBudget{}
		err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: chainNode.CosmoGuardName()}, stale)
		if err == nil {
			if metav1.IsControlledBy(stale, chainNode) {
				if err := client.IgnoreNotFound(r.Delete(ctx, stale)); err != nil {
					return err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	if hpa := params.HPA(); hpa != nil {
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, hpa); err != nil {
			return fmt.Errorf("failed to apply cosmoguard hpa for %s: %w", chainNode.GetName(), err)
		}
	} else {
		// Autoscaling was disabled: remove any HPA we previously created so it stops driving the
		// StatefulSet's replica count.
		stale := &autoscalingv2.HorizontalPodAutoscaler{}
		err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: chainNode.CosmoGuardName()}, stale)
		if err == nil {
			if metav1.IsControlledBy(stale, chainNode) {
				if err := client.IgnoreNotFound(r.Delete(ctx, stale)); err != nil {
					return err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}

	if ing := params.DashboardIngress(); ing != nil {
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, ing); err != nil {
			return fmt.Errorf("failed to apply cosmoguard dashboard ingress for %s: %w", chainNode.GetName(), err)
		}
	} else {
		// Dashboard ingress disabled: remove a previously-created one.
		stale := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: params.DashboardIngressName(), Namespace: chainNode.GetNamespace()}}
		if err := r.Get(ctx, client.ObjectKeyFromObject(stale), stale); err == nil {
			if metav1.IsControlledBy(stale, chainNode) {
				if err := client.IgnoreNotFound(r.Delete(ctx, stale)); err != nil {
					return err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}
