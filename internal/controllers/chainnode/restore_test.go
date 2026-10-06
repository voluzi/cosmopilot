package chainnode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

func restoreNode() *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-node", Namespace: "default", UID: "node-uid"},
		Spec: appsv1.ChainNodeSpec{
			App: appsv1.AppSpec{App: "appd", Image: "app", Version: ptr.To("v1")},
			Persistence: &appsv1.Persistence{InitTimeout: ptr.To("2h"), Restore: &appsv1.SnapshotRestoreConfig{
				Snapshot:     appsv1.SnapshotRestoreSource{Provider: "s3", Bucket: "backups", Name: "prefix/snapshot.tar.zst", Region: "us-east-1", Endpoint: "https://example.com", ForcePathStyle: true, CredentialsSecret: &appsv1.SnapshotExportSecretReference{Name: "reader"}, ServiceAccountName: "snapshot-reader"},
				Verification: &appsv1.SnapshotRestoreVerification{SHA256: strings.Repeat("a", 64)},
			}},
		},
	}
}

func restoreReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(objects...).Build()
	return &Reconciler{Client: c, APIReader: c, Scheme: scheme, recorder: record.NewFakeRecorder(30), opts: &controllers.ControllerRunOptions{DataExporterImage: "dataexporter:test"}}
}

func TestRestoreInitPodUsesConfiguredSourceForAllNodes(t *testing.T) {
	for _, mode := range []string{"full node", "local validator", "signer target"} {
		t.Run(mode, func(t *testing.T) {
			node := restoreNode()
			if mode == "local validator" {
				node.Spec.Validator = &appsv1.ValidatorConfig{}
			}
			if mode == "signer target" {
				node.Spec.RemoteSignerTarget = true
			}
			node.Spec.Persistence.AdditionalInitCommands = []appsv1.InitCommand{{Command: []string{"echo"}, Args: []string{"after restore"}}}
			r := restoreReconciler(t, node)
			app, err := r.newApp(node)
			require.NoError(t, err)
			pod, err := r.buildDataInitPod(app, node, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name}})
			require.NoError(t, err)
			require.Equal(t, int64(7200), *pod.Spec.ActiveDeadlineSeconds)
			require.Equal(t, "snapshot-reader", pod.Spec.ServiceAccountName)
			require.Len(t, pod.Spec.InitContainers, 2)
			restore := pod.Spec.InitContainers[0]
			require.Equal(t, "dataexporter:test", restore.Image)
			require.Equal(t, []string{"s3", "restore", "--sha256", strings.Repeat("a", 64), "--", "/home/app/data", "backups", "prefix/snapshot.tar.zst"}, restore.Args)
			require.Nil(t, restore.Command)
			require.Equal(t, "reader", restore.EnvFrom[0].SecretRef.Name)
			require.Contains(t, restore.Env, corev1.EnvVar{Name: "S3_FORCE_PATH_STYLE", Value: "true"})
			require.Equal(t, []string{"after restore"}, pod.Spec.InitContainers[1].Args)
			require.True(t, *restore.SecurityContext.RunAsNonRoot)
		})
	}
}

func TestRestoreInitPodGCSCredentialsAndOptionalVerification(t *testing.T) {
	node := restoreNode()
	restore := node.Spec.Persistence.Restore
	restore.Snapshot.Provider = "gcs"
	restore.Snapshot.CredentialsSecret.Key = "reader.json"
	restore.Verification = nil
	r := restoreReconciler(t, node)
	app, err := r.newApp(node)
	require.NoError(t, err)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}
	pod, err := r.buildDataInitPod(app, node, pvc)
	require.NoError(t, err)
	require.NotContains(t, pod.Spec.InitContainers[0].Args, "--sha256")
	require.Contains(t, pod.Spec.InitContainers[0].Env, corev1.EnvVar{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: "/creds/credentials.json"})
	require.Equal(t, []corev1.KeyToPath{{Key: "reader.json", Path: "credentials.json"}}, pod.Spec.Volumes[len(pod.Spec.Volumes)-1].Secret.Items)
	restore.Snapshot.CredentialsSecret = nil
	pod, err = r.buildDataInitPod(app, node, pvc)
	require.NoError(t, err)
	require.Empty(t, pod.Spec.InitContainers[0].Env)
	require.Equal(t, "snapshot-reader", pod.Spec.ServiceAccountName)
}

