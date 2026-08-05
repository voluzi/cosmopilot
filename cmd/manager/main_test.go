package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func TestRootProtectionReadinessWaitsForStartupMigration(t *testing.T) {
	ready := make(chan struct{})
	check := rootProtectionReadiness(ready)
	if err := check(nil); err == nil {
		t.Fatal("readiness must fail before root protection and durable migration complete")
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

func TestIsRecordedStartupNodeSetChildChecksNodeSetsAcrossWorkers(t *testing.T) {
	runOpts.WorkerName = "worker-a"
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "set-fullnodes-0", Namespace: "default", UID: types.UID("node-uid"),
		Labels: map[string]string{"cosmopilot.voluzi.com/worker-name": "worker-a"},
	}}
	nodeSet := appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "set", Namespace: "default",
			Labels: map[string]string{"cosmopilot.voluzi.com/worker-name": "worker-b"},
		},
		Status: appsv1.ChainNodeSetStatus{Nodes: []appsv1.ChainNodeSetNodeStatus{{Name: node.Name, UID: node.UID}}},
	}
	if !isRecordedStartupNodeSetChild(node, []appsv1.ChainNodeSet{nodeSet}) {
		t.Fatal("a child recorded by another worker's ChainNodeSet must not be migrated as standalone")
	}
}
