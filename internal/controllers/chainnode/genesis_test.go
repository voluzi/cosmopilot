package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func TestRecordGenesisDigestRefreshesForManagedSignerMigration(t *testing.T) {
	tokenSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
		Key:                  "token",
	}
	original := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{
			Init: &appsv1.GenesisInitConfig{ChainID: "chain-1", Assets: []string{"1stake"}, StakeAmount: "1stake"},
			TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{Hashicorp: &appsv1.TmKmsHashicorpProvider{
				Address: "https://vault:8200", Key: "validator-key", TokenSecret: tokenSecret,
			}}},
		}},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	original.Status.GenesisSigningDigest = original.Spec.Validator.GenesisSigningFingerprint("validator-priv-key")

	for _, tc := range []struct {
		name                string
		disableWebhooks     bool
		mutateInit          bool
		wantUpdated         bool
		wantValidationError bool
	}{
		{name: "enabled", wantUpdated: true},
		{name: "disabled", disableWebhooks: true, wantUpdated: true},
		{name: "enabled with unrelated init mutation", mutateInit: true, wantValidationError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chainNode := original.DeepCopy()
			chainNode.Spec.Validator.TmKMS = nil
			chainNode.Spec.Cosmosigner = &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: "https://vault:8200", KeyName: "validator-key", TokenSecret: tokenSecret,
			}}}
			chainNode.Status.CosmosignerSigningDigest = chainNode.CosmosignerSigningDigest()
			chainNode.Status.CosmosignerServingIdentity = chainNode.EffectiveSigningIdentity()
			chainNode.Status.CosmosignerReplicas = ptr.To(int32(1))
			if tc.mutateInit {
				chainNode.Spec.Validator.Init.StakeAmount = "2stake"
			}
			require.NotEqual(t, chainNode.Spec.Validator.GenesisSigningFingerprint("validator-priv-key"), chainNode.Status.GenesisSigningDigest)
			_, err := chainNode.Validate(nil)
			if tc.wantValidationError {
				require.Error(t, err)
				require.Contains(t, err.Error(), ".spec.validator.init")
			} else {
				require.NoError(t, err)
			}

			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode).Build()
			r := &Reconciler{Client: cl, opts: &controllers.ControllerRunOptions{DisableWebhooks: tc.disableWebhooks}}

			current := &appsv1.ChainNode{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(chainNode), current))
			updated, err := r.recordGenesisDigestIfMissing(context.Background(), current)
			require.NoError(t, err)
			require.Equal(t, tc.wantUpdated, updated)

			persisted := &appsv1.ChainNode{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(chainNode), persisted))
			if tc.wantUpdated {
				require.Equal(t, persisted.Spec.Validator.GenesisSigningFingerprint("validator-priv-key"), persisted.Status.GenesisSigningDigest)
				_, err = persisted.Validate(nil)
				require.NoError(t, err)
			} else {
				require.Equal(t, original.Status.GenesisSigningDigest, persisted.Status.GenesisSigningDigest)
			}
		})
	}
}
