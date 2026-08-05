package main

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/controllers/chainnode"
	"github.com/voluzi/cosmopilot/v2/internal/controllers/chainnodeset"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestRootProtectionReadinessAllowsStandbyBeforeLeadership(t *testing.T) {
	elected := make(chan struct{})
	ready := make(chan struct{})
	if err := rootProtectionReadiness(true, elected, ready)(nil); err != nil {
		t.Fatalf("standby readiness must not wait for leader-only startup migration: %v", err)
	}
}

func TestRootProtectionReadinessWaitsWithoutLeaderElection(t *testing.T) {
	elected := make(chan struct{})
	ready := make(chan struct{})
	check := rootProtectionReadiness(false, elected, ready)
	if err := check(nil); err == nil {
		t.Fatal("readiness must wait for startup migration when leader election is disabled")
	}
	close(ready)
	if err := check(nil); err != nil {
		t.Fatalf("readiness remained blocked after startup migration completed: %v", err)
	}
}

func TestRootProtectionReadinessWaitsForStartupMigrationAfterLeadership(t *testing.T) {
	elected := make(chan struct{})
	ready := make(chan struct{})
	check := rootProtectionReadiness(true, elected, ready)
	close(elected)
	if err := check(nil); err == nil {
		t.Fatal("elected leader readiness must fail before root protection and durable migration complete")
	}
	close(ready)
	if err := check(nil); err != nil {
		t.Fatalf("readiness remained blocked after startup migration completed: %v", err)
	}
}

func TestIsRecordedStartupNodeSetChildRequiresExactUID(t *testing.T) {
	runOpts.WorkerName = ""
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "set-fullnodes-0", Namespace: "default", UID: types.UID("node-uid")}}
	nodeSet := appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default"},
		Status: appsv1.ChainNodeSetStatus{Nodes: []appsv1.ChainNodeSetNodeStatus{{
			Name: node.Name, UID: node.UID,
		}}},
	}
	if !isRecordedStartupNodeSetChild(node, []appsv1.ChainNodeSet{nodeSet}) {
		t.Fatal("status-recorded generated child must be migrated through its ChainNodeSet root")
	}
	node.UID = types.UID("replacement-uid")
	if isRecordedStartupNodeSetChild(node, []appsv1.ChainNodeSet{nodeSet}) {
		t.Fatal("same-name replacement must not inherit the recorded ChainNodeSet root")
	}
}

func TestIsRecordedStartupNodeSetChildBlocksUIDLessLegacyStatus(t *testing.T) {
	runOpts.WorkerName = ""
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "set-fullnodes-0", Namespace: "default", UID: types.UID("node-uid")}}
	nodeSet := appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default"},
		Status: appsv1.ChainNodeSetStatus{Nodes: []appsv1.ChainNodeSetNodeStatus{{
			Name: node.Name,
		}}},
	}
	if !isRecordedStartupNodeSetChild(node, []appsv1.ChainNodeSet{nodeSet}) {
		t.Fatal("UID-less pre-upgrade status must block standalone child migration")
	}
}

func TestIsRecordedStartupNodeSetChildIgnoresWorkerPartition(t *testing.T) {
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "set-fullnodes-0", Namespace: "default", UID: types.UID("node-uid"),
		Labels: map[string]string{controllers.LabelWorkerName: "worker-a"},
	}}
	nodeSet := appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "set", Namespace: "default",
			Labels: map[string]string{controllers.LabelWorkerName: "worker-b"},
		},
		Status: appsv1.ChainNodeSetStatus{Nodes: []appsv1.ChainNodeSetNodeStatus{{Name: node.Name, UID: node.UID}}},
	}
	if !isRecordedStartupNodeSetChild(node, []appsv1.ChainNodeSet{nodeSet}) {
		t.Fatal("a child recorded by another worker's ChainNodeSet must not be migrated as standalone")
	}
}

// A root whose migration fails must not abort the sweep: the manager would crashloop and
// WaitForRootProtection would block reconciliation for every other root in the cluster.
func TestMigrateLegacyDurableResourcesIsolatesPerRootFailures(t *testing.T) {
	runOpts.WorkerName = ""
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	failing := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "wedged", Namespace: "default", UID: "wedged-uid"}}
	healthy := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "healthy", Namespace: "default", UID: "healthy-uid"}}
	failingPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: failing.Name, Namespace: failing.Namespace, UID: "wedged-pvc"}}
	healthyPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: healthy.Name, Namespace: healthy.Namespace, UID: "healthy-pvc"}}
	if err := controllerutil.SetControllerReference(failing, failingPVC, scheme); err != nil {
		t.Fatal(err)
	}
	if err := controllerutil.SetControllerReference(healthy, healthyPVC, scheme); err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(failing, healthy, failingPVC, healthyPVC).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if obj.GetName() == failingPVC.Name {
					return fmt.Errorf("simulated migration failure")
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()

	nodeReconciler := &chainnode.Reconciler{Client: c, Scheme: scheme}
	nodeSetReconciler := &chainnodeset.Reconciler{Client: c, Scheme: scheme}

	if _, err := migrateLegacyDurableResources(context.Background(), c, nodeReconciler, nodeSetReconciler); err != nil {
		t.Fatalf("a single failing root must not fail the whole startup migration: %v", err)
	}

	fresh := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(healthyPVC), fresh); err != nil {
		t.Fatal(err)
	}
	if !resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(healthy), resourcecleanup.ClassDataVolumes) {
		t.Fatal("the healthy root must still be migrated when another root fails")
	}
}
