package chainnode

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/cometbft"
	"github.com/voluzi/cosmopilot/v4/internal/cosmosigner"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEnsureValidatorConsensusKeyReservationStopsConflictingLocalSigner(t *testing.T) {
	const namespace, name = "default", "validator"

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", parsed.PubKey.Value)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: parsed.PubKey.Value, OwnerUID: "other-root",
			OwnerKind: "ChainNodeSet", Namespace: "other", OwnerName: "other-validator", Claim: "validator",
		},
	}
	conflictingOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "other-validator", Namespace: "other", UID: "other-root",
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: name,
			UID: chainNode.UID, Controller: ptr.To(true),
		}},
	}}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(keySecret, reservation, conflictingOwner, pod).Build()}

	_, err = r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode)
	if err == nil {
		t.Fatal("a local validator must not sign a key reserved by another controller root")
	}
	remaining := &corev1.Pod{}
	err = r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, remaining)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("conflicting validator pod must be deleted, got %v", err)
	}
}

func TestEnsureValidatorConsensusKeyReservationUsesChainNodeSetRoot(t *testing.T) {
	const namespace, name = "default", "nodes-validators-0"
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: "child-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "nodes",
				UID: "nodeset-uid", Controller: ptr.To(true),
			}},
		},
		Spec:   appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")}},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", parsed.PubKey.Value)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: parsed.PubKey.Value, OwnerUID: "nodeset-uid",
			OwnerKind: "ChainNodeSet", Namespace: namespace, OwnerName: "nodes", Claim: name,
		},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(keySecret, reservation).Build()}

	if _, err := r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode); err != nil {
		t.Fatalf("a ChainNodeSet child must share its parent root reservation: %v", err)
	}
}

func TestEnsureValidatorConsensusKeyReservationSkipsRemoteSignerTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "child-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator:          &appsv1.ValidatorConfig{},
			RemoteSignerTarget: true,
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	if _, err := r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode); err != nil {
		t.Fatalf("a remote signer target must rely on its parent reservation: %v", err)
	}
}

func TestEnsureValidatorConsensusKeyReservationRejectsMismatchedKeyBeforeClaim(t *testing.T) {
	const namespace, name = "default", "validator"
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	onChainKey, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	onChain, err := cometbft.LoadPrivKey(onChainKey)
	if err != nil {
		t.Fatal(err)
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "validator-uid"},
		Spec:       appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")}},
		Status:     appsv1.ChainNodeStatus{ChainID: "chain-1", PubKey: `{"key":"` + onChain.PubKey.Value + `"}`},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(keySecret).Build()}

	_, err = r.ensureValidatorConsensusKeyReservation(context.Background(), chainNode)
	if err == nil {
		t.Fatal("a validator key that differs from recorded on-chain status must fail closed")
	}
	reservation := &appsv1.ConsensusKeyReservation{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: cosmosigner.ConsensusKeyReservationName("chain-1", parsed.PubKey.Value)}, reservation); !apierrors.IsNotFound(err) {
		t.Fatalf("a rejected signing key must not leave an immutable reservation, got %v", err)
	}
}