func TestRestoreUsesInitializedMarkerAndRecreatesDeletedVolume(t *testing.T) {
	node := restoreNode()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Annotations: map[string]string{controllers.AnnotationDataInitialized: "true"}}}
	r := restoreReconciler(t, node, pvc)
	_, result, err := r.ensureDataVolume(t.Context(), nil, node)
	require.NoError(t, err)
	require.Zero(t, result)
	initPod := &corev1.Pod{}
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKey{Name: node.Name + "-init-data", Namespace: node.Namespace}, initPod)))
	node.Spec.Persistence.Restore.Snapshot.Name = "new-backup.tar"
	require.NoError(t, r.Update(t.Context(), node))
	require.NoError(t, r.Delete(t.Context(), pvc))
	app, err := r.newApp(node)
	require.NoError(t, err)
	_, result, err = r.ensureDataVolume(t.Context(), app, node)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: node.Name + "-init-data", Namespace: node.Namespace}, initPod))
	require.Contains(t, initPod.Spec.InitContainers[0].Args, "new-backup.tar")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
	require.Equal(t, "false", pvc.Annotations[controllers.AnnotationDataInitialized])
}

func TestRestoreCompletionPersistsMarkerBeforeCleanup(t *testing.T) {
	node := restoreNode()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Annotations: map[string]string{controllers.AnnotationDataInitialized: "false"}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	r := restoreReconciler(t, node, pvc, pod)
	original := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(original, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			return errors.New("PVC update unavailable")
		}
		return c.Update(ctx, obj, opts...)
	}})
	_, err := r.initializeData(t.Context(), nil, node, pvc)
	require.Error(t, err)
	require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
	current := &corev1.PersistentVolumeClaim{}
	require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pvc), current))
	require.Equal(t, "false", current.Annotations[controllers.AnnotationDataInitialized])
	r.Client = interceptor.NewClient(original, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		return errors.New("cleanup unavailable")
	}})
	_, err = r.initializeData(t.Context(), nil, node, pvc)
	require.Error(t, err)
	require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pvc), current))
	require.Equal(t, "true", current.Annotations[controllers.AnnotationDataInitialized])
	r.Client = original
	_, result, err := r.ensureDataVolume(t.Context(), nil, node)
	require.NoError(t, err)
	require.Zero(t, result)
	require.True(t, apierrors.IsNotFound(original.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
}

func TestRestoreFailureEventIncludesStage(t *testing.T) {
	node := restoreNode()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace}, Status: corev1.PodStatus{
		Phase:                 corev1.PodFailed,
		InitContainerStatuses: []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: `{"stage":"verification","message":"SHA-256 mismatch"}`}}}},
	}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Annotations: map[string]string{controllers.AnnotationDataInitialized: "false"}}}
	r := restoreReconciler(t, node, pvc, pod)
	_, err := r.initializeData(t.Context(), nil, node, pvc)
	require.NoError(t, err)
	event := <-r.recorder.(*record.FakeRecorder).Events
	require.Contains(t, event, "DataInitFailed")
	require.Contains(t, event, "verification: SHA-256 mismatch")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
	require.Equal(t, "false", pvc.Annotations[controllers.AnnotationDataInitialized])
}

