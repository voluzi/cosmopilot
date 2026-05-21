package controllers

import (
	"context"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// IsCRDNotInstalled returns true when the Gateway API CRDs are not installed.
func IsCRDNotInstalled(err error) bool {
	return meta.IsNoMatchError(err)
}

// EnsureHTTPRoute creates or updates the given HTTPRoute. The returned bool is
// true when the route was successfully reconciled; it is false when the Gateway
// API CRDs are not installed in the cluster (the call is then a no-op so the
// caller can degrade gracefully without tearing down legacy resources).
//
// The desired object is annotated with banzaicloud's last-applied-configuration
// before Create/Update so subsequent reconciles can do a 3-way merge and ignore
// server-side defaults the Gateway API admission stamps onto the live object
// (e.g. BackendRef.Group/Kind/Weight, default path Match). Without this, every
// reconcile saw spurious drift and re-issued an Update that the API server then
// dropped as a no-op.
func EnsureHTTPRoute(ctx context.Context, c client.Client, route *gwapiv1.HTTPRoute) (bool, error) {
	logger := log.FromContext(ctx)

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
// for the meaning of the bool return value and the rationale for the
// last-applied-configuration annotation.
func EnsureGRPCRoute(ctx context.Context, c client.Client, route *gwapiv1.GRPCRoute) (bool, error) {
	logger := log.FromContext(ctx)

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
// for the meaning of the bool return value and the rationale for the
// last-applied-configuration annotation.
func EnsureTCPRoute(ctx context.Context, c client.Client, route *gwapiv1a2.TCPRoute) (bool, error) {
	logger := log.FromContext(ctx)

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
