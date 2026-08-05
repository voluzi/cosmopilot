package chainnode

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

type GenerationChangedPredicate struct {
	predicate.Funcs
}

var temporaryPodSuffixes = []string{
	"config-generator", "data-init", "init-data", "genesis-init", "tmkms-vault-upload", "tmkms-generate-identity",
	"write-file", "create-validator", "signer-pubkey", "signer-import",
}

func isTemporaryPodName(name string) bool {
	for _, suffix := range temporaryPodSuffixes {
		if strings.HasSuffix(name, "-"+suffix) {
			return true
		}
	}
	return false
}

func isChainNodeSigningPodName(name, chainNodeName string) bool {
	if name == chainNodeName {
		return true
	}
	for _, suffix := range temporaryPodSuffixes {
		baseName := chainNodeName + "-" + suffix
		if name == baseName || strings.HasPrefix(name, baseName+"-") {
			return true
		}
	}
	return false
}

// Create implements default CreateEvent filter
func (p GenerationChangedPredicate) Create(e event.CreateEvent) bool {
	if e.Object == nil {
		return false
	}

	// Ignore events from temporary pods
	if isTemporaryPodName(e.Object.(metav1.Object).GetName()) {
		return false
	}

	return p.Funcs.Create(e)
}

// Delete implements default DeleteEvent filter
func (p GenerationChangedPredicate) Delete(e event.DeleteEvent) bool {
	if e.Object == nil {
		return false
	}

	// Ignore events from temporary pods
	if isTemporaryPodName(e.Object.(metav1.Object).GetName()) {
		return false
	}

	return p.Funcs.Delete(e)
}

// Update implements default UpdateEvent filter for validating generation change
func (p GenerationChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil {
		return false
	}
	if e.ObjectNew == nil {
		return false
	}

	switch o := e.ObjectNew.(type) {
	case *appsv1.ChainNode:
		oldNode := e.ObjectOld.(*appsv1.ChainNode)
		return e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() ||
			oldNode.GetAnnotations()[controllers.AnnotationCosmosignerRollout] != o.GetAnnotations()[controllers.AnnotationCosmosignerRollout] ||
			(e.ObjectOld.GetDeletionTimestamp().IsZero() && !e.ObjectNew.GetDeletionTimestamp().IsZero())

	case *corev1.Pod:
		// Ignore events from temporary pods
		if isTemporaryPodName(o.Name) {
			return false
		}
		return p.Funcs.Update(e)

	default:
		return p.Funcs.Update(e)
	}
}
