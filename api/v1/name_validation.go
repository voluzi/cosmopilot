package v1

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// reservedPodVolumeNames are the built-in volume names of the pods that mount additional volumes
// (internal/controllers/chainnode/pod.go, internal/tmkms, internal/chainutils/data.go). An additional
// volume with one of these names would collide with it.
var reservedPodVolumeNames = []string{
	"app-empty-dir", "data", "config-empty-dir", "config", "node-key", "upgrades-config", "genesis", "priv-key",
	"vault-token", "vault-ca-cert", "tmkms-identity", "tmkms-config", "tmkms-data",
	// Data-init pod (internal/chainutils/data.go), which also mounts the additional volumes.
	"home", "temp",
}

// validateDNS1123Label rejects a user-supplied name that becomes (part of) a Kubernetes object,
// container or volume name but is not a DNS-1123 label.
func validateDNS1123Label(path, value string) error {
	if errs := validation.IsDNS1123Label(value); len(errs) > 0 {
		return fmt.Errorf("%s %q is invalid: %s", path, value, strings.Join(errs, "; "))
	}
	return nil
}

// validateAdditionalVolumes checks names (DNS-1123, unique, not a built-in volume) and sizes. A smaller
// size is not rejected: the webhook cannot see the claim, and an expansion the storage class refused
// must stay revertible; the controller skips a shrink with an event.
func validateAdditionalVolumes(path string, persistence *Persistence) error {
	if persistence == nil {
		return nil
	}
	seen := make(map[string]int, len(persistence.AdditionalVolumes))
	for i, v := range persistence.AdditionalVolumes {
		p := fmt.Sprintf("%s.additionalVolumes[%d]", path, i)
		if err := validateDNS1123Label(p+".name", v.Name); err != nil {
			return err
		}
		if slices.Contains(reservedPodVolumeNames, v.Name) {
			return fmt.Errorf("%s.name %q collides with a built-in pod volume", p, v.Name)
		}
		if prev, ok := seen[v.Name]; ok {
			return fmt.Errorf("%s.name %q duplicates %s.additionalVolumes[%d].name", p, v.Name, path, prev)
		}
		seen[v.Name] = i
		size, err := resource.ParseQuantity(v.Size)
		if err != nil {
			return fmt.Errorf("bad format for %s.size: %v", p, err)
		}
		if size.Sign() <= 0 {
			return fmt.Errorf("%s.size must be greater than zero", p)
		}
	}
	return nil
}

// validateAppBinaryName checks the binary name, which is also the node container's name. Emptiness is
// left to the CRD's minLength.
func validateAppBinaryName(path, app string) error {
	if app == "" {
		return nil
	}
	return validateDNS1123Label(path, app)
}

// reservedPodContainerNames are the node Pod's built-in container names (internal/controllers/chainnode
// and internal/tmkms); the app container is named after .spec.app.app. A sidecar with one of these names
// would collide with it.
var reservedPodContainerNames = []string{
	"node-utils", "wait-cosmosigner-discovery", "link-genesis", "tmkms", "vault-token-renewer",
}

func validateSidecarNames(path string, config *Config, appName string) error {
	if config == nil {
		return nil
	}
	seen := make(map[string]int, len(config.Sidecars))
	for i, sidecar := range config.Sidecars {
		p := fmt.Sprintf("%s.sidecars[%d].name", path, i)
		if err := validateDNS1123Label(p, sidecar.Name); err != nil {
			return err
		}
		if sidecar.Name == appName || slices.Contains(reservedPodContainerNames, sidecar.Name) {
			return fmt.Errorf("%s %q collides with a built-in pod container", p, sidecar.Name)
		}
		if prev, ok := seen[sidecar.Name]; ok {
			return fmt.Errorf("%s %q duplicates %s.sidecars[%d].name", p, sidecar.Name, path, prev)
		}
		seen[sidecar.Name] = i
	}
	return nil
}

// validateGenesisDurations requires Go duration syntax: the values are written verbatim into
// genesis.json, which the chain parses with time.ParseDuration (the CRD's duration format also
// admits forms like "21d").
func validateGenesisDurations(path string, init *GenesisInitConfig) error {
	if init == nil {
		return nil
	}
	for _, d := range []struct {
		field string
		value *string
	}{
		{"unbondingTime", init.UnbondingTime},
		{"votingPeriod", init.VotingPeriod},
		{"expeditedVotingPeriod", init.ExpeditedVotingPeriod},
	} {
		if d.value == nil {
			continue
		}
		if _, err := time.ParseDuration(*d.value); err != nil {
			return fmt.Errorf("%s.%s %q must be a Go duration such as \"504h\": %v", path, d.field, *d.value, err)
		}
	}
	return nil
}

// validateNames checks the ChainNodeSet-level names that become object or container names, the legacy
// validator's volumes and sidecars, and genesis durations.
func (nodeSet *ChainNodeSet) validateNames() error {
	if err := validateAppBinaryName(".spec.app.app", nodeSet.Spec.App.App); err != nil {
		return err
	}
	for i, ingress := range nodeSet.Spec.Ingresses {
		if err := validateDNS1123Label(fmt.Sprintf(".spec.ingresses[%d].name", i), ingress.Name); err != nil {
			return err
		}
	}
	for i, route := range nodeSet.Spec.GatewayRoutes {
		if err := validateDNS1123Label(fmt.Sprintf(".spec.gatewayRoutes[%d].name", i), route.Name); err != nil {
			return err
		}
	}
	if v := nodeSet.Spec.Validator; v != nil {
		if err := validateAdditionalVolumes(".spec.validator.persistence", v.Persistence); err != nil {
			return err
		}
		if err := validateSidecarNames(".spec.validator.config", v.Config, nodeSet.Spec.App.App); err != nil {
			return err
		}
		if err := validateGenesisDurations(".spec.validator.init", v.Init); err != nil {
			return err
		}
	}
	return nil
}

// validateNodeSetGroupNames checks a group's name (part of every child ChainNode and Service name) and
// the volumes, sidecars and genesis durations of the group and its validator.
func validateNodeSetGroupNames(i int, group NodeGroupSpec, appName string) error {
	path := fmt.Sprintf(".spec.nodes[%d]", i)
	if err := validateDNS1123Label(path+".name", group.Name); err != nil {
		return err
	}
	// A validator group ignores its group-level persistence and config (its .validator ones apply), so
	// only a regular group's are checked.
	if group.Validator == nil {
		if err := validateAdditionalVolumes(path+".persistence", group.Persistence); err != nil {
			return err
		}
		if err := validateSidecarNames(path+".config", group.Config, appName); err != nil {
			return err
		}
	}
	if v := group.Validator; v != nil {
		if err := validateAdditionalVolumes(path+".validator.persistence", v.Persistence); err != nil {
			return err
		}
		if err := validateSidecarNames(path+".validator.config", v.Config, appName); err != nil {
			return err
		}
		if err := validateGenesisDurations(path+".validator.init", v.Init); err != nil {
			return err
		}
	}
	return nil
}
