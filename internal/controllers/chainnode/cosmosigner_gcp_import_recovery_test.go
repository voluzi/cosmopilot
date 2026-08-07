package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

const (
	gcpImportPodName = "validator-signer-import"
	gcpPubkeyPodName = "validator-signer-pubkey"
)

// gcpImportPodLogs is what a one-shot pod prints in these tests. It carries BOTH stable lines the
// controller parses — the created crypto key version and a public key — so the same fake output can
// serve the import pod and the verification probe that follows it.
func gcpImportPodLogs(version, publicKey string) string {
	return "imported key version: " + version + "\npubkey (base64): " + publicKey + "\n"
}

// fakeSignerPods stands in for the kubelet and the cosmosigner CLI: a one-shot pod succeeds the
// moment it is created and every container log read returns logs.
func fakeSignerPods(logs string) *k8sfake.Clientset {
	clientSet := k8sfake.NewSimpleClientset()
	clientSet.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		err := clientSet.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), action.GetNamespace(), action.(k8stesting.DeleteAction).GetName())
		return true, nil, err
	})
	clientSet.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.Status.Phase = corev1.PodSucceeded
		err := clientSet.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, action.GetNamespace())
		return true, pod, err
	})
	clientSet.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &runtime.Unknown{Raw: []byte(logs)}, nil
	})
	return clientSet
}

// createdPodNames is the ordered list of one-shot pods that actually ran. Every `<signer>-import` in
// it is one more ImportCryptoKeyVersion call against the destination crypto key.
func createdPodNames(clientSet *k8sfake.Clientset) []string {
	names := []string{}
	for _, action := range clientSet.Actions() {
		if action.GetVerb() != "create" || action.GetResource().Resource != "pods" || action.GetSubresource() != "" {
			continue
		}
		if pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok {
			names = append(names, pod.GetName())
		}
	}
	return names
}

func signerPodExists(t *testing.T, clientSet *k8sfake.Clientset, namespace, name string) bool {
	t.Helper()
	_, err := clientSet.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// TestGcpImportPodOutlivesFailedVersionPersist is the restart/status-conflict regression. The first
// reconcile's import succeeds in Cloud KMS but its status write never lands, which is
// indistinguishable from the controller being killed in that window. The next reconcile must resume
// from the pod that ran — importing again would leave the destination key holding TWO versions of the
// validator's consensus key.
func TestGcpImportPodOutlivesFailedVersionPersist(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, material, publicKey := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	version := gcpDestinationKey + "/cryptoKeyVersions/5"

	rejectStatusWrites := true
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode.DeepCopy(), source).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if rejectStatusWrites {
					return apierrors.NewConflict(schema.GroupResource{Resource: "chainnodes"}, obj.GetName(), nil)
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	r := &Reconciler{
		Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{},
		recorder: record.NewFakeRecorder(10), cosmosignerClientSet: clientSet,
	}

	params, err := r.cosmosignerParams(context.Background(), chainNode)
	require.NoError(t, err)

	// Reconcile 1: the import runs and Cloud KMS creates version 5, but the status write is lost.
	_, err = r.maybeImportCosmosignerKey(context.Background(), chainNode, params)
	require.Error(t, err, "a lost status write must fail the reconcile rather than look like progress")
	require.Empty(t, chainNode.Status.CosmosignerImportedKeyVersion)
	require.True(t, signerPodExists(t, clientSet, chainNode.Namespace, gcpImportPodName),
		"the succeeded import pod is the only surviving record of the created key version; it must not be deleted before that version is persisted")

	// A restart drops every in-memory mutation, so reconcile 2 starts from what actually persisted.
	restarted := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(chainNode), restarted))
	require.Empty(t, restarted.Status.CosmosignerImportedKeyVersion)

	// Reconcile 2: the status write works again. The import must be RECOVERED, not repeated.
	rejectStatusWrites = false
	pending, err := r.maybeImportCosmosignerKey(context.Background(), restarted, params)
	require.NoError(t, err)
	require.True(t, pending, "the signer is re-rendered against the now-known key version on the next pass")

	require.Equal(t, []string{gcpImportPodName, gcpPubkeyPodName}, createdPodNames(clientSet),
		"the key must be imported exactly once: a second import pod is a second Cloud KMS crypto key version")
	require.Equal(t, version, restarted.Status.CosmosignerImportedKeyVersion)
	require.Equal(t, chainNode.Spec.Cosmosigner.Backend.GcpKMS.ImportFingerprint("validator-key", material),
		restarted.Status.CosmosignerKeyImported, "the recovered version must still be verified against the source key")
	require.False(t, signerPodExists(t, clientSet, chainNode.Namespace, gcpImportPodName),
		"the import pod is cleaned up once its version is durably persisted")
}

