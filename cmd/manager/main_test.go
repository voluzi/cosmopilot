package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func TestRootProtectionReadinessAllowsStandbyBeforeLeadership(t *testing.T) {
	elected := make(chan struct{})
	ready := make(chan struct{})
	if err := rootProtectionReadiness(elected, ready)(nil); err != nil {
		t.Fatalf("standby readiness must not wait for leader-only startup migration: %v", err)
	}
}

func TestRootProtectionReadinessWaitsForStartupMigrationAfterLeadership(t *testing.T) {
	elected := make(chan struct{})
	ready := make(chan struct{})
	check := rootProtectionReadiness(elected, ready)
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
