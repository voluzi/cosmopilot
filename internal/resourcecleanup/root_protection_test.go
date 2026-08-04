package resourcecleanup

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func TestProtectExistingRootsInstallsCleanupFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"}}
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, nodeSet).Build()

	if err := ProtectExistingRoots(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{node, nodeSet} {
		fresh := object.DeepCopyObject().(client.Object)
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(object), fresh); err != nil {
			t.Fatal(err)
		}
		if !containsString(fresh.GetFinalizers(), Finalizer) {
			t.Fatalf("%T did not receive cleanup finalizer", object)
		}
	}
}

func TestProtectExistingRootsDoesNotMutateAlreadyDeletingRoot(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "node", Namespace: "default", DeletionTimestamp: &now, Finalizers: []string{"existing"},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

	if err := ProtectExistingRoots(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	fresh := &appsv1.ChainNode{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(node), fresh); err != nil {
		t.Fatal(err)
	}
	if containsString(fresh.GetFinalizers(), Finalizer) {
		t.Fatal("a finalizer cannot be installed after deletion has begun")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
