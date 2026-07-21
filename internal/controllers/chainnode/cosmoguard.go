package chainnode

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmoguard"
)

// cosmoGuardParams builds the CosmoGuard render parameters for a standalone ChainNode. The guard
// fronts this single node, discovered statically through its internal Service.
func (r *Reconciler) cosmoGuardParams(chainNode *appsv1.ChainNode) cosmoguard.Params {
	cfg := chainNode.Spec.Config

	p := cosmoguard.Params{
		Name:         chainNode.CosmoGuardName(),
		Namespace:    chainNode.GetNamespace(),
		Image:        cfg.GetCosmoGuardImage(r.opts.CosmoGuardImage),
		Replicas:     cfg.GetCosmoGuardReplicas(),
		UpstreamHost: chainNode.GetNodeFQDN(),
		EvmEnabled:   cfg.IsEvmEnabled(),
		ConfigMap:    cfg.GetCosmoGuardConfig(),
		Resources:    cfg.GetCosmoGuardResources(),
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

// ensureCosmoGuard reconciles the standalone CosmoGuard deployment for a ChainNode. Nodes that are
// part of a ChainNodeSet are skipped: their group's guard is managed by the ChainNodeSet controller.
func (r *Reconciler) ensureCosmoGuard(ctx context.Context, chainNode *appsv1.ChainNode) error {
	logger := log.FromContext(ctx)

	// A ChainNodeSet child is fronted by its group's guard; never manage a per-node guard for it.
	if _, isChild := chainNode.Labels[controllers.LabelChainNodeSet]; isChild {
		return nil
	}

	if !chainNode.Spec.Config.CosmoGuardEnabled() {
		return cosmoguard.Undeploy(ctx, r.Client, chainNode, chainNode.GetNamespace(), chainNode.CosmoGuardName())
	}

	if chainNode.Spec.Config.GetCosmoGuardConfig() == nil {
		logger.Info("cosmoguard enabled without a config ConfigMap; skipping")
		return nil
	}

	params := r.cosmoGuardParams(chainNode)

	if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, params.Deployment()); err != nil {
		return fmt.Errorf("failed to apply cosmoguard deployment for %s: %w", chainNode.GetName(), err)
	}
	if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, params.Service()); err != nil {
		return fmt.Errorf("failed to apply cosmoguard service for %s: %w", chainNode.GetName(), err)
	}
	if hpa := params.HPA(); hpa != nil {
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, hpa); err != nil {
			return fmt.Errorf("failed to apply cosmoguard hpa for %s: %w", chainNode.GetName(), err)
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
