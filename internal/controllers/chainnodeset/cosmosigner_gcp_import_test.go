package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

const nodeSetGcpDestinationKey = "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/consensus"

// gcpImportNodeSet is a validator group fronted by a signer that imports that validator's own
// consensus key into Cloud KMS. Genesis comes from outside, so the source key is user-supplied and
// no controller flow will ever recreate it.
func gcpImportNodeSet() *appsv1.ChainNodeSet {
	return &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators"},
				Backend: appsv1.CosmosignerBackend{GcpKMS: &appsv1.CosmosignerGcpKmsBackend{
					Import: &appsv1.CosmosignerGcpKmsImport{
						Project:  "example-project",
						Location: ptr.To("europe-west1"),
						KeyRing:  "validators",
						Key:      "consensus",
					},
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
}

// gcpImportNodeSetSource returns a source Secret holding a freshly generated consensus key together
// with its canonical base64 public key.
func gcpImportNodeSetSource(t *testing.T, nodeSet *appsv1.ChainNodeSet, name string) (*corev1.Secret, []byte, string) {
	t.Helper()
	material, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	key, err := cometbft.LoadPrivKey(material)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nodeSet.GetNamespace()},
		Data:       map[string][]byte{privKeyFilename: material},
	}, material, key.PubKey.Value
}

func gcpImportNodeSetStatefulSet(t *testing.T, nodeSet *appsv1.ChainNodeSet, name string, replicas int32) *k8sappsv1.StatefulSet {
	t.Helper()
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nodeSet.GetNamespace()},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(replicas)},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	return sts
}

func gcpImportNodeSetReplicas(t *testing.T, r *Reconciler, sts *k8sappsv1.StatefulSet) int32 {
	t.Helper()
	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	return ptr.Deref(fresh.Spec.Replicas, -1)
}

// TestNodeSetGcpImportKeepsSignerQuiescedUntilVerified is the PENDING_IMPORT gate on the ChainNodeSet
// path. `cosmosigner import` exits ZERO while Cloud KMS is still finalizing the imported version, so
// a recorded key version means only "a pod succeeded". Until that exact version has been read back
// and matched against the source, the import stays pending and the signer stays scaled to zero.
func TestNodeSetGcpImportKeepsSignerQuiescedUntilVerified(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, _, _ := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               signer.Name,
		ResourceName:       nodeSet.CosmosignerResourceName(signer),
		ImportedKeyVersion: nodeSetGcpDestinationKey + "/cryptoKeyVersions/1",
	}}
	sts := gcpImportNodeSetStatefulSet(t, nodeSet, nodeSet.CosmosignerResourceName(signer), 1)
	r := newValidatorTestReconciler(t, nodeSet, source, sts)

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.True(t, pending, "a succeeded import pod is not durable completion until the version is verified")
	require.Zero(t, gcpImportNodeSetReplicas(t, r, sts), "the signer must not serve an unverified imported key")
	require.Empty(t, nodeSet.GetCosmosignerStatus(signer.Name).KeyImported, "durable completion must not be recorded before verification")
}

// TestNodeSetGcpImportCompleteLeavesSignerRunning is the other half: once the record proves this
// exact source material was imported into this destination AND the serving version is recorded,
// reconciles are a no-op. A verified import must never quiesce a healthy signer.
func TestNodeSetGcpImportCompleteLeavesSignerRunning(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, material, _ := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	gcp := signer.Spec.Backend.GcpKMS
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               signer.Name,
		ResourceName:       nodeSet.CosmosignerResourceName(signer),
		ImportedKeyVersion: nodeSetGcpDestinationKey + "/cryptoKeyVersions/1",
		KeyImported:        gcp.ImportFingerprint(signer.SoftwareKeySecret, material),
	}}
	sts := gcpImportNodeSetStatefulSet(t, nodeSet, nodeSet.CosmosignerResourceName(signer), 1)
	r := newValidatorTestReconciler(t, nodeSet, source, sts)

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.False(t, pending)
	require.Equal(t, int32(1), gcpImportNodeSetReplicas(t, r, sts), "a verified import must not disturb the serving signer")
}

