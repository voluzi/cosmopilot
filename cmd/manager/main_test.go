package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

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
