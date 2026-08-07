package v1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// registeredKeyRule is the fragment of the error that forces a validator registering a freshly
// generated consensus key to sign with that same key. A managed GCP import satisfies the rule (it
// imports exactly that key), so the message must stop naming only the two older backends.
const registeredKeyRule = "so the registered consensus key matches the signer"

func gcpImportChainNode(name string, validator *ValidatorConfig) *ChainNode {
	return &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ChainNodeSpec{
			App:         AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Genesis:     &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Validator:   validator,
			Cosmosigner: &Cosmosigner{Backend: gcpImportBackend("validators", "consensus")},
		},
	}
}

// TestChainNodeGcpImportRequiresValidatorSource pins the ChainNode half of decision #2: a managed
// import consumes the node's OWN validator key, so a node that has no such key — a sentry, or an
// external-genesis validator that never generates one — has nothing to import. Accepting it would
// create an empty Cloud KMS key and quiesce the signer forever.
func TestChainNodeGcpImportRequiresValidatorSource(t *testing.T) {
	const wantNoValidator = "requires the node to be a validator"
	const wantNoKey = "initialize genesis, use createValidator, or set an explicit privateKeySecret"

	sentry := gcpImportChainNode("sentry", nil)
	if _, err := sentry.Validate(nil); err == nil || !strings.Contains(err.Error(), wantNoValidator) {
		t.Fatalf("GCP import on a non-validator must be rejected, got: %v", err)
	}

	external := gcpImportChainNode("external", &ValidatorConfig{})
	if _, err := external.Validate(nil); err == nil || !strings.Contains(err.Error(), wantNoKey) {
		t.Fatalf("GCP import on an external-genesis validator with no key must be rejected, got: %v", err)
	}

	explicit := gcpImportChainNode("explicit", &ValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")})
	if _, err := explicit.Validate(nil); err != nil {
		t.Fatalf("GCP import of an explicitly supplied validator key must be accepted, got: %v", err)
	}

	created := gcpImportChainNode("created", &ValidatorConfig{CreateValidator: &CreateValidatorConfig{}})
	if _, err := created.Validate(nil); err != nil {
		t.Fatalf("GCP import on a createValidator node must be accepted, got: %v", err)
	}
}

// TestChainNodeGcpImportSatisfiesRegisteredKeyRule is the flip side: a managed import DOES make the
// backend hold the registered key, so it must pass the rule that rejects a pre-provisioned key —
// while the pre-provisioned form stays rejected, because nothing proves it holds the same identity.
func TestChainNodeGcpImportSatisfiesRegisteredKeyRule(t *testing.T) {
	imports := gcpImportChainNode("genesis", &ValidatorConfig{Init: &GenesisInitConfig{ChainID: "test-1"}})
	imports.Spec.Genesis = nil
	if _, err := imports.Validate(nil); err != nil {
		t.Fatalf("genesis-init validator with a managed GCP import must be accepted, got: %v", err)
	}

	preProvisioned := gcpImportChainNode("preprovisioned", &ValidatorConfig{Init: &GenesisInitConfig{ChainID: "test-1"}})
	preProvisioned.Spec.Genesis = nil
	preProvisioned.Spec.Cosmosigner.Backend = CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{
		KeyVersion: "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
	}}
	if _, err := preProvisioned.Validate(nil); err == nil || !strings.Contains(err.Error(), registeredKeyRule) {
		t.Fatalf("pre-provisioned GCP key on a genesis-init validator must stay rejected, got: %v", err)
	}
}

func gcpImportNodeSetFor(group NodeGroupSpec) *ChainNodeSet {
	return &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			App:         AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Genesis:     &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes:       []NodeGroupSpec{group},
			Cosmosigner: &Cosmosigner{NodeGroups: []string{group.Name}, Backend: gcpImportBackend("validators", "consensus")},
		},
	}
}

// TestNodeSetGcpImportRequiresValidatorTarget mirrors the ChainNode rule on the ChainNodeSet path,
// where the source is the TARGETED validator's key rather than the signer's own node.
func TestNodeSetGcpImportRequiresValidatorTarget(t *testing.T) {
	sentry := gcpImportNodeSetFor(NodeGroupSpec{Name: "sentries", Instances: ptr.To(1)})
	if _, err := sentry.Validate(nil); err == nil ||
		!strings.Contains(err.Error(), "requires targeting a validator whose generated key can be imported") {
		t.Fatalf("GCP import on a sentry-only signer must be rejected, got: %v", err)
	}

	external := gcpImportNodeSetFor(NodeGroupSpec{
		Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{},
	})
	if _, err := external.Validate(nil); err == nil ||
		!strings.Contains(err.Error(), "initialize genesis, use createValidator, or set an explicit privateKeySecret") {
		t.Fatalf("GCP import targeting a validator with no key must be rejected, got: %v", err)
	}

	explicit := gcpImportNodeSetFor(NodeGroupSpec{
		Name: "validators", Instances: ptr.To(1),
		Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
	})
	if _, err := explicit.Validate(nil); err != nil {
		t.Fatalf("GCP import of an explicitly supplied validator key must be accepted, got: %v", err)
	}
}

// TestNodeSetGcpImportSatisfiesRegisteredKeyRule is the ChainNodeSet counterpart of the
// registered-key rule: the managed import passes it, the pre-provisioned key version does not.
func TestNodeSetGcpImportSatisfiesRegisteredKeyRule(t *testing.T) {
	imports := gcpImportNodeSetFor(NodeGroupSpec{
		Name: "validators", Instances: ptr.To(1),
		Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}},
	})
	if _, err := imports.Validate(nil); err != nil {
		t.Fatalf("createValidator group with a managed GCP import must be accepted, got: %v", err)
	}

	preProvisioned := gcpImportNodeSetFor(NodeGroupSpec{
		Name: "validators", Instances: ptr.To(1),
		Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}},
	})
	preProvisioned.Spec.Cosmosigner.Backend = CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{
		KeyVersion: "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
	}}
	if _, err := preProvisioned.Validate(nil); err == nil || !strings.Contains(err.Error(), registeredKeyRule) {
		t.Fatalf("pre-provisioned GCP key on a createValidator group must stay rejected, got: %v", err)
	}
}
