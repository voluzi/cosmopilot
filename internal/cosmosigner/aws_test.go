package cosmosigner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const awsTestKeyARN = "arn:aws:kms:eu-west-1:123456789012:key/12345678-1234-1234-1234-123456789012"

func awsParams() Params {
	p := testParams()
	p.Image = "ghcr.io/voluzi/cosmosigner:edge"
	p.ServiceAccountName = "aws-signer"
	p.Backend = Backend{AWS: &AwsBackend{KeyID: awsTestKeyARN, Region: "eu-west-1"}}
	return p
}

func TestAWSRuntimeAndPubkeyCredentials(t *testing.T) {
	for _, static := range []bool{false, true} {
		t.Run(map[bool]string{false: "SDK chain", true: "shared credentials"}[static], func(t *testing.T) {
			p := awsParams()
			p.Backend.AWS.Timeout = "20s"
			p.Backend.AWS.ClaimRoleARN = "arn:aws:iam::123456789012:role/claimer"
			if static {
				p.Backend.AWS.CredentialsSecret = secretKey("aws-credentials", "ini")
			}
			config, err := p.ConfigYAML()
			require.NoError(t, err)
			require.Contains(t, config, "type: awskms")
			require.Contains(t, config, "key_id: "+awsTestKeyARN)
			require.Contains(t, config, "region: eu-west-1")
			require.NotContains(t, config, "timeout:")
			env := signerEnv(t, p)
			require.Equal(t, "true", env["COSMOSIGNER_CLAIM_IF_UNCLAIMED"])
			require.Equal(t, p.Backend.AWS.ClaimRoleARN, env["COSMOSIGNER_AWS_CLAIM_ROLE_ARN"])
			require.Equal(t, "20s", env["COSMOSIGNER_AWS_TIMEOUT"])
			sts := mustStatefulSet(t, p)
			pod := JobRunner{Params: p}.buildPubkeyPod("")
			require.Equal(t, "aws-signer", pod.Spec.ServiceAccountName)
			require.Equal(t, p.Image, pod.Spec.Containers[0].Image)
			require.Equal(t, []string{"pubkey"}, pod.Spec.Containers[0].Args)
			jobEnv := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				jobEnv[e.Name] = e.Value
			}
			require.Equal(t, "awskms", jobEnv["COSMOSIGNER_BACKEND"])
			require.Equal(t, awsTestKeyARN, jobEnv["COSMOSIGNER_AWS_KEY_ID"])
			require.Equal(t, "eu-west-1", jobEnv["COSMOSIGNER_AWS_REGION"])
			require.Equal(t, "20s", jobEnv["COSMOSIGNER_AWS_TIMEOUT"])
			require.NotContains(t, jobEnv, "COSMOSIGNER_AWS_CLAIM_ROLE_ARN")
			require.NotContains(t, jobEnv, "COSMOSIGNER_CLAIM_IF_UNCLAIMED")
			if static {
				require.Equal(t, "/aws/credentials", env["AWS_SHARED_CREDENTIALS_FILE"])
				require.Equal(t, "/aws/credentials", jobEnv["AWS_SHARED_CREDENTIALS_FILE"])
				require.Len(t, pod.Spec.Volumes, 1)
				secret := pod.Spec.Volumes[0].Secret
				require.Equal(t, "aws-credentials", secret.SecretName)
				require.Len(t, secret.Items, 1)
				require.Equal(t, "ini", secret.Items[0].Key)
				require.Equal(t, "credentials", secret.Items[0].Path)
				mount := pod.Spec.Containers[0].VolumeMounts[0]
				require.Equal(t, "/aws", mount.MountPath)
				require.True(t, mount.ReadOnly)
				require.Empty(t, mount.SubPath)
				require.Contains(t, sts.Spec.Template.Spec.Volumes, pod.Spec.Volumes[0])
				require.Contains(t, sts.Spec.Template.Spec.Containers[0].VolumeMounts, mount)
			} else {
				require.NotContains(t, env, "AWS_SHARED_CREDENTIALS_FILE")
				require.NotContains(t, jobEnv, "AWS_SHARED_CREDENTIALS_FILE")
				require.Empty(t, pod.Spec.Volumes)
			}
		})
	}
}

func TestAWSLifecycleDigest(t *testing.T) {
	base, err := awsParams().LifecycleDigest("identity")
	require.NoError(t, err)
	same, err := awsParams().LifecycleDigest("identity")
	require.NoError(t, err)
	require.Equal(t, base, same)
	for name, mutate := range map[string]func(*AwsBackend){
		"key":         func(b *AwsBackend) { b.KeyID += "-other" },
		"region":      func(b *AwsBackend) { b.Region = "us-east-1" },
		"credentials": func(b *AwsBackend) { b.CredentialsSecret = secretKey("creds", "ini") },
		"claim role":  func(b *AwsBackend) { b.ClaimRoleARN = "role" },
		"timeout":     func(b *AwsBackend) { b.Timeout = "20s" },
	} {
		t.Run(name, func(t *testing.T) {
			p := awsParams()
			mutate(p.Backend.AWS)
			digest, err := p.LifecycleDigest("identity")
			require.NoError(t, err)
			require.NotEqual(t, base, digest)
		})
	}
}

func TestAWSRecoveredSigningPublicKey(t *testing.T) {
	owner := fakeOwner("owner", types.UID("owner-uid"))
	p := awsParams()
	config, err := p.ConfigYAML()
	require.NoError(t, err)
	cm, err := p.ConfigMap(config)
	require.NoError(t, err)
	sts, err := p.StatefulSet(config)
	require.NoError(t, err)
	cm.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
	sts.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(cm, sts).Build()
	key, found, err := RecoveredSigningPublicKey(context.Background(), c, owner, p)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, p.ExpectedPublicKey, key)
	p.Backend.AWS.KeyID += "-other"
	_, _, err = RecoveredSigningPublicKey(context.Background(), c, owner, p)
	require.ErrorIs(t, err, ErrRecoveredIdentityMismatch)
}

func TestAWSPublicKeyUsesOrdinaryDiscoveryPods(t *testing.T) {
	p := awsParams()
	owner := importOwner()
	scheme := importPodScheme(t)
	clientSet := fakeSignerPods("aws key arn:    " + awsTestKeyARN + "\naddress:        0000000000000000000000000000000000000000\npubkey (base64): " + p.ExpectedPublicKey + "\n")
	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: p}
	for i := 0; i < 2; i++ {
		key, err := runner.PublicKey(context.Background())
		require.NoError(t, err)
		require.Equal(t, p.ExpectedPublicKey, key)
		_, err = getPod(t, clientSet, p.Namespace, p.Name+"-pubkey")
		require.True(t, apierrors.IsNotFound(err))
	}
	require.Equal(t, 2, podVerbs(clientSet, "create"))
}
