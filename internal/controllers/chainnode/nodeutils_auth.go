package chainnode

import (
	"context"
	"crypto/sha256"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

type nodeUtilsShutdownClient interface {
	ShutdownNodeUtilsServer(context.Context) error
}

type nodeUtilsShutdownClientFactory func(host, token string) nodeUtilsShutdownClient

func defaultNodeUtilsShutdownClientFactory(host, token string) nodeUtilsShutdownClient {
	return nodeutils.NewClientWithShutdownToken(host, token)
}

func (r *Reconciler) ensureNodeUtilsShutdownSecret(ctx context.Context, chainNode *appsv1.ChainNode) error {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: chainNode.GetName() + nodeUtilsSecretSuffix}
	err := r.Get(ctx, key, secret)
	if err == nil {
		_, err := nodeUtilsShutdownToken(secret, chainNode)
		return err
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get node-utils shutdown Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	token, err := nodeutils.GenerateShutdownToken()
	if err != nil {
		return fmt.Errorf("generate node-utils shutdown token: %w", err)
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    WithChainNodeLabels(chainNode),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{nodeutils.ShutdownTokenSecretKey: []byte(token)},
	}
	if err := controllerutil.SetControllerReference(chainNode, secret, r.Scheme); err != nil {
		return fmt.Errorf("own node-utils shutdown Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("create node-utils shutdown Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

func (r *Reconciler) loadNodeUtilsShutdownToken(ctx context.Context, chainNode *appsv1.ChainNode) (string, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: chainNode.GetName() + nodeUtilsSecretSuffix}
	if err := r.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("get node-utils shutdown Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nodeUtilsShutdownToken(secret, chainNode)
}

func nodeUtilsShutdownToken(secret *corev1.Secret, chainNode *appsv1.ChainNode) (string, error) {
	if !metav1.IsControlledBy(secret, chainNode) {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s is not owned by this ChainNode", secret.GetNamespace(), secret.GetName())
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s has type %q, want %q", secret.GetNamespace(), secret.GetName(), secret.Type, corev1.SecretTypeOpaque)
	}
	token := string(secret.Data[nodeutils.ShutdownTokenSecretKey])
	if !nodeutils.ValidShutdownToken(token) {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s has a missing or malformed %q value", secret.GetNamespace(), secret.GetName(), nodeutils.ShutdownTokenSecretKey)
	}
	return token, nil
}

func (r *Reconciler) setNodeUtilsShutdownTokenHash(ctx context.Context, chainNode *appsv1.ChainNode, pod *corev1.Pod) error {
	token, err := r.loadNodeUtilsShutdownToken(ctx, chainNode)
	if err != nil {
		return err
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash] = fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	return nil
}