// TestGcpImportPodSurvivesUntilVersionPersisted pins the persistence boundary the recovery protocol
// depends on. The pod may only be deleted after a status write carrying its reported version;
// deleting it first is what makes a crash in that window import the key twice. The test deliberately
// does not require the version and verified fingerprint to be separate status writes.
func TestGcpImportPodSurvivesUntilVersionPersisted(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, _, publicKey := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	version := gcpDestinationKey + "/cryptoKeyVersions/2"

	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	var podLivedThroughVersionWrite *bool
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode.DeepCopy(), source).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				// Any status write carrying the version ends the recovery window, whether or not a future
				// implementation persists the verified fingerprint in the same update.
				status := obj.(*appsv1.ChainNode).Status
				if podLivedThroughVersionWrite == nil && status.CosmosignerImportedKeyVersion != "" {
					lived := signerPodExists(t, clientSet, chainNode.Namespace, gcpImportPodName)
					podLivedThroughVersionWrite = &lived
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &Reconciler{
		Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{},
		recorder: record.NewFakeRecorder(10), cosmosignerClientSet: clientSet,
	}

	params, err := r.cosmosignerParams(context.Background(), chainNode)
	require.NoError(t, err)
	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.True(t, pending)

	require.NotNil(t, podLivedThroughVersionWrite, "the import must record the version it created")
	require.True(t, *podLivedThroughVersionWrite,
		"the import pod was deleted before the version it reported was persisted: a crash in that window re-imports the key")
	require.False(t, signerPodExists(t, clientSet, chainNode.Namespace, gcpImportPodName),
		"the import pod is cleaned up once its version is durably persisted")
	require.Equal(t, []string{gcpImportPodName, gcpPubkeyPodName}, createdPodNames(clientSet))
}

// TestGcpImportCleansUpImportPodLeftByAnEarlierCrash closes the last gap: a controller killed AFTER
// the version was persisted but BEFORE the pod was removed resumes on the verification path, which
// never calls the import again — so that path owns the leftover pod.
func TestGcpImportCleansUpImportPodLeftByAnEarlierCrash(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, material, publicKey := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	version := gcpDestinationKey + "/cryptoKeyVersions/4"
	chainNode.Status.CosmosignerImportedKeyVersion = version

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode.DeepCopy(), source).
		Build()
	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	r := &Reconciler{
		Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{},
		recorder: record.NewFakeRecorder(10), cosmosignerClientSet: clientSet,
	}
	params, err := r.cosmosignerParams(context.Background(), chainNode)
	require.NoError(t, err)

	// Leave behind exactly what the crash would have: this owner's succeeded import pod.
	leftover, err := clientSet.CoreV1().Pods(chainNode.Namespace).Create(context.Background(),
		gcpImportLeftoverPod(t, chainNode, scheme), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Equal(t, gcpImportPodName, leftover.GetName())
	clientSet.ClearActions() // setup residue is not a logical import retry

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.False(t, pending, "a verified import is complete")
	require.Equal(t, chainNode.Spec.Cosmosigner.Backend.GcpKMS.ImportFingerprint("validator-key", material),
		chainNode.Status.CosmosignerKeyImported)
	require.Equal(t, []string{gcpPubkeyPodName}, createdPodNames(clientSet),
		"a recorded version is verified, never re-imported")
	require.False(t, signerPodExists(t, clientSet, chainNode.Namespace, gcpImportPodName),
		"the leftover import pod must be cleaned up once its version is durably persisted")
}

func gcpImportLeftoverPod(t *testing.T, chainNode *appsv1.ChainNode, scheme *runtime.Scheme) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: gcpImportPodName, Namespace: chainNode.Namespace, UID: "import-pod-uid"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, pod, scheme))
	return pod
}

func TestGcpImportCompleteRetriesImportPodCleanup(t *testing.T) {
	chainNode := gcpImportNode()
	scheme := gcpImportTestScheme(t)
	source, material, publicKey := gcpImportSourceSecret(t, chainNode.Namespace, "validator-key")
	version := gcpDestinationKey + "/cryptoKeyVersions/6"
	chainNode.Status.CosmosignerImportedKeyVersion = version
	chainNode.Status.CosmosignerKeyImported = chainNode.Spec.Cosmosigner.Backend.GcpKMS.ImportFingerprint("validator-key", material)

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode.DeepCopy(), source).Build()
	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{},
		recorder: record.NewFakeRecorder(10), cosmosignerClientSet: clientSet}
	params, err := r.cosmosignerParams(context.Background(), chainNode)
	require.NoError(t, err)
	_, err = clientSet.CoreV1().Pods(chainNode.Namespace).Create(context.Background(),
		gcpImportLeftoverPod(t, chainNode, scheme), metav1.CreateOptions{})
	require.NoError(t, err)
	clientSet.ClearActions()

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.False(t, pending)
	require.Empty(t, createdPodNames(clientSet), "completed cleanup must not execute import or pubkey pods")
	require.False(t, signerPodExists(t, clientSet, chainNode.Namespace, gcpImportPodName))
}
