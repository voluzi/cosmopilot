package cosmosigner

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cosmopilotv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

// A Cloud KMS import is NOT idempotent: every `cosmosigner import` run calls ImportCryptoKeyVersion
// and adds another version of the destination key. The tests below pin the protocol that keeps a
// controller crash from turning into a second imported version — an owned import pod that already ran
// is READ rather than replaced, and it outlives the call until its version has been persisted.

const importSourceSecretName = "validator-key"

func importOwner() *cosmopilotv1.ChainNode {
	return &cosmopilotv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "validator-uid",
	}}
}

// foreignImportOwner is a same-namespace owner that happens to produce the same signer name.
func foreignImportOwner() *cosmopilotv1.ChainNode {
	return &cosmopilotv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "another-owner-uid",
	}}
}

func importPodScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cosmopilotv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// importedVersion is a crypto key version of the destination gcpImportParams() names.
func importedVersion(params Params, ordinal string) string {
	return params.Backend.GCP.Import.CryptoKeyName() + "/cryptoKeyVersions/" + ordinal
}

// podLogResponse is one container-log read: either what the pod printed, or the failure a controller
// gets when the logs of a terminated pod can no longer be read.
type podLogResponse struct {
	out string
	err error
}

// fakeSignerPods stands in for the kubelet and for the cosmosigner CLI: a one-shot pod succeeds the
// moment it is created, and every container log read returns logs. What is under test is what the
// controller does with the pod, not what the binary does inside it.
func fakeSignerPods(logs string, objects ...runtime.Object) *k8sfake.Clientset {
	return fakeSignerPodLogs([]podLogResponse{{out: logs}}, objects...)
}

// fakeSignerPodLogs serves one response per container-log read, in order, repeating the last once the
// sequence runs out. It lets a test tell apart what a pod that ALREADY ran reported from what its
// replacement reports — the distinction the failed-import recovery path turns on.
func fakeSignerPodLogs(responses []podLogResponse, objects ...runtime.Object) *k8sfake.Clientset {
	clientSet := k8sfake.NewSimpleClientset(objects...)
	clientSet.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.Status.Phase = corev1.PodSucceeded
		err := clientSet.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, action.GetNamespace())
		return true, pod, err
	})
	reads := 0
	clientSet.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		next := responses[min(reads, len(responses)-1)]
		reads++
		if next.err != nil {
			return true, nil, next.err
		}
		return true, &runtime.Unknown{Raw: []byte(next.out)}, nil
	})
	return clientSet
}

// podVerbs counts pod actions of one verb, excluding subresources so a log read is not counted as a
// plain get.
func podVerbs(clientSet *k8sfake.Clientset, verb string) int {
	count := 0
	for _, action := range clientSet.Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == "pods" && action.GetSubresource() == "" {
			count++
		}
	}
	return count
}

// ownedImportPod renders the import pod this signer would run and attributes it to owner.
func ownedImportPod(t *testing.T, owner *cosmopilotv1.ChainNode, scheme *runtime.Scheme, params Params, phase corev1.PodPhase) *corev1.Pod {
	t.Helper()
	pod := JobRunner{Params: params}.buildImportPod(importSourceSecretName)
	pod.UID = "import-pod-uid"
	pod.Status.Phase = phase
	if err := controllerutil.SetControllerReference(owner, pod, scheme); err != nil {
		t.Fatal(err)
	}
	return pod
}

func getPod(t *testing.T, clientSet *k8sfake.Clientset, namespace, name string) (*corev1.Pod, error) {
	t.Helper()
	return clientSet.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
}