func TestDataInitializationDiscardsStaleHelperBeforeCreatingVolume(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed, corev1.PodPending} {
		t.Run(string(phase), func(t *testing.T) {
			for _, keepRestore := range []bool{true, false} {
				name := "restore configured"
				if !keepRestore {
					name = "restore removed"
				}
				t.Run(name, func(t *testing.T) {
					node := restoreNode()
					if !keepRestore {
						node.Spec.Persistence.Restore = nil
					}
					oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace}, Status: corev1.PodStatus{Phase: phase}}
					if phase != corev1.PodPending {
						oldPod.Spec.NodeName = "worker"
					}
					r := restoreReconciler(t, node, oldPod)
					pvc, result, err := r.ensureDataVolume(t.Context(), nil, node)
					require.NoError(t, err)
					require.Nil(t, pvc)
					require.Positive(t, result.RequeueAfter)
					require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(oldPod), &corev1.Pod{})))
					app, err := r.newApp(node)
					require.NoError(t, err)
					pvc, result, err = r.ensureDataVolume(t.Context(), app, node)
					require.NoError(t, err)
					require.Equal(t, "false", pvc.Annotations[controllers.AnnotationDataInitialized])
					require.Positive(t, result.RequeueAfter)
					current := &corev1.Pod{}
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(oldPod), current))
					expected := "app"
					if keepRestore {
						expected = "data-restore"
					}
					require.Equal(t, expected, current.Spec.InitContainers[0].Name)
					require.NotEqual(t, corev1.PodSucceeded, current.Status.Phase)
				})
			}
		})
	}
}

func TestDataInitializationPreservesActiveHelperOnMissingCachedVolume(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning} {
		t.Run(string(phase), func(t *testing.T) {
			node := restoreNode()
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace}, Spec: corev1.PodSpec{NodeName: "worker"}, Status: corev1.PodStatus{Phase: phase}}
			r := restoreReconciler(t, node, pvc, pod)
			original := r.Client.(client.WithWatch)
			r.Client = interceptor.NewClient(original, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return apierrors.NewNotFound(corev1.Resource("persistentvolumeclaims"), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			_, result, err := r.ensureDataVolume(t.Context(), nil, node)
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter)
			current := &corev1.Pod{}
			require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pod), current))
			require.Equal(t, phase, current.Status.Phase)
			require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}))
		})
	}
}

