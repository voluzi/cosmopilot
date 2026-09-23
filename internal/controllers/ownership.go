package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RequireSameController refuses to overwrite an existing object unless its controller reference is
// the one the desired object carries. Deterministic names are not proof of ownership: a same-name
// object created by a user or another controller must never be claimed implicitly.
func RequireSameController(existing, desired metav1.Object, kind string) error {
	want := metav1.GetControllerOf(desired)
	got := metav1.GetControllerOf(existing)
	if want == nil {
		return fmt.Errorf("desired %s %q has no controller owner", kind, desired.GetName())
	}
	if got == nil || got.UID != want.UID {
		return fmt.Errorf("%s %q is managed by another owner or is unowned; refusing to overwrite it", kind, existing.GetName())
	}
	return nil
}

// DeleteIfControlledBy deletes the object named by obj only when its live controller is owner. obj
// needs only a name and namespace and is overwritten with the live object. A missing object, a
// missing CRD and an object controlled by someone else are all left alone without error.
func DeleteIfControlledBy(ctx context.Context, c client.Client, obj client.Object, owner metav1.Object) (bool, error) {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if errors.IsNotFound(err) || IsCRDNotInstalled(err) {
			return false, nil
		}
		return false, err
	}
	return DeleteControlledObject(ctx, c, obj, owner)
}

// DeleteControlledObject deletes an object already read from the API (for example from a label-
// selected list) only when its controller is owner. The UID precondition makes the delete a no-op
// when the object was replaced by a same-name one after it was read.
func DeleteControlledObject(ctx context.Context, c client.Client, obj client.Object, owner metav1.Object) (bool, error) {
	if !metav1.IsControlledBy(obj, owner) {
		log.FromContext(ctx).Info("not deleting object controlled by another owner",
			"kind", fmt.Sprintf("%T", obj), "name", obj.GetName())
		return false, nil
	}
	uid := obj.GetUID()
	if err := c.Delete(ctx, obj, client.Preconditions{UID: &uid}); err != nil {
		if errors.IsNotFound(err) || errors.IsConflict(err) || IsCRDNotInstalled(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
