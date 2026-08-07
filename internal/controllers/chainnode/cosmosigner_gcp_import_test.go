package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

const gcpDestinationKey = "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/consensus"

func gcpImportNode() *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
				GcpKMS: &appsv1.CosmosignerGcpKmsBackend{Import: &appsv1.CosmosignerGcpKmsImport{
					Project:  "example-project",
					Location: ptr.To("europe-west1"),
					KeyRing:  "validators",
					Key:      "consensus",
				}},
			}},
		},
	}
}

func gcpImportTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	return scheme
}

// gcpImportSourceSecret returns a source Secret holding a freshly generated consensus key together
// with its canonical base64 public key.
func gcpImportSourceSecret(t *testing.T, namespace, name string) (*corev1.Secret, []byte, string) {
	t.Helper()
	material, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	key, err := cometbft.LoadPrivKey(material)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: material},
	}, material, key.PubKey.Value
}

func gcpImportSignerStatefulSet(t *testing.T, chainNode *appsv1.ChainNode, scheme *runtime.Scheme, replicas int32) *k8sappsv1.StatefulSet {
	t.Helper()
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(replicas)},
	}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	return sts
}

func gcpImportSignerReplicas(t *testing.T, c client.Client, sts *k8sappsv1.StatefulSet) int32 {
	t.Helper()
	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	return ptr.Deref(fresh.Spec.Replicas, -1)
}

// TestGcpImportKeepsSignerQuiescedUntilVerified is the PENDING_IMPORT gate. `cosmosigner import`
// exits ZERO while Cloud KMS is still finalizing the imported version, so a recorded key version
// means only "a pod succeeded". Until the version has been read back and matched against the source,
// the import stays pending and the signer stays scaled to zero.
func TestGcpImportKeepsSignerQuiescedUntilVerified(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, _, _ := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	chainNode.Status.CosmosignerImportedKeyVersion = gcpDestinationKey + "/cryptoKeyVersions/1"

	sts := gcpImportSignerStatefulSet(t, chainNode, scheme, 1)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, sts).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.True(t, pending, "a succeeded import pod is not durable completion until the version is verified")
	require.Zero(t, gcpImportSignerReplicas(t, c, sts), "the signer must not serve an unverified imported key")
	require.Empty(t, chainNode.Status.CosmosignerKeyImported, "durable completion must not be recorded before verification")
}

// TestGcpImportCompleteLeavesSignerRunning is the other half: once the record proves this exact
// source material was imported into this destination AND the serving version is recorded, reconciles
// are a no-op. A verified import must never quiesce a healthy signer.
func TestGcpImportCompleteLeavesSignerRunning(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, material, _ := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	gcp := chainNode.Spec.Cosmosigner.Backend.GcpKMS
	chainNode.Status.CosmosignerImportedKeyVersion = gcpDestinationKey + "/cryptoKeyVersions/1"
	chainNode.Status.CosmosignerKeyImported = gcp.ImportFingerprint("validator-key", material)

	sts := gcpImportSignerStatefulSet(t, chainNode, scheme, 1)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, sts).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.False(t, pending)
	require.Equal(t, int32(1), gcpImportSignerReplicas(t, c, sts), "a verified import must not disturb the serving signer")
}

// TestGcpImportRejectsChangedSourceMaterial covers the hard-error acceptance criterion: the
// destination crypto key already holds a DIFFERENT consensus identity for this source. Importing
// again would add a version holding new material and silently retarget the validator, so the import
// fails terminally and the signer is left quiesced.
func TestGcpImportRejectsChangedSourceMaterial(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, _, _ := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	gcp := chainNode.Spec.Cosmosigner.Backend.GcpKMS
	chainNode.Status.CosmosignerImportedKeyVersion = gcpDestinationKey + "/cryptoKeyVersions/1"
	chainNode.Status.CosmosignerKeyImported = gcp.ImportFingerprint("validator-key", []byte("the-previously-imported-key"))

	sts := gcpImportSignerStatefulSet(t, chainNode, scheme, 1)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, sts).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.Error(t, err)
	require.False(t, pending)
	require.Contains(t, err.Error(), "consensus")
	require.Zero(t, gcpImportSignerReplicas(t, c, sts), "a mismatched destination key must leave the signer quiesced")
}

// TestGcpImportRequiresSourceThroughVerification enforces the backup-retention rule: the source key
// must remain readable until the imported version has been verified. Deleting it after the import
// pod succeeded (but before verification) leaves nothing to compare the backend against, so the
// controller reports it rather than adopting an unverified identity.
func TestGcpImportRequiresSourceThroughVerification(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	chainNode.Status.CosmosignerImportedKeyVersion = gcpDestinationKey + "/cryptoKeyVersions/1"

	sts := gcpImportSignerStatefulSet(t, chainNode, scheme, 1)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.False(t, pending)
	require.ErrorContains(t, err, "validator-key")
	require.Equal(t, int32(1), gcpImportSignerReplicas(t, c, sts), "an unreadable source is a configuration error, not a reason to stop a running signer")
}