// TestNodeSetGcpImportRejectsChangedSourceMaterial covers the hard-error acceptance criterion: the
// destination crypto key already holds a DIFFERENT consensus identity for this source. Importing
// again would add a version holding new material and silently retarget the validator, so the import
// fails terminally and the signer is left quiesced.
func TestNodeSetGcpImportRejectsChangedSourceMaterial(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, _, _ := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	gcp := signer.Spec.Backend.GcpKMS
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               signer.Name,
		ResourceName:       nodeSet.CosmosignerResourceName(signer),
		ImportedKeyVersion: nodeSetGcpDestinationKey + "/cryptoKeyVersions/1",
		KeyImported:        gcp.ImportFingerprint(signer.SoftwareKeySecret, []byte("the-previously-imported-key")),
	}}
	sts := gcpImportNodeSetStatefulSet(t, nodeSet, nodeSet.CosmosignerResourceName(signer), 1)
	r := newValidatorTestReconciler(t, nodeSet, source, sts)

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, cosmosigner.Params{Name: sts.Name})
	require.Error(t, err)
	require.False(t, pending)
	require.Contains(t, err.Error(), "consensus")
	require.Zero(t, gcpImportNodeSetReplicas(t, r, sts), "a mismatched destination key must leave the signer quiesced")
}

// TestNodeSetGcpImportRequiresSourceThroughVerification enforces the backup-retention rule: the
// source key must remain readable until the imported version has been verified. Deleting it after
// the import pod succeeded (but before verification) leaves nothing to compare the backend against,
// so the controller reports it rather than adopting an unverified identity.
func TestNodeSetGcpImportRequiresSourceThroughVerification(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               signer.Name,
		ResourceName:       nodeSet.CosmosignerResourceName(signer),
		ImportedKeyVersion: nodeSetGcpDestinationKey + "/cryptoKeyVersions/1",
	}}
	sts := gcpImportNodeSetStatefulSet(t, nodeSet, nodeSet.CosmosignerResourceName(signer), 1)
	r := newValidatorTestReconciler(t, nodeSet, sts)

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, cosmosigner.Params{Name: sts.Name})
	require.False(t, pending)
	require.ErrorContains(t, err, signer.SoftwareKeySecret)
	require.Equal(t, int32(1), gcpImportNodeSetReplicas(t, r, sts), "an unreadable source is a configuration error, not a reason to stop a running signer")
}

// TestNodeSetGcpImportWaitsForGeneratedSourceKey keeps the genesis/create-validator flow working:
// the key the child ChainNode is about to generate is not an error, it is a wait.
func TestNodeSetGcpImportWaitsForGeneratedSourceKey(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	nodeSet.Spec.Nodes[0].Validator.PrivateKeySecret = nil
	nodeSet.Spec.Nodes[0].Validator.Init = &appsv1.GenesisInitConfig{
		ChainID: "test-localnet", Assets: []string{"100stake"}, StakeAmount: "1stake",
	}
	signer := resolveSingleSigner(t, nodeSet)
	sts := gcpImportNodeSetStatefulSet(t, nodeSet, nodeSet.CosmosignerResourceName(signer), 1)
	r := newValidatorTestReconciler(t, nodeSet, sts)

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.True(t, pending)
}

