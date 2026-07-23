package chainnode

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/datasnapshot"
)

func TestGetTarballExportProvider(t *testing.T) {
	reconciler := &Reconciler{opts: &controllers.ControllerRunOptions{}}
	tests := []struct {
		name       string
		export     *appsv1.ExportTarballConfig
		assertType func(t *testing.T, provider datasnapshot.SnapshotProvider)
	}{
		{
			name: "GCS",
			export: &appsv1.ExportTarballConfig{
				GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"},
			},
			assertType: func(t *testing.T, provider datasnapshot.SnapshotProvider) {
				_, ok := provider.(*datasnapshot.GCS)
				assert.True(t, ok)
			},
		},
		{
			name: "S3",
			export: &appsv1.ExportTarballConfig{
				S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"},
			},
			assertType: func(t *testing.T, provider datasnapshot.SnapshotProvider) {
				_, ok := provider.(*datasnapshot.S3)
				assert.True(t, ok)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{Spec: appsv1.ChainNodeSpec{
				Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
					Frequency:     "24h",
					ExportTarball: tt.export,
				}},
			}}
			provider, err := reconciler.getTarballExportProvider(node)
			require.NoError(t, err)
			tt.assertType(t, provider)
		})
	}
}

func TestRecordTarballExportFailureStopsAtRetryLimit(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snapshot",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationExportingTarball: "true",
			},
		},
	}
	client := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).Build()
	reconciler := &Reconciler{Client: client}

	for attempt := 1; attempt <= tarballExportMaxAttempts; attempt++ {
		retry, err := reconciler.recordTarballExportFailure(context.Background(), snapshot)
		require.NoError(t, err)
		assert.Equal(t, attempt < tarballExportMaxAttempts, retry)

		stored := &snapshotv1.VolumeSnapshot{}
		require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: "snapshot", Namespace: "default"}, stored))
		assert.Equal(t, strconv.Itoa(attempt), stored.Annotations[controllers.AnnotationTarballExportAttempts])
		if attempt < tarballExportMaxAttempts {
			_, exporting := stored.Annotations[controllers.AnnotationExportingTarball]
			assert.False(t, exporting)
			stored.Annotations[controllers.AnnotationExportingTarball] = "true"
			require.NoError(t, client.Update(context.Background(), stored))
		} else {
			assert.Equal(t, tarballFailed, stored.Annotations[controllers.AnnotationExportingTarball])
		}
		snapshot = stored
	}
}

func TestFinishTarballExportPersistsBeforeCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		TypeMeta:   metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency:     "24h",
			ExportTarball: &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
		}}},
	}
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Name: "snapshot", Namespace: "default",
		Annotations: map[string]string{
			controllers.AnnotationExportingTarball:      "true",
			controllers.AnnotationTarballExportAttempts: "2",
		},
	}}
	baseClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).Build()
	clientSet := fake.NewSimpleClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "-00010101000000-upload", Namespace: "default"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "-00010101000000-upload", Namespace: "default"}},
	)
	reconciler := &Reconciler{
		Client:            &updateFailingClient{Client: baseClient},
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	err := reconciler.finishTarballExport(context.Background(), chainNode, snapshot)
	require.ErrorContains(t, err, "update failed")
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, baseClient.Get(context.Background(), types.NamespacedName{Name: "snapshot", Namespace: "default"}, stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationExportingTarball])

	_, err = clientSet.BatchV1().Jobs("default").Get(context.Background(), "-00010101000000-upload", metav1.GetOptions{})
	require.NoError(t, err)
	_, err = clientSet.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "-00010101000000-upload", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestFinishTarballExportCleansAfterSuccessfulUpdate(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		TypeMeta:   metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency:     "24h",
			ExportTarball: &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
		}}},
	}
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Name: "snapshot", Namespace: "default",
		Annotations: map[string]string{
			controllers.AnnotationExportingTarball:      "true",
			controllers.AnnotationTarballExportAttempts: "2",
		},
	}}
	controllerClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).Build()
	clientSet := fake.NewSimpleClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "-00010101000000-upload", Namespace: "default"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "-00010101000000-upload", Namespace: "default"}},
	)
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
	}

	require.NoError(t, reconciler.finishTarballExport(context.Background(), chainNode, snapshot))

	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), types.NamespacedName{Name: "snapshot", Namespace: "default"}, stored))
	assert.Equal(t, tarballFinished, stored.Annotations[controllers.AnnotationExportingTarball])
	_, hasAttempts := stored.Annotations[controllers.AnnotationTarballExportAttempts]
	assert.False(t, hasAttempts)
	_, err := clientSet.BatchV1().Jobs("default").Get(context.Background(), "-00010101000000-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = clientSet.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "-00010101000000-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

type updateFailingClient struct {
	client.Client
}

func (c *updateFailingClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errors.New("update failed")
}

func TestIsSnapshotReady(t *testing.T) {
	tests := []struct {
		name     string
		snapshot *snapshotv1.VolumeSnapshot
		want     bool
	}{
		{
			name:     "nil snapshot",
			snapshot: nil,
			want:     false,
		},
		{
			name: "snapshot with nil status",
			snapshot: &snapshotv1.VolumeSnapshot{
				Status: nil,
			},
			want: false,
		},
		{
			name: "snapshot with nil ReadyToUse",
			snapshot: &snapshotv1.VolumeSnapshot{
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: nil,
				},
			},
			want: false,
		},
		{
			name: "snapshot not ready",
			snapshot: &snapshotv1.VolumeSnapshot{
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: ptr.To(false),
				},
			},
			want: false,
		},
		{
			name: "snapshot ready",
			snapshot: &snapshotv1.VolumeSnapshot{
				Status: &snapshotv1.VolumeSnapshotStatus{
					ReadyToUse: ptr.To(true),
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSnapshotReady(tt.snapshot)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsSnapshotExpired(t *testing.T) {
	now := time.Now()
	pastTime := now.Add(-25 * time.Hour) // More than 24h ago
	recentTime := now.Add(-1 * time.Hour)

	tests := []struct {
		name      string
		snapshot  *snapshotv1.VolumeSnapshot
		want      bool
		wantError bool
	}{
		{
			name: "no retention annotation",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			want:      false,
			wantError: false,
		},
		{
			name: "invalid retention format",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						controllers.AnnotationSnapshotRetention: "invalid",
					},
				},
			},
			want:      false,
			wantError: true,
		},
		{
			name: "snapshot expired",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Time{Time: pastTime},
					Annotations: map[string]string{
						controllers.AnnotationSnapshotRetention: "24h",
					},
				},
			},
			want:      true,
			wantError: false,
		},
		{
			name: "snapshot not expired",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Time{Time: recentTime},
					Annotations: map[string]string{
						controllers.AnnotationSnapshotRetention: "24h",
					},
				},
			},
			want:      false,
			wantError: false,
		},
		{
			name: "long retention period",
			snapshot: &snapshotv1.VolumeSnapshot{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.Time{Time: recentTime},
					Annotations: map[string]string{
						controllers.AnnotationSnapshotRetention: "720h", // 30 days
					},
				},
			},
			want:      false,
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isSnapshotExpired(tt.snapshot)
			if tt.wantError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestVolumeSnapshotInProgress(t *testing.T) {
	tests := []struct {
		name      string
		chainNode *appsv1.ChainNode
		want      bool
	}{
		{
			name: "no annotations",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: nil,
				},
			},
			want: false,
		},
		{
			name: "empty annotations",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			want: false,
		},
		{
			name: "snapshot in progress",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						controllers.AnnotationPvcSnapshotInProgress: "true",
					},
				},
			},
			want: true,
		},
		{
			name: "snapshot not in progress",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						controllers.AnnotationPvcSnapshotInProgress: "false",
					},
				},
			},
			want: false,
		},
		{
			name: "invalid annotation value",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						controllers.AnnotationPvcSnapshotInProgress: "not-a-bool",
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := volumeSnapshotInProgress(tt.chainNode)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSetSnapshotInProgress(t *testing.T) {
	tests := []struct {
		name         string
		chainNode    *appsv1.ChainNode
		snapshotting bool
		wantPhase    appsv1.ChainNodePhase
		wantValue    string
	}{
		{
			name: "start snapshotting",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			snapshotting: true,
			wantPhase:    appsv1.PhaseChainNodeSnapshotting,
			wantValue:    "true",
		},
		{
			name: "stop snapshotting",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			snapshotting: false,
			wantPhase:    appsv1.PhaseChainNodeRunning,
			wantValue:    "false",
		},
		{
			name: "start with nil annotations",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: nil,
				},
			},
			snapshotting: true,
			wantPhase:    appsv1.PhaseChainNodeSnapshotting,
			wantValue:    "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSnapshotInProgress(tt.chainNode, tt.snapshotting)

			assert.Equal(t, tt.wantPhase, tt.chainNode.Status.Phase)
			assert.Equal(t, tt.wantValue, tt.chainNode.Annotations[controllers.AnnotationPvcSnapshotInProgress])
		})
	}
}