// TestGcpImportWaitsForGeneratedSourceKey keeps the genesis/create-validator flow working: the key
// Cosmopilot is about to generate is not an error, it is a wait.
func TestGcpImportWaitsForGeneratedSourceKey(t *testing.T) {
	chainNode := gcpImportNode()
	chainNode.Spec.Validator.PrivateKeySecret = nil
	chainNode.Spec.Validator.Init = &appsv1.GenesisInitConfig{ChainID: "test-1"}
	scheme := gcpImportTestScheme(t)

	sts := gcpImportSignerStatefulSet(t, chainNode, scheme, 1)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.True(t, pending)
}

// TestGcpImportBackendNeverFabricatesKeyVersion pins what the signer is configured with. Until the
// import resolves a version under the CONFIGURED destination there is no key version at all — a
// guessed `/cryptoKeyVersions/1` would point the validator at whatever happened to be created first.
func TestGcpImportBackendNeverFabricatesKeyVersion(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	backend, err := r.cosmosignerBackend(context.Background(), chainNode)
	require.NoError(t, err)
	require.NotNil(t, backend.GCP)
	require.Empty(t, backend.GCP.KeyVersion, "an unresolved managed import has no key version")
	require.NotNil(t, backend.GCP.Import)
	require.Equal(t, "europe-west1", backend.GCP.Import.Location)
	require.Equal(t, "consensus-import", backend.GCP.Import.ImportJob, "the CLI default must be rendered explicitly")
	require.Equal(t, appsv1.CosmosignerGcpProtectionSoftware, backend.GCP.Import.ProtectionLevel)
	require.Equal(t, gcpDestinationKey, backend.GCP.Import.CryptoKeyName())

	// A version recorded for a previous destination is not this destination's version.
	chainNode.Status.CosmosignerImportedKeyVersion = "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/old/cryptoKeyVersions/4"
	backend, err = r.cosmosignerBackend(context.Background(), chainNode)
	require.NoError(t, err)
	require.Empty(t, backend.GCP.KeyVersion)

	version := gcpDestinationKey + "/cryptoKeyVersions/4"
	chainNode.Status.CosmosignerImportedKeyVersion = version
	backend, err = r.cosmosignerBackend(context.Background(), chainNode)
	require.NoError(t, err)
	require.Equal(t, version, backend.GCP.KeyVersion)

	// A pre-provisioned signer is untouched by any of this.
	preProvisioned := gcpImportNode()
	preProvisioned.Spec.Cosmosigner.Backend.GcpKMS = &appsv1.CosmosignerGcpKmsBackend{KeyVersion: version}
	preProvisioned.Status.CosmosignerImportedKeyVersion = "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/9"
	backend, err = r.cosmosignerBackend(context.Background(), preProvisioned)
	require.NoError(t, err)
	require.Equal(t, version, backend.GCP.KeyVersion)
	require.Nil(t, backend.GCP.Import)
}

// TestCosmosignerParamsThreadsImagePullSecrets lets the one-shot import/pubkey pods pull the
// cosmosigner image from a private registry. The signer StatefulSet deliberately does not carry
// them (that would change every existing signer's lifecycle digest); see the builder-side test.
func TestCosmosignerParamsThreadsImagePullSecrets(t *testing.T) {
	chainNode := gcpImportNode()
	chainNode.Spec.Config = &appsv1.Config{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-creds"}}}
	scheme := gcpImportTestScheme(t)
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme: scheme, opts: &controllers.ControllerRunOptions{},
	}

	params, err := r.cosmosignerParams(context.Background(), chainNode)
	require.NoError(t, err)
	require.Equal(t, chainNode.Spec.Config.ImagePullSecrets, params.ImagePullSecrets)
}

// TestCosmosignerPublicKeyForGcpImportReadsSource records that the EXPECTED consensus key of a
// managed import is the source key itself — resolved from the Secret, exactly as Vault's
// uploadGenerated does, with no pod and no backend round-trip (the destination version does not
// exist yet).
func TestCosmosignerPublicKeyForGcpImportReadsSource(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, _, publicKey := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	params, err := r.cosmosignerParams(context.Background(), chainNode)
	require.NoError(t, err)
	got, err := r.cosmosignerPublicKey(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.Equal(t, publicKey, got)
}

// TestPreflightCosmosignerImportSourceCoversGcpImport runs the read-only source check before the
// immutable raft/PVC locks are recorded and before anything is created in Cloud KMS, so a signer
// that can never import does not trap the operator into its first replica/storage choice.
func TestPreflightCosmosignerImportSourceCoversGcpImport(t *testing.T) {
	scheme := gcpImportTestScheme(t)

	missing := gcpImportNode()
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	require.ErrorContains(t, r.preflightCosmosignerImportSource(context.Background(), missing), "validator-key")

	chainNode := gcpImportNode()
	source, material, _ := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	r = &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build(), Scheme: scheme}
	require.NoError(t, r.preflightCosmosignerImportSource(context.Background(), chainNode))

	// Once the import is recorded for this destination and source, the bootstrap Secret is no longer
	// required: the backend already holds the registered key.
	deleted := gcpImportNode()
	deleted.Status.CosmosignerKeyImported = deleted.Spec.Cosmosigner.Backend.GcpKMS.ImportFingerprint("validator-key", material)
	r = &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	require.NoError(t, r.preflightCosmosignerImportSource(context.Background(), deleted))
}
