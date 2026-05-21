package controllers

import (
	"context"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// Gateway API server-side defaults that the apiserver stamps onto routes when
// the corresponding field is omitted. We pre-populate them on the desired
// object so that patch.DefaultPatchMaker.Calculate(current, desired) returns
// an empty patch — without this every reconcile loop emits a no-op Update
// because neither strategic-merge nor JSON-merge patching can preserve
// server-added fields inside arrays of objects.
var (
	groupGateway      = gwapiv1.Group("gateway.networking.k8s.io")
	kindGateway       = gwapiv1.Kind("Gateway")
	groupCore         = gwapiv1.Group("")
	kindService       = gwapiv1.Kind("Service")
	defaultWeight     = ptr.To(int32(1))
	defaultPathPrefix = gwapiv1.PathMatchPathPrefix
	defaultPathValue  = "/"
)

// IsCRDNotInstalled returns true when the Gateway API CRDs are not installed.
func IsCRDNotInstalled(err error) bool {
	return meta.IsNoMatchError(err)
}

// applyHTTPRouteDefaults mirrors the defaults the apiserver applies to an
// HTTPRoute so the patch maker sees no drift on subsequent reconciles.
func applyHTTPRouteDefaults(route *gwapiv1.HTTPRoute) {
	for i := range route.Spec.ParentRefs {
		if route.Spec.ParentRefs[i].Group == nil {
			route.Spec.ParentRefs[i].Group = &groupGateway
		}
		if route.Spec.ParentRefs[i].Kind == nil {
			route.Spec.ParentRefs[i].Kind = &kindGateway
		}
	}
	for ri := range route.Spec.Rules {
		rule := &route.Spec.Rules[ri]
		if rule.Matches == nil {
			rule.Matches = []gwapiv1.HTTPRouteMatch{{
				Path: &gwapiv1.HTTPPathMatch{
					Type:  &defaultPathPrefix,
					Value: &defaultPathValue,
				},
			}}
		}
		for bi := range rule.BackendRefs {
			br := &rule.BackendRefs[bi]
			if br.Group == nil {
				br.Group = &groupCore
			}
			if br.Kind == nil {
				br.Kind = &kindService
			}
			if br.Weight == nil {
				br.Weight = defaultWeight
			}
		}
	}
}

// applyGRPCRouteDefaults mirrors the defaults the apiserver applies to a
// GRPCRoute. GRPCRoute rules do not default Matches (the live spec leaves
// the array nil), so we only default ParentRefs and BackendRefs.
func applyGRPCRouteDefaults(route *gwapiv1.GRPCRoute) {
	for i := range route.Spec.ParentRefs {
		if route.Spec.ParentRefs[i].Group == nil {
			route.Spec.ParentRefs[i].Group = &groupGateway
		}
		if route.Spec.ParentRefs[i].Kind == nil {
			route.Spec.ParentRefs[i].Kind = &kindGateway
		}
	}
	for ri := range route.Spec.Rules {
		rule := &route.Spec.Rules[ri]
		for bi := range rule.BackendRefs {
			br := &rule.BackendRefs[bi]
			if br.Group == nil {
				br.Group = &groupCore
			}
			if br.Kind == nil {
				br.Kind = &kindService
			}
			if br.Weight == nil {
				br.Weight = defaultWeight
			}
		}
	}
}

// applyTCPRouteDefaults mirrors the defaults the apiserver applies to a
// TCPRoute. TCPRoute has no Matches field; only ParentRefs and BackendRefs
// need defaulting.
func applyTCPRouteDefaults(route *gwapiv1a2.TCPRoute) {
	for i := range route.Spec.ParentRefs {
		if route.Spec.ParentRefs[i].Group == nil {
			g := gwapiv1a2.Group(groupGateway)
			route.Spec.ParentRefs[i].Group = &g
		}
		if route.Spec.ParentRefs[i].Kind == nil {
			k := gwapiv1a2.Kind(kindGateway)
			route.Spec.ParentRefs[i].Kind = &k
		}
	}
	for ri := range route.Spec.Rules {
		rule := &route.Spec.Rules[ri]
		for bi := range rule.BackendRefs {
			br := &rule.BackendRefs[bi]
			if br.Group == nil {
				g := gwapiv1a2.Group(groupCore)
				br.Group = &g
			}
			if br.Kind == nil {
				k := gwapiv1a2.Kind(kindService)
				br.Kind = &k
			}
			if br.Weight == nil {
				br.Weight = defaultWeight
			}
		}
	}
}

// EnsureHTTPRoute creates or updates the given HTTPRoute. The returned bool is
// true when the route was successfully reconciled; it is false when the Gateway
// API CRDs are not installed in the cluster (the call is then a no-op so the
// caller can degrade gracefully without tearing down legacy resources).
//
// Server-side defaults are pre-populated on the desired route and the
// banzaicloud last-applied-configuration annotation is set on Create/Update
// so subsequent reconciles see no spurious drift.
func EnsureHTTPRoute(ctx context.Context, c client.Client, route *gwapiv1.HTTPRoute) (bool, error) {
	logger := log.FromContext(ctx)
	applyHTTPRouteDefaults(route)

	current := &gwapiv1.HTTPRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), current)
	if err != nil {
		if IsCRDNotInstalled(err) {
			return false, nil
		}
		if errors.IsNotFound(err) {
			logger.Info("creating httproute", "httproute", route.GetName())
			if err = patch.DefaultAnnotator.SetLastAppliedAnnotation(route); err != nil {
				return false, err
			}
			if err = c.Create(ctx, route); err != nil {
				if IsCRDNotInstalled(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}
		return false, err
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(current, route)
	if err != nil {
		return false, err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating httproute", "httproute", route.GetName())
		if err = patch.DefaultAnnotator.SetLastAppliedAnnotation(route); err != nil {
			return false, err
		}
		route.ObjectMeta.ResourceVersion = current.ObjectMeta.ResourceVersion
		if err = c.Update(ctx, route); err != nil {
			if IsCRDNotInstalled(err) {
				return false, nil
			}
			return false, err
		}
	}

	*route = *current
	return true, nil
}

// EnsureGRPCRoute creates or updates the given GRPCRoute. See EnsureHTTPRoute
// for the meaning of the bool return value and the defaulting/annotation
// rationale.
func EnsureGRPCRoute(ctx context.Context, c client.Client, route *gwapiv1.GRPCRoute) (bool, error) {
	logger := log.FromContext(ctx)
	applyGRPCRouteDefaults(route)

	current := &gwapiv1.GRPCRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), current)
	if err != nil {
		if IsCRDNotInstalled(err) {
			return false, nil
		}
		if errors.IsNotFound(err) {
			logger.Info("creating grpcroute", "grpcroute", route.GetName())
			if err = patch.DefaultAnnotator.SetLastAppliedAnnotation(route); err != nil {
				return false, err
			}
			if err = c.Create(ctx, route); err != nil {
				if IsCRDNotInstalled(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}
		return false, err
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(current, route)
	if err != nil {
		return false, err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating grpcroute", "grpcroute", route.GetName())
		if err = patch.DefaultAnnotator.SetLastAppliedAnnotation(route); err != nil {
			return false, err
		}
		route.ObjectMeta.ResourceVersion = current.ObjectMeta.ResourceVersion
		if err = c.Update(ctx, route); err != nil {
			if IsCRDNotInstalled(err) {
				return false, nil
			}
			return false, err
		}
	}

	*route = *current
	return true, nil
}

// EnsureTCPRoute creates or updates the given TCPRoute. See EnsureHTTPRoute
// for the meaning of the bool return value and the defaulting/annotation
// rationale.
func EnsureTCPRoute(ctx context.Context, c client.Client, route *gwapiv1a2.TCPRoute) (bool, error) {
	logger := log.FromContext(ctx)
	applyTCPRouteDefaults(route)

	current := &gwapiv1a2.TCPRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), current)
	if err != nil {
		if IsCRDNotInstalled(err) {
			return false, nil
		}
		if errors.IsNotFound(err) {
			logger.Info("creating tcproute", "tcproute", route.GetName())
			if err = patch.DefaultAnnotator.SetLastAppliedAnnotation(route); err != nil {
				return false, err
			}
			if err = c.Create(ctx, route); err != nil {
				if IsCRDNotInstalled(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}
		return false, err
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(current, route)
	if err != nil {
		return false, err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating tcproute", "tcproute", route.GetName())
		if err = patch.DefaultAnnotator.SetLastAppliedAnnotation(route); err != nil {
			return false, err
		}
		route.ObjectMeta.ResourceVersion = current.ObjectMeta.ResourceVersion
		if err = c.Update(ctx, route); err != nil {
			if IsCRDNotInstalled(err) {
				return false, nil
			}
			return false, err
		}
	}

	*route = *current
	return true, nil
}