func TestRestoreReplacementVolumeRebasesApplicationAtArchiveHeight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		height *int64
		image  string
		phases []appsv1.UpgradePhase
	}{
		{"unset", nil, "app:v3", []appsv1.UpgradePhase{appsv1.UpgradeCompleted, appsv1.UpgradeCompleted}},
		{"zero", ptr.To(int64(0)), "app:v1", []appsv1.UpgradePhase{appsv1.UpgradeScheduled, appsv1.UpgradeScheduled}},
		{"between upgrades", ptr.To(int64(150)), "app:v2", []appsv1.UpgradePhase{appsv1.UpgradeCompleted, appsv1.UpgradeScheduled}},
		{"after upgrades", ptr.To(int64(250)), "app:v3", []appsv1.UpgradePhase{appsv1.UpgradeCompleted, appsv1.UpgradeCompleted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := restoreNode()
			node.Spec.Persistence.Restore.Height = tc.height
			node.Spec.Persistence.AdditionalInitCommands = []appsv1.InitCommand{{Command: []string{"echo"}, Args: []string{"initialized"}}}
			node.Status.LatestHeight = 300
			node.Status.AppImage = "app:v3"
			node.Status.AppVersion = "v3"
			node.Status.Upgrades = []appsv1.Upgrade{
				{Height: 100, Image: "app:v2", Status: appsv1.UpgradeCompleted},
				{Height: 200, Image: "app:v3", Status: appsv1.UpgradeCompleted},
			}
			r := restoreReconciler(t, node)
			app, err := r.newApp(node)
			require.NoError(t, err)
			pvc, result, err := r.ensureDataVolume(t.Context(), app, node)
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter)
			current := &appsv1.ChainNode{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(node), current))
			height := int64(300)
			if tc.height != nil {
				height = *tc.height
			}
			require.Equal(t, height, current.Status.LatestHeight)
			require.Equal(t, tc.image, current.GetAppImage())
			for i, phase := range tc.phases {
				require.Equal(t, phase, current.Status.Upgrades[i].Status)
			}
			require.Equal(t, "false", pvc.Annotations[controllers.AnnotationDataInitialized])
			pod := &corev1.Pod{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: node.Name + "-init-data", Namespace: node.Namespace}, pod))
			commandImage := tc.image
			if tc.height == nil {
				commandImage = "app:v1"
			}
			require.Equal(t, commandImage, pod.Spec.InitContainers[1].Image)
		})
	}
	for _, managed := range []bool{false, true} {
		name := "new standalone node"
		if managed {
			name = "new ChainNodeSet child"
		}
		t.Run(name, func(t *testing.T) {
			for _, height := range []int64{200, 250} {
				t.Run(fmt.Sprint(height), func(t *testing.T) {
					node := restoreNode()
					node.Spec.Persistence.Restore.Height = ptr.To(height)
					node.Spec.App.Upgrades = []appsv1.UpgradeSpec{
						{Height: 100, Image: "app:v2"},
						{Height: 200, Image: "app:v3"},
						{Height: 300, Image: "app:v4"},
					}
					if managed {
						node.OwnerReferences = []metav1.OwnerReference{{
							APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet",
							Name: "set", UID: "set-uid", Controller: ptr.To(true),
						}}
						set := &appsv1.ChainNodeSet{Spec: appsv1.ChainNodeSetSpec{App: node.Spec.App}}
						set.Spec.App.Upgrades = nil
						set.Status.Upgrades = []appsv1.Upgrade{
							{Height: 100, Image: "app:v2", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeCompleted},
							{Height: 200, Image: "app:v3", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeCompleted},
							{Height: 300, Image: "app:v4", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled},
						}
						node.Spec.App = set.GetAppSpecWithUpgrades()
					}
					r := restoreReconciler(t, node)
					app, err := r.newApp(node)
					require.NoError(t, err)
					_, result, err := r.ensureDataVolume(t.Context(), app, node)
					require.NoError(t, err)
					require.Positive(t, result.RequeueAfter)
					require.Empty(t, node.Status.Upgrades)
					require.Equal(t, height, node.Status.LatestHeight)
					require.Equal(t, appsv1.PhaseChainNodeInitData, node.Status.Phase)
					require.NoError(t, r.ensureUpgrades(t.Context(), node, false))
					require.Len(t, node.Status.Upgrades, 3)
					require.Equal(t, appsv1.UpgradeSkipped, node.Status.Upgrades[0].Status)
					require.Equal(t, appsv1.UpgradeSkipped, node.Status.Upgrades[1].Status)
					require.Equal(t, appsv1.UpgradeScheduled, node.Status.Upgrades[2].Status)
					require.Equal(t, "app:v3", r.buildAppContainer(node, nil, "/ready", corev1.ResourceRequirements{}, nil).Image)
				})
			}
		})
	}

}

