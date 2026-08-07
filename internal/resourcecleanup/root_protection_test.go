package resourcecleanup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
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
		Name: "mine", Namespace: "default", Labels: map[string]string{controllers.LabelWorkerName: "worker-a"},
	}}
	other := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "other", Namespace: "default", Labels: map[string]string{controllers.LabelWorkerName: "worker-b"},
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

func TestRootProtectorReleasesControllerGateAfterMigration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&RootProtector{Client: c, Ready: ready}).Start(ctx)
	}()

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("root-protection migration did not release the controller gate")
	}

	fresh := &appsv1.ChainNode{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(node), fresh); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(fresh, Finalizer) {
		t.Fatal("controller gate opened before root migration installed the cleanup finalizer")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRootProtectorRunsDurableMigrationBeforeReleasingControllerGate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	migrationStarted := make(chan struct{})
	allowMigration := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&RootProtector{
			Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
			Ready:  ready,
			Migrate: func(ctx context.Context) error {
				close(migrationStarted)
				select {
				case <-allowMigration:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		}).Start(ctx)
	}()

	<-migrationStarted
	select {
	case <-ready:
		t.Fatal("controller gate opened before legacy durable-resource migration completed")
	default:
	}
	close(allowMigration)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("controller gate did not open after legacy durable-resource migration completed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRootProtectorKeepsControllerGateClosedWhenMigrationFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	wantErr := errors.New("legacy durable migration failed")
	err := (&RootProtector{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Ready:  ready,
		Migrate: func(context.Context) error {
			return wantErr
		},
	}).Start(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want %v", err, wantErr)
	}
	select {
	case <-ready:
		t.Fatal("controller gate opened after legacy durable-resource migration failed")
	default:
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
