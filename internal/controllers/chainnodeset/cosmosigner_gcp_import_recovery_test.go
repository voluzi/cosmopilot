package chainnodeset

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

// gcpImportRecoveryReconciler mirrors newValidatorTestReconciler but wires the one-shot pod clientset
// and lets a test intercept the status writes the import protocol depends on.
func gcpImportRecoveryReconciler(t *testing.T, clientSet *k8sfake.Clientset, funcs interceptor.Funcs, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := testScheme(t)
	return &Reconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&appsv1.ChainNodeSet{}).
			WithObjects(objs...).
			WithInterceptorFuncs(funcs).
			Build(),
		Scheme:               scheme,
		recorder:             record.NewFakeRecorder(100),
		opts:                 &controllers.ControllerRunOptions{},
		cosmosignerClientSet: clientSet,
	}
}

// TestNodeSetGcpImportPodOutlivesFailedVersionPersist is the restart/status-conflict regression on the
// ChainNodeSet path. The first reconcile's import succeeds in Cloud KMS but its status write never
// lands, which is indistinguishable from the controller being killed in that window. The next
// reconcile must resume from the pod that ran — importing again would leave the destination key
// holding TWO versions of the validator's consensus key.
func TestNodeSetGcpImportPodOutlivesFailedVersionPersist(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, material, publicKey := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	resourceName := nodeSet.CosmosignerResourceName(signer)
	importPod, pubkeyPod := resourceName+"-import", resourceName+"-pubkey"
	version := nodeSetGcpDestinationKey + "/cryptoKeyVersions/5"

	rejectStatusWrites := true
	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	r := gcpImportRecoveryReconciler(t, clientSet, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if rejectStatusWrites {
				return apierrors.NewConflict(schema.GroupResource{Resource: "chainnodesets"}, obj.GetName(), nil)
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	}, nodeSet, source)

	params, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)

	// Reconcile 1: the import runs and Cloud KMS creates version 5, but the status write is lost.
	_, _, err = r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, params)
	require.Error(t, err, "a lost status write must fail the reconcile rather than look like progress")
	require.True(t, signerPodExists(t, clientSet, nodeSet.Namespace, importPod),
		"the succeeded import pod is the only surviving record of the created key version; it must not be deleted before that version is persisted")

	// A restart drops every in-memory mutation, so reconcile 2 starts from what actually persisted.
	restarted := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), restarted))
	require.Nil(t, restarted.GetCosmosignerStatus(signer.Name))

	// Reconcile 2: the status write works again. The import must be RECOVERED, not repeated.
	rejectStatusWrites = false
	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), restarted, signer, params)
	require.NoError(t, err)
	require.True(t, pending, "the signer is re-rendered against the now-known key version on the next pass")

	require.Equal(t, []string{importPod, pubkeyPod}, createdPodNames(clientSet),
		"the key must be imported exactly once: a second import pod is a second Cloud KMS crypto key version")
	status := restarted.GetCosmosignerStatus(signer.Name)
	require.NotNil(t, status)
	require.Equal(t, version, status.ImportedKeyVersion)
	require.Equal(t, signer.Spec.Backend.GcpKMS.ImportFingerprint(signer.SoftwareKeySecret, material),
		status.KeyImported, "the recovered version must still be verified against the source key")
	require.False(t, signerPodExists(t, clientSet, nodeSet.Namespace, importPod),
		"the import pod is cleaned up once its version is durably persisted")
}

// TestNodeSetGcpImportPodSurvivesUntilVersionPersisted pins the ORDER the recovery protocol depends
// on. The pod may only be deleted after the version it reported is durably persisted; deleting it
// first is what makes a crash in that window import the key twice.
func TestNodeSetGcpImportPodSurvivesUntilVersionPersisted(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, _, publicKey := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	resourceName := nodeSet.CosmosignerResourceName(signer)
	importPod, pubkeyPod := resourceName+"-import", resourceName+"-pubkey"
	version := nodeSetGcpDestinationKey + "/cryptoKeyVersions/2"

	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	var podLivedThroughVersionWrite *bool
	r := gcpImportRecoveryReconciler(t, clientSet, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			// The version write is the one that ends the window: it is the first status write that
			// records a key version and it precedes the verified-import record.
			written := obj.(*appsv1.ChainNodeSet).GetCosmosignerStatus(signer.Name)
			if podLivedThroughVersionWrite == nil && written != nil &&
				written.ImportedKeyVersion != "" && written.KeyImported == "" {
				lived := signerPodExists(t, clientSet, nodeSet.Namespace, importPod)
				podLivedThroughVersionWrite = &lived
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	}, nodeSet, source)

	params, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, params)
	require.NoError(t, err)
	require.True(t, pending)

	require.NotNil(t, podLivedThroughVersionWrite, "the import must record the version it created")
	require.True(t, *podLivedThroughVersionWrite,
		"the import pod was deleted before the version it reported was persisted: a crash in that window re-imports the key")
	require.False(t, signerPodExists(t, clientSet, nodeSet.Namespace, importPod),
		"the import pod is cleaned up once its version is durably persisted")
	require.Equal(t, []string{importPod, pubkeyPod}, createdPodNames(clientSet))
}

