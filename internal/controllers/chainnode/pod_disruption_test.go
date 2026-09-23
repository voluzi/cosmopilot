package chainnode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

type fixedDisruptionUpgradeStatusClient struct {
	status nodeutils.UpgradeStatus
}

func (c fixedDisruptionUpgradeStatusClient) GetUpgradeStatus(context.Context) (nodeutils.UpgradeStatus, error) {
	return c.status, nil
}

type failingDisruptionReader struct {
	client.Reader
	getErr  error
	listErr error
}

func (r failingDisruptionReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	return r.getErr
}

func (r failingDisruptionReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return r.listErr
}

func disruptionTestPod(namespace, name string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{"domain": "a"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func TestDisruptionAllowanceIgnoresOtherNamespaces(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disruptionTestPod("target", "ready", true),
		disruptionTestPod("other", "unready", false),
	).Build()
	r := &Reconciler{Client: reader, APIReader: reader, opts: &controllers.ControllerRunOptions{DisruptionMaxUnavailable: 1}}
	require.NoError(t, r.checkDisruptionAllowance(context.Background(), "target", map[string]string{"domain": "a"}))
}

func TestDisruptionAllowanceCountsSameNamespaceUnavailable(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disruptionTestPod("target", "ready", true),
		disruptionTestPod("target", "unready", false),
	).Build()
	r := &Reconciler{Client: reader, APIReader: reader, opts: &controllers.ControllerRunOptions{DisruptionMaxUnavailable: 1}}
	require.Error(t, r.checkDisruptionAllowance(context.Background(), "target", map[string]string{"domain": "a"}))
}

func TestDisruptionBudgetEvidenceExplainsDomain(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disruptionTestPod("target", "unready", false)).Build()
	r := &Reconciler{Client: reader, APIReader: reader, opts: &controllers.ControllerRunOptions{DisruptionMaxUnavailable: 1}}
	err := r.checkDisruptionAllowance(t.Context(), "target", map[string]string{"domain": "a"})
	var evidence *disruptionBudgetExhaustedError
	require.ErrorAs(t, err, &evidence)
	require.Equal(t, 1, evidence.Unavailable)
	require.Equal(t, 1, evidence.Maximum)
	require.ErrorContains(t, err, "1/1")
	require.ErrorContains(t, err, "target")
	require.ErrorContains(t, err, "domain=a")
}

func TestDisruptionAllowanceReadsFreshSiblings(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disruptionTestPod("target", "sibling", true)).Build()
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disruptionTestPod("target", "sibling", false)).Build()
	r := &Reconciler{Client: cached, APIReader: direct, opts: &controllers.ControllerRunOptions{DisruptionMaxUnavailable: 1}}
	require.Error(t, r.checkDisruptionAllowance(context.Background(), "target", map[string]string{"domain": "a"}))
}

func TestFreshDisruptionTargetUsesDirectReader(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	stale := disruptionTestPod("target", "node", true)
	stale.UID = types.UID("original")
	fresh := disruptionTestPod("target", "node", false)
	fresh.UID = stale.UID
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).Build()
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fresh).Build()
	r := &Reconciler{Client: cached, APIReader: direct}
	got, err := r.freshDisruptionTarget(context.Background(), stale)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.False(t, isPodRunningAndReady(got))
}

