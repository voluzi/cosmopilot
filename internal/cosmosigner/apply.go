package cosmosigner

import (
	"context"
	"fmt"
	"reflect"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v2/internal/k8s"
)

// ApplyOwned creates or updates a cosmosigner-managed object owned by owner. It refuses to
// overwrite an object owned by a different controller (a same-name CR collision), preserves
// StatefulSet fields Kubernetes forbids updating, and skips the write entirely when nothing
// changed (patch-equality), so steady-state reconciles do not churn resourceVersions.
func ApplyOwned(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, obj client.Object) error {
	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return err
	}
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object is not a client.Object")
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(existing, owner) {
		return fmt.Errorf("cosmosigner resource %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", obj.GetName())
	}

	k8s.PreserveImmutableStatefulSetFields(obj, existing)

	// Skip the write when nothing changed, so steady-state reconciles do not bump
	// resourceVersions (which would re-trigger the owner watch every cycle). The live object is
	// copied back into obj either way, so callers can read current status (e.g. ReadyReplicas).
	patchResult, err := patch.DefaultPatchMaker.Calculate(existing, obj, patch.IgnoreStatusFields())
	if err != nil {
		return err
	}
	if patchResult.IsEmpty() && reflect.DeepEqual(existing.GetLabels(), obj.GetLabels()) {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(existing).Elem())
		return nil
	}

	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

// Undeploy removes the managed signer resources for the given base name, deleting only objects the
// owner controls. If a signer StatefulSet with the derived name exists but is owned by a different
// CR (a same-name collision), nothing — including PVCs — is touched.
func Undeploy(ctx context.Context, c client.Client, owner client.Object, namespace, name string) error {
	sts := &appsv1.StatefulSet{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts)
	switch {
	case err == nil && !metav1.IsControlledBy(sts, owner):
		return nil
	case err != nil && !errors.IsNotFound(err):
		return err
	}

	objects := []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + discoveryServiceSuffix, Namespace: namespace}},
	}
	for _, obj := range objects {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, owner) {
			continue
		}
		if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	// StatefulSet PVCs are not garbage-collected with the StatefulSet; the foreign-owner
	// short-circuit above guarantees these are ours.
	return DeletePVCs(ctx, c, namespace, name)
}
