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

func EnsureHTTPRoute(ctx context.Context, c client.Client, route *gwapiv1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	current := &gwapiv1.HTTPRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), current)
	if err != nil {
		if IsCRDNotInstalled(err) {
			return nil
		}
		if errors.IsNotFound(err) {
			logger.Info("creating httproute", "httproute", route.GetName())
			if err = c.Create(ctx, route); err != nil && !IsCRDNotInstalled(err) {
				return err
			}
			return nil
		}
		return err
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(current, route)
	if err != nil {
		return err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating httproute", "httproute", route.GetName())
		route.ObjectMeta.ResourceVersion = current.ObjectMeta.ResourceVersion
		if err = c.Update(ctx, route); err != nil && !IsCRDNotInstalled(err) {
			return err
		}
	}

	*route = *current
	return nil
}

func EnsureGRPCRoute(ctx context.Context, c client.Client, route *gwapiv1a2.GRPCRoute) error {
	logger := log.FromContext(ctx)

	current := &gwapiv1a2.GRPCRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), current)
	if err != nil {
		if IsCRDNotInstalled(err) {
			return nil
		}
		if errors.IsNotFound(err) {
			logger.Info("creating grpcroute", "grpcroute", route.GetName())
			if err = c.Create(ctx, route); err != nil && !IsCRDNotInstalled(err) {
				return err
			}
			return nil
		}
		return err
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(current, route)
	if err != nil {
		return err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating grpcroute", "grpcroute", route.GetName())
		route.ObjectMeta.ResourceVersion = current.ObjectMeta.ResourceVersion
		if err = c.Update(ctx, route); err != nil && !IsCRDNotInstalled(err) {
			return err
		}
	}

	*route = *current
	return nil
}

func EnsureTCPRoute(ctx context.Context, c client.Client, route *gwapiv1a2.TCPRoute) error {
	logger := log.FromContext(ctx)

	current := &gwapiv1a2.TCPRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), current)
	if err != nil {
		if IsCRDNotInstalled(err) {
			return nil
		}
		if errors.IsNotFound(err) {
			logger.Info("creating tcproute", "tcproute", route.GetName())
			if err = c.Create(ctx, route); err != nil && !IsCRDNotInstalled(err) {
				return err
			}
			return nil
		}
		return err
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(current, route)
	if err != nil {
		return err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating tcproute", "tcproute", route.GetName())
		route.ObjectMeta.ResourceVersion = current.ObjectMeta.ResourceVersion
		if err = c.Update(ctx, route); err != nil && !IsCRDNotInstalled(err) {
			return err
		}
	}

	*route = *current
	return nil
}
