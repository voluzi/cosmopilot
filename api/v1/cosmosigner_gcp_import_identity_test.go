package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// gcpImportBackend builds a managed-import GCP backend pointing at the given key ring/key.
func gcpImportBackend(keyRing, key string) CosmosignerBackend {
	return CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{Import: &CosmosignerGcpKmsImport{
		Project: "example-project", KeyRing: keyRing, Key: key,
	}}}
}

// TestGcpImportUsesLocalValidatorKey closes the double-sign hole a managed import would otherwise
// open: unlike a pre-provisioned GCP key version, a managed import CONSUMES the targeted
// validator's local priv-key secret as its source, so that secret is live consensus material and
// LocalKeyEverServed must be recorded as true.
func TestGcpImportUsesLocalValidatorKey(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ns"},
		Spec: ChainNodeSetSpec{
			Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
			Cosmosigner: &Cosmosigner{Backend: gcpImportBackend("validators", "consensus")},
		},
	}
	signers := nodeSet.ResolveCosmosigners()
	if len(signers) != 1 {
		t.Fatalf("expected one resolved signer, got %d", len(signers))
	}
	if !nodeSet.SignerUsesLocalValidatorKey(signers[0]) {
		t.Fatal("a managed GCP import consumes the targeted validator's local key and must report it")
	}

	preProvisioned := nodeSet.DeepCopy()
	preProvisioned.Spec.Cosmosigner = &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{
		KeyVersion: "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
	}}}
	if preProvisioned.SignerUsesLocalValidatorKey(preProvisioned.ResolveCosmosigners()[0]) {
		t.Fatal("a pre-provisioned GCP key version must keep reporting that no local key is used")
	}

	// A sentry signer has no validator key to import from at all.
	sentry := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ns"},
		Spec: ChainNodeSetSpec{
			Nodes:       []NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
			Cosmosigner: &Cosmosigner{NodeGroups: []string{"sentries"}, Backend: gcpImportBackend("validators", "consensus")},
		},
	}
	for _, s := range sentry.ResolveCosmosigners() {
		if sentry.SignerUsesLocalValidatorKey(s) {
			t.Fatal("a sentry signer never consumes a validator's local key")
		}
	}
}

// TestGcpImportKeepsValidatorLocalKeyReserved verifies the uniqueness consequence of the predicate
// above: because the import reads the validator's local priv-key secret, that secret still holds
// the live consensus key and must stay reserved against every other validator.
func TestGcpImportKeepsValidatorLocalKeyReserved(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ns"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{
				Name:        "a",
				Instances:   ptr.To(1),
				Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")},
				Cosmosigner: &Cosmosigner{Backend: gcpImportBackend("validators", "consensus")},
			},
			{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
		}},
	}
	err := nodeSet.validateUniqueSigningKeys()
	if err == nil {
		t.Fatal("a GCP-import signer's source key must stay reserved against another validator")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected a descriptive uniqueness error")
	}

	// The pre-provisioned form is unchanged: its validator's local secret provably never served.
	preProvisioned := nodeSet.DeepCopy()
	preProvisioned.Spec.Nodes[0].Cosmosigner = &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{
		KeyVersion: "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
	}}}
	if err := preProvisioned.validateUniqueSigningKeys(); err != nil {
		t.Fatalf("pre-provisioned GCP signers must keep releasing the unused local key: %v", err)
	}
}

// TestGcpImportDestinationUniqueness rejects two signers importing into the SAME Cloud KMS crypto
// key. They would share one destination (and one import job) while running independent raft
// clusters, and their signer digests — derived from the destination — would be indistinguishable.
func TestGcpImportDestinationUniqueness(t *testing.T) {
	build := func(secondKey string) *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{
					Name:        "a",
					Instances:   ptr.To(1),
					Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("a-priv-key")},
					Cosmosigner: &Cosmosigner{Backend: gcpImportBackend("validators", "consensus")},
				},
				{
					Name:        "b",
					Instances:   ptr.To(1),
					Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("b-priv-key")},
					Cosmosigner: &Cosmosigner{Backend: gcpImportBackend("validators", secondKey)},
				},
			}},
		}
	}

	if err := build("consensus").validateUniqueSigningKeys(); err == nil {
		t.Fatal("two managed imports into the same crypto key must be rejected")
	}
	if err := build("consensus-b").validateUniqueSigningKeys(); err != nil {
		t.Fatalf("managed imports into distinct crypto keys must be accepted: %v", err)
	}
}