// TestImportKeyResumesSucceededImportPod is the crash-recovery core of the managed import. When the
// controller dies between a succeeded `cosmosigner import` and the status write that records the
// version it created, the pod's logs are the only surviving record of that version. Replacing the pod
// would import the key a SECOND time, so an owned pod that already ran is resumed instead.
func TestImportKeyResumesSucceededImportPod(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := gcpImportParams()
	version := importedVersion(params, "7")
	existing := ownedImportPod(t, owner, scheme, params, corev1.PodSucceeded)
	clientSet := fakeSignerPods("imported key version: "+version+"\n", existing)

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	got, err := runner.ImportKey(context.Background(), importSourceSecretName)
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("ImportKey() = %q, want the version the surviving pod reported (%q)", got, version)
	}
	if created := podVerbs(clientSet, "create"); created != 0 {
		t.Fatalf("resuming a succeeded import created %d import pod(s); each one adds another Cloud KMS crypto key version", created)
	}
	if deleted := podVerbs(clientSet, "delete"); deleted != 0 {
		t.Fatalf("the succeeded import pod was deleted %d time(s) before its version could be persisted", deleted)
	}
	if _, err := getPod(t, clientSet, params.Namespace, existing.GetName()); err != nil {
		t.Fatalf("the succeeded import pod must survive ImportKey: %v", err)
	}
}

// TestImportKeyRetainsImportPodUntilCleanup covers the other half of the same window: after a FRESH
// import the pod is the only record of the created version, so ImportKey must not delete it. Only the
// caller — once the version is durably persisted — may, through CleanupImportPod.
func TestImportKeyRetainsImportPodUntilCleanup(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := gcpImportParams()
	version := importedVersion(params, "3")
	clientSet := fakeSignerPods("imported key version: " + version + "\n")

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	got, err := runner.ImportKey(context.Background(), importSourceSecretName)
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("ImportKey() = %q, want %q", got, version)
	}
	if created := podVerbs(clientSet, "create"); created != 1 {
		t.Fatalf("a first import ran %d pods, want exactly 1", created)
	}

	podName := params.Name + "-" + importJobSuffix
	if _, err := getPod(t, clientSet, params.Namespace, podName); err != nil {
		t.Fatalf("ImportKey must retain the succeeded import pod as the only record of %q until the caller persists it: %v", version, err)
	}

	if err := runner.CleanupImportPod(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := getPod(t, clientSet, params.Namespace, podName); !apierrors.IsNotFound(err) {
		t.Fatalf("CleanupImportPod must delete the import pod once its version is persisted, got err=%v", err)
	}
	// The caller cleans up on every pass through the completed import, so it must tolerate a pod that
	// is already gone.
	if err := runner.CleanupImportPod(context.Background()); err != nil {
		t.Fatalf("cleaning up an already-deleted import pod must be a no-op: %v", err)
	}
}

// TestImportKeyRecoversFailedImportPod treats a terminal Pod as evidence, not permission to re-run:
// Cloud KMS may have created the version before a later CLI verification step failed.
func TestImportKeyRecoversFailedImportPod(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := gcpImportParams()
	version := importedVersion(params, "1")
	existing := ownedImportPod(t, owner, scheme, params, corev1.PodFailed)
	clientSet := fakeSignerPods("imported key version: "+version+"\n", existing)

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	got, err := runner.ImportKey(context.Background(), importSourceSecretName)
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("ImportKey() = %q, want %q", got, version)
	}
	if created := podVerbs(clientSet, "create"); created != 0 {
		t.Fatalf("recovering a failed import created %d replacement pod(s)", created)
	}
	if deleted := podVerbs(clientSet, "delete"); deleted != 0 {
		t.Fatalf("recovering a failed import deleted the evidence pod %d time(s)", deleted)
	}
}