// TestNodeSetGcpImportBackendNeverFabricatesKeyVersion pins what the signer is configured with.
// Until the import resolves a version under the CONFIGURED destination there is no key version at
// all — a guessed `/cryptoKeyVersions/1` would point the validator at whatever happened to be
// created first.
func TestNodeSetGcpImportBackendNeverFabricatesKeyVersion(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	r := newValidatorTestReconciler(t, nodeSet)

	backend, err := r.cosmosignerBackend(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	require.NotNil(t, backend.GCP)
	require.Empty(t, backend.GCP.KeyVersion, "an unresolved managed import has no key version")
	require.NotNil(t, backend.GCP.Import)
	require.Equal(t, "europe-west1", backend.GCP.Import.Location)
	require.Equal(t, "consensus-import", backend.GCP.Import.ImportJob, "the CLI default must be rendered explicitly")
	require.Equal(t, appsv1.CosmosignerGcpProtectionSoftware, backend.GCP.Import.ProtectionLevel)
	require.Equal(t, nodeSetGcpDestinationKey, backend.GCP.Import.CryptoKeyName())

	// A version recorded for a previous destination is not this destination's version.
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               signer.Name,
		ImportedKeyVersion: "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/old/cryptoKeyVersions/4",
	}}
	backend, err = r.cosmosignerBackend(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	require.Empty(t, backend.GCP.KeyVersion)

	version := nodeSetGcpDestinationKey + "/cryptoKeyVersions/4"
	nodeSet.Status.Cosmosigners[0].ImportedKeyVersion = version
	backend, err = r.cosmosignerBackend(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	require.Equal(t, version, backend.GCP.KeyVersion)

	// A pre-provisioned signer is untouched by any of this.
	preProvisioned := gcpImportNodeSet()
	preProvisioned.Spec.Cosmosigner.Backend.GcpKMS = &appsv1.CosmosignerGcpKmsBackend{KeyVersion: version}
	preSigner := resolveSingleSigner(t, preProvisioned)
	preProvisioned.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               preSigner.Name,
		ImportedKeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/9",
	}}
	backend, err = r.cosmosignerBackend(context.Background(), preProvisioned, preSigner)
	require.NoError(t, err)
	require.Equal(t, version, backend.GCP.KeyVersion)
	require.Nil(t, backend.GCP.Import)
}

// TestNodeSetCosmosignerParamsThreadsImagePullSecrets lets the one-shot import/pubkey pods pull the
// cosmosigner image from the same private registry as the group they front. The signer StatefulSet
// deliberately does not carry them (that would change every existing signer's lifecycle digest).
func TestNodeSetCosmosignerParamsThreadsImagePullSecrets(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	pullSecrets := []corev1.LocalObjectReference{{Name: "registry-creds"}}
	nodeSet.Spec.Nodes[0].Validator.Config = &appsv1.Config{ImagePullSecrets: pullSecrets}
	signer := resolveSingleSigner(t, nodeSet)
	r := newValidatorTestReconciler(t, nodeSet)

	params, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	require.Equal(t, pullSecrets, params.ImagePullSecrets)
}

// TestNodeSetCosmosignerPublicKeyForGcpImportReadsSource records that the EXPECTED consensus key of
// a managed import is the source key itself — resolved from the Secret, exactly as Vault's
// uploadGenerated does, with no pod and no backend round-trip (the destination version does not
// exist yet, and this reconciler has no clientset to run a pod with).
func TestNodeSetCosmosignerPublicKeyForGcpImportReadsSource(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, _, publicKey := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	r := newValidatorTestReconciler(t, nodeSet, source)

	got, err := r.cosmosignerPublicKey(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	require.Equal(t, publicKey, got)
}

// TestPrepareCosmosignerImportsCoversGcpImport drives the gate the child ChainNodes depend on: an
// unverified import must report "not ready" so validators keep their existing signing path, and a
// verified one must let reconciliation proceed without blocking the signer's targets.
func TestPrepareCosmosignerImportsCoversGcpImport(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, material, _ := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               signer.Name,
		ResourceName:       nodeSet.CosmosignerResourceName(signer),
		ImportedKeyVersion: nodeSetGcpDestinationKey + "/cryptoKeyVersions/1",
	}}
	sts := gcpImportNodeSetStatefulSet(t, nodeSet, nodeSet.CosmosignerResourceName(signer), 1)
	r := newValidatorTestReconciler(t, nodeSet, source, sts)

	_, ready, err := r.prepareCosmosignerImports(context.Background(), nodeSet)
	require.NoError(t, err)
	require.False(t, ready, "children must not be retargeted while the imported version is unverified")

	// Verified: the same call is a no-op that blocks nothing.
	verified := gcpImportNodeSet()
	verifiedSigner := resolveSingleSigner(t, verified)
	gcp := verifiedSigner.Spec.Backend.GcpKMS
	verified.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               verifiedSigner.Name,
		ResourceName:       verified.CosmosignerResourceName(verifiedSigner),
		ImportedKeyVersion: nodeSetGcpDestinationKey + "/cryptoKeyVersions/1",
		KeyImported:        gcp.ImportFingerprint(verifiedSigner.SoftwareKeySecret, material),
	}}
	verifiedSource, _, _ := gcpImportNodeSetSource(t, verified, verifiedSigner.SoftwareKeySecret)
	verifiedSource.Data[privKeyFilename] = material
	r = newValidatorTestReconciler(t, verified, verifiedSource)

	blocked, ready, err := r.prepareCosmosignerImports(context.Background(), verified)
	require.NoError(t, err)
	require.True(t, ready)
	require.NotContains(t, blocked, verifiedSigner.Name)
}

