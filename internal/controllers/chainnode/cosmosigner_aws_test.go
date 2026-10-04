package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

func TestAWSCosmosignerParamsAndDiscovery(t *testing.T) {
	const arn = "arn:aws:kms:eu-west-1:123456789012:key/12345678-1234-1234-1234-123456789012"
	const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	node := signerChainNode(appsv1.CosmosignerBackend{AwsKMS: &appsv1.CosmosignerAwsKmsBackend{
		KeyID: arn, Region: "eu-west-1", CredentialsSecret: claimSelector("aws-credentials"), ClaimRoleARN: ptr.To("claim-role"), Timeout: ptr.To("30s"),
	}})
	node.Spec.Cosmosigner.Image = ptr.To("ghcr.io/voluzi/cosmosigner:edge")
	r := claimTestReconciler(t, claimSecret("aws-credentials"))
	r.opts = &controllers.ControllerRunOptions{}
	r.cosmosignerClientSet = fakeSignerPods("aws key arn:    " + arn + "\npubkey (base64): " + publicKey + "\n")
	params, err := r.cosmosignerParams(context.Background(), node)
	require.NoError(t, err)
	require.Equal(t, arn, params.Backend.AWS.KeyID)
	require.Equal(t, "eu-west-1", params.Backend.AWS.Region)
	require.Equal(t, claimSelector("aws-credentials"), params.Backend.AWS.CredentialsSecret)
	require.Equal(t, "claim-role", params.Backend.AWS.ClaimRoleARN)
	require.Equal(t, "30s", params.Backend.AWS.Timeout)
	require.Equal(t, "ghcr.io/voluzi/cosmosigner:edge", params.Image)
	key, err := r.cosmosignerPublicKey(context.Background(), node, params)
	require.NoError(t, err)
	require.Equal(t, publicKey, key)
	pending, err := r.maybeImportCosmosignerKey(context.Background(), node, params)
	require.NoError(t, err)
	require.False(t, pending)
}
