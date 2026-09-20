package chainnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

// stopNodeHeightAnnotation is asserted as a literal so a rename of the exported constant cannot
// silently change the on-cluster contract that a running operator depends on across restarts.
const stopNodeHeightAnnotation = "cosmopilot.voluzi.com/stop-node-snapshot-height"

// stopNodePodAPI serves the subset of the Kubernetes API that a stop-node snapshot touches: the
// node Pod (deleted, then polled until gone) and the Job listing done by the snapshot prelude.
type stopNodePodAPI struct {
	namespace string
	name      string
	present   bool
	deletes   int
	onDelete  func()
}

func (a *stopNodePodAPI) roundTrip(req *http.Request) (*http.Response, error) {
	podPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", a.namespace, a.name)
	switch {
	case req.URL.Path == podPath && req.Method == http.MethodDelete:
		if !a.present {
			return stopNodeAPIResponse(http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`), nil
		}
		a.deletes++
		a.present = false
		if a.onDelete != nil {
			a.onDelete()
		}
		return stopNodeAPIResponse(http.StatusOK, `{"kind":"Status","apiVersion":"v1","status":"Success"}`), nil

	case req.URL.Path == podPath && req.Method == http.MethodGet:
		if a.present {
			return stopNodeAPIResponse(http.StatusOK, fmt.Sprintf(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":%q,"namespace":%q}}`, a.name, a.namespace)), nil
		}
		return stopNodeAPIResponse(http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`), nil

	case strings.Contains(req.URL.Path, "/jobs") && req.Method == http.MethodGet:
		return stopNodeAPIResponse(http.StatusOK, `{"kind":"JobList","apiVersion":"batch/v1","items":[]}`), nil
	}
	return stopNodeAPIResponse(http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`), nil
}

func stopNodeAPIResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// countingUpgradeStatusClient records every node-utils call. A stop-node snapshot must never make
// one: the Pod it would reach is the one being deleted.
type countingUpgradeStatusClient struct{ calls *atomic.Int32 }

func (c countingUpgradeStatusClient) GetUpgradeStatus(context.Context) (nodeutils.UpgradeStatus, error) {
	c.calls.Add(1)
	return nodeutils.UpgradeStatus{}, errors.New("connection refused")
}

func countingUpgradeStatusClientFactory(calls *atomic.Int32) upgradeStatusClientFactory {
	return func(string) upgradeStatusClient { return countingUpgradeStatusClient{calls: calls} }
}

// volumeSnapshotCreateFailingClient fails the first failuresLeft VolumeSnapshot creations.
type volumeSnapshotCreateFailingClient struct {
	client.Client
	failuresLeft int
}

func (c *volumeSnapshotCreateFailingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*snapshotv1.VolumeSnapshot); ok && c.failuresLeft > 0 {
		c.failuresLeft--
		return errors.New("create volumesnapshot: api unavailable")
	}
	return c.Client.Create(ctx, obj, opts...)
}

func stopNodeSnapshotScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	return scheme
}

// stopNodeSnapshotChainNode is a ChainNode due for a stop-node snapshot: old enough to pass the
// first-snapshot delay, with a known height and PVC size.
func stopNodeSnapshotChainNode(height int64) *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency: "24h",
			StopNode:  ptr.To(true),
		}}},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeRunning,
			PvcSize:      "1Gi",
			LatestHeight: height,
		},
	}
}

func newStopNodeSnapshotReconciler(t *testing.T, chainNode *appsv1.ChainNode, podAPI *stopNodePodAPI, rpcCalls *atomic.Int32, objects ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	scheme := stopNodeSnapshotScheme(t)
	seed := append([]client.Object{chainNode}, objects...)
	backing := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(seed...).
		Build()
	clientSet, err := kubernetes.NewForConfigAndClient(
		&rest.Config{Host: "https://kubernetes.invalid"},
		&http.Client{Transport: roundTripperFunc(podAPI.roundTrip)},
	)
	require.NoError(t, err)
	return &Reconciler{
		Client:               backing,
		APIReader:            backing,
		ClientSet:            clientSet,
		Scheme:               scheme,
		recorder:             record.NewFakeRecorder(20),
		opts:                 &controllers.ControllerRunOptions{},
		upgradeClientFactory: countingUpgradeStatusClientFactory(rpcCalls),
	}, backing
}

