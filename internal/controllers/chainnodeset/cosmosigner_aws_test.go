package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func TestAWSCosmosignerParamsAndDiscovery(t *testing.T) {
	const arn = "arn:aws:kms:eu-west-1:123456789012:key/12345678-1234-1234-1234-123456789012"
	const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, placement := range []string{"validator", "sentry", "group"} {
		t.Run(placement, func(t *testing.T) {
			signer := &appsv1.Cosmosigner{Image: ptr.To("ghcr.io/voluzi/cosmosigner:edge"), Backend: appsv1.CosmosignerBackend{AwsKMS: &appsv1.CosmosignerAwsKmsBackend{
				KeyID: arn, Region: "eu-west-1", CredentialsSecret: claimSelector("aws-credentials"), ClaimRoleARN: ptr.To("claim-role"), Timeout: ptr.To("30s"),
			}}}
			set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default"}, Spec: appsv1.ChainNodeSetSpec{Cosmosigner: signer}, Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"}}
			if placement == "validator" {
				set.Spec.Validator = &appsv1.NodeSetValidatorConfig{}
			} else {
				set.Spec.Nodes = []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}}
				if placement == "sentry" {
					signer.NodeGroups = []string{"sentries"}
				} else {
					set.Spec.Cosmosigner = nil
					set.Spec.Nodes[0].Cosmosigner = signer
				}
			}
			r := newValidatorTestReconciler(t, set, claimSecret("aws-credentials"))
			r.cosmosignerClientSet = fakeSignerPods("aws key arn:    " + arn + "\npubkey (base64): " + publicKey + "\n")
			resolved := resolveSingleSigner(t, set)
			params, err := r.cosmosignerParams(context.Background(), set, resolved)
			require.NoError(t, err)
			require.Equal(t, arn, params.Backend.AWS.KeyID)
			require.Equal(t, "eu-west-1", params.Backend.AWS.Region)
			require.Equal(t, claimSelector("aws-credentials"), params.Backend.AWS.CredentialsSecret)
			require.Equal(t, "claim-role", params.Backend.AWS.ClaimRoleARN)
			require.Equal(t, "30s", params.Backend.AWS.Timeout)
			require.Equal(t, "ghcr.io/voluzi/cosmosigner:edge", params.Image)
			key, err := r.cosmosignerPublicKeyWithParams(context.Background(), set, resolved, params)
			require.NoError(t, err)
			require.Equal(t, publicKey, key)
			pending, changed, err := r.maybeImportCosmosignerKey(context.Background(), set, resolved, params)
			require.NoError(t, err)
			require.False(t, pending)
			require.False(t, changed)
		})
	}
}
