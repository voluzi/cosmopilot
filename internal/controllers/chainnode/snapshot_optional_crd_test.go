package chainnode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
)

func TestChainNodeManagerWithoutSnapshotCRDs(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires verified local envtest assets")
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "helm", "cosmopilot", "crds")}, ErrorIfCRDPathMissing: true}
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	cfg, err := environment.Start()
	require.NoError(t, err)
	testScheme := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(testScheme))
	require.NoError(t, appsv1.AddToScheme(testScheme))
	require.NoError(t, snapshotv1.AddToScheme(testScheme))
	admin, err := client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)
	_, err = admin.RESTMapper().RESTMapping(snapshotv1.SchemeGroupVersion.WithKind("VolumeSnapshot").GroupKind(), "v1")
	require.True(t, apiMeta.IsNoMatchError(err), "this environment must not install snapshot CRDs")
	ctx := t.Context()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "optional-snapshot-"}}
	require.NoError(t, admin.Create(ctx, ns))
	local := &Reconciler{Client: admin, APIReader: admin, Scheme: testScheme, opts: &controllers.ControllerRunOptions{}}
	t.Run("absent owner still cleans Jobs and PVCs", func(t *testing.T) {
		id := string(uuid.NewUUID())
		a := &appsv1.SnapshotExportAttempt{ID: id, Sequence: 1}
		recipe := exportprotocol.Recipe{Namespace: ns.Name, Owner: appsv1.ExportResourceIdentity{Name: "deleted-owner", UID: "deleted-uid"}, ResourceName: "export-" + id, Image: "exporter:test", RestoreSize: "1Gi", StorageClass: "optional", Snapshot: appsv1.ExportResourceIdentity{Name: "gone"}, Credentials: appsv1.ExportResourceIdentity{Name: "credentials"}, ServiceAccount: appsv1.ExportResourceIdentity{Name: "export-owner"}}
		job := datasnapshot.NativeJob(a, recipe)
		require.NoError(t, admin.Create(ctx, job))
		pvc := datasnapshot.NativePVC(a, recipe, job.UID)
		require.NoError(t, admin.Create(ctx, pvc))
		controllerutil.RemoveFinalizer(pvc, "kubernetes.io/pvc-protection")
		require.NoError(t, admin.Update(ctx, pvc))
		_, err := local.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns.Name, Name: "deleted-owner"}})
		require.NoError(t, err, "missing snapshot kind must not block orphan recovery")
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})))
	})
	t.Run("manager creates and deletes an ordinary node", func(t *testing.T) {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: testScheme, Metrics: metricsserver.Options{BindAddress: "0"}, Controller: controllerconfig.Controller{SkipNameValidation: ptr.To(true)}})
		require.NoError(t, err)
		clients, err := kubernetes.NewForConfig(cfg)
		require.NoError(t, err)
		managed, err := New(mgr, clients, &controllers.ControllerRunOptions{WorkerCount: 1, DisableWebhooks: true})
		require.NoError(t, err)
		t.Cleanup(func() { managed.configCache.Stop(); managed.nodeClients.Stop() })
		managerCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(15 * time.Second):
				t.Error("manager did not join")
			}
		})
		go func() { done <- mgr.Start(managerCtx) }()
		node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "ordinary-", Namespace: ns.Name}, Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{Image: "application:test", App: "app"}, Genesis: &appsv1.GenesisConfig{ConfigMap: ptr.To("genesis")}, Persistence: &appsv1.Persistence{Restore: &appsv1.SnapshotRestoreConfig{Snapshot: appsv1.SnapshotRestoreSource{Provider: "s3", Bucket: "backups", Name: "fixture.tar.gz"}}}}}
		require.NoError(t, admin.Create(ctx, node))
		initKey := client.ObjectKey{Namespace: node.Namespace, Name: node.Name + "-init-data"}
		require.Eventually(t, func() bool { return admin.Get(ctx, initKey, &corev1.Pod{}) == nil }, 15*time.Second, 100*time.Millisecond)
		require.NoError(t, admin.Delete(ctx, node))
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(node), &appsv1.ChainNode{}))
		}, 15*time.Second, 100*time.Millisecond)
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, initKey, &corev1.Pod{})))
		_, err = local.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
		require.NoError(t, err)
		id := string(uuid.NewUUID())
		a := &appsv1.SnapshotExportAttempt{ID: id, Sequence: 1}
		recipe := exportprotocol.Recipe{Namespace: ns.Name, Owner: appsv1.ExportResourceIdentity{Name: "watched-" + id, UID: "old-uid"}, ResourceName: "export-" + id, Image: "exporter:test", Credentials: appsv1.ExportResourceIdentity{Name: "credentials"}, ServiceAccount: appsv1.ExportResourceIdentity{Name: "export-owner"}}
		job := datasnapshot.NativeJob(a, recipe)
		require.NoError(t, admin.Create(ctx, job))
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{}))
		}, 10*time.Second, 100*time.Millisecond, "attributed Job events must reach the real manager")
	})
}