func storedChainNode(t *testing.T, c client.Client, chainNode *appsv1.ChainNode) *appsv1.ChainNode {
	t.Helper()
	stored := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	return stored
}

func storedSnapshots(t *testing.T, c client.Client) []snapshotv1.VolumeSnapshot {
	t.Helper()
	list := &snapshotv1.VolumeSnapshotList{}
	require.NoError(t, c.List(context.Background(), list))
	return list.Items
}

// A stop-node snapshot must stamp the height observed while the node was still running, and must
// not ask the deleted Pod for it.
func TestStartNewSnapshotStopNodeCreatesSnapshotWithPreShutdownHeightWithoutRPC(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: true}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)

	// The height must already be persisted when the Pod goes away, or a controller restart in this
	// window loses it for good.
	var markerAtDelete string
	var phaseAtDelete appsv1.ChainNodePhase
	podAPI.onDelete = func() {
		atDelete := storedChainNode(t, backing, chainNode)
		markerAtDelete = atDelete.Annotations[stopNodeHeightAnnotation]
		phaseAtDelete = atDelete.Status.Phase
	}

	require.NoError(t, r.startNewSnapshot(ctx, chainNode))

	assert.Equal(t, 1, podAPI.deletes)
	assert.Equal(t, "123", markerAtDelete)
	assert.Equal(t, appsv1.PhaseChainNodeSnapshotting, phaseAtDelete)
	assert.Zero(t, rpcCalls.Load())

	snapshots := storedSnapshots(t, backing)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "123", snapshots[0].Annotations[controllers.AnnotationDataHeight])
	assert.Equal(t, "false", snapshots[0].Annotations[controllers.AnnotationPvcSnapshotReady])
	require.NotNil(t, snapshots[0].Spec.Source.PersistentVolumeClaimName)
	assert.Equal(t, "node", *snapshots[0].Spec.Source.PersistentVolumeClaimName)

	stored := storedChainNode(t, backing, chainNode)
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.NotContains(t, stored.Annotations, stopNodeHeightAnnotation)
	assert.Empty(t, stored.Annotations[controllers.AnnotationLastPvcSnapshot])
	assert.Equal(t, appsv1.PhaseChainNodeSnapshotting, stored.Status.Phase)
}

// A resumed attempt stops the node again, so it must announce Snapshotting before doing so: while
// the phase says Running the ChainNode reports itself ready even though its pod is gone.
func TestCreateSnapshotStopNodeMarksSnapshottingBeforeStoppingNodeOnResume(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	chainNode.Annotations = map[string]string{stopNodeHeightAnnotation: "123"}
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: true}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)

	var phaseAtDelete appsv1.ChainNodePhase
	podAPI.onDelete = func() {
		phaseAtDelete = storedChainNode(t, backing, chainNode).Status.Phase
	}

	_, err := r.createSnapshot(ctx, chainNode)

	require.NoError(t, err)
	assert.Equal(t, 1, podAPI.deletes)
	assert.Equal(t, appsv1.PhaseChainNodeSnapshotting, phaseAtDelete)
}

