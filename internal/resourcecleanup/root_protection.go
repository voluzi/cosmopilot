package resourcecleanup

import (
	"context"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
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
	key := client.ObjectKeyFromObject(object)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := object.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, key, fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !fresh.GetDeletionTimestamp().IsZero() || controllerutil.ContainsFinalizer(fresh, Finalizer) {
			return nil
		}
		base := fresh.DeepCopyObject().(client.Object)
		controllerutil.AddFinalizer(fresh, Finalizer)
		err := c.Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	})
}
