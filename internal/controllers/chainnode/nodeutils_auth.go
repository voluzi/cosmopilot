package chainnode

import (
	"context"
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

var nodeUtilsShutdownSecretNamePattern = regexp.MustCompile(`^node-utils\.[0-9a-f]{32}\.[0-9a-f]{32}$`)

type nodeUtilsShutdownCredential struct {
	name  string
	uid   types.UID
	token string
}

type nodeUtilsShutdownClient interface {
	ShutdownNodeUtilsServer(context.Context) error
}

type nodeUtilsShutdownClientFactory func(host, token string) nodeUtilsShutdownClient

func defaultNodeUtilsShutdownClientFactory(host, token string) nodeUtilsShutdownClient {
	return nodeutils.NewClientWithShutdownToken(host, token)
}

func nodeUtilsShutdownSecretNameForToken(token string) string {
	digest := nodeutils.ShutdownTokenHash(token)
	return "node-utils." + digest[:32] + "." + digest[32:]
}

func (r *Reconciler) ensureNodeUtilsShutdownCredential(ctx context.Context, chainNode *appsv1.ChainNode) (nodeUtilsShutdownCredential, error) {
	name, uid, bound, err := nodeUtilsShutdownBinding(chainNode)
	if err != nil {
		return nodeUtilsShutdownCredential{}, err
	}
	if !bound {
		return r.createAndBindNodeUtilsShutdownCredential(ctx, chainNode)
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: name}
	if err := r.reservationReader().Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return r.createAndBindNodeUtilsShutdownCredential(ctx, chainNode)
		}
		return nodeUtilsShutdownCredential{}, fmt.Errorf("get node-utils shutdown Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	if secret.GetUID() != uid {
		return r.createAndBindNodeUtilsShutdownCredential(ctx, chainNode)
	}
	token, err := validateNodeUtilsShutdownSecret(secret, chainNode, name)
	if err != nil {
		return nodeUtilsShutdownCredential{}, err
	}
	return nodeUtilsShutdownCredential{name: name, uid: uid, token: token}, nil
}

func (r *Reconciler) createAndBindNodeUtilsShutdownCredential(ctx context.Context, chainNode *appsv1.ChainNode) (nodeUtilsShutdownCredential, error) {
	fresh := &appsv1.ChainNode{}
	key := client.ObjectKeyFromObject(chainNode)
	if err := r.reservationReader().Get(ctx, key, fresh); err != nil {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("get current ChainNode %s/%s before binding node-utils shutdown credential: %w", key.Namespace, key.Name, err)
	}
	if fresh.GetUID() != chainNode.GetUID() || fresh.GetResourceVersion() != chainNode.GetResourceVersion() {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("ChainNode %s/%s changed before binding node-utils shutdown credential", key.Namespace, key.Name)
	}
	if !fresh.GetDeletionTimestamp().IsZero() {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("ChainNode %s/%s is terminating", key.Namespace, key.Name)
	}

	generate := r.shutdownTokenGenerator
	if generate == nil {
		generate = nodeutils.GenerateShutdownToken
	}
	token, err := generate()
	if err != nil {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("generate node-utils shutdown token: %w", err)
	}
	if !nodeutils.ValidShutdownToken(token) {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("generated node-utils shutdown token is malformed")
	}
	name := nodeUtilsShutdownSecretNameForToken(token)
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fresh.GetNamespace(), Labels: WithChainNodeLabels(fresh)},
		Immutable:  &immutable,
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{nodeutils.ShutdownTokenSecretKey: []byte(token)},
	}
	if err := controllerutil.SetControllerReference(fresh, secret, r.Scheme); err != nil {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("own node-utils shutdown Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	if err := r.Create(ctx, secret); err != nil {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("create node-utils shutdown Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	if secret.GetUID() == "" {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("create node-utils shutdown Secret %s/%s: API response has no UID", secret.Namespace, secret.Name)
	}

	base := fresh.DeepCopy()
	annotations := make(map[string]string, len(fresh.GetAnnotations())+2)
	for annotation, value := range fresh.GetAnnotations() {
		annotations[annotation] = value
	}
	annotations[controllers.AnnotationNodeUtilsShutdownSecretName] = secret.GetName()
	annotations[controllers.AnnotationNodeUtilsShutdownSecretUID] = string(secret.GetUID())
	fresh.SetAnnotations(annotations)
	if err := r.Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("bind node-utils shutdown Secret %s/%s to ChainNode: %w", secret.Namespace, secret.Name, err)
	}
	chainNode.SetAnnotations(fresh.GetAnnotations())
	chainNode.SetResourceVersion(fresh.GetResourceVersion())
	return nodeUtilsShutdownCredential{name: secret.GetName(), uid: secret.GetUID(), token: token}, nil
}