// An unrelated snapshot that happens to be in flight is not this attempt's snapshot: adopting it
// would drop the snapshot the node was stopped for.
func TestEnsureVolumeSnapshotsStopNodeDoesNotAdoptUnrelatedSnapshot(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	chainNode.Annotations = map[string]string{stopNodeHeightAnnotation: "123"}
	unrelated := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node-unrelated",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotReady: "false",
				controllers.AnnotationDataHeight:       "999",
			},
			Labels: map[string]string{controllers.LabelChainNode: "node"},
		},
	}
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: false}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls, unrelated)

	require.NoError(t, r.ensureVolumeSnapshots(ctx, chainNode, false))

	assert.Len(t, storedSnapshots(t, backing), 1)
	stored := storedChainNode(t, backing, chainNode)
	assert.Equal(t, "123", stored.Annotations[stopNodeHeightAnnotation],
		"the pending snapshot must not be satisfied by an unrelated one")

	// Once the unrelated snapshot settles, the pending one is finally taken.
	settled := storedSnapshots(t, backing)[0]
	settled.CreationTimestamp = metav1.Now()
	settled.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)}
	require.NoError(t, backing.Update(ctx, &settled))

	require.NoError(t, r.ensureVolumeSnapshots(ctx, storedChainNode(t, backing, chainNode), false))

	snapshots := storedSnapshots(t, backing)
	require.Len(t, snapshots, 2)
	heights := []string{
		snapshots[0].Annotations[controllers.AnnotationDataHeight],
		snapshots[1].Annotations[controllers.AnnotationDataHeight],
	}
	assert.Contains(t, heights, "123")
	assert.NotContains(t, storedChainNode(t, backing, chainNode).Annotations, stopNodeHeightAnnotation)
}

// Without a known height there is nothing to stamp, so the node must be left running.
func TestCreateSnapshotStopNodeRefusesToStopWithoutKnownHeight(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(0)
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: true}
	podAPI.onDelete = func() { t.Error("node pod was deleted without a known height") }
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)

	snapshot, err := r.createSnapshot(ctx, chainNode)

	require.Error(t, err)
	assert.Nil(t, snapshot)
	assert.Zero(t, podAPI.deletes)
	assert.Zero(t, rpcCalls.Load())
	assert.Empty(t, storedSnapshots(t, backing))
	assert.NotContains(t, storedChainNode(t, backing, chainNode).Annotations, stopNodeHeightAnnotation)
}

// If the height cannot be persisted, stopping the node would lose it. Keep the node running.
func TestCreateSnapshotStopNodeDoesNotStopNodeWhenMarkerPersistFails(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: true}
	podAPI.onDelete = func() { t.Error("node pod was deleted before the height was persisted") }
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)
	r.Client = &updateFailingClient{Client: r.Client}

	snapshot, err := r.createSnapshot(ctx, chainNode)

	require.Error(t, err)
	assert.Nil(t, snapshot)
	assert.Zero(t, podAPI.deletes)
	assert.Empty(t, storedSnapshots(t, backing))
}

// A VolumeSnapshot creation failure after the node is already down must not restart the node: the
// attempt is retried at the same height until it lands.
func TestEnsureVolumeSnapshotsStopNodeRetriesCreateWithoutRecreatingPodAfterShutdown(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: true}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)
	r.Client = &volumeSnapshotCreateFailingClient{Client: r.Client, failuresLeft: 1}

	require.Error(t, r.ensureVolumeSnapshots(ctx, chainNode, true))

	stored := storedChainNode(t, backing, chainNode)
	assert.Equal(t, "123", stored.Annotations[stopNodeHeightAnnotation])
	assert.NotEqual(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.Equal(t, 1, podAPI.deletes)
	assert.Empty(t, storedSnapshots(t, backing))
	assert.True(t, stopNodeSnapshotHoldsPod(stored), "node must stay stopped while the snapshot is pending")

	// Second pass: the node is still down, so the snapshot is simply retried.
	require.NoError(t, r.ensureVolumeSnapshots(ctx, stored, true))

	snapshots := storedSnapshots(t, backing)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "123", snapshots[0].Annotations[controllers.AnnotationDataHeight])
	assert.Equal(t, 1, podAPI.deletes)
	assert.Zero(t, rpcCalls.Load())
	stored = storedChainNode(t, backing, chainNode)
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.NotContains(t, stored.Annotations, stopNodeHeightAnnotation)
}