func TestRestoreReportedHeightSelectsNodeImage(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, replacement := range []bool{false, true} {
			for _, tc := range []struct {
				name, message string
				explicit      *int64
				height        int64
				image         string
				removeRestore bool
			}{
				{"metadata", `{"stage":"complete","message":"","height":"150"}`, nil, 150, "app:v2", false},
				{"metadata zero", `{"stage":"complete","message":"","height":"0"}`, nil, 0, "app:v1", false},
				{"metadata overridden", `{"stage":"complete","message":"","height":"250"}`, ptr.To(int64(150)), 150, "app:v2", false},
				{"explicit zero", `{"stage":"complete","message":"","height":"250"}`, ptr.To(int64(0)), 0, "app:v1", false},
				{"invalid metadata overridden", `not JSON`, ptr.To(int64(150)), 150, "app:v2", false},
				{"new image old object", `{"stage":"complete","message":""}`, nil, 0, "app:v1", false},
				{"old image", "", nil, 0, "app:v1", false},
				{"new image old object explicit", `{"stage":"complete","message":""}`, ptr.To(int64(150)), 150, "app:v2", false},
				{"old image explicit", "", ptr.To(int64(150)), 150, "app:v2", false},
				{"restore removed while running", `{"stage":"complete","message":"","height":"150"}`, nil, 150, "app:v2", true},
			} {
				t.Run(fmt.Sprintf("managed=%t/replacement=%t/%s", managed, replacement, tc.name), func(t *testing.T) {
					node := restoreNode()
					node.Spec.Persistence.Restore.Height = tc.explicit
					node.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 100, Image: "app:v2"}, {Height: 200, Image: "app:v3"}}
					if managed {
						set := &appsv1.ChainNodeSet{Spec: appsv1.ChainNodeSetSpec{App: node.Spec.App}}
						set.Spec.App.Upgrades = nil
						set.Status.Upgrades = []appsv1.Upgrade{{Height: 100, Image: "app:v2", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeCompleted}, {Height: 200, Image: "app:v3", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled}}
						node.Spec.App = set.GetAppSpecWithUpgrades()
						node.OwnerReferences = []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "set", UID: "set-uid", Controller: ptr.To(true)}}
					}
					if replacement {
						node.Status.LatestHeight = 300
						node.Status.AppImage = "app:v3"
						node.Status.AppVersion = "v3"
						node.Status.Upgrades = []appsv1.Upgrade{{Height: 50, Image: "app:historical", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeCompleted}, {Height: 75, Image: "app:skipped", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeSkipped}, {Height: 200, Image: "app:v3", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeCompleted}}
					}
					r := restoreReconciler(t, node)
					app, err := r.newApp(node)
					require.NoError(t, err)
					pvc, _, err := r.ensureDataVolume(t.Context(), app, node)
					require.NoError(t, err)
					if replacement && tc.explicit == nil {
						require.Equal(t, int64(300), node.Status.LatestHeight)
						require.Equal(t, appsv1.UpgradeCompleted, node.Status.Upgrades[0].Status)
						require.Equal(t, appsv1.UpgradeSkipped, node.Status.Upgrades[1].Status)
						require.Equal(t, "0", pvc.Annotations[controllers.AnnotationDataHeight])
					}
					pod := &corev1.Pod{}
					require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: node.Namespace, Name: node.Name + "-init-data"}, pod))
					if tc.removeRestore {
						pod.Status.Phase = corev1.PodRunning
						require.NoError(t, r.Status().Update(t.Context(), pod))
						node.Spec.Persistence.Restore = nil
						require.NoError(t, r.Update(t.Context(), node))
						_, err = r.initializeData(t.Context(), nil, node, pvc)
						require.NoError(t, err)
					}
					pod.Status.Phase = corev1.PodSucceeded
					pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: tc.message}}}}
					require.NoError(t, r.Status().Update(t.Context(), pod))
					_, err = r.initializeData(t.Context(), nil, node, pvc)
					require.NoError(t, err)
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(node), node))
					require.Equal(t, tc.height, node.Status.LatestHeight)
					require.Empty(t, node.Status.AppImage)
					require.Empty(t, node.Status.AppVersion)
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
					require.Equal(t, fmt.Sprint(tc.height), pvc.Annotations[controllers.AnnotationDataHeight])
					require.Equal(t, "true", pvc.Annotations[controllers.AnnotationDataInitialized])
					require.Equal(t, tc.image, node.GetAppImage())
					require.NoError(t, r.ensureUpgrades(t.Context(), node, false))
					require.Equal(t, tc.image, r.buildAppContainer(node, nil, "/ready", corev1.ResourceRequirements{}, nil).Image)
					if replacement && tc.height > 0 {
						require.Equal(t, appsv1.UpgradeCompleted, node.Status.Upgrades[0].Status)
						require.Equal(t, appsv1.UpgradeSkipped, node.Status.Upgrades[1].Status)
					}
					node.Status.LatestHeight = 400
					require.NoError(t, r.Status().Update(t.Context(), node))
					_, _, err = r.ensureDataVolume(t.Context(), nil, node)
					require.NoError(t, err)
					require.Equal(t, int64(400), node.Status.LatestHeight)
				})
			}
		}
	}
}