func nodeUtilsShutdownBinding(object metav1.Object) (string, types.UID, bool, error) {
	annotations := object.GetAnnotations()
	name, hasName := annotations[controllers.AnnotationNodeUtilsShutdownSecretName]
	uidValue, hasUID := annotations[controllers.AnnotationNodeUtilsShutdownSecretUID]
	if !hasName && !hasUID {
		return "", "", false, nil
	}
	if !hasName || !hasUID || !nodeUtilsShutdownSecretNamePattern.MatchString(name) || uidValue == "" {
		return "", "", false, fmt.Errorf("node-utils shutdown Secret binding on %s/%s is partial or malformed", object.GetNamespace(), object.GetName())
	}
	return name, types.UID(uidValue), true, nil
}

func validateNodeUtilsShutdownSecret(secret *corev1.Secret, chainNode *appsv1.ChainNode, expectedName string) (string, error) {
	if secret.GetName() != expectedName {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s has an unexpected name", secret.GetNamespace(), secret.GetName())
	}
	if !secret.GetDeletionTimestamp().IsZero() {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s is terminating", secret.GetNamespace(), secret.GetName())
	}
	if !metav1.IsControlledBy(secret, chainNode) {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s is not owned by this ChainNode", secret.GetNamespace(), secret.GetName())
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s has type %q, want %q", secret.GetNamespace(), secret.GetName(), secret.Type, corev1.SecretTypeOpaque)
	}
	if secret.Immutable == nil || !*secret.Immutable {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s is not immutable", secret.GetNamespace(), secret.GetName())
	}
	token := string(secret.Data[nodeutils.ShutdownTokenSecretKey])
	if !nodeutils.ValidShutdownToken(token) {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s has a missing or malformed %q value", secret.GetNamespace(), secret.GetName(), nodeutils.ShutdownTokenSecretKey)
	}
	if nodeUtilsShutdownSecretNameForToken(token) != expectedName {
		return "", fmt.Errorf("node-utils shutdown Secret %s/%s token does not match its name", secret.GetNamespace(), secret.GetName())
	}
	return token, nil
}

func stampNodeUtilsShutdownCredential(pod *corev1.Pod, credential nodeUtilsShutdownCredential) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[controllers.AnnotationNodeUtilsShutdownSecretName] = credential.name
	pod.Annotations[controllers.AnnotationNodeUtilsShutdownSecretUID] = string(credential.uid)
	pod.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash] = nodeutils.ShutdownTokenHash(credential.token)
}

