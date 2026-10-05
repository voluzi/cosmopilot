package v1

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const awsTestKeyARN = "arn:aws:kms:eu-west-1:123456789012:key/12345678-1234-1234-1234-123456789012"

func awsKeySigner(keyID string) *Cosmosigner {
	return &Cosmosigner{Backend: CosmosignerBackend{AwsKMS: &CosmosignerAwsKmsBackend{KeyID: keyID, Region: "eu-west-1"}}}
}

func TestAWSBackendIdentityAndValidation(t *testing.T) {
	c := awsKeySigner(awsTestKeyARN)
	require.NoError(t, c.Validate(".spec.cosmosigner", false))
	require.Equal(t, "awskms\x00"+awsTestKeyARN, c.effectiveSigningIdentity(""))
	require.False(t, c.ImportsGeneratedKey(true))
	require.True(t, c.UsesPubkeyPod(false))
	original := c.effectiveSigningIdentity("")
	c.Backend.AwsKMS.Region = "different-region"
	c.Backend.AwsKMS.ClaimRoleARN = ptr.To("role")
	c.Backend.AwsKMS.Timeout = ptr.To("20s")
	c.Backend.AwsKMS.CredentialsSecret = &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}, Key: "ini"}
	require.Equal(t, original, c.effectiveSigningIdentity(""))
	c.Backend.Software = &CosmosignerSoftwareBackend{}
	require.ErrorContains(t, c.Validate(".spec.cosmosigner", false), "not multiple")
}

func TestAWSUniqueSigningKeys(t *testing.T) {
	set := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
		{Name: "first", Instances: ptr.To(1), Cosmosigner: awsKeySigner(awsTestKeyARN)},
		{Name: "second", Instances: ptr.To(1), Cosmosigner: awsKeySigner(awsTestKeyARN)},
	}}}
	require.ErrorContains(t, set.validateUniqueSigningKeys(), "same AWS KMS signing key")
	set.Spec.Nodes[1].Cosmosigner.Backend.AwsKMS.KeyID += "-other"
	require.NoError(t, set.validateUniqueSigningKeys())
}
