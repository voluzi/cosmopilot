package chainnodeset

import (
	"context"
	"strings"
	"testing"

	k8sappsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
)

func TestPreflightRefusesOldSignerImageWithoutStoppingSigner(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default"}, Spec: appsv1.ChainNodeSetSpec{
		Cosmosigner: &appsv1.Cosmosigner{Image: ptr.To("cosmosigner:3.0.0"), NodeGroups: []string{"sentries"}},
		Nodes:       []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
	}}
	signer := resolveSingleSigner(t, nodeSet)
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: nodeSet.CosmosignerResourceName(signer), Namespace: nodeSet.Namespace}, Spec: k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))}}
	r := newValidatorTestReconciler(t, nodeSet, sts)
	err := r.preflightCosmosigners(context.Background(), nodeSet)
	if err == nil || !strings.Contains(err.Error(), "3.1.0") {
		t.Fatalf("want minimum-version refusal, got %v", err)
	}
	current := &k8sappsv1.StatefulSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sts), current); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(current.Spec.Replicas, 0) != 3 {
		t.Fatal("running signer was scaled down")
	}
}