// TestNodeSetGcpImportCleansUpImportPodLeftByAnEarlierCrash closes the last gap: a controller killed
// AFTER the version was persisted but BEFORE the pod was removed resumes on the verification path,
// which never calls the import again — so that path owns the leftover pod.
func TestNodeSetGcpImportCleansUpImportPodLeftByAnEarlierCrash(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := resolveSingleSigner(t, nodeSet)
	source, material, publicKey := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	resourceName := nodeSet.CosmosignerResourceName(signer)
	importPod, pubkeyPod := resourceName+"-import", resourceName+"-pubkey"
	version := nodeSetGcpDestinationKey + "/cryptoKeyVersions/4"
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name: signer.Name, ResourceName: resourceName, ImportedKeyVersion: version,
	}}

	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	r := gcpImportRecoveryReconciler(t, clientSet, interceptor.Funcs{}, nodeSet, source)
	params, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)

	// Leave behind exactly what the crash would have: this owner's succeeded import pod.
	leftover := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: importPod, Namespace: nodeSet.Namespace, UID: "import-pod-uid"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, leftover, r.Scheme))
	_, err = clientSet.CoreV1().Pods(nodeSet.Namespace).Create(context.Background(), leftover, metav1.CreateOptions{})
	require.NoError(t, err)
	clientSet.ClearActions() // setup residue is not a logical import retry

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, params)
	require.NoError(t, err)
	require.False(t, pending, "a verified import is complete")
	require.Equal(t, signer.Spec.Backend.GcpKMS.ImportFingerprint(signer.SoftwareKeySecret, material),
		nodeSet.GetCosmosignerStatus(signer.Name).KeyImported)
	require.Equal(t, []string{pubkeyPod}, createdPodNames(clientSet),
		"a recorded version is verified, never re-imported")
	require.False(t, signerPodExists(t, clientSet, nodeSet.Namespace, importPod),
		"the leftover import pod must be cleaned up once its version is durably persisted")
}

func TestNodeSetGcpImportCompleteRetriesImportPodCleanup(t *testing.T) {
	nodeSet := gcpImportNodeSet()
	signer := nodeSet.ResolveCosmosigners()[0]
	source, material, publicKey := gcpImportNodeSetSource(t, nodeSet, signer.SoftwareKeySecret)
	resourceName := nodeSet.CosmosignerResourceName(signer)
	importPod := resourceName + "-import"
	version := nodeSetGcpDestinationKey + "/cryptoKeyVersions/6"
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name: signer.Name, ResourceName: resourceName, ImportedKeyVersion: version,
		KeyImported: signer.Spec.Backend.GcpKMS.ImportFingerprint(signer.SoftwareKeySecret, material),
	}}

	clientSet := fakeSignerPods(gcpImportPodLogs(version, publicKey))
	r := gcpImportRecoveryReconciler(t, clientSet, interceptor.Funcs{}, nodeSet, source)
	params, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	leftover := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: importPod, Namespace: nodeSet.Namespace, UID: "import-pod-uid"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, leftover, r.Scheme))
	_, err = clientSet.CoreV1().Pods(nodeSet.Namespace).Create(context.Background(), leftover, metav1.CreateOptions{})
	require.NoError(t, err)
	clientSet.ClearActions()

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, params)
	require.NoError(t, err)
	require.False(t, pending)
	require.Empty(t, createdPodNames(clientSet), "completed cleanup must not execute import or pubkey pods")
	require.False(t, signerPodExists(t, clientSet, nodeSet.Namespace, importPod))
}
