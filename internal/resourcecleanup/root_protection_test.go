package resourcecleanup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

	if err := ProtectExistingRoots(context.Background(), c, ""); err != nil {
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

	if err := ProtectExistingRoots(context.Background(), c, ""); err != nil {
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

func TestProtectExistingRootsRetriesConflictWithoutClobberingConcurrentFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "node", Namespace: "default", ResourceVersion: "1",
	}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	c := &conflictOnceRootClient{Client: base}

	if err := ProtectExistingRoots(context.Background(), c, ""); err != nil {
		t.Fatal(err)
	}
	if c.patchAttempts != 2 {
		t.Fatalf("patch attempts = %d, want 2", c.patchAttempts)
	}
	fresh := &appsv1.ChainNode{}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(node), fresh); err != nil {
		t.Fatal(err)
	}
	for _, finalizer := range []string{Finalizer, concurrentFinalizer} {
		if !containsString(fresh.GetFinalizers(), finalizer) {
			t.Fatalf("finalizers %v do not contain %q", fresh.GetFinalizers(), finalizer)
		}
	}
}

func TestProtectExistingRootsScopesFinalizersToWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mine := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "mine", Namespace: "default", Labels: map[string]string{"cosmopilot.voluzi.com/worker-name": "worker-a"},
	}}
	other := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "other", Namespace: "default", Labels: map[string]string{"cosmopilot.voluzi.com/worker-name": "worker-b"},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mine, other).Build()

	if err := ProtectExistingRoots(context.Background(), c, "worker-a"); err != nil {
		t.Fatal(err)
	}
	for object, want := range map[client.Object]bool{mine: true, other: false} {
		fresh := object.DeepCopyObject().(client.Object)
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(object), fresh); err != nil {
			t.Fatal(err)
		}
		if got := containsString(fresh.GetFinalizers(), Finalizer); got != want {
			t.Fatalf("%s cleanup finalizer = %v, want %v", object.GetName(), got, want)
		}
	}
}

func TestRootProtectorRequiresLeaderElection(t *testing.T) {
	if !(&RootProtector{}).NeedLeaderElection() {
		t.Fatal("root protection must run only after leadership is acquired")
	}
}

const concurrentFinalizer = "example.com/concurrent"

type conflictOnceRootClient struct {
	client.Client
	patchAttempts int
}

func (c *conflictOnceRootClient) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patchAttempts++
	data, err := patch.Data(object)
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(`"resourceVersion"`)) {
		return fmt.Errorf("root-protection patch has no resourceVersion precondition: %s", data)
	}
	if c.patchAttempts == 1 {
		fresh := object.DeepCopyObject().(client.Object)
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
			return err
		}
		controllerutil.AddFinalizer(fresh, concurrentFinalizer)
		if err := c.Client.Update(ctx, fresh); err != nil {
			return err
		}
		return apierrors.NewConflict(
			schema.GroupResource{Group: appsv1.GroupVersion.Group, Resource: "chainnodes"},
			object.GetName(),
			errors.New("concurrent finalizer update"),
		)
	}
	return c.Client.Patch(ctx, object, patch, opts...)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
