package chainnode

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmoguard"
)

// apiServiceName returns the Service that a node's ingress/gateway API routes (RPC/LCD/gRPC/EVM)
// should target. Only a STANDALONE ChainNode has its own CosmoGuard Service; a ChainNodeSet child is
// fronted by its group's guard (a different, per-group Service that load-balances the whole group),
// so a child's per-node routes must target the raw node Service rather than a never-created
// "<child>-cosmoguard" Service.
func (r *Reconciler) apiServiceName(chainNode *appsv1.ChainNode) string {
	if chainNode.UseInternal() {
		return fmt.Sprintf("%s-internal", chainNode.GetName())
	}
	if _, isChild := chainNode.Labels[controllers.LabelChainNodeSet]; !isChild && chainNode.Spec.Config.CosmoGuardEnabled() {
		return chainNode.CosmoGuardName()
	}
	return chainNode.GetName()
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
// fronts this single node, discovered statically through its internal Service.
func (r *Reconciler) cosmoGuardParams(chainNode *appsv1.ChainNode) cosmoguard.Params {
	cfg := chainNode.Spec.Config

	name := chainNode.CosmoGuardName()
	p := cosmoguard.Params{
		Name:                name,
		Namespace:           chainNode.GetNamespace(),
		Image:               cfg.GetCosmoGuardImage(r.opts.CosmoGuardImage),
		Replicas:            cfg.GetCosmoGuardReplicas(),
		UpstreamHost:        chainNode.GetNodeFQDN(),
		EvmEnabled:          cfg.IsEvmEnabled(),
		ConfigMap:           cfg.GetCosmoGuardConfig(),
		Resources:           cfg.GetCosmoGuardResources(),
		PeerServiceName:     cosmoguard.PeerServiceName(name),
		EncryptionKeySecret: cosmoguard.EncryptionKeySecretName(name),
		ImagePullSecrets:    cfg.ImagePullSecrets,
	}

	if cfg.CosmoGuardAutoscalingEnabled() {
		as := cfg.GetCosmoGuardAutoscaling()
		target := as.TargetCPUUtilizationPercentage
		if target == nil && as.TargetMemoryUtilizationPercentage == nil {
			target = ptr.To(appsv1.DefaultCosmoGuardAutoscalingCPUTarget)
		}
		p.Autoscaling = &cosmoguard.AutoscalingParams{
			MinReplicas:  as.MinReplicas,
			MaxReplicas:  as.MaxReplicas,
			TargetCPU:    target,
			TargetMemory: as.TargetMemoryUtilizationPercentage,
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
// A ChainNodeSet child is fronted by its group's guard (managed by the ChainNodeSet controller), so
// it never manages a per-node guard.
func (r *Reconciler) standaloneGuardManaged(chainNode *appsv1.ChainNode) bool {
	if _, isChild := chainNode.Labels[controllers.LabelChainNodeSet]; isChild {
		return false
	}
	return chainNode.Spec.Config.CosmoGuardEnabled()
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
