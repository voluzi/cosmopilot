package chainnode

import (
	"context"
	"strings"
	"testing"

	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/controllers"
)

func TestPreflightRefusesOldSignerImageWithoutStoppingSigner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8sappsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"}, Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Image: ptr.To("cosmosigner:3.0.0")}}}
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(node), Namespace: node.Namespace}, Spec: k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))}}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()}
	_, err := r.preflightCosmosigner(context.Background(), node)
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

func TestRemoteTargetRefusesParentsOldSignerImage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parent := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default", UID: "parent-uid"}, Spec: appsv1.ChainNodeSetSpec{
		Cosmosigner: &appsv1.Cosmosigner{Image: ptr.To("cosmosigner:3.0.0"), NodeGroups: []string{"sentries"}},
		Nodes:       []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
	}}
	signer := parent.ResolveCosmosigners()[0]
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "chain-sentries-0", Namespace: parent.Namespace,
		Labels:          map[string]string{controllers.LabelCosmosignerTarget: parent.CosmosignerResourceName(signer)},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: parent.Name, UID: parent.UID, Controller: ptr.To(true)}},
	}, Spec: appsv1.ChainNodeSpec{RemoteSignerTarget: true}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, pod).Build()}
	_, err := r.reconcileSigningConfigs(context.Background(), node)
	if err == nil || !strings.Contains(err.Error(), "3.1.0") {
		t.Fatalf("want minimum-version refusal, got %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatal(err)
	}
}