// TestImportKeyRetriesFailedImportPodWithoutUsableVersion is the anti-wedge half of the same rule. A
// Failed pod is only evidence while its logs name a usable version of the CURRENT destination;
// without one there is nothing to lose by running the import again, and re-reading the same unusable
// output every reconcile would leave the import stuck forever with no operator recourse.
func TestImportKeyRetriesFailedImportPodWithoutUsableVersion(t *testing.T) {
	params := gcpImportParams()
	otherKey := "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/other"
	for _, tc := range []struct {
		name string
		read podLogResponse
	}{
		{"no version line", podLogResponse{out: "Error: import job consensus-import is not enabled yet\n"}},
		{"empty logs", podLogResponse{out: ""}},
		{"unreadable logs", podLogResponse{err: stderrors.New("container log unavailable")}},
		{"version of another crypto key", podLogResponse{out: "imported key version: " + otherKey + "/cryptoKeyVersions/1\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := importOwner()
			scheme := importPodScheme(t)
			retried := importedVersion(params, "4")
			existing := ownedImportPod(t, owner, scheme, params, corev1.PodFailed)
			clientSet := fakeSignerPodLogs([]podLogResponse{tc.read, {out: "imported key version: " + retried + "\n"}}, existing)

			runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
			got, err := runner.ImportKey(context.Background(), importSourceSecretName)
			if err != nil {
				t.Fatalf("a failed import carrying no usable version must be retried, not wedged: %v", err)
			}
			if got != retried {
				t.Fatalf("ImportKey() = %q, want the version the replacement pod reported (%q)", got, retried)
			}
			if created := podVerbs(clientSet, "create"); created != 1 {
				t.Fatalf("the unusable pod must be replaced exactly once, created %d pod(s)", created)
			}
			if deleted := podVerbs(clientSet, "delete"); deleted != 1 {
				t.Fatalf("the unusable pod must be deleted before the retry, deleted %d time(s)", deleted)
			}
		})
	}
}

// TestImportKeyNeverReplacesSucceededImportPod keeps the retry strictly scoped to pods that failed. A
// pod that EXITED ZERO reported an accepted import whatever its logs turned out to say, so replacing
// it would add a second Cloud KMS version; the unusable output is surfaced as an error instead.
func TestImportKeyNeverReplacesSucceededImportPod(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := gcpImportParams()
	existing := ownedImportPod(t, owner, scheme, params, corev1.PodSucceeded)
	clientSet := fakeSignerPodLogs([]podLogResponse{
		{out: "verified: backend public key matches the source file\n"},
		{out: "imported key version: " + importedVersion(params, "4") + "\n"},
	}, existing)

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	if got, err := runner.ImportKey(context.Background(), importSourceSecretName); err == nil {
		t.Fatalf("a succeeded pod with no readable version must be reported, got version %q", got)
	}
	if created := podVerbs(clientSet, "create"); created != 0 {
		t.Fatalf("a succeeded import pod was replaced by %d new pod(s); each one is another Cloud KMS crypto key version", created)
	}
	if deleted := podVerbs(clientSet, "delete"); deleted != 0 {
		t.Fatalf("a succeeded import pod was deleted %d time(s) before its result could be persisted", deleted)
	}
}

func TestImportKeyDoesNotReplaceRunningImportPod(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := gcpImportParams()
	existing := ownedImportPod(t, owner, scheme, params, corev1.PodRunning)
	clientSet := fakeSignerPods("", existing)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	if _, err := runner.ImportKey(ctx, importSourceSecretName); err == nil {
		t.Fatal("waiting for an in-flight import should end with the test context")
	}
	if created := podVerbs(clientSet, "create"); created != 0 {
		t.Fatalf("waiting for a running import created %d replacement pod(s)", created)
	}
	if deleted := podVerbs(clientSet, "delete"); deleted != 0 {
		t.Fatalf("waiting for a running import deleted the in-flight pod %d time(s)", deleted)
	}
}

// TestImportKeyRefusesForeignImportPod pins the ownership guard the recovery path must not weaken: a
// same-named pod this owner does not control is neither read (its logs would attribute another
// signer's imported version to this one) nor deleted.
func TestImportKeyRefusesForeignImportPod(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := gcpImportParams()
	foreign := ownedImportPod(t, foreignImportOwner(), scheme, params, corev1.PodSucceeded)
	clientSet := fakeSignerPods("imported key version: "+importedVersion(params, "9")+"\n", foreign)

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	got, err := runner.ImportKey(context.Background(), importSourceSecretName)
	if err == nil {
		t.Fatalf("a foreign-owned import pod must not be adopted, got version %q", got)
	}
	if deleted := podVerbs(clientSet, "delete"); deleted != 0 {
		t.Fatalf("a foreign-owned import pod was deleted %d time(s)", deleted)
	}
	if _, err := getPod(t, clientSet, params.Namespace, foreign.GetName()); err != nil {
		t.Fatalf("a foreign-owned import pod must be left untouched: %v", err)
	}
}