// A controller that restarts between the shutdown and the VolumeSnapshot must finish the snapshot
// at the recorded height instead of just bringing the node back.
func TestEnsureVolumeSnapshotsStopNodeResumesPendingMarkerAfterControllerRestart(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(700)
	chainNode.Annotations = map[string]string{stopNodeHeightAnnotation: "500"}
	chainNode.Status.Phase = appsv1.PhaseChainNodeSnapshotting
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: false}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)

	require.NoError(t, r.ensureVolumeSnapshots(ctx, chainNode, false))

	snapshots := storedSnapshots(t, backing)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "500", snapshots[0].Annotations[controllers.AnnotationDataHeight],
		"the recorded pre-shutdown height wins over the current status")
	assert.Zero(t, podAPI.deletes)
	assert.Zero(t, rpcCalls.Load())
	stored := storedChainNode(t, backing, chainNode)
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.NotContains(t, stored.Annotations, stopNodeHeightAnnotation)
}

// If the VolumeSnapshot was created but the ChainNode update was lost, the next pass must adopt the
// existing snapshot rather than take a second one.
func TestEnsureVolumeSnapshotsStopNodeRepairClearsMarkerWhenSnapshotAlreadyExists(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	chainNode.Annotations = map[string]string{stopNodeHeightAnnotation: "123"}
	existing := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node-already-created",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotReady: "false",
				controllers.AnnotationDataHeight:       "123",
			},
			Labels: map[string]string{controllers.LabelChainNode: "node"},
		},
	}
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: false}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls, existing)

	require.NoError(t, r.ensureVolumeSnapshots(ctx, chainNode, false))

	assert.Len(t, storedSnapshots(t, backing), 1, "no duplicate snapshot")
	assert.Zero(t, podAPI.deletes)
	stored := storedChainNode(t, backing, chainNode)
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.NotContains(t, stored.Annotations, stopNodeHeightAnnotation)
}

// A snapshot that became usable before the controller managed to record it is still this attempt's
// snapshot: adopt it rather than stopping the node again for a second one.
func TestEnsureVolumeSnapshotsStopNodeAdoptsSnapshotThatBecameReadyBeforeBeingRecorded(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	chainNode.Annotations = map[string]string{stopNodeHeightAnnotation: "123"}
	existing := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-already-created",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotReady: "false",
				controllers.AnnotationDataHeight:       "123",
			},
			Labels: map[string]string{controllers.LabelChainNode: "node"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: false}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls, existing)

	require.NoError(t, r.ensureVolumeSnapshots(ctx, chainNode, false))

	assert.Len(t, storedSnapshots(t, backing), 1, "no second snapshot for an attempt already finished")
	assert.Zero(t, podAPI.deletes)
	stored := storedChainNode(t, backing, chainNode)
	assert.NotContains(t, stored.Annotations, stopNodeHeightAnnotation)
	assert.NotEmpty(t, stored.Annotations[controllers.AnnotationLastPvcSnapshot])
}

// The node comes back only once the snapshot is usable, and the cadence then holds: no second
// shutdown until the frequency has elapsed.
func TestEnsureVolumeSnapshotsStopNodeResumesOnlyAfterSnapshotReadyAndDoesNotLoop(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: true}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)

	require.NoError(t, r.ensureVolumeSnapshots(ctx, chainNode, true))
	require.Len(t, storedSnapshots(t, backing), 1)

	// While the snapshot is not ready the node stays down and nothing else is started.
	for range 2 {
		stored := storedChainNode(t, backing, chainNode)
		require.True(t, stopNodeSnapshotHoldsPod(stored))
		require.NoError(t, r.ensureVolumeSnapshots(ctx, stored, false))
		require.Len(t, storedSnapshots(t, backing), 1)
		require.Equal(t, 1, podAPI.deletes)
	}

	snapshot := storedSnapshots(t, backing)[0]
	snapshot.CreationTimestamp = metav1.Now()
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)}
	require.NoError(t, backing.Update(ctx, &snapshot))

	stored := storedChainNode(t, backing, chainNode)
	require.NoError(t, r.ensureVolumeSnapshots(ctx, stored, false))

	stored = storedChainNode(t, backing, chainNode)
	assert.Equal(t, "false", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.NotEmpty(t, stored.Annotations[controllers.AnnotationLastPvcSnapshot])
	assert.False(t, stopNodeSnapshotHoldsPod(stored), "the node may resume once the snapshot is usable")

	// The node is running again; the next pass must not immediately stop it a second time.
	podAPI.present = true
	require.NoError(t, r.ensureVolumeSnapshots(ctx, stored, true))
	assert.Equal(t, 1, podAPI.deletes)
	assert.Len(t, storedSnapshots(t, backing), 1)
	assert.Zero(t, rpcCalls.Load())
}

