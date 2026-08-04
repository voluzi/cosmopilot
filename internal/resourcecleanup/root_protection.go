package resourcecleanup

import (
	"context"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ProtectExistingRoots installs the durable-resource cleanup finalizer on every existing root before
// controllers begin processing deletion events. This closes the upgrade window where an old root
// could be deleted before its first normal post-upgrade reconcile installed the new finalizer.
func ProtectExistingRoots(ctx context.Context, c client.Client) error {
	if err := protectChainNodes(ctx, c); err != nil {
		return err
	}
	return protectChainNodeSets(ctx, c)
}

func protectChainNodes(ctx context.Context, c client.Client) error {
	list := &appsv1.ChainNodeList{}
	if err := c.List(ctx, list); err != nil {
		return err
	}
	for i := range list.Items {
		if err := protectRoot(ctx, c, &list.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

func protectChainNodeSets(ctx context.Context, c client.Client) error {
	list := &appsv1.ChainNodeSetList{}
	if err := c.List(ctx, list); err != nil {
		return err
	}
	for i := range list.Items {
		if err := protectRoot(ctx, c, &list.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

func protectRoot(ctx context.Context, c client.Client, object client.Object) error {
	if !object.GetDeletionTimestamp().IsZero() || controllerutil.ContainsFinalizer(object, Finalizer) {
		return nil
	}
	base := object.DeepCopyObject().(client.Object)
	controllerutil.AddFinalizer(object, Finalizer)
	if err := c.Patch(ctx, object, client.MergeFrom(base)); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}