func TestFreshDisruptionTargetSkipsReplacedUID(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	stale := disruptionTestPod("target", "node", true)
	stale.UID = types.UID("original")
	replacement := stale.DeepCopy()
	replacement.UID = types.UID("replacement")
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(replacement).Build()
	r := &Reconciler{APIReader: direct}
	got, err := r.freshDisruptionTarget(context.Background(), stale)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestFreshDisruptionTargetSkipsDeletedPod(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	stale := disruptionTestPod("target", "node", true)
	direct := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{APIReader: direct}
	got, err := r.freshDisruptionTarget(context.Background(), stale)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestDisruptionDirectReadErrorsPropagate(t *testing.T) {
	want := errors.New("API unavailable")
	r := &Reconciler{APIReader: failingDisruptionReader{getErr: want, listErr: want}, opts: &controllers.ControllerRunOptions{DisruptionMaxUnavailable: 1}}
	_, err := r.freshDisruptionTarget(context.Background(), disruptionTestPod("target", "node", true))
	require.ErrorIs(t, err, want)
	require.ErrorIs(t, r.checkDisruptionAllowance(context.Background(), "target", map[string]string{"domain": "a"}), want)
}

func TestRecreatePodDefersWithoutChangingPhase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		busy   bool
		reason string
	}{
		{name: "busy domain", busy: true, reason: appsv1.ReasonDisruptionLockBusy},
		{name: "unavailable sibling", reason: appsv1.ReasonDisruptionBudgetExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "node"}}
			node.Status.ChainID = "chain"
			node.Status.Phase = appsv1.PhaseChainNodeRunning
			target := disruptionTestPod("target", "node", true)
			target.Labels[controllers.LabelChainID] = "chain"
			sibling := disruptionTestPod("target", "sibling", false)
			sibling.Labels[controllers.LabelChainID] = "chain"
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, target, sibling).WithStatusSubresource(node).Build()
			r := &Reconciler{Client: cl, APIReader: cl, recorder: record.NewFakeRecorder(10),
				opts: &controllers.ControllerRunOptions{DisruptionMaxUnavailable: 1}, disruptionLocks: newLockManager()}
			if tc.busy {
				release, acquired := r.disruptionLocks.tryAcquire("target", map[string]string{controllers.LabelChainID: "chain"})
				require.True(t, acquired)
				t.Cleanup(release)
			}
			require.NoError(t, r.recreatePod(context.Background(), node, target, target.DeepCopy(), true))
			if !tc.busy {
				require.Empty(t, r.disruptionLocks.active)
			}
			require.Equal(t, appsv1.PhaseChainNodeRunning, node.Status.Phase)
			condition := apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred)
			require.NotNil(t, condition)
			require.Equal(t, tc.reason, condition.Reason)
			if !tc.busy {
				require.Contains(t, condition.Message, "1/1")
				require.Contains(t, condition.Message, "target")
				require.Contains(t, condition.Message, controllers.LabelChainID+"=chain")
			}
			stored := &corev1.Pod{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(target), stored))
		})
	}
}

func TestRecreatePodDeleteRejectsReplacedUID(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Status.ChainID = "chain"
	node.Status.Phase = appsv1.PhaseChainNodeRunning
	oldPod := disruptionTestPod(node.Namespace, node.Name, true)
	oldPod.UID = types.UID("old-uid")
	oldPod.Labels[controllers.LabelChainID] = "chain"
	backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, oldPod).WithStatusSubresource(node).Build()
	var replacedPodDeleted bool
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid", ContentConfig: rest.ContentConfig{ContentType: "application/json"}},
		&http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			status := http.StatusInternalServerError
			body := `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unexpected request","code":500}`
			if req.Method == http.MethodDelete {
				var options metav1.DeleteOptions
				payload, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(payload, &options); err != nil {
					return nil, err
				}
				if options.Preconditions != nil && options.Preconditions.UID != nil && *options.Preconditions.UID != types.UID("replacement-uid") {
					status = http.StatusConflict
					body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"UID precondition failed","reason":"Conflict","code":409}`
				} else {
					replacedPodDeleted = true
					status = http.StatusOK
					body = `{"kind":"Status","apiVersion":"v1","status":"Success"}`
				}
			} else if req.Method == http.MethodGet && replacedPodDeleted {
				status = http.StatusNotFound
				body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
	require.NoError(t, err)
	r := &Reconciler{Client: backing, APIReader: backing, ClientSet: clientSet, recorder: record.NewFakeRecorder(10),
		opts: &controllers.ControllerRunOptions{DisruptionMaxUnavailable: 1}, disruptionLocks: newLockManager()}
	err = r.recreatePod(t.Context(), node, oldPod, oldPod.DeepCopy(), true)
	require.True(t, apierrors.IsConflict(err), "expected a retryable UID precondition conflict, got %v", err)
	require.False(t, replacedPodDeleted, "replacement Pod must survive an old UID deletion attempt")
}

func TestPodRecreationDeferredTransitionsAndPreservesConditions(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "node", Generation: 2}}
	node.Status.Conditions = []metav1.Condition{{Type: "Unrelated", Status: metav1.ConditionTrue, Reason: "Stable", LastTransitionTime: metav1.Now()}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).WithStatusSubresource(node).Build()
	recorder := record.NewFakeRecorder(10)
	r := &Reconciler{Client: cl, recorder: recorder}
	ctx := context.Background()
	require.NoError(t, r.setPodRecreationDeferred(ctx, node, appsv1.ReasonDisruptionLockBusy, "waiting for the domain"))
	condition := apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred)
	require.NotNil(t, condition)
	require.Equal(t, appsv1.ReasonDisruptionLockBusy, condition.Reason)
	require.NotNil(t, apiMeta.FindStatusCondition(node.Status.Conditions, "Unrelated"))
	require.Len(t, recorder.Events, 1)
	require.NoError(t, r.setPodRecreationDeferred(ctx, node, appsv1.ReasonDisruptionLockBusy, "waiting for the domain"))
	require.Len(t, recorder.Events, 1)
	require.NoError(t, r.setPodRecreationDeferred(ctx, node, appsv1.ReasonDisruptionBudgetExhausted, "one pod is unavailable"))
	condition = apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred)
	require.Equal(t, appsv1.ReasonDisruptionBudgetExhausted, condition.Reason)
	require.Len(t, recorder.Events, 2)
	require.NoError(t, r.clearPodRecreationDeferred(ctx, node))
	require.Nil(t, apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred))
	require.NotNil(t, apiMeta.FindStatusCondition(node.Status.Conditions, "Unrelated"))
	require.NoError(t, r.clearPodRecreationDeferred(ctx, node))
	require.Len(t, recorder.Events, 2)
}

func TestCreatePodClearsStaleRecreationDeferral(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Status.Conditions = []metav1.Condition{{Type: appsv1.ConditionPodRecreationDeferred,
		Status: metav1.ConditionTrue, Reason: appsv1.ReasonDisruptionBudgetExhausted, LastTransitionTime: metav1.Now()}}
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node).Build()
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"},
		&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusInternalServerError,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"stop after create attempt","code":500}`))}, nil
		})})
	require.NoError(t, err)
	r := &Reconciler{Client: backing, ClientSet: clientSet, recorder: record.NewFakeRecorder(10)}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: node.Namespace, Name: node.Name}}
	require.ErrorContains(t, r.createPod(t.Context(), node, pod), "stop after create attempt")
	require.Nil(t, apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred))
}