// TestPreflightCosmosignersCoversGcpImportSource runs the read-only source check before children are
// retargeted and before anything is created in Cloud KMS, so a signer that can never import does not
// trap the operator into its first replica/storage choice.
func TestPreflightCosmosignersCoversGcpImportSource(t *testing.T) {
	missing := gcpImportNodeSet()
	signer := resolveSingleSigner(t, missing)
	r := newValidatorTestReconciler(t, missing)
	require.ErrorContains(t, r.preflightCosmosigners(context.Background(), missing), signer.SoftwareKeySecret)

	present := gcpImportNodeSet()
	source, material, _ := gcpImportNodeSetSource(t, present, signer.SoftwareKeySecret)
	r = newValidatorTestReconciler(t, present, source)
	require.NoError(t, r.preflightCosmosigners(context.Background(), present))

	// Once the import is recorded for this destination and source, the bootstrap Secret is no longer
	// required: Cloud KMS already holds the verified key.
	imported := gcpImportNodeSet()
	importedSigner := resolveSingleSigner(t, imported)
	imported.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:        importedSigner.Name,
		KeyImported: importedSigner.Spec.Backend.GcpKMS.ImportFingerprint(importedSigner.SoftwareKeySecret, material),
	}}
	r = newValidatorTestReconciler(t, imported)
	require.NoError(t, r.preflightCosmosigners(context.Background(), imported))
}

// TestNodeSetInitCosmosignerReplacementNamesCarriesImportedKeyVersion keeps a manifest placement move
// (top-level .spec.cosmosigner ↔ group .cosmosigner, same resource name) from importing the key a
// second time: the replacement inherits the exact version the previous entry created, so the state
// machine resumes verification instead of creating another crypto key version.
func TestNodeSetInitCosmosignerReplacementNamesCarriesImportedKeyVersion(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	// Move the signer from the top-level block onto the group: same resource name, new signer name.
	nodeSet.Spec.Nodes[0].Cosmosigner = nodeSet.Spec.Cosmosigner.DeepCopy()
	nodeSet.Spec.Nodes[0].Cosmosigner.NodeGroups = nil
	nodeSet.Spec.Cosmosigner = nil
	replacement := resolveSingleSigner(t, nodeSet)

	version := nodeSetGcpDestinationKey + "/cryptoKeyVersions/1"
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:               "test-nodeset-signer",
		ResourceName:       "test-nodeset-signer",
		TargetGroups:       []string{"validators"},
		ImportedKeyVersion: version,
	}}
	live := gcpImportNodeSetStatefulSet(t, nodeSet, "test-nodeset-signer", 1)
	r := newValidatorTestReconciler(t, nodeSet, live)

	recorded, err := r.initCosmosignerReplacementNames(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, recorded)

	status := nodeSet.GetCosmosignerStatus(replacement.Name)
	require.NotNil(t, status)
	require.Equal(t, version, status.ImportedKeyVersion, "a placement move must not orphan the created crypto key version")
}
