package v1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func SetupChainNodeSetValidationWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&ChainNodeSet{}).Complete()
}

var _ webhook.Validator = &ChainNodeSet{}
var chainNodeSetLogger = log.Log.WithName("chainnodeset-webhook")

func (nodeSet *ChainNodeSet) ValidateCreate() (warnings admission.Warnings, err error) {
	chainNodeSetLogger.V(1).Info("validating resource creation",
		"kind", "ChainNodeSet",
		"resource", nodeSet.GetNamespacedName(),
	)
	return nodeSet.Validate(nil)
}

func (nodeSet *ChainNodeSet) ValidateUpdate(old runtime.Object) (warnings admission.Warnings, err error) {
	chainNodeSetLogger.V(1).Info("validating resource update",
		"kind", "ChainNodeSet",
		"resource", nodeSet.GetNamespacedName(),
	)
	return nodeSet.Validate(old.(*ChainNodeSet))
}

func (nodeSet *ChainNodeSet) ValidateDelete() (warnings admission.Warnings, err error) {
	chainNodeSetLogger.V(1).Info("validating resource deletion (not implemented)",
		"kind", "ChainNodeSet",
		"resource", nodeSet.GetNamespacedName(),
	)
	return nil, nil
}

func (nodeSet *ChainNodeSet) Validate(old *ChainNodeSet) (admission.Warnings, error) {
	// Ensure a genesis is specified when .spec.validator.init is not.
	if (nodeSet.Spec.Validator == nil || nodeSet.Spec.Validator.Init == nil) && nodeSet.Spec.Genesis == nil {
		return nil, fmt.Errorf(".spec.genesis is required except when initializing new genesis with .spec.validator.init")
	}

	// Do not accept both genesis and validator init
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil && nodeSet.Spec.Genesis != nil {
		return nil, fmt.Errorf(".spec.genesis and .spec.validator.init are mutually exclusive")
	}

	// Validate validator snapshots config
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Persistence != nil && nodeSet.Spec.Validator.Persistence.Snapshots != nil {
		if err := validateSnapshotsConfig(nodeSet.Spec.Validator.Persistence.Snapshots, ".spec.validator.persistence.snapshots"); err != nil {
			return nil, err
		}
	}

	// Validate each node group
	for i, group := range nodeSet.Spec.Nodes {
		// Validate persistence size
		if group.Persistence != nil && group.Persistence.Size != nil {
			_, err := resource.ParseQuantity(*group.Persistence.Size)
			if err != nil {
				return nil, fmt.Errorf("bad format for .spec.nodes[%d].persistence.size: %v", i, err)
			}
		}

		// Validate snapshots config
		if group.Persistence != nil && group.Persistence.Snapshots != nil {
			if err := validateSnapshotsConfig(group.Persistence.Snapshots, fmt.Sprintf(".spec.nodes[%d].persistence.snapshots", i)); err != nil {
				return nil, err
			}
		}

		if group.GetSnapshotNodeIndex() < 0 || group.GetSnapshotNodeIndex() >= group.GetInstances() {
			return nil, fmt.Errorf(".spec.nodes[%d].snapshotNodeIndex is out of range", i)
		}
	}

	// Names in .spec.ingresses and .spec.gatewayRoutes must be unique across both lists,
	// because both produce identically-named global Services (<name>-global-<name>).
	seenRouteNames := make(map[string]string, len(nodeSet.Spec.Ingresses)+len(nodeSet.Spec.GatewayRoutes))
	for i, ing := range nodeSet.Spec.Ingresses {
		if existing, ok := seenRouteNames[ing.Name]; ok {
			return nil, fmt.Errorf(".spec.ingresses[%d].name %q duplicates %s", i, ing.Name, existing)
		}
		seenRouteNames[ing.Name] = fmt.Sprintf(".spec.ingresses[%d]", i)
	}
	for i, gw := range nodeSet.Spec.GatewayRoutes {
		if existing, ok := seenRouteNames[gw.Name]; ok {
			return nil, fmt.Errorf(".spec.gatewayRoutes[%d].name %q duplicates %s", i, gw.Name, existing)
		}
		seenRouteNames[gw.Name] = fmt.Sprintf(".spec.gatewayRoutes[%d]", i)
	}

	return nil, nil
}