// TestImportKeyReplacesPodFromPreviousDestination pins the destination-migration wedge: a same-owner
// succeeded pod whose annotation names a DIFFERENT crypto key is stale evidence and must be replaced,
// never re-read.
func TestImportKeyReplacesPodFromPreviousDestination(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := gcpImportParams()
	oldParams := gcpImportParams()
	oldParams.Backend.GCP.Import.Key = "previous-key"
	existing := ownedImportPod(t, owner, scheme, oldParams, corev1.PodSucceeded)
	existing.Annotations = map[string]string{importTargetAnnotation: oldParams.Backend.GCP.Import.CryptoKeyName()}
	clientSet := fakeSignerPods("imported key version: "+importedVersion(params, "1")+"\n", existing)

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	got, err := runner.ImportKey(context.Background(), importSourceSecretName)
	if err != nil {
		t.Fatal(err)
	}
	if got != importedVersion(params, "1") {
		t.Fatalf("ImportKey() = %q, want a fresh import into the new destination", got)
	}
	if created := podVerbs(clientSet, "create"); created != 1 {
		t.Fatalf("a previous-destination pod must be replaced, created %d pod(s)", created)
	}
	if deleted := podVerbs(clientSet, "delete"); deleted != 1 {
		t.Fatalf("a previous-destination pod must be deleted, deleted %d time(s)", deleted)
	}
}

// TestCleanupImportPodLeavesForeignPodAlone applies the same guard to the cleanup half: the pod at
// this name may have been recreated by another owner while the version was being persisted.
func TestCleanupImportPodLeavesForeignPodAlone(t *testing.T) {
	scheme := importPodScheme(t)
	params := gcpImportParams()
	foreign := ownedImportPod(t, foreignImportOwner(), scheme, params, corev1.PodSucceeded)
	clientSet := fakeSignerPods("", foreign)

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: importOwner(), Params: params}
	if err := runner.CleanupImportPod(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deleted := podVerbs(clientSet, "delete"); deleted != 0 {
		t.Fatalf("cleanup deleted %d foreign-owned pod(s)", deleted)
	}
	if _, err := getPod(t, clientSet, params.Namespace, foreign.GetName()); err != nil {
		t.Fatalf("a foreign-owned import pod must be left untouched: %v", err)
	}
}

// TestVaultImportKeyReplacesAndDeletesItsPod pins that the resumable protocol stays scoped to Cloud
// KMS. A Vault upload writes the same named transit key and verifies the stored pubkey, so re-running
// it costs nothing and its pod carries no result worth retaining.
func TestVaultImportKeyReplacesAndDeletesItsPod(t *testing.T) {
	owner := importOwner()
	scheme := importPodScheme(t)
	params := testParams()
	existing := ownedImportPod(t, owner, scheme, params, corev1.PodSucceeded)
	clientSet := fakeSignerPods("uploaded key to vault\n", existing)

	runner := JobRunner{Client: clientSet, Scheme: scheme, Owner: owner, Params: params}
	version, err := runner.ImportKey(context.Background(), importSourceSecretName)
	if err != nil {
		t.Fatal(err)
	}
	if version != "" {
		t.Fatalf("a Vault upload reports no crypto key version, got %q", version)
	}
	if created := podVerbs(clientSet, "create"); created != 1 {
		t.Fatalf("a Vault import ran %d pods, want exactly 1 replacement of the previous run", created)
	}
	if _, err := getPod(t, clientSet, params.Namespace, existing.GetName()); !apierrors.IsNotFound(err) {
		t.Fatalf("a Vault import pod is cleaned up by ImportKey, got err=%v", err)
	}
}
