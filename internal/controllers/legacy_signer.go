package controllers

import (
	"context"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

// LegacySignerGuard shares authoritative passes between the node and set controllers.
// Cosmopilot 5 cannot create tmKMS artifacts, so a clean pass is final for that UID's lifetime.
// The lock covers only the record: holding it across the API reads would stall every worker of
// both controllers behind one slow read, and a duplicate check of the same UID is harmless.
type LegacySignerGuard struct {
	mu     sync.Mutex
	passed map[types.UID]struct{}
}

func (g *LegacySignerGuard) hasPassed(uid types.UID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, passed := g.passed[uid]
	return uid != "" && passed
}

func (g *LegacySignerGuard) remember(uid types.UID) {
	if uid == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.passed == nil {
		g.passed = make(map[types.UID]struct{})
	}
	g.passed[uid] = struct{}{}
}

// RefuseLegacyTmKMS prevents reconciliation from replacing a legacy signing path or its keys.
func (g *LegacySignerGuard) RefuseLegacyTmKMS(ctx context.Context, reader client.Reader, recorder record.EventRecorder, chainNode *appsv1.ChainNode) error {
	if reader == nil {
		return fmt.Errorf("legacy tmKMS guard requires an authoritative APIReader")
	}
	if g.hasPassed(chainNode.UID) {
		return nil
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(chainNode), pod); err != nil && !errors.IsNotFound(err) {
		return err
	}
	config := &corev1.ConfigMap{}
	artifactKey := client.ObjectKey{Namespace: chainNode.Namespace, Name: chainNode.Name + "-tmkms"}
	if err := reader.Get(ctx, artifactKey, config); err != nil && !errors.IsNotFound(err) {
		return err
	}
	identity := &corev1.Secret{}
	if err := reader.Get(ctx, artifactKey, identity); err != nil && !errors.IsNotFound(err) {
		return err
	}
	if err := refuseLegacyTmKMSArtifacts(recorder, chainNode, pod, config, identity); err != nil {
		return err
	}
	g.remember(chainNode.UID)
	return nil
}

// RefuseLegacyTmKMSChildren checks active children from authoritative namespace snapshots.
func (g *LegacySignerGuard) RefuseLegacyTmKMSChildren(ctx context.Context, reader client.Reader, recorder record.EventRecorder, nodeSet *appsv1.ChainNodeSet) error {
	if reader == nil {
		return fmt.Errorf("legacy tmKMS guard requires an authoritative APIReader")
	}
	if g.hasPassed(nodeSet.UID) {
		return nil
	}
	nodes := &appsv1.ChainNodeList{}
	pods := &corev1.PodList{}
	configs := &corev1.ConfigMapList{}
	identities := &corev1.SecretList{}
	for _, list := range []client.ObjectList{nodes, pods, configs, identities} {
		if err := reader.List(ctx, list, client.InNamespace(nodeSet.Namespace)); err != nil {
			return err
		}
	}
	podsByName := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		podsByName[pod.Name] = pod
	}
	configsByName := make(map[string]*corev1.ConfigMap, len(configs.Items))
	for i := range configs.Items {
		config := &configs.Items[i]
		configsByName[config.Name] = config
	}
	identitiesByName := make(map[string]*corev1.Secret, len(identities.Items))
	for i := range identities.Items {
		identity := &identities.Items[i]
		identitiesByName[identity.Name] = identity
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !metav1.IsControlledBy(node, nodeSet) || !node.DeletionTimestamp.IsZero() || g.hasPassed(node.UID) {
			continue
		}
		artifactName := node.Name + "-tmkms"
		if err := refuseLegacyTmKMSArtifacts(recorder, node, podsByName[node.Name], configsByName[artifactName], identitiesByName[artifactName]); err != nil {
			return err
		}
		g.remember(node.UID)
	}
	g.remember(nodeSet.UID)
	return nil
}

func refuseLegacyTmKMSArtifacts(recorder record.EventRecorder, chainNode *appsv1.ChainNode, pod *corev1.Pod, config *corev1.ConfigMap, identity *corev1.Secret) error {
	var artifacts, remedies []string
	if pod != nil {
		for _, container := range pod.Spec.Containers {
			if container.Name == "tmkms" {
				artifacts = append(artifacts, fmt.Sprintf("tmkms container in Pod %s/%s", pod.Namespace, pod.Name))
				remedies = append(remedies, "the Pod must be retired by completing the migration on Cosmopilot 4.x or deleted manually")
				break
			}
		}
	}
	if config != nil {
		if owner := metav1.GetControllerOf(config); owner != nil {
			// A predecessor's ConfigMap can still identify a live signing path after restore or recreation.
			groupVersion, err := schema.ParseGroupVersion(owner.APIVersion)
			if err == nil && groupVersion.Group == appsv1.GroupVersion.Group && owner.Kind == "ChainNode" && owner.Name == chainNode.Name {
				artifacts = append(artifacts, fmt.Sprintf("owned ConfigMap %s/%s", config.Namespace, config.Name))
				remedies = append(remedies, fmt.Sprintf("if already migrated to cosmosigner, delete the stale %s-tmkms ConfigMap", chainNode.Name))
			}
		}
	}
	if identity != nil && isLegacyTmKMSIdentity(identity, chainNode) {
		artifacts = append(artifacts, fmt.Sprintf("identity Secret %s/%s", identity.Namespace, identity.Name))
		remedies = append(remedies, fmt.Sprintf("if already migrated to cosmosigner, verify signing with the existing validator public key before deleting the stale %s-tmkms identity Secret", chainNode.Name))
	}
	if len(artifacts) == 0 {
		return nil
	}
	err := fmt.Errorf("validator %s/%s cannot be reconciled by Cosmopilot 5: found %s; migrate to cosmosigner on Cosmopilot 4.x before upgrading; %s", chainNode.Namespace, chainNode.Name, strings.Join(artifacts, " and "), strings.Join(remedies, "; "))
	if recorder != nil {
		recorder.Event(chainNode, corev1.EventTypeWarning, appsv1.ReasonInvalid, err.Error())
	}
	return err
}

func isLegacyTmKMSIdentity(secret *corev1.Secret, node *appsv1.ChainNode) bool {
	annotations := secret.Annotations
	if class, stamped := annotations["cosmopilot.voluzi.com/resource-class"]; stamped {
		rootKind, rootName := "ChainNode", node.Name
		if owner := metav1.GetControllerOf(node); owner != nil && owner.APIVersion == appsv1.GroupVersion.String() && owner.Kind == "ChainNodeSet" {
			rootKind, rootName = owner.Kind, owner.Name
		}
		groupVersion, err := schema.ParseGroupVersion(annotations["cosmopilot.voluzi.com/root-owner-api-version"])
		// Durable identity attribution follows the root name across deletion and recreation.
		return err == nil && groupVersion.Group == appsv1.GroupVersion.Group && class == "tmkmsIdentity" &&
			annotations["cosmopilot.voluzi.com/root-owner-kind"] == rootKind &&
			annotations["cosmopilot.voluzi.com/root-owner-name"] == rootName &&
			annotations["cosmopilot.voluzi.com/root-owner-namespace"] == node.Namespace
	}
	_, hasKey := secret.Data["kms-identity.key"]
	return metav1.GetControllerOf(secret) == nil && secret.Immutable != nil && *secret.Immutable &&
		hasKey && len(secret.Data) == 1 && (secret.Type == "" || secret.Type == corev1.SecretTypeOpaque)
}
