package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

// resolveSingleSigner returns the sole resolved signer of a ChainNodeSet, failing if there is not
// exactly one.
func resolveSingleSigner(t *testing.T, nodeSet *appsv1.ChainNodeSet) appsv1.ResolvedSigner {
	t.Helper()
	signers := nodeSet.ResolveCosmosigners()
	require.Len(t, signers, 1, "expected exactly one resolved signer")
	return signers[0]
}

// TestReconcileSignerTeardownDropsStatusEntry verifies that once a removed signer's StatefulSet and
// its PVCs are gone, teardown drops its per-signer status entry, so a later re-add (e.g. a sentry
// signer with a different replica count) is not rejected against stale state on the no-webhook path.
func TestReconcileSignerTeardownDropsStatusEntry(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:          "test-nodeset-signer",
				Replicas:      ptr.To(int32(3)),
				SigningDigest: "stale-digest",
			}},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done, "teardown of an absent signer is complete")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	assert.Empty(t, fresh.Status.Cosmosigners, "the removed signer's status entry must be dropped on teardown")
}

func TestReconcileSignerTeardownPreservesLegacyPerInstanceSigners(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(2),
			Validator: &appsv1.NodeSetValidatorConfig{},
			Cosmosigner: &appsv1.Cosmosigner{
				Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}},
			},
		}}},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{
				{Name: "test-nodeset-validators-0-signer"},
				{Name: "test-nodeset-validators-1-signer"},
			},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "legacy per-instance cosmosigners")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	require.Len(t, fresh.Status.Cosmosigners, 2)
}

func TestPreflightCosmosignersRequiresGenesisSentrySecrets(t *testing.T) {
	const (
		privSecret    = "genesis-sentry-key"
		accountSecret = "genesis-sentry-account"
	)
	newNodeSet := func() *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
			Spec: appsv1.ChainNodeSetSpec{
				Validator: &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
					ChainID:     "test-1",
					Assets:      []string{"1stake"},
					StakeAmount: "1stake",
					GenesisValidators: []appsv1.GenesisValidator{{
						PrivKeySecret:         privSecret,
						AccountMnemonicSecret: accountSecret,
						Moniker:               "sentry",
						Assets:                []string{"1stake"},
						StakeAmount:           "1stake",
					}},
				}},
				Nodes: []appsv1.NodeGroupSpec{{
					Name:      "sentries",
					Instances: ptr.To(1),
					Cosmosigner: &appsv1.Cosmosigner{
						Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(privSecret)}},
					},
				}},
			},
		}
	}

	t.Run("missing private key", func(t *testing.T) {
		nodeSet := newNodeSet()
		r := newValidatorTestReconciler(t, nodeSet)
		err := r.preflightCosmosigners(context.Background(), nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), privSecret)
	})

	t.Run("missing account mnemonic", func(t *testing.T) {
		nodeSet := newNodeSet()
		key := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: privSecret, Namespace: "default"},
			Data:       map[string][]byte{privKeyFilename: []byte("key")},
		}
		r := newValidatorTestReconciler(t, nodeSet, key)
		err := r.preflightCosmosigners(context.Background(), nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), accountSecret)
	})
}

// TestReconcileSignerTeardownKeepsStatusWhileTerminating verifies that while the signer StatefulSet is
// still present (teardown is asynchronous), the recorded status entry is preserved — dropping it early
// would let a remove-and-immediate-re-add bind the surviving PVCs and inherit stale raft membership.
func TestReconcileSignerTeardownKeepsStatusWhileTerminating(t *testing.T) {
	const signerName = "test-nodeset-signer"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:          signerName,
				Replicas:      ptr.To(int32(3)),
				SigningDigest: "stale-digest",
			}},
		},
	}
	// A StatefulSet owned by the nodeSet with a finalizer: Undeploy issues a delete, but the fake
	// client retains it (deletionTimestamp set, object kept until finalizers clear), modelling the
	// window where teardown is still in flight.
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       signerName,
			Namespace:  "default",
			Finalizers: []string{"cosmopilot.voluzi.com/test-hold"},
			Labels:     cosmosigner.InstanceLabels(signerName),
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))

	r := newValidatorTestReconciler(t, nodeSet, sts)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, done, "teardown is not complete while the StatefulSet is still terminating")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	require.Len(t, fresh.Status.Cosmosigners, 1, "the status entry must be preserved while the signer is still terminating")
	assert.Equal(t, ptr.To(int32(3)), fresh.Status.Cosmosigners[0].Replicas)
	assert.Equal(t, "stale-digest", fresh.Status.Cosmosigners[0].SigningDigest)
}

// TestReconcileSignerTeardownDropsStatusWithForeignSameNameSigner verifies that a same-name
// StatefulSet owned by ANOTHER CR does not permanently block dropping this nodeSet's recorded status
// entry: Undeploy skips the foreign resource, and IsTornDown treats it as unrelated, so the stale
// entry is dropped and a later valid re-add is not rejected against it.
func TestReconcileSignerTeardownDropsStatusWithForeignSameNameSigner(t *testing.T) {
	const signerName = "test-nodeset-signer"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:          signerName,
				Replicas:      ptr.To(int32(3)),
				SigningDigest: "stale-digest",
			}},
		},
	}
	// A same-name StatefulSet owned by a DIFFERENT ChainNodeSet (distinct UID).
	foreignOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "other-nodeset", Namespace: "default", UID: "other-uid"}}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      signerName,
			Namespace: "default",
			Labels:    cosmosigner.InstanceLabels(signerName),
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(foreignOwner, sts, testScheme(t)))

	r := newValidatorTestReconciler(t, nodeSet, sts)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done, "a foreign same-name signer must not block completion")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	assert.Empty(t, fresh.Status.Cosmosigners, "a foreign same-name signer must not block dropping our status entry")

	// The foreign StatefulSet must be left untouched.
	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: signerName}, remaining))
	assert.True(t, metav1.IsControlledBy(remaining, foreignOwner), "foreign signer must remain owned by the other CR")
}

