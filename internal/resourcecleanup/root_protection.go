package resourcecleanup

import (
	"context"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// RootProtector installs cleanup finalizers after this manager instance acquires leadership. It stays
// alive after the one-time migration so successful completion does not stop the manager.
type RootProtector struct {
	Client     client.Client
	WorkerName string
	Ready      chan<- struct{}
	Migrate    func(context.Context) error
}

func (p *RootProtector) Start(ctx context.Context) error {
	if err := ProtectExistingRoots(ctx, p.Client, p.WorkerName); err != nil {
		return err
	}
	if p.Migrate != nil {
		if err := p.Migrate(ctx); err != nil {
			return err
		}
	}
	if p.Ready != nil {
		close(p.Ready)
	}
	<-ctx.Done()
	return nil
}

func (*RootProtector) NeedLeaderElection() bool { return true }

// ProtectExistingRoots installs the durable-resource cleanup finalizer on existing roots assigned
// to workerName. The empty worker name intentionally matches only unpartitioned roots.
func ProtectExistingRoots(ctx context.Context, c client.Client, workerName string) error {
	if err := protectChainNodes(ctx, c, workerName); err != nil {
		return err
	}
	return protectChainNodeSets(ctx, c, workerName)
}

func protectChainNodes(ctx context.Context, c client.Client, workerName string) error {
	list := &appsv1.ChainNodeList{}
	if err := c.List(ctx, list); err != nil {
		return err
	}
	for i := range list.Items {
		if !controllers.MatchesWorker(list.Items[i].GetLabels(), workerName) {
			continue
		}
		if err := protectRoot(ctx, c, &list.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

func protectChainNodeSets(ctx context.Context, c client.Client, workerName string) error {
	list := &appsv1.ChainNodeSetList{}
	if err := c.List(ctx, list); err != nil {
		return err
	}
	for i := range list.Items {
		if !controllers.MatchesWorker(list.Items[i].GetLabels(), workerName) {
			continue
		}
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
