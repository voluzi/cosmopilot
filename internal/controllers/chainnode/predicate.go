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

var ignoreSuffixes = []string{"config-generator", "data-init", "genesis-init", "tmkms-vault-upload", "tmkms-generate-identity", "write-file", "create-validator"}

// Create implements default CreateEvent filter
func (p GenerationChangedPredicate) Create(e event.CreateEvent) bool {
	if e.Object == nil {
		return false
	}

	// Ignore events from temporary pods
	for _, suffix := range ignoreSuffixes {
		if strings.HasSuffix(e.Object.(metav1.Object).GetName(), suffix) {
			return false
		}
	}

	return p.Funcs.Create(e)
}

// Delete implements default DeleteEvent filter
func (p GenerationChangedPredicate) Delete(e event.DeleteEvent) bool {
	if e.Object == nil {
		return false
	}

	// Ignore events from temporary pods
	for _, suffix := range ignoreSuffixes {
		if strings.HasSuffix(e.Object.(metav1.Object).GetName(), suffix) {
			return false
		}
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
		return e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration()

	case *corev1.Pod:
		// Ignore events from temporary pods
		for _, suffix := range ignoreSuffixes {
			if strings.HasSuffix(o.Name, suffix) {
				return false
			}
		}
		return p.Funcs.Update(e)

	default:
		return p.Funcs.Update(e)
	}
}

// PeerServicePredicate filters events for peer services only.
// This is used to watch for changes in peer services that should trigger
// ConfigMap updates in ChainNodes with auto-discover peers enabled.
type PeerServicePredicate struct {
	predicate.Funcs
}

// isPeerService checks if a service is a peer service by checking the peer label.
func isPeerService(obj metav1.Object) bool {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return false
	}
	return svc.Labels[controllers.LabelPeer] == controllers.StringValueTrue
}

// Create implements CreateEvent filter for peer services
func (p PeerServicePredicate) Create(e event.CreateEvent) bool {
	return isPeerService(e.Object)
}

// Delete implements DeleteEvent filter for peer services
func (p PeerServicePredicate) Delete(e event.DeleteEvent) bool {
	return isPeerService(e.Object)
}

// Update implements UpdateEvent filter for peer services.
// Only triggers if the service labels or annotations changed, to avoid
// unnecessary reconciliation loops.
func (p PeerServicePredicate) Update(e event.UpdateEvent) bool {
	if !isPeerService(e.ObjectNew) {
		return false
	}

	oldSvc, ok := e.ObjectOld.(*corev1.Service)
	if !ok {
		return false
	}
	newSvc, ok := e.ObjectNew.(*corev1.Service)
	if !ok {
		return false
	}

	// Only reconcile if labels or annotations changed (relevant for peer discovery)
	return !labelsEqual(oldSvc.Labels, newSvc.Labels) ||
		!annotationsEqual(oldSvc.Annotations, newSvc.Annotations)
}

// labelsEqual compares two label maps for equality
func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// annotationsEqual compares two annotation maps for equality
func annotationsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