func (r *Reconciler) loadLivePodNodeUtilsShutdownCredential(ctx context.Context, chainNode *appsv1.ChainNode) (nodeUtilsShutdownCredential, error) {
	pod := &corev1.Pod{}
	if err := r.reservationReader().Get(ctx, client.ObjectKeyFromObject(chainNode), pod); err != nil {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("get live Pod %s/%s for node-utils shutdown: %w", chainNode.Namespace, chainNode.Name, err)
	}
	if !metav1.IsControlledBy(pod, chainNode) {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("live Pod %s/%s is not owned by this ChainNode", pod.Namespace, pod.Name)
	}
	name, uid, bound, err := nodeUtilsShutdownBinding(pod)
	if err != nil {
		return nodeUtilsShutdownCredential{}, err
	}
	if !bound {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("live Pod %s/%s has no node-utils shutdown Secret binding", pod.Namespace, pod.Name)
	}
	hash := pod.Annotations[controllers.AnnotationNodeUtilsShutdownTokenHash]
	if !nodeutils.ValidShutdownTokenHash(hash) {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("live Pod %s/%s has a missing or malformed node-utils shutdown token hash", pod.Namespace, pod.Name)
	}
	if err := validateNodeUtilsShutdownPodEnvironment(pod, name); err != nil {
		return nodeUtilsShutdownCredential{}, err
	}
	secret := &corev1.Secret{}
	if err := r.reservationReader().Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: name}, secret); err != nil {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("get live Pod node-utils shutdown Secret %s/%s: %w", pod.Namespace, name, err)
	}
	if secret.GetUID() != uid {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("live Pod node-utils shutdown Secret %s/%s UID does not match", pod.Namespace, name)
	}
	token, err := validateNodeUtilsShutdownSecret(secret, chainNode, name)
	if err != nil {
		return nodeUtilsShutdownCredential{}, err
	}
	if nodeutils.ShutdownTokenHash(token) != hash {
		return nodeUtilsShutdownCredential{}, fmt.Errorf("live Pod node-utils shutdown Secret %s/%s token hash does not match", pod.Namespace, name)
	}
	return nodeUtilsShutdownCredential{name: name, uid: uid, token: token}, nil
}

func validateNodeUtilsShutdownPodEnvironment(pod *corev1.Pod, secretName string) error {
	var nodeUtilsContainer *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == nodeUtilsContainerName {
			nodeUtilsContainer = &pod.Spec.InitContainers[i]
			break
		}
	}
	if nodeUtilsContainer == nil {
		return fmt.Errorf("live Pod %s/%s has no node-utils container", pod.Namespace, pod.Name)
	}
	var tokenEnv, hashEnv *corev1.EnvVar
	for i := range nodeUtilsContainer.Env {
		env := &nodeUtilsContainer.Env[i]
		switch env.Name {
		case nodeutils.ShutdownTokenEnvironmentVariable:
			if tokenEnv != nil {
				return fmt.Errorf("live Pod %s/%s has duplicate shutdown token environment", pod.Namespace, pod.Name)
			}
			tokenEnv = env
		case nodeutils.ExpectedShutdownTokenHashEnvironmentVariable:
			if hashEnv != nil {
				return fmt.Errorf("live Pod %s/%s has duplicate shutdown token hash environment", pod.Namespace, pod.Name)
			}
			hashEnv = env
		}
	}
	if tokenEnv == nil || tokenEnv.Value != "" || tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil || tokenEnv.ValueFrom.SecretKeyRef.Name != secretName || tokenEnv.ValueFrom.SecretKeyRef.Key != nodeutils.ShutdownTokenSecretKey || tokenEnv.ValueFrom.SecretKeyRef.Optional != nil {
		return fmt.Errorf("live Pod %s/%s has an untrusted shutdown token Secret reference", pod.Namespace, pod.Name)
	}
	expectedFieldPath := "metadata.annotations['" + controllers.AnnotationNodeUtilsShutdownTokenHash + "']"
	if hashEnv == nil || hashEnv.Value != "" || hashEnv.ValueFrom == nil || hashEnv.ValueFrom.FieldRef == nil || hashEnv.ValueFrom.FieldRef.FieldPath != expectedFieldPath {
		return fmt.Errorf("live Pod %s/%s has an untrusted shutdown token hash reference", pod.Namespace, pod.Name)
	}
	return nil
}
