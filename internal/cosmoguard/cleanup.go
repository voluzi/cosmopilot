package cosmoguard

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Undeploy deletes the owned CosmoGuard Deployment, Service and HorizontalPodAutoscaler for the
// given name. Each object is only deleted when it exists and is controlled by owner, so a
// name-collision with a resource owned by a different CR is never destroyed. Missing objects are
// ignored, making the call idempotent. Owner-reference GC covers CR deletion; Undeploy covers the
// case where CosmoGuard is disabled while the owning CR lives on.
func Undeploy(ctx context.Context, c client.Client, owner client.Object, namespace, name string) error {
	objs := []client.Object{
		&autoscalingv2.HorizontalPodAutoscaler{},
		&corev1.Service{},
		&appsv1.Deployment{},
	}
	for _, obj := range objs {
		if err := deleteOwned(ctx, c, owner, namespace, name, obj); err != nil {
			return err
		}
	}
	// The dashboard Ingress (when present) is named "<name>-dashboard".
	return deleteOwned(ctx, c, owner, namespace, name+"-dashboard", &networkingv1.Ingress{})
}

func deleteOwned(ctx context.Context, c client.Client, owner client.Object, namespace, name string, obj client.Object) error {
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(obj, owner) {
		// A resource of the same name owned by someone else — leave it untouched.
		return nil
	}
	if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete cosmoguard %T %q: %w", obj, name, err)
	}
	return nil
}