// TestSignerNameForNode verifies each node maps to the signer that must dial it: every pod of a
// signer's target groups is a signing endpoint — the (single-instance) validator group it serves and
// any sentry groups fronted alongside it.
func TestSignerNameForNode(t *testing.T) {
	// Top-level signer fronting a single-instance validator group AND a multi-instance sentry group.
	topLevel := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"vg", "fullnodes"}, Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{Address: "https://v:8200", KeyName: "k", TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "t"}, Key: "token"}}}},
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "vg", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("vgk")}},
				{Name: "fullnodes", Instances: ptr.To(3)},
			},
		},
	}
	name, ok := signerNameForNode(topLevel, "vg")
	assert.True(t, ok)
	assert.Equal(t, "cs-signer", name)
	// The sentry fan-out group is a signing endpoint too.
	name, ok = signerNameForNode(topLevel, "fullnodes")
	assert.True(t, ok, "fullnodes must be a signing endpoint")
	assert.Equal(t, "cs-signer", name)

	// An untargeted group maps to no signer.
	_, ok = signerNameForNode(topLevel, "other")
	assert.False(t, ok)

	// A single-instance validator group with its own per-group signer maps to that signer.
	perGroup := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "vg", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{Address: "https://v:8200", KeyName: "k", TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "t"}, Key: "token"}}}},
			}},
		},
	}
	name, ok = signerNameForNode(perGroup, "vg")
	assert.True(t, ok)
	assert.Equal(t, "cs-vg-signer", name)
}

// testScheme builds a scheme with the API + core + apps types for owner references in tests.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	return scheme
}

// TestMaybeImportCosmosignerKeyPreservesCompletedImport verifies the absent-source fast-path: a
// recorded import (in the signer's CosmosignerStatus.KeyImported) whose TARGET half matches the
// current Vault destination and source secret keeps a completed import valid when the bootstrap Secret
// is deleted (the signer keeps running — Vault still holds the registered key). A record from a
// DIFFERENT target/source, or none at all, keeps the import pending: nothing usable was ever imported
// for the current spec.
func TestMaybeImportCosmosignerKeyPreservesCompletedImport(t *testing.T) {
	const signerName = "test-nodeset-signer"
	mk := func(imported string) *appsv1.ChainNodeSet {
		ns := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Cosmosigner: &appsv1.Cosmosigner{
					NodeGroups: []string{"validators"},
					Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
						Address:         "https://vault.example:8200",
						KeyName:         "val-key",
						TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
						UploadGenerated: true,
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
		if imported != "" {
			ns.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: signerName, KeyImported: imported}}
		}
		return ns
	}
	params := cosmosigner.Params{Name: signerName, Namespace: "default"}
	// The value a completed import would have recorded for the CURRENT target/source (key material hash
	// differs from target hash, but only the target half matters when the source is gone).
	matching := mk("").Spec.Cosmosigner.Backend.Vault.ImportFingerprint("val-priv-key", []byte("imported-key-bytes"))

	// Source Secret absent but a prior import for the CURRENT target/source recorded: NOT pending.
	ns := mk(matching)
	r := newValidatorTestReconciler(t, ns)
	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.NoError(t, err)
	assert.False(t, pending, "a completed import must survive deletion of the bootstrap source Secret")

	// Source Secret absent and the recorded import belongs to a DIFFERENT Vault target: error — this
	// validator uses an explicit external-genesis privateKeySecret, so no controller flow will create it
	// later. Keeping the signer merely pending would leave target children in remote-signer mode forever.
	otherTarget := mk("")
	otherTarget.Spec.Cosmosigner.Backend.Vault.KeyName = "old-key"
	stale := otherTarget.Spec.Cosmosigner.Backend.Vault.ImportFingerprint("val-priv-key", []byte("imported-key-bytes"))
	ns = mk(stale)
	r = newValidatorTestReconciler(t, ns)
	pending, _, err = r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.Error(t, err)
	assert.False(t, pending)

	// Source Secret absent and nothing imported yet: explicit external-genesis key is missing -> error.
	ns = mk("")
	r = newValidatorTestReconciler(t, ns)
	pending, _, err = r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.Error(t, err)
	assert.False(t, pending)

	// Generated init/createValidator key flow still pending (no status pubkey yet, no explicit secret):
	// wait instead of erroring because ensureValidator will create the source key.
	ns = mk("")
	ns.Spec.Nodes[0].Validator.PrivateKeySecret = nil
	ns.Spec.Nodes[0].Validator.Init = &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"100stake"}, StakeAmount: "1stake"}
	r = newValidatorTestReconciler(t, ns)
	pending, _, err = r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.NoError(t, err)
	assert.True(t, pending, "generated key flow with no recorded pubkey may still produce the source key")
}