func TestEnsurePodClearsDeferralBeforeNodeUtilsProbe(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Status.Conditions = []metav1.Condition{{Type: appsv1.ConditionPodRecreationDeferred,
		Status: metav1.ConditionTrue, Reason: appsv1.ReasonDisruptionBudgetExhausted, LastTransitionTime: metav1.Now()}}
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(node, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	current, err := specReconciler.getPodSpec(ctx, node, "config-hash", credential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(current, credential)
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config).Build()
	r := &Reconciler{Client: backing, Scheme: scheme, recorder: record.NewFakeRecorder(10),
		opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
		upgradeClientFactory: func(string) upgradeStatusClient {
			return failingUpgradeStatusClient{err: errors.New("node-utils unavailable")}
		},
	}
	require.ErrorContains(t, r.ensurePod(ctx, nil, node, "config-hash"), "node-utils unavailable")
	require.Nil(t, apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred))
}

func TestRecoveredUpgradeClearsStaleRecreationDeferral(t *testing.T) {
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Status.Upgrades = []appsv1.Upgrade{{Height: 100, Source: appsv1.ManualUpgrade, Name: "v2", Image: "app:v2", Status: appsv1.UpgradeOnGoing}}
	node.Status.Conditions = []metav1.Condition{{Type: appsv1.ConditionPodRecreationDeferred,
		Status: metav1.ConditionTrue, Reason: appsv1.ReasonDisruptionLockBusy, LastTransitionTime: metav1.Now()}}
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node).Build()
	r := &Reconciler{Client: backing}
	pod := &corev1.Pod{}
	require.NoError(t, stampUpgradeIdentity(pod, &node.Status.Upgrades[0]))
	handled, err := r.recoverOngoingUpgrade(t.Context(), node, pod, pod.DeepCopy())
	require.NoError(t, err)
	require.True(t, handled)
	require.Nil(t, apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred))
}

func TestRequiredUpgradeClearsStaleRecreationDeferral(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Status.Conditions = []metav1.Condition{{Type: appsv1.ConditionPodRecreationDeferred,
		Status: metav1.ConditionTrue, Reason: appsv1.ReasonDisruptionBudgetExhausted, LastTransitionTime: metav1.Now()}}
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(node, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	current, err := specReconciler.getPodSpec(ctx, node, "config-hash", credential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(current, credential)
	delete(current.Annotations, controllers.AnnotationConfigHash)
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config).Build()
	r := &Reconciler{Client: backing, Scheme: scheme, recorder: record.NewFakeRecorder(10),
		opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
		upgradeClientFactory: func(string) upgradeStatusClient {
			return fixedDisruptionUpgradeStatusClient{status: nodeutils.UpgradeStatus{RequiredUpgrade: &nodeutils.RequiredUpgrade{
				Height: 100, Source: nodeutils.ManualUpgrade, Name: "v2", Image: "app:v2",
			}}}
		},
	}
	require.ErrorContains(t, r.ensurePod(ctx, nil, node, "config-hash"), "missing upgrade or image")
	require.Nil(t, apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred))
}