func TestGetLastSnapshotTime(t *testing.T) {
	now := time.Now().UTC()
	timeStr := now.Format(timeLayout)

	tests := []struct {
		name      string
		chainNode *appsv1.ChainNode
		want      time.Time
	}{
		{
			name: "has snapshot time",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						controllers.AnnotationLastPvcSnapshot: timeStr,
					},
				},
			},
			want: now,
		},
		{
			name: "no snapshot time annotation",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			want: time.Time{},
		},
		{
			name: "nil annotations",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: nil,
				},
			},
			want: time.Time{},
		},
		{
			name: "invalid time format",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						controllers.AnnotationLastPvcSnapshot: "invalid",
					},
				},
			},
			want: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLastSnapshotTime(tt.chainNode)
			// Compare with a small tolerance for rounding
			if !got.Equal(tt.want) {
				assert.WithinDuration(t, tt.want, got, time.Second)
			}
		})
	}
}

func TestSetSnapshotTime(t *testing.T) {
	now := time.Now().UTC()
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: nil,
		},
	}

	setSnapshotTime(chainNode, now)

	// Verify annotation was set
	assert.NotNil(t, chainNode.Annotations)

	timeStr, ok := chainNode.Annotations[controllers.AnnotationLastPvcSnapshot]
	assert.True(t, ok)

	// Verify time can be parsed back
	parsed, err := time.Parse(timeLayout, timeStr)
	assert.NoError(t, err)

	// timeLayout doesn't include subseconds, so truncate to seconds for comparison
	expectedTime := now.Truncate(time.Second)
	assert.True(t, parsed.Equal(expectedTime))
}

func TestGetRetainCount(t *testing.T) {
	tests := []struct {
		name   string
		config *appsv1.VolumeSnapshotsConfig
		want   *int32
	}{
		{
			name:   "nil config",
			config: nil,
			want:   nil,
		},
		{
			name:   "nil retain",
			config: &appsv1.VolumeSnapshotsConfig{},
			want:   nil,
		},
		{
			name: "retain set to 3",
			config: &appsv1.VolumeSnapshotsConfig{
				Retain: ptr.To[int32](3),
			},
			want: ptr.To[int32](3),
		},
		{
			name: "retain set to 1",
			config: &appsv1.VolumeSnapshotsConfig{
				Retain: ptr.To[int32](1),
			},
			want: ptr.To[int32](1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.GetRetainCount()
			if tt.want == nil {
				assert.Nil(t, got)
			} else {
				assert.NotNil(t, got)
				assert.Equal(t, *tt.want, *got)
			}
		})
	}
}

func TestValidateSnapshotsConfigMutualExclusion(t *testing.T) {
	tests := []struct {
		name      string
		chainNode *appsv1.ChainNode
		wantErr   bool
	}{
		{
			name: "retention only",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: appsv1.ChainNodeSpec{
					Genesis: &appsv1.GenesisConfig{
						Url: ptr.To("https://example.com/genesis.json"),
					},
					App: appsv1.AppSpec{
						Image: "test-image",
					},
					Persistence: &appsv1.Persistence{
						Snapshots: &appsv1.VolumeSnapshotsConfig{
							Frequency: "24h",
							Retention: ptr.To("72h"),
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "retain only",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: appsv1.ChainNodeSpec{
					Genesis: &appsv1.GenesisConfig{
						Url: ptr.To("https://example.com/genesis.json"),
					},
					App: appsv1.AppSpec{
						Image: "test-image",
					},
					Persistence: &appsv1.Persistence{
						Snapshots: &appsv1.VolumeSnapshotsConfig{
							Frequency: "24h",
							Retain:    ptr.To[int32](5),
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "both retention and retain",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: appsv1.ChainNodeSpec{
					Genesis: &appsv1.GenesisConfig{
						Url: ptr.To("https://example.com/genesis.json"),
					},
					App: appsv1.AppSpec{
						Image: "test-image",
					},
					Persistence: &appsv1.Persistence{
						Snapshots: &appsv1.VolumeSnapshotsConfig{
							Frequency: "24h",
							Retention: ptr.To("72h"),
							Retain:    ptr.To[int32](5),
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "neither retention nor retain",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Spec: appsv1.ChainNodeSpec{
					Genesis: &appsv1.GenesisConfig{
						Url: ptr.To("https://example.com/genesis.json"),
					},
					App: appsv1.AppSpec{
						Image: "test-image",
					},
					Persistence: &appsv1.Persistence{
						Snapshots: &appsv1.VolumeSnapshotsConfig{
							Frequency: "24h",
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.chainNode.Validate(nil)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "mutually exclusive")
			} else {
				// Note: Validate may return error for other reasons (e.g., missing genesis)
				// We just check it doesn't contain our specific error
				if err != nil {
					assert.NotContains(t, err.Error(), "mutually exclusive")
				}
			}
		})
	}
}