func TestRestoreRemovalRebasesReplacementVolume(t *testing.T) {
	for _, tc := range []struct {
		name               string
		removeWhileRunning bool
		restoreSucceeds    bool
		explicit           *int64
	}{
		{name: "after failed restore"},
		{name: "while running before failure", removeWhileRunning: true},
		{name: "while running before success", removeWhileRunning: true, restoreSucceeds: true},
		{name: "after failed explicit restore", explicit: ptr.To(int64(150))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := restoreNode()
			if !tc.restoreSucceeds {
				node.Spec.Persistence.Restore.Snapshot.Name = "missing-object.tar"
			}
			node.Spec.Persistence.Restore.Height = tc.explicit
			node.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 100, Image: "app:v2"}, {Height: 200, Image: "app:v3"}}
			node.Status.LatestHeight = 300
			node.Status.AppImage = "app:v3"
			node.Status.AppVersion = "v3"
			node.Status.Upgrades = []appsv1.Upgrade{{Height: 100, Image: "app:v2", Source: appsv1.ManualUpgrade, Status: appsv1.UpgradeCompleted}, {Height: 200, Image: "app:v3", Source: appsv1.ManualUpgrade, Status: appsv1.UpgradeCompleted}}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Annotations: map[string]string{controllers.AnnotationDataInitialized: "true", controllers.AnnotationDataHeight: "300"}}}
			r := restoreReconciler(t, node, pvc)
			require.NoError(t, r.Delete(t.Context(), pvc))
			app, err := r.newApp(node)
			require.NoError(t, err)
			pvc, _, err = r.ensureDataVolume(t.Context(), app, node)
			require.NoError(t, err)
			initialHeight := int64(300)
			if tc.explicit != nil {
				initialHeight = *tc.explicit
			}
			require.Equal(t, initialHeight, node.Status.LatestHeight)
			pod := &corev1.Pod{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: node.Namespace, Name: node.Name + "-init-data"}, pod))
			require.Equal(t, "data-restore", pod.Spec.InitContainers[0].Name)
			if tc.removeWhileRunning {
				pod.Status.Phase = corev1.PodRunning
				require.NoError(t, r.Status().Update(t.Context(), pod))
				node.Spec.Persistence.Restore = nil
				require.NoError(t, r.Update(t.Context(), node))
				_, err = r.initializeData(t.Context(), nil, node, pvc)
				require.NoError(t, err)
				require.Equal(t, int64(300), node.Status.LatestHeight)
			}
			pod.Status.Phase = corev1.PodFailed
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: `{"stage":"download","message":"object not found"}`}}}}
			if tc.restoreSucceeds {
				pod.Status.Phase = corev1.PodSucceeded
				pod.Status.InitContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"stage":"complete","message":"","height":"150"}`}
			}
			require.NoError(t, r.Status().Update(t.Context(), pod))
			_, err = r.initializeData(t.Context(), nil, node, pvc)
			require.NoError(t, err)
			if !tc.restoreSucceeds {
				node.Spec.Persistence.Restore = nil
				require.NoError(t, r.Update(t.Context(), node))
				app, err = r.newApp(node)
				require.NoError(t, err)
				_, _, err = r.ensureDataVolume(t.Context(), app, node)
				require.NoError(t, err)
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
				require.Equal(t, "app", pod.Spec.InitContainers[0].Name)
				pod.Status.Phase = corev1.PodSucceeded
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
				require.NoError(t, r.Status().Update(t.Context(), pod))
				_, err = r.initializeData(t.Context(), nil, node, pvc)
				require.NoError(t, err)
			}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(node), node))
			height, image := int64(0), "app:v1"
			if tc.restoreSucceeds {
				height, image = 150, "app:v2"
			}
			require.Equal(t, height, node.Status.LatestHeight)
			require.Empty(t, node.Status.AppImage)
			require.Empty(t, node.Status.AppVersion)
			for _, upgrade := range node.Status.Upgrades {
				phase := appsv1.UpgradeScheduled
				if tc.restoreSucceeds && upgrade.Height <= height {
					phase = appsv1.UpgradeCompleted
				}
				require.Equal(t, phase, upgrade.Status)
			}
			require.Equal(t, image, r.buildAppContainer(node, nil, "/ready", corev1.ResourceRequirements{}, nil).Image)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
			require.Equal(t, "true", pvc.Annotations[controllers.AnnotationDataInitialized])
			require.Equal(t, fmt.Sprint(height), pvc.Annotations[controllers.AnnotationDataHeight])
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
		})
	}
}

func TestRestoreCompletionResumesAfterPersistenceFailures(t *testing.T) {
	for _, failure := range []string{"height status", "upgrade status", "PVC checkpoint", "Pod deletion"} {
		t.Run(failure, func(t *testing.T) {
			node := restoreNode()
			node.Status.Phase = appsv1.PhaseChainNodeInitData
			node.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 100, Image: "app:v2"}}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Annotations: map[string]string{controllers.AnnotationDataInitialized: "false"}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded, InitContainerStatuses: []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: `{"stage":"complete","message":"","height":"150"}`}}}}}}
			r := restoreReconciler(t, node, pvc, pod)
			original := r.Client.(client.WithWatch)
			statusUpdates := 0
			r.Client = interceptor.NewClient(original, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if _, ok := obj.(*appsv1.ChainNode); ok && sub == "status" {
						statusUpdates++
						if failure == "height status" || failure == "upgrade status" && statusUpdates == 2 {
							return errors.New("status unavailable")
						}
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*corev1.PersistentVolumeClaim); ok && failure == "PVC checkpoint" {
						return errors.New("PVC unavailable")
					}
					return c.Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if failure == "Pod deletion" {
						return errors.New("Pod deletion unavailable")
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			_, err := r.initializeData(t.Context(), nil, node, pvc)
			require.Error(t, err)
			require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
			require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
			if failure == "Pod deletion" {
				require.Equal(t, "true", pvc.Annotations[controllers.AnnotationDataInitialized])
				require.Equal(t, "150", pvc.Annotations[controllers.AnnotationDataHeight])
			} else {
				require.Equal(t, "false", pvc.Annotations[controllers.AnnotationDataInitialized])
			}
			current := &appsv1.ChainNode{}
			require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(node), current))
			fresh := &Reconciler{Client: original, APIReader: original, Scheme: r.Scheme, recorder: record.NewFakeRecorder(30), opts: r.opts}
			_, _, err = fresh.ensureDataVolume(t.Context(), nil, current)
			require.NoError(t, err)
			require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(current), current))
			require.Equal(t, int64(150), current.Status.LatestHeight)
			require.Equal(t, "app:v2", fresh.buildAppContainer(current, nil, "/ready", corev1.ResourceRequirements{}, nil).Image)
			require.NoError(t, original.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
			require.Equal(t, "true", pvc.Annotations[controllers.AnnotationDataInitialized])
			require.Equal(t, "150", pvc.Annotations[controllers.AnnotationDataHeight])
			require.True(t, apierrors.IsNotFound(original.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
		})
	}
}

func TestRestoreMalformedResultRetainsSuccessEvidence(t *testing.T) {
	for _, message := range []string{`not JSON`, `{"stage":"complete","height":"invalid"}`, `{"stage":"extraction","message":"failed"}`} {
		t.Run(message, func(t *testing.T) {
			node := restoreNode()
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Annotations: map[string]string{controllers.AnnotationDataInitialized: "false"}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded, InitContainerStatuses: []corev1.ContainerStatus{{Name: "data-restore", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message}}}}}}
			r := restoreReconciler(t, node, pvc, pod)
			_, err := r.initializeData(t.Context(), nil, node, pvc)
			require.Error(t, err)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
			require.Equal(t, "false", pvc.Annotations[controllers.AnnotationDataInitialized])
		})
	}
}