// Turning snapshots (or stopNode) off must release a node that is being held down for a snapshot.
func TestEnsureVolumeSnapshotsClearsStopNodeMarkerWhenSnapshotsDisabled(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	chainNode.Annotations = map[string]string{stopNodeHeightAnnotation: "123"}
	chainNode.Spec.Persistence.Snapshots = nil
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: false}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)

	require.NoError(t, r.ensureVolumeSnapshots(ctx, chainNode, false))

	stored := storedChainNode(t, backing, chainNode)
	assert.NotContains(t, stored.Annotations, stopNodeHeightAnnotation)
	assert.False(t, stopNodeSnapshotHoldsPod(stored))
	assert.Zero(t, podAPI.deletes)
	assert.Empty(t, storedSnapshots(t, backing))
}

func TestEnsureVolumeSnapshotsClearsStopNodeMarkerWhenStopNodeDisabled(t *testing.T) {
	ctx := t.Context()
	chainNode := stopNodeSnapshotChainNode(123)
	chainNode.Annotations = map[string]string{stopNodeHeightAnnotation: "123"}
	chainNode.Spec.Persistence.Snapshots.StopNode = ptr.To(false)
	podAPI := &stopNodePodAPI{namespace: "default", name: "node", present: false}
	rpcCalls := &atomic.Int32{}
	r, backing := newStopNodeSnapshotReconciler(t, chainNode, podAPI, rpcCalls)

	require.NoError(t, r.ensureVolumeSnapshots(ctx, chainNode, false))

	stored := storedChainNode(t, backing, chainNode)
	assert.NotContains(t, stored.Annotations, stopNodeHeightAnnotation)
	assert.False(t, stopNodeSnapshotHoldsPod(stored))
	assert.Zero(t, podAPI.deletes)
	assert.Empty(t, storedSnapshots(t, backing))
}

func TestStopNodeSnapshotHoldsPod(t *testing.T) {
	withAnnotations := func(chainNode *appsv1.ChainNode, annotations map[string]string) *appsv1.ChainNode {
		chainNode.Annotations = annotations
		return chainNode
	}
	tests := []struct {
		name      string
		chainNode *appsv1.ChainNode
		want      bool
	}{
		{
			name:      "no persistence config",
			chainNode: &appsv1.ChainNode{},
		},
		{
			name:      "stop-node snapshot pending",
			chainNode: withAnnotations(stopNodeSnapshotChainNode(1), map[string]string{stopNodeHeightAnnotation: "1"}),
			want:      true,
		},
		{
			name:      "stop-node snapshot in progress",
			chainNode: withAnnotations(stopNodeSnapshotChainNode(1), map[string]string{controllers.AnnotationPvcSnapshotInProgress: "true"}),
			want:      true,
		},
		{
			name:      "no snapshot activity",
			chainNode: stopNodeSnapshotChainNode(1),
		},
		{
			name: "pending marker but stop-node disabled",
			chainNode: func() *appsv1.ChainNode {
				chainNode := withAnnotations(stopNodeSnapshotChainNode(1), map[string]string{stopNodeHeightAnnotation: "1"})
				chainNode.Spec.Persistence.Snapshots.StopNode = ptr.To(false)
				return chainNode
			}(),
		},
		{
			name: "in progress but snapshots disabled",
			chainNode: func() *appsv1.ChainNode {
				chainNode := withAnnotations(stopNodeSnapshotChainNode(1), map[string]string{controllers.AnnotationPvcSnapshotInProgress: "true"})
				chainNode.Spec.Persistence.Snapshots = nil
				return chainNode
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stopNodeSnapshotHoldsPod(tt.chainNode))
		})
	}
}
