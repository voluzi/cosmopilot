package v1

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	// DefaultCosmosignerImage is the cosmosigner image used when none is specified (either via
	// .spec.cosmosigner.image or the operator-wide cosmosignerImage Helm value / COSMOSIGNER_IMAGE).
	DefaultCosmosignerImage = "ghcr.io/voluzi/cosmosigner:0.1.0"

	// DefaultCosmosignerReplicas is the default number of signer replicas (single instance).
	DefaultCosmosignerReplicas int32 = 1

	// DefaultCosmosignerStateStorageSize is the default size of the per-replica state PVC that
	// holds the raft double-sign-protection state and the connection key.
	DefaultCosmosignerStateStorageSize = "1Gi"

	// DefaultCosmosignerVaultMount is the default Vault transit mount path.
	DefaultCosmosignerVaultMount = "transit"

	// DefaultCosmosignerCpu and DefaultCosmosignerMemory are the default compute resources.
	DefaultCosmosignerCpu    = "100m"
	DefaultCosmosignerMemory = "128Mi"

	// DefaultCosmosignerLogLevel is the default log level for the signer.
	DefaultCosmosignerLogLevel = "info"
)

// Cosmosigner configures a Cosmopilot-managed cosmosigner remote-signer deployment
// (github.com/voluzi/cosmosigner). Unlike TmKMS, which runs as a sidecar in the validator
// pod, cosmosigner runs as a separate StatefulSet that dials the targeted nodes'
// priv_validator_laddr over the network. This allows any group of nodes to act as the
// signing endpoint for a single consensus identity (horcrux-style fan-out), and enables
// raft-based high availability across multiple signer replicas.
//
// On a ChainNodeSet, .nodeGroups selects which node groups the signer connects to; when it
// is empty and a validator is configured, the validator group is targeted by default. On a
// standalone ChainNode, the ChainNode itself is the target and .nodeGroups must be empty.
type Cosmosigner struct {
	// NodeGroups is the list of node group names (.spec.nodes[].name) the signer will connect
	// to and sign for. Only valid on a ChainNodeSet. When empty, the configured validator group
	// is targeted by default. Every targeted node listens for the signer and shares the single
	// consensus identity held by the configured backend.
	// +optional
	NodeGroups []string `json:"nodeGroups,omitempty"`

	// Replicas is the number of signer instances to run. Must be an odd number so the embedded
	// raft cluster can form a quorum. Defaults to `1` (a single-instance signer with no HA).
	// +optional
	// +default=1
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image is the cosmosigner container image to use. Defaults to the operator-wide cosmosigner
	// image (configured via the `-cosmosigner-image`/`COSMOSIGNER_IMAGE` operator flag, itself
	// defaulting to `ghcr.io/voluzi/cosmosigner:0.1.0`). Set this to pin or override the image for
	// this specific signer only.
	// +optional
	Image *string `json:"image,omitempty"`

	// Backend selects and configures where the consensus key material lives and how signing is
	// performed. Exactly one backend must be configured.
	Backend CosmosignerBackend `json:"backend"`

	// StateStorageSize is the size of the per-replica PVC used for the raft double-sign
	// protection state and the persisted connection key. Defaults to `1Gi`.
	// +optional
	StateStorageSize *string `json:"stateStorageSize,omitempty"`

	// StorageClassName is the storage class for the per-replica state PVC. Defaults to the
	// cluster default storage class when unset.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Resources are the compute resources for the signer container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// RaftTLSSecret is the name of a secret containing `tls.crt`, `tls.key` and `ca.crt` used to
	// secure the inter-replica raft transport with mutual TLS. When unset, the raft transport is
	// plain TCP (only safe on a trusted network).
	// +optional
	RaftTLSSecret *string `json:"raftTLSSecret,omitempty"`

	// LogLevel is the log level for the signer. Defaults to `info`.
	// +optional
	LogLevel *string `json:"logLevel,omitempty"`

	// ServiceAccountName is the Kubernetes service account the signer pods run as. Required in
	// practice for the GCP KMS backend without credentialsSecret (Workload Identity binds the Google
	// service account to a specific Kubernetes service account). Defaults to the namespace default.
	// +optional
	ServiceAccountName *string `json:"serviceAccountName,omitempty"`
}

// CosmosignerBackend selects the signing backend. Exactly one field must be set.
type CosmosignerBackend struct {
	// Software uses a local ed25519 priv_validator_key.json held in a Kubernetes secret. This is
	// the simplest backend and is mainly intended for testnets and testing.
	// +optional
	Software *CosmosignerSoftwareBackend `json:"software,omitempty"`

	// Vault uses a non-exportable ed25519 key in HashiCorp Vault Transit.
	// +optional
	Vault *CosmosignerVaultBackend `json:"vault,omitempty"`

	// GcpKMS uses a non-exportable EC_SIGN_ED25519 key in Google Cloud KMS.
	// +optional
	GcpKMS *CosmosignerGcpKmsBackend `json:"gcpKms,omitempty"`
}

// CosmosignerSoftwareBackend configures the local software signing backend.
type CosmosignerSoftwareBackend struct {
	// PrivateKeySecret is the name of the secret holding `priv_validator_key.json`. When the signer
	// targets a validator this must be left empty — the validator's own key secret is used (created
	// by its genesis/create-validator flow when it generates one). For a sentry-mode signer (no
	// validator targeted) it is required and must reference a pre-provisioned secret whose consensus
	// key is already registered on-chain (e.g. via init.genesisValidators): a fresh key is never
	// generated here, since it could not be in the validator set.
	// +optional
	PrivateKeySecret *string `json:"privateKeySecret,omitempty"`
}

// CosmosignerVaultBackend configures the HashiCorp Vault Transit signing backend.
type CosmosignerVaultBackend struct {
	// Address is the full address of the Vault cluster (e.g. `https://vault:8200`).
	Address string `json:"address"`

	// KeyName is the name of the Vault transit key used for signing.
	KeyName string `json:"keyName"`

	// Mount is the Vault transit mount path. Defaults to `transit`.
	// +optional
	// +default="transit"
	Mount *string `json:"mount,omitempty"`

	// TokenSecret references the secret containing the Vault token used to authenticate.
	TokenSecret *corev1.SecretKeySelector `json:"tokenSecret"`

	// CertificateSecret references the secret containing the CA certificate of the Vault cluster.
	// +optional
	CertificateSecret *corev1.SecretKeySelector `json:"certificateSecret,omitempty"`

	// Namespace is the Vault namespace (Vault Enterprise), when applicable.
	// +optional
	Namespace *string `json:"namespace,omitempty"`

	// UploadGenerated indicates that the controller should generate a consensus key locally and
	// import it into Vault. Defaults to `false`. It is set to `true` automatically when this
	// validator initializes a new genesis. This should not be used in production.
	// +optional
	UploadGenerated bool `json:"uploadGenerated,omitempty"`

	// AutoRenewToken indicates whether to run a sidecar that automatically renews the Vault token.
	// Defaults to `false`.
	// +optional
	AutoRenewToken bool `json:"autoRenewToken,omitempty"`
}

// CosmosignerGcpKmsBackend configures the Google Cloud KMS signing backend.
type CosmosignerGcpKmsBackend struct {
	// KeyVersion is the full resource name of the KMS crypto key version used for signing
	// (e.g. `projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1`).
	KeyVersion string `json:"keyVersion"`

	// CredentialsSecret references a secret containing a Google service account JSON key. When
	// unset, Workload Identity / Application Default Credentials are used.
	// +optional
	CredentialsSecret *corev1.SecretKeySelector `json:"credentialsSecret,omitempty"`
}

// CosmosignerStatus is the controller-recorded state of one managed cosmosigner deployment. All
// fields are controller-managed and not meant to be set by hand.
type CosmosignerStatus struct {
	// Name is the signer's resource name (<nodeset>-signer | <nodeset>-<group>-signer |
	// <nodeset>-<group>-<index>-signer) and the key of this entry.
	Name string `json:"name"`

	// Replicas records the raft replica count this signer was rolled out with, so the no-webhook
	// reconcile path can reject a later replica change: scaling the embedded raft cluster is not a
	// plain Kubernetes scale, since the membership baked into the existing per-pod raft state is not
	// migrated by rendering a new bootstrap list.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// StateStorageSize records the per-replica raft-state PVC size this signer was rolled out with.
	// Together with StateStorageClassName it locks the PVC template while the signer (or its
	// still-terminating PVCs, on a remove-and-re-add) exists: StatefulSet volumeClaimTemplates cannot
	// be updated and surviving claims would be re-bound at their old size/class.
	// +optional
	StateStorageSize string `json:"stateStorageSize,omitempty"`

	// StateStorageClassName records the storage class of this signer's raft-state PVCs, mirroring the
	// spec's storageClassName semantics: absent (nil) means the cluster default class was selected,
	// while an explicit "" means no class was requested. See StateStorageSize.
	// +optional
	StateStorageClassName *string `json:"stateStorageClassName,omitempty"`

	// SigningDigest is a fingerprint of this signer's effective signing identity, replica count and
	// target-group set, captured once it has rolled out. It lets the no-webhook reconcile path reject
	// a later change to the signing configuration that would make the validator sign with a key not in
	// the on-chain validator set.
	// +optional
	SigningDigest string `json:"signingDigest,omitempty"`

	// AtEstablishment is a write-once record of the on-chain consensus identity this signer was
	// responsible for at the moment the chain ID was first recorded: its validator-targeted identity,
	// or — for a SOFTWARE sentry signer whose privateKeySecret is listed in this ChainNodeSet's own
	// init.genesisValidators — that sentry key's identity. It is the empty string when the signer was
	// responsible for no such provable on-chain key then; this includes sentries the controller cannot
	// tie to a genesis entry from spec alone (a Vault/GCP-backed sentry, or a sentry for an
	// externally-imported genesis), which stay rotatable/removable on the no-webhook path. It lets that
	// path tell an establishing signer (admitted) apart from one introduced afterwards (rejected unless
	// the backend provably imports the registered key), and reject a later key change/removal of a
	// genesis-registered software sentry signer. Absent for a signer added after establishment.
	// +optional
	AtEstablishment *string `json:"atEstablishment,omitempty"`

	// ServingIdentity records the effective signing identity this validator-targeted signer served,
	// captured with SigningDigest and cleared on teardown. It lets the no-webhook path judge a REMOVAL:
	// removal is only admitted when the served validator still resolves this identity through its own
	// signing path (e.g. a software-backed signer using the validator's own key).
	// +optional
	ServingIdentity string `json:"servingIdentity,omitempty"`

	// ServingGroup records which validator group this signer served (the reserved "validator" name for
	// the legacy singleton). The removal guard checks that this specific validator's own path resolves
	// ServingIdentity — a different validator referencing the same key must not satisfy it.
	// +optional
	ServingGroup string `json:"servingGroup,omitempty"`

	// ServingInstance records which instance of ServingGroup this signer served (for a per-instance
	// signer of a multi-instance validator group). Nil for a whole-group / legacy-singleton signer.
	// +optional
	ServingInstance *int `json:"servingInstance,omitempty"`

	// KeyImported is the fingerprint of a completed Vault key import (Vault target + source secret +
	// key material). It lets the controller skip a repeated import and detect a source/target change.
	// +optional
	KeyImported string `json:"keyImported,omitempty"`
}
