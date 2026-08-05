package chainnode

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/datasnapshot"
)

func TestStartSnapshotIntegrityCheckPropagatesAppEnvOnlyToAppContainer(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	restoreSize := resource.MustParse("1Gi")
	snapshot := &snapshotv1.VolumeSnapshot{
		TypeMeta: metav1.TypeMeta{APIVersion: snapshotv1.SchemeGroupVersion.String(), Kind: "VolumeSnapshot"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "snapshot",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{RestoreSize: &restoreSize},
	}
	env := []corev1.EnvVar{
		{Name: "HOME", Value: "/home/app"},
		{
			Name: "FROM_SECRET",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "app-secret"},
				Key:                  "token",
			}},
		},
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{
			App:         appsv1.AppSpec{Image: "example/app", App: "appd"},
			Config:      &appsv1.Config{Env: env},
			Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{}},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain"},
	}
	clientSet := fake.NewSimpleClientset()
	reconciler := &Reconciler{
		Client:            fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).Build(),
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
	}

	require.NoError(t, reconciler.startSnapshotIntegrityCheck(context.Background(), chainNode, snapshot))
	job, err := clientSet.BatchV1().Jobs("default").Get(context.Background(), "snapshot-ichk", metav1.GetOptions{})
	require.NoError(t, err)

	require.Len(t, job.Spec.Template.Spec.InitContainers, 2)
	assert.Empty(t, job.Spec.Template.Spec.InitContainers[0].Env)
	require.Equal(t, env, job.Spec.Template.Spec.InitContainers[1].Env)
	job.Spec.Template.Spec.InitContainers[1].Env[1].ValueFrom.SecretKeyRef.Name = "mutated"
	assert.Equal(t, "app-secret", chainNode.Spec.Config.Env[1].ValueFrom.SecretKeyRef.Name)
	assert.Empty(t, job.Spec.Template.Spec.Containers[0].Env)
}

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

func TestGetTarballExportProviderUsesConfiguredDataExporterImage(t *testing.T) {
	const image = "registry.example.com/dataexporter:custom"
	tests := []struct {
		name   string
		export *appsv1.ExportTarballConfig
	}{
		{
			name: "GCS",
			export: &appsv1.ExportTarballConfig{
				GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"},
			},
		},
		{
			name: "S3",
			export: &appsv1.ExportTarballConfig{
				S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, batchv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			clientSet := fake.NewSimpleClientset()
			reconciler := &Reconciler{
				Scheme:            scheme,
				snapshotClientSet: clientSet,
				opts:              &controllers.ControllerRunOptions{DataExporterImage: image},
			}
			node := &appsv1.ChainNode{
				TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "node",
					Namespace: "default",
					UID:       "node-uid",
				},
				Spec: appsv1.ChainNodeSpec{
					Config: &appsv1.Config{
						ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-creds"}},
					},
					Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
						Frequency:     "24h",
						ExportTarball: tt.export,
					}},
				},
			}
			provider, err := reconciler.getTarballExportProvider(node)
			require.NoError(t, err)

			snapshot := &snapshotv1.VolumeSnapshot{
				TypeMeta:   metav1.TypeMeta{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot"},
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"},
				Status: &snapshotv1.VolumeSnapshotStatus{
					RestoreSize: resource.NewQuantity(1024, resource.BinarySI),
				},
			}
			require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", snapshot))

			job, err := clientSet.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, image, job.Spec.Template.Spec.Containers[0].Image)
			assert.Equal(t, node.Spec.Config.ImagePullSecrets, job.Spec.Template.Spec.ImagePullSecrets)
		})
	}
}

func TestCleanUpTarballDeletionWaitsForTerminatingJob(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	node := &appsv1.ChainNode{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			UID:       "node-uid",
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency: "24h",
			ExportTarball: &appsv1.ExportTarballConfig{
				GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"},
			},
		}}},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Name:              "snapshot",
		Namespace:         "default",
		CreationTimestamp: metav1.NewTime(time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)),
	}}
	deletionTimestamp := metav1.Now()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:              getTarballName(node, snapshot) + "-delete",
		Namespace:         "default",
		UID:               "delete-job-uid",
		DeletionTimestamp: &deletionTimestamp,
		Labels: map[string]string{
			"exporter": "gcs-exporter",
			"owner":    node.Name,
			"type":     "delete",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: node.APIVersion,
			Kind:       node.Kind,
			Name:       node.Name,
			UID:        node.UID,
			Controller: ptr.To(true),
		}},
	}}
	clientSet := fake.NewSimpleClientset(job)
	reconciler := &Reconciler{
		Scheme:            scheme,
		snapshotClientSet: clientSet,
		opts:              &controllers.ControllerRunOptions{},
	}

	err := reconciler.cleanUpTarballDeletion(context.Background(), node, snapshot)
	require.ErrorIs(t, err, datasnapshot.ErrStaleJobTerminating)
	for _, action := range clientSet.Actions() {
		assert.False(t, action.GetVerb() == "delete" && action.GetResource().Resource == "jobs")
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

// TestStaleUploadJobReplacementDoesNotChargeFailedAttempt covers a provider switch on a snapshot that
// has already burned attempts. Deleting the mismatched Job must clear the export markers: otherwise the
// next poll sees the intentionally removed Job as SnapshotNotFound, charges another failure, hits the
// retry limit, and permanently marks the export failed without ever starting one for the new provider.
func TestStaleUploadJobReplacementDoesNotChargeFailedAttempt(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		TypeMeta:   metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency: "24h",
			// Now exporting to GCS...
			ExportTarball: &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
				Bucket: "snapshots",
				CredentialsSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "desired-gcs-credentials"},
					Key:                  "credentials.json",
				},
			}},
		}}},
	}
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Name: "snapshot", Namespace: "default",
		Annotations: map[string]string{
			controllers.AnnotationExportingTarball:      "true",
			controllers.AnnotationTarballExportAttempts: strconv.Itoa(tarballExportMaxAttempts - 1),
		},
	}}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, snapshot).
		Build()
	// ...but the in-flight upload Job belongs to the previous S3 exporter.
	clientSet := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "-00010101000000-upload",
			Namespace: "default",
			UID:       "11111111-2222-3333-4444-555555555555",
			Labels:    map[string]string{"exporter": "s3-exporter"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(),
				Kind:       "ChainNode",
				Name:       "node",
				UID:        "node-uid",
				Controller: ptr.To(true),
			}},
		},
	})
	recorder := record.NewFakeRecorder(10)
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          recorder,
	}

	ready, err := reconciler.isTarballReady(context.Background(), chainNode, snapshot)
	assert.False(t, ready)
	require.NoError(t, err)

	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), types.NamespacedName{Name: "snapshot", Namespace: "default"}, stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationExportingTarball])
	assert.Equal(t, strconv.Itoa(tarballExportMaxAttempts-1), stored.Annotations[controllers.AnnotationTarballExportAttempts])
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), types.NamespacedName{Name: "node", Namespace: "default"}, storedNode))
	require.Len(t, storedNode.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportProviderUnknown, storedNode.Status.SnapshotExports[0].Destination.Provider)
	assert.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, storedNode.Status.SnapshotExports[0].Phase)
	jobs, listErr := clientSet.BatchV1().Jobs("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	require.Len(t, jobs.Items, 1)
	assert.Equal(t, "s3-exporter", jobs.Items[0].Labels["exporter"])
	event := requireRecordedEvent(t, recorder)
	assert.Contains(t, event, appsv1.ReasonTarballCleanupRequired)
}

func TestTarballExportReplacementDoesNotEmitWarningEvent(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	reconciler := &Reconciler{recorder: recorder}
	replacement := &datasnapshot.StaleJobReplacedError{
		Purpose: "upload",
		Name:    "snapshot-upload",
		UID:     "11111111-2222-3333-4444-555555555555",
	}

	reconciler.recordTarballExportError(&appsv1.ChainNode{}, replacement)
	reconciler.recordTarballExportError(&appsv1.ChainNode{}, fmt.Errorf("waiting for delete: %w", datasnapshot.ErrStaleJobTerminating))

	assertNoRecordedEvent(t, recorder)
}

func TestSnapshotJobReplacementEventPreservesUIDBeforeTruncation(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	reconciler := &Reconciler{recorder: recorder}
	uid := types.UID("11111111-2222-3333-4444-555555555555")
	reconciler.recordSnapshotJobReplacement(&appsv1.ChainNode{}, &datasnapshot.StaleJobReplacedError{
		Purpose:          "upload",
		Namespace:        strings.Repeat("n", 63),
		Name:             strings.Repeat("j", 63),
		UID:              uid,
		ConflictingLabel: "exporter",
		PreviousValue:    strings.Repeat("previous-provider-", 20),
		DesiredValue:     strings.Repeat("desired-provider-", 20),
	})

	event := requireRecordedEvent(t, recorder)
	assert.Contains(t, event, string(uid))
	message := strings.TrimPrefix(event, "Normal "+appsv1.ReasonSnapshotJobReplaced+" ")
	assert.LessOrEqual(t, len(message), 256)
}

func TestStaleDeleteJobReplacementEmitsEvent(t *testing.T) {
	scheme := runtime.NewScheme()
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
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"}}
	clientSet := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "-00010101000000-delete",
		Namespace: "default",
		UID:       "stale-delete-uid",
		Labels: map[string]string{
			"exporter": "s3-exporter",
			"owner":    "node",
			"type":     "delete",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(),
			Kind:       "ChainNode",
			Name:       "node",
			UID:        "node-uid",
			Controller: ptr.To(true),
		}},
	}})
	deleteCalls := 0
	clientSet.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		resource := batchv1.SchemeGroupVersion.WithResource("jobs")
		stored, err := clientSet.Tracker().Get(resource, "default", "-00010101000000-delete")
		if err != nil {
			return true, nil, err
		}
		terminating := stored.(*batchv1.Job).DeepCopy()
		deletionTimestamp := metav1.Now()
		terminating.DeletionTimestamp = &deletionTimestamp
		if err = clientSet.Tracker().Update(resource, terminating, "default"); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})
	recorder := record.NewFakeRecorder(10)
	reconciler := &Reconciler{
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          recorder,
	}

	deleted, err := reconciler.isTarballDeleted(context.Background(), chainNode, snapshot)
	assert.False(t, deleted)
	require.ErrorIs(t, err, datasnapshot.ErrStaleJobReplaced)

	event := requireRecordedEvent(t, recorder)
	assert.Contains(t, event, "Normal "+appsv1.ReasonSnapshotJobReplaced)
	assert.Contains(t, event, "Replaced stale delete Job default/-00010101000000-delete")
	assert.Contains(t, event, "stale-delete-uid")
	assert.Contains(t, event, "s3-exporter")
	assert.Contains(t, event, "gcs-exporter")
	assert.Equal(t, 1, deleteCalls)

	deleted, err = reconciler.isTarballDeleted(context.Background(), chainNode, snapshot)
	assert.False(t, deleted)
	require.ErrorIs(t, err, datasnapshot.ErrStaleJobTerminating)
	assert.NotErrorIs(t, err, datasnapshot.ErrStaleJobReplaced)
	var replacement *datasnapshot.StaleJobReplacedError
	assert.False(t, errors.As(err, &replacement), "waiting for a terminating Job must not report another successful replacement")
	assert.Equal(t, 1, deleteCalls)
	assertNoRecordedEvent(t, recorder)
}

func TestStaleDeleteJobReplacementDoesNotEmitEventWhenUIDPreconditionConflicts(t *testing.T) {
	scheme := runtime.NewScheme()
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
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"}}
	clientSet := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "-00010101000000-delete",
		Namespace: "default",
		UID:       "stale-delete-uid",
		Labels: map[string]string{
			"exporter": "s3-exporter",
			"owner":    "node",
			"type":     "delete",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(),
			Kind:       "ChainNode",
			Name:       "node",
			UID:        "node-uid",
			Controller: ptr.To(true),
		}},
	}})
	clientSet.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: batchv1.GroupName, Resource: "jobs"},
			"-00010101000000-delete",
			errors.New("UID precondition did not match"),
		)
	})
	recorder := record.NewFakeRecorder(10)
	reconciler := &Reconciler{
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          recorder,
	}

	deleted, err := reconciler.isTarballDeleted(context.Background(), chainNode, snapshot)
	assert.False(t, deleted)
	require.True(t, apierrors.IsConflict(err))
	assertNoRecordedEvent(t, recorder)
}

func TestStaleDeleteJobReplacementEventReportsActualNonExporterLabelMismatch(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	reconciler := &Reconciler{recorder: recorder}
	reconciler.recordSnapshotJobReplacement(&appsv1.ChainNode{}, &datasnapshot.StaleJobReplacedError{
		Purpose:          "delete",
		Namespace:        "default",
		Name:             "snapshot-work",
		UID:              "stale-delete-uid",
		ConflictingLabel: "owner",
		PreviousValue:    "previous-node",
		DesiredValue:     "node",
	})

	event := requireRecordedEvent(t, recorder)
	assert.Contains(t, event, "Replaced stale delete Job default/snapshot-work")
	assert.Contains(t, event, "conflicting label \"owner\"")
	assert.Contains(t, event, "previous value \"previous-node\"")
	assert.Contains(t, event, "desired value \"node\"")
	assert.NotContains(t, event, "desired provider")
	assert.NotContains(t, event, "previous provider")
}

func requireRecordedEvent(t *testing.T, recorder *record.FakeRecorder) string {
	t.Helper()
	select {
	case event := <-recorder.Events:
		return event
	default:
		t.Fatal("expected Kubernetes event")
		return ""
	}
}

func assertNoRecordedEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected Kubernetes event: %s", event)
	default:
	}
}

func TestTarballExportFailureWaitsForCleanupBeforeRetry(t *testing.T) {
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
		Annotations: map[string]string{controllers.AnnotationExportingTarball: "true"},
	}}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, snapshot).
		Build()
	clientSet := fake.NewSimpleClientset(
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "-00010101000000-upload",
				Namespace: "default",
				Labels: map[string]string{
					"exporter":    "gcs-exporter",
					"owner":       chainNode.Name,
					"type":        "upload",
					"destination": datasnapshot.SnapshotDestinationLabel("gcs", "snapshots", "", "", false, "credentialsSecret", "", "", "serviceAccount", ""),
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.GroupVersion.String(),
					Kind:       "ChainNode",
					Name:       "node",
					UID:        "node-uid",
					Controller: ptr.To(true),
				}},
			},
			Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			}}},
		},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "-00010101000000-upload", Namespace: "default"}},
	)
	deleteCalls := 0
	clientSet.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		if deleteCalls == 1 {
			return true, nil, errors.New("delete failed")
		}
		return false, nil, nil
	})
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	ready, err := reconciler.isTarballReady(context.Background(), chainNode, snapshot)
	assert.False(t, ready)
	require.NoError(t, err)
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), types.NamespacedName{Name: "snapshot", Namespace: "default"}, stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationExportingTarball])
	_, hasAttempts := stored.Annotations[controllers.AnnotationTarballExportAttempts]
	assert.False(t, hasAttempts)
	assert.Equal(t, 0, deleteCalls)
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), types.NamespacedName{Name: "node", Namespace: "default"}, storedNode))
	require.Len(t, storedNode.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportProviderUnknown, storedNode.Status.SnapshotExports[0].Destination.Provider)
}

func TestIsTarballDeletedWaitsForDeleteJobSuccess(t *testing.T) {
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
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"}}
	export, err := newSnapshotExportStatus(chainNode, snapshot)
	require.NoError(t, err)
	export.Phase = appsv1.SnapshotExportPhaseUploaded
	chainNode.Status.SnapshotExports = []appsv1.SnapshotExportStatus{export}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, snapshot).
		Build()
	clientSet := fake.NewSimpleClientset()
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	deleted, err := reconciler.isTarballDeleted(context.Background(), chainNode, snapshot)
	require.NoError(t, err)
	assert.False(t, deleted)

	job, err := clientSet.BatchV1().Jobs("default").Get(context.Background(), "-00010101000000-delete", metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Failed = 1
	_, err = clientSet.BatchV1().Jobs("default").Update(context.Background(), job, metav1.UpdateOptions{})
	require.NoError(t, err)
	deleted, err = reconciler.isTarballDeleted(context.Background(), chainNode, snapshot)
	require.NoError(t, err)
	assert.False(t, deleted)

	job, err = clientSet.BatchV1().Jobs("default").Get(context.Background(), "-00010101000000-delete", metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, err = clientSet.BatchV1().Jobs("default").Update(context.Background(), job, metav1.UpdateOptions{})
	require.NoError(t, err)
	deleted, err = reconciler.isTarballDeleted(context.Background(), chainNode, snapshot)
	require.NoError(t, err)
	assert.True(t, deleted)
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(snapshot), stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationTarballDeletionComplete])
}

func TestIsTarballDeletedRequiresDurableSuccessBeforeReturning(t *testing.T) {
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
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"}}
	export, err := newSnapshotExportStatus(chainNode, snapshot)
	require.NoError(t, err)
	export.Phase = appsv1.SnapshotExportPhaseUploaded
	chainNode.Status.SnapshotExports = []appsv1.SnapshotExportStatus{export}
	baseClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, snapshot).
		Build()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "-00010101000000-delete",
			Namespace: "default",
			Labels: map[string]string{
				"exporter":    "gcs-exporter",
				"owner":       chainNode.Name,
				"type":        "delete",
				"destination": datasnapshot.SnapshotDestinationLabel("gcs", "snapshots", "", "", false, "credentialsSecret", "", "", "serviceAccount", ""),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: chainNode.APIVersion,
				Kind:       chainNode.Kind,
				Name:       chainNode.Name,
				UID:        chainNode.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: batchv1.JobSpec{BackoffLimit: ptr.To[int32](0)},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		}}},
	}
	clientSet := fake.NewSimpleClientset(job)
	reconciler := &Reconciler{
		Client:            &updateFailingClient{Client: baseClient},
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	deleted, err := reconciler.isTarballDeleted(context.Background(), chainNode, snapshot)
	assert.False(t, deleted)
	require.ErrorContains(t, err, "update failed")

	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, baseClient.Get(context.Background(), client.ObjectKeyFromObject(snapshot), stored))
	_, complete := stored.Annotations[controllers.AnnotationTarballDeletionComplete]
	assert.False(t, complete)
	_, complete = snapshot.Annotations[controllers.AnnotationTarballDeletionComplete]
	assert.False(t, complete)
	_, err = clientSet.BatchV1().Jobs("default").Get(context.Background(), job.Name, metav1.GetOptions{})
	require.NoError(t, err)
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

func TestFinishTarballExportRetriesCleanupAfterUpload(t *testing.T) {
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
		Status: appsv1.ChainNodeStatus{ChainID: "chain", PvcSize: "1Gi", LatestHeight: 1},
	}
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Name:      "snapshot",
		Namespace: "default",
		Labels:    map[string]string{controllers.LabelChainNode: chainNode.Name},
		Annotations: map[string]string{
			controllers.AnnotationPvcSnapshotReady: "true",
			controllers.AnnotationExportingTarball: "true",
		},
	}}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, snapshot).
		Build()
	clientSet := fake.NewSimpleClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "chain-00010101000000-upload", Namespace: "default"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "chain-00010101000000-upload", Namespace: "default"}},
	)
	deleteCalls := 0
	clientSet.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		if deleteCalls == 1 {
			return true, nil, errors.New("delete failed")
		}
		return false, nil, nil
	})
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	err := reconciler.finishTarballExport(context.Background(), chainNode, snapshot)
	require.ErrorContains(t, err, "delete failed")
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), types.NamespacedName{Name: "snapshot", Namespace: "default"}, stored))
	assert.Equal(t, "uploaded", stored.Annotations[controllers.AnnotationExportingTarball])

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, false))
	require.NoError(t, controllerClient.Get(context.Background(), types.NamespacedName{Name: "snapshot", Namespace: "default"}, stored))
	assert.Equal(t, tarballFinished, stored.Annotations[controllers.AnnotationExportingTarball])
	_, err = clientSet.BatchV1().Jobs("default").Get(context.Background(), "chain-00010101000000-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = clientSet.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "chain-00010101000000-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

type updateFailingClient struct {
	client.Client
}

func (c *updateFailingClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errors.New("update failed")
}

type replaceSnapshotBeforeDeleteClient struct {
	client.Client
	replacement     *snapshotv1.VolumeSnapshot
	markerPersisted bool
	deleteUID       types.UID
}

func (c *replaceSnapshotBeforeDeleteClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if err := c.Client.Update(ctx, object, opts...); err != nil {
		return err
	}
	if snapshot, ok := object.(*snapshotv1.VolumeSnapshot); ok &&
		snapshot.Annotations[controllers.AnnotationTarballDeletionComplete] == strconv.FormatBool(true) {
		c.markerPersisted = true
	}
	return nil
}

func (c *replaceSnapshotBeforeDeleteClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	snapshot, ok := object.(*snapshotv1.VolumeSnapshot)
	if !ok {
		return c.Client.Delete(ctx, object, opts...)
	}
	if !c.markerPersisted {
		return errors.New("snapshot deleted before tarball deletion marker was persisted")
	}
	if err := c.Client.Delete(ctx, snapshot); err != nil {
		return err
	}
	if err := c.Client.Create(ctx, c.replacement.DeepCopy()); err != nil {
		return err
	}

	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
	if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
		c.deleteUID = *deleteOptions.Preconditions.UID
	}
	if c.deleteUID != c.replacement.UID {
		return apierrors.NewConflict(
			schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshots"},
			snapshot.Name,
			errors.New("UID precondition did not match"),
		)
	}
	return c.Client.Delete(ctx, c.replacement)
}

func TestEnsureVolumeSnapshotsDoesNotDeleteSameNameReplacementAfterTarballMarker(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	retention := "1h"
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node",
			Namespace:         "default",
			UID:               "node-uid",
			CreationTimestamp: metav1.NewTime(now),
			Annotations: map[string]string{
				controllers.AnnotationLastPvcSnapshot: now.Format(timeLayout),
			},
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency:            "24h",
			Retention:            &retention,
			PreserveLastSnapshot: ptr.To(false),
			ExportTarball: &appsv1.ExportTarballConfig{
				DeleteOnExpire: ptr.To(true),
				GCS:            &appsv1.GcsExportConfig{Bucket: "snapshots"},
			},
		}}},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeRunning,
			ChainID:      "chain-1",
			PvcSize:      "1Gi",
			LatestHeight: 1,
		},
	}
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "snapshot",
			Namespace:         chainNode.Namespace,
			UID:               "original-snapshot-uid",
			CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Hour)),
			Labels:            map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotReady:  "true",
				controllers.AnnotationExportingTarball:  tarballFinished,
				controllers.AnnotationSnapshotRetention: retention,
			},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
	replacement := snapshot.DeepCopy()
	replacement.UID = "replacement-snapshot-uid"
	replacement.ResourceVersion = ""
	replacement.CreationTimestamp = metav1.NewTime(now)
	baseClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, snapshot).
		Build()
	tracingClient := &replaceSnapshotBeforeDeleteClient{Client: baseClient, replacement: replacement}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getTarballName(chainNode, snapshot) + "-delete",
			Namespace: chainNode.Namespace,
			Labels: map[string]string{
				"exporter": "gcs-exporter",
				"owner":    chainNode.Name,
				"type":     "delete",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: chainNode.APIVersion,
				Kind:       chainNode.Kind,
				Name:       chainNode.Name,
				UID:        chainNode.UID,
				Controller: ptr.To(true),
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	reconciler := &Reconciler{
		Client:            tracingClient,
		snapshotClientSet: fake.NewSimpleClientset(job),
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	err := reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true)
	require.True(t, apierrors.IsConflict(err))
	assert.True(t, tracingClient.markerPersisted)
	assert.Equal(t, snapshot.UID, tracingClient.deleteUID)
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, baseClient.Get(context.Background(), client.ObjectKeyFromObject(replacement), stored))
	assert.Equal(t, replacement.UID, stored.UID)
}

func TestEnsureVolumeSnapshotsHonorsTarballDeletionMarkerForRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	retention := "1h"
	retain := int32(1)
	tests := []struct {
		name      string
		export    *appsv1.ExportTarballConfig
		retention *string
		retain    *int32
		times     []time.Time
	}{
		{
			name:      "time-based GCS retention",
			export:    &appsv1.ExportTarballConfig{DeleteOnExpire: ptr.To(true), GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
			retention: &retention,
			times:     []time.Time{now.Add(-2 * time.Hour)},
		},
		{
			name:   "count-based S3 retention",
			export: &appsv1.ExportTarballConfig{DeleteOnExpire: ptr.To(true), S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}},
			retain: &retain,
			times:  []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Hour)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, snapshotv1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, batchv1.AddToScheme(scheme))

			chainNode := &appsv1.ChainNode{
				TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node",
					Namespace:         "default",
					UID:               "node-uid",
					CreationTimestamp: metav1.NewTime(now),
					Annotations: map[string]string{
						controllers.AnnotationLastPvcSnapshot: tt.times[len(tt.times)-1].Format(timeLayout),
					},
				},
				Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
					Frequency:            "240h",
					Retention:            tt.retention,
					Retain:               tt.retain,
					PreserveLastSnapshot: ptr.To(false),
					ExportTarball:        tt.export,
				}}},
				Status: appsv1.ChainNodeStatus{
					Phase:        appsv1.PhaseChainNodeRunning,
					ChainID:      "chain-1",
					PvcSize:      "1Gi",
					LatestHeight: 1,
				},
			}
			objects := []client.Object{chainNode}
			for i, createdAt := range tt.times {
				annotations := map[string]string{
					controllers.AnnotationPvcSnapshotReady: "true",
					controllers.AnnotationExportingTarball: tarballFinished,
				}
				if i == 0 {
					annotations[controllers.AnnotationTarballDeletionComplete] = "true"
				}
				if tt.retention != nil {
					annotations[controllers.AnnotationSnapshotRetention] = *tt.retention
				}
				objects = append(objects, &snapshotv1.VolumeSnapshot{
					ObjectMeta: metav1.ObjectMeta{
						Name:              fmt.Sprintf("snapshot-%d", i),
						Namespace:         chainNode.Namespace,
						UID:               types.UID(fmt.Sprintf("snapshot-%d-uid", i)),
						CreationTimestamp: metav1.NewTime(createdAt),
						Labels:            map[string]string{controllers.LabelChainNode: chainNode.Name},
						Annotations:       annotations,
					},
					Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
				})
			}
			controllerClient := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(objects...).
				Build()
			clientSet := fake.NewSimpleClientset()
			reconciler := &Reconciler{
				Client:            controllerClient,
				snapshotClientSet: clientSet,
				Scheme:            scheme,
				opts:              &controllers.ControllerRunOptions{},
				recorder:          record.NewFakeRecorder(10),
			}

			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			oldest := &snapshotv1.VolumeSnapshot{}
			err := controllerClient.Get(context.Background(), types.NamespacedName{Name: "snapshot-0", Namespace: chainNode.Namespace}, oldest)
			assert.True(t, apierrors.IsNotFound(err))
			jobs, err := clientSet.BatchV1().Jobs(chainNode.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, jobs.Items, "a durable deletion marker must not schedule another delete Job")
		})
	}
}

func TestEnsureVolumeSnapshotsReconcilesOrphanJobs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	providers := []struct {
		name     string
		exporter string
		export   *appsv1.ExportTarballConfig
	}{
		{
			name:     "GCS",
			exporter: "gcs-exporter",
			export:   &appsv1.ExportTarballConfig{DeleteOnExpire: ptr.To(true), GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
		},
		{
			name:     "S3",
			exporter: "s3-exporter",
			export:   &appsv1.ExportTarballConfig{DeleteOnExpire: ptr.To(true), S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}},
		},
	}
	scenarios := []struct {
		name            string
		purpose         string
		status          batchv1.JobStatus
		disappearOnList bool
		wantCreateCalls int
		wantDeletionJob bool
	}{
		{
			name:    "cleans successful deletion Job without creating first",
			purpose: "delete",
			status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}}},
		},
		{
			name:            "does not recreate deletion Job that disappears after listing",
			purpose:         "delete",
			disappearOnList: true,
		},
		{
			name:            "starts deletion for orphan upload Job",
			purpose:         "upload",
			wantCreateCalls: 1,
			wantDeletionJob: true,
		},
	}

	for _, provider := range providers {
		for _, scenario := range scenarios {
			t.Run(provider.name+"/"+scenario.name, func(t *testing.T) {
				scheme := runtime.NewScheme()
				require.NoError(t, snapshotv1.AddToScheme(scheme))
				require.NoError(t, appsv1.AddToScheme(scheme))
				require.NoError(t, corev1.AddToScheme(scheme))
				require.NoError(t, batchv1.AddToScheme(scheme))

				chainNode := &appsv1.ChainNode{
					TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node",
						Namespace:         "default",
						UID:               "node-uid",
						CreationTimestamp: metav1.NewTime(now),
						Annotations: map[string]string{
							controllers.AnnotationLastPvcSnapshot: now.Format(timeLayout),
						},
					},
					Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
						Frequency:     "24h",
						ExportTarball: provider.export,
					}}},
					Status: appsv1.ChainNodeStatus{
						Phase:        appsv1.PhaseChainNodeRunning,
						ChainID:      "chain-1",
						PvcSize:      "1Gi",
						LatestHeight: 1,
					},
				}
				jobName := "orphan-tarball-" + scenario.purpose
				controllerClient := fakeclient.NewClientBuilder().
					WithScheme(scheme).
					WithStatusSubresource(&appsv1.ChainNode{}).
					WithObjects(chainNode).
					Build()
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      jobName,
						Namespace: chainNode.Namespace,
						UID:       "orphan-job-uid",
						Labels: map[string]string{
							"exporter": provider.exporter,
							"owner":    chainNode.Name,
							"type":     scenario.purpose,
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: chainNode.APIVersion,
							Kind:       chainNode.Kind,
							Name:       chainNode.Name,
							UID:        chainNode.UID,
							Controller: ptr.To(true),
						}},
					},
					Status: scenario.status,
				}
				objects := []runtime.Object{job}
				if scenario.purpose == "upload" {
					objects = append(objects, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
						Name:      job.Name,
						Namespace: job.Namespace,
						UID:       "orphan-pvc-uid",
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: batchv1.SchemeGroupVersion.String(),
							Kind:       "Job",
							Name:       job.Name,
							UID:        job.UID,
							Controller: ptr.To(true),
						}},
					}})
				}
				clientSet := fake.NewSimpleClientset(objects...)
				if scenario.disappearOnList {
					listedJob := job.DeepCopy()
					clientSet.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
						err := clientSet.Tracker().Delete(batchv1.SchemeGroupVersion.WithResource("jobs"), chainNode.Namespace, job.Name)
						if err != nil && !apierrors.IsNotFound(err) {
							require.NoError(t, err)
						}
						return true, &batchv1.JobList{Items: []batchv1.Job{*listedJob}}, nil
					})
				}
				if scenario.purpose == "upload" {
					clientSet.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
						createAction, ok := action.(k8stesting.CreateAction)
						require.True(t, ok)
						createdJob, ok := createAction.GetObject().(*batchv1.Job)
						require.True(t, ok)
						createdJob.UID = "orphan-delete-job-uid"
						return false, nil, nil
					})
				}
				reconciler := &Reconciler{
					Client:            controllerClient,
					snapshotClientSet: clientSet,
					Scheme:            scheme,
					opts:              &controllers.ControllerRunOptions{},
					recorder:          record.NewFakeRecorder(10),
				}

				require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))
				createCalls := 0
				for _, action := range clientSet.Actions() {
					if action.GetVerb() == "create" && action.GetResource().Resource == "jobs" {
						createCalls++
					}
				}
				assert.Equal(t, scenario.wantCreateCalls, createCalls)
				if scenario.purpose == "upload" {
					_, getErr := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
					assert.True(t, apierrors.IsNotFound(getErr), "the upload Job must be absent before archive deletion starts")
					_, getErr = clientSet.CoreV1().PersistentVolumeClaims(chainNode.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
					assert.True(t, apierrors.IsNotFound(getErr), "the upload PVC must be torn down before archive deletion starts")
					deleteJob, getErr := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), "orphan-tarball-delete", metav1.GetOptions{})
					require.NoError(t, getErr)
					assert.Equal(t, types.UID("orphan-delete-job-uid"), deleteJob.UID)
					require.NotNil(t, deleteJob.Spec.Suspend)
					assert.False(t, *deleteJob.Spec.Suspend)
					assert.Equal(t, string(job.UID), deleteJob.Labels["cleanup-upload-uid"])
					assert.Equal(t, "orphan-pvc-uid", deleteJob.Labels["cleanup-pvc-uid"])
					assert.Equal(t, provider.exporter, deleteJob.Labels["cleanup-exporter"])
					assert.Equal(t, chainNode.Name, deleteJob.Labels["cleanup-owner"])
					assert.Equal(t, "upload", deleteJob.Labels["cleanup-type"])

					deleteJob.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
					_, getErr = clientSet.BatchV1().Jobs(chainNode.Namespace).UpdateStatus(context.Background(), deleteJob, metav1.UpdateOptions{})
					require.NoError(t, getErr)
					require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))
					require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

					createCalls = 0
					deleteActions := make([]k8stesting.DeleteAction, 0, 3)
					for _, action := range clientSet.Actions() {
						switch action.GetVerb() {
						case "create":
							if action.GetResource().Resource == "jobs" {
								createCalls++
							}
						case "delete":
							deleteAction, ok := action.(k8stesting.DeleteAction)
							require.True(t, ok)
							deleteActions = append(deleteActions, deleteAction)
						}
					}
					assert.Equal(t, 1, createCalls, "a successful deletion must not be scheduled twice")
					require.Len(t, deleteActions, 3)
					assertGuardedDeleteAction(t, deleteActions[0], "jobs", job.Name, job.UID)
					assertGuardedDeleteAction(t, deleteActions[1], "persistentvolumeclaims", job.Name, "orphan-pvc-uid")
					assertGuardedDeleteAction(t, deleteActions[2], "jobs", deleteJob.Name, deleteJob.UID)

					_, getErr = clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
					assert.True(t, apierrors.IsNotFound(getErr), "orphan upload Job must be cleaned up after deletion succeeds")
					_, getErr = clientSet.CoreV1().PersistentVolumeClaims(chainNode.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
					assert.True(t, apierrors.IsNotFound(getErr), "orphan upload PVC must be cleaned up after deletion succeeds")
					_, getErr = clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), deleteJob.Name, metav1.GetOptions{})
					assert.True(t, apierrors.IsNotFound(getErr), "successful deletion Job must be cleaned up last")
					return
				}
				_, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), "orphan-tarball-upload", metav1.GetOptions{})
				assert.True(t, apierrors.IsNotFound(err), "orphan upload Job must be cleaned up")
				_, err = clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), "orphan-tarball-delete", metav1.GetOptions{})
				if scenario.wantDeletionJob {
					require.NoError(t, err)
				} else {
					assert.True(t, apierrors.IsNotFound(err), "completed or disappeared deletion Job must not be recreated")
				}
			})
		}
	}
}

func TestEnsureVolumeSnapshotsResumesOrphanDeletionAfterCrashFollowingDeleteJobCreation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	providers := []struct {
		name     string
		exporter string
		export   *appsv1.ExportTarballConfig
	}{
		{
			name:     "GCS",
			exporter: "gcs-exporter",
			export:   &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
		},
		{
			name:     "S3",
			exporter: "s3-exporter",
			export:   &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}},
		},
	}

	for _, provider := range providers {
		t.Run(provider.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, snapshotv1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, batchv1.AddToScheme(scheme))

			chainNode := orphanSnapshotTestChainNode(now, provider.export)
			ownerReference := metav1.OwnerReference{
				APIVersion: chainNode.APIVersion,
				Kind:       chainNode.Kind,
				Name:       chainNode.Name,
				UID:        chainNode.UID,
				Controller: ptr.To(true),
			}
			uploadJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:            "orphan-tarball-upload",
				Namespace:       chainNode.Namespace,
				UID:             "upload-job-uid",
				Labels:          map[string]string{"exporter": provider.exporter, "owner": chainNode.Name, "type": "upload"},
				OwnerReferences: []metav1.OwnerReference{ownerReference},
			}}
			uploadPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name:      uploadJob.Name,
				Namespace: uploadJob.Namespace,
				UID:       "upload-pvc-uid",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       "Job",
					Name:       uploadJob.Name,
					UID:        uploadJob.UID,
					Controller: ptr.To(true),
				}},
			}}
			controllerClient := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(chainNode).
				Build()
			clientSet := fake.NewSimpleClientset(uploadJob, uploadPVC)
			crashErr := errors.New("simulated crash after delete Job creation")
			createdDeleteUIDs := make([]types.UID, 0, 1)
			clientSet.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				createAction, ok := action.(k8stesting.CreateAction)
				require.True(t, ok)
				job, ok := createAction.GetObject().(*batchv1.Job)
				require.True(t, ok)
				if job.Labels["type"] != "delete" {
					return false, nil, nil
				}

				uid := types.UID(fmt.Sprintf("delete-job-uid-%d", len(createdDeleteUIDs)+1))
				createdDeleteUIDs = append(createdDeleteUIDs, uid)
				job.UID = uid
				if len(createdDeleteUIDs) != 1 {
					return false, nil, nil
				}

				created := job.DeepCopy()
				require.NoError(t, clientSet.Tracker().Create(
					batchv1.SchemeGroupVersion.WithResource("jobs"), created, created.Namespace,
				))
				return true, nil, crashErr
			})
			reconciler := &Reconciler{
				Client:            controllerClient,
				snapshotClientSet: clientSet,
				Scheme:            scheme,
				opts:              &controllers.ControllerRunOptions{},
				recorder:          record.NewFakeRecorder(10),
			}

			err := reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true)
			require.ErrorIs(t, err, crashErr)
			_, err = clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), uploadJob.Name, metav1.GetOptions{})
			require.NoError(t, err, "the upload Job is the durable cleanup state while deletion runs")
			_, err = clientSet.CoreV1().PersistentVolumeClaims(chainNode.Namespace).Get(context.Background(), uploadPVC.Name, metav1.GetOptions{})
			require.NoError(t, err, "the upload PVC is the durable cleanup state while deletion runs")

			deleteJob, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), "orphan-tarball-delete", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, types.UID("delete-job-uid-1"), deleteJob.UID)
			deleteJob.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			_, err = clientSet.BatchV1().Jobs(chainNode.Namespace).UpdateStatus(context.Background(), deleteJob, metav1.UpdateOptions{})
			require.NoError(t, err)

			for range 3 {
				require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))
			}

			assert.Equal(t, []types.UID{"delete-job-uid-1"}, createdDeleteUIDs,
				"the successful external deletion must never be scheduled a second time")
			deleteActions := make([]k8stesting.DeleteAction, 0, 3)
			for _, action := range clientSet.Actions() {
				if action.GetVerb() != "delete" {
					continue
				}
				deleteAction, ok := action.(k8stesting.DeleteAction)
				require.True(t, ok)
				deleteActions = append(deleteActions, deleteAction)
			}
			require.Len(t, deleteActions, 3)
			assertGuardedDeleteAction(t, deleteActions[0], "jobs", uploadJob.Name, uploadJob.UID)
			assertGuardedDeleteAction(t, deleteActions[1], "persistentvolumeclaims", uploadPVC.Name, uploadPVC.UID)
			assertGuardedDeleteAction(t, deleteActions[2], "jobs", deleteJob.Name, deleteJob.UID)

			jobs, err := clientSet.BatchV1().Jobs(chainNode.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, jobs.Items)
			_, err = clientSet.CoreV1().PersistentVolumeClaims(chainNode.Namespace).Get(context.Background(), uploadPVC.Name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
		})
	}
}

func TestEnsureVolumeSnapshotsDoesNotRestartDeletionForTerminatingOrphanUpload(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	deletionTimestamp := metav1.NewTime(now.Add(-time.Minute))
	providers := []struct {
		name     string
		exporter string
		export   *appsv1.ExportTarballConfig
	}{
		{
			name:     "GCS",
			exporter: "gcs-exporter",
			export:   &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
		},
		{
			name:     "S3",
			exporter: "s3-exporter",
			export:   &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}},
		},
	}

	for _, provider := range providers {
		t.Run(provider.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, snapshotv1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, batchv1.AddToScheme(scheme))

			chainNode := orphanSnapshotTestChainNode(now, provider.export)
			ownerReference := metav1.OwnerReference{
				APIVersion: chainNode.APIVersion,
				Kind:       chainNode.Kind,
				Name:       chainNode.Name,
				UID:        chainNode.UID,
				Controller: ptr.To(true),
			}
			uploadJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:              "orphan-tarball-upload",
				Namespace:         chainNode.Namespace,
				UID:               "orphan-upload-uid",
				DeletionTimestamp: &deletionTimestamp,
				Finalizers:        []string{"foregroundDeletion"},
				Labels: map[string]string{
					"exporter": provider.exporter,
					"owner":    chainNode.Name,
					"type":     "upload",
				},
				OwnerReferences: []metav1.OwnerReference{ownerReference},
			}}
			deleteJob := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "orphan-tarball-delete",
					Namespace:       chainNode.Namespace,
					UID:             "orphan-delete-uid",
					Labels:          map[string]string{"exporter": provider.exporter, "owner": chainNode.Name, "type": "delete"},
					OwnerReferences: []metav1.OwnerReference{ownerReference},
				},
				Spec: batchv1.JobSpec{BackoffLimit: ptr.To[int32](0)},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				}}},
			}
			controllerClient := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(chainNode).
				Build()
			clientSet := fake.NewSimpleClientset(uploadJob, deleteJob)
			reconciler := &Reconciler{
				Client:            controllerClient,
				snapshotClientSet: clientSet,
				Scheme:            scheme,
				opts:              &controllers.ControllerRunOptions{},
				recorder:          record.NewFakeRecorder(10),
			}

			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))
			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			purgeJob, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), "orphan-tarball-purge", metav1.GetOptions{})
			require.NoError(t, err, "terminal legacy deletion must persist a post-upload cleanup Job")
			require.NotNil(t, purgeJob.Spec.Suspend)
			assert.True(t, *purgeJob.Spec.Suspend, "post-upload deletion must remain suspended while the upload terminates")
			assert.Equal(t, string(uploadJob.UID), purgeJob.Labels["cleanup-upload-uid"])
			_, err = clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), uploadJob.Name, metav1.GetOptions{})
			require.NoError(t, err, "the foreground-terminating upload Job must be left alone")
		})
	}
}

func TestEnsureVolumeSnapshotsDoesNotActOnSameNameOrphanUploadReplacement(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	providers := []struct {
		name     string
		exporter string
		export   *appsv1.ExportTarballConfig
	}{
		{
			name:     "GCS",
			exporter: "gcs-exporter",
			export:   &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
		},
		{
			name:     "S3",
			exporter: "s3-exporter",
			export:   &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"}},
		},
	}

	for _, provider := range providers {
		t.Run(provider.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, snapshotv1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, batchv1.AddToScheme(scheme))

			chainNode := orphanSnapshotTestChainNode(now, provider.export)
			ownerReference := metav1.OwnerReference{
				APIVersion: chainNode.APIVersion,
				Kind:       chainNode.Kind,
				Name:       chainNode.Name,
				UID:        chainNode.UID,
				Controller: ptr.To(true),
			}
			originalJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:            "orphan-tarball-upload",
				Namespace:       chainNode.Namespace,
				UID:             "listed-upload-uid",
				Labels:          map[string]string{"exporter": provider.exporter, "owner": chainNode.Name, "type": "upload"},
				OwnerReferences: []metav1.OwnerReference{ownerReference},
			}}
			originalPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name:      originalJob.Name,
				Namespace: originalJob.Namespace,
				UID:       "listed-pvc-uid",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       "Job",
					Name:       originalJob.Name,
					UID:        originalJob.UID,
					Controller: ptr.To(true),
				}},
			}}
			replacementJob := originalJob.DeepCopy()
			replacementJob.UID = "replacement-upload-uid"
			replacementPVC := originalPVC.DeepCopy()
			replacementPVC.UID = "replacement-pvc-uid"
			replacementPVC.OwnerReferences[0].UID = replacementJob.UID
			controllerClient := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(chainNode).
				Build()
			clientSet := fake.NewSimpleClientset(originalJob, originalPVC)
			listedJob := originalJob.DeepCopy()
			clientSet.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				require.NoError(t, clientSet.Tracker().Delete(batchv1.SchemeGroupVersion.WithResource("jobs"), chainNode.Namespace, originalJob.Name))
				require.NoError(t, clientSet.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), chainNode.Namespace, originalPVC.Name))
				require.NoError(t, clientSet.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), replacementJob.DeepCopy(), chainNode.Namespace))
				require.NoError(t, clientSet.Tracker().Create(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), replacementPVC.DeepCopy(), chainNode.Namespace))
				return true, &batchv1.JobList{Items: []batchv1.Job{*listedJob}}, nil
			})
			reconciler := &Reconciler{
				Client:            controllerClient,
				snapshotClientSet: clientSet,
				Scheme:            scheme,
				opts:              &controllers.ControllerRunOptions{},
				recorder:          record.NewFakeRecorder(10),
			}

			err := reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true)
			require.ErrorContains(t, err, "UID")

			for _, action := range clientSet.Actions() {
				assert.False(t, action.GetVerb() == "create" && action.GetResource().Resource == "jobs",
					"a stale listing must not create an archive deletion Job")
				assert.NotEqual(t, "delete", action.GetVerb(), "replacement resources must not be deleted")
			}
			storedJob, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), replacementJob.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, replacementJob.UID, storedJob.UID)
			storedPVC, err := clientSet.CoreV1().PersistentVolumeClaims(chainNode.Namespace).Get(context.Background(), replacementPVC.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, replacementPVC.UID, storedPVC.UID)
		})
	}
}

func TestEnsureVolumeSnapshotsCleansImmediateOrphanDeletionSuccessWithPairedIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, provider := range orphanSnapshotProviderCases() {
		t.Run(provider.name, func(t *testing.T) {
			reconciler, chainNode, clientSet, uploadJob, uploadPVC := newOrphanUploadTestReconciler(
				t, now, provider.exporter, provider.export,
			)
			clientSet.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				createAction, ok := action.(k8stesting.CreateAction)
				require.True(t, ok)
				job, ok := createAction.GetObject().(*batchv1.Job)
				require.True(t, ok)
				if job.Labels["type"] == "delete" {
					job.UID = "orphan-delete-uid"
					job.Status.Conditions = []batchv1.JobCondition{{
						Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
					}}
				}
				return false, nil, nil
			})

			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			_, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), uploadJob.Name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
			_, err = clientSet.CoreV1().PersistentVolumeClaims(chainNode.Namespace).Get(context.Background(), uploadPVC.Name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
			_, err = clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), "orphan-tarball-delete", metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
		})
	}
}

func TestEnsureVolumeSnapshotsRecoversWhenListedUploadJobVanishedButPVCRemains(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, provider := range orphanSnapshotProviderCases() {
		t.Run(provider.name, func(t *testing.T) {
			reconciler, chainNode, clientSet, uploadJob, uploadPVC := newOrphanUploadTestReconciler(
				t, now, provider.exporter, provider.export,
			)
			listedUpload := uploadJob.DeepCopy()
			clientSet.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				err := clientSet.Tracker().Delete(
					batchv1.SchemeGroupVersion.WithResource("jobs"), chainNode.Namespace, uploadJob.Name,
				)
				if err != nil && !apierrors.IsNotFound(err) {
					require.NoError(t, err)
				}
				return true, &batchv1.JobList{Items: []batchv1.Job{*listedUpload}}, nil
			})
			clientSet.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				createAction, ok := action.(k8stesting.CreateAction)
				require.True(t, ok)
				job, ok := createAction.GetObject().(*batchv1.Job)
				require.True(t, ok)
				if job.Labels["type"] == "delete" {
					job.UID = "orphan-delete-uid"
					job.Status.Conditions = []batchv1.JobCondition{{
						Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
					}}
				}
				return false, nil, nil
			})

			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			_, err := clientSet.CoreV1().PersistentVolumeClaims(chainNode.Namespace).Get(context.Background(), uploadPVC.Name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err))
			deleteActions := make([]k8stesting.DeleteAction, 0, 2)
			for _, action := range clientSet.Actions() {
				if action.GetVerb() == "delete" {
					deleteAction, ok := action.(k8stesting.DeleteAction)
					require.True(t, ok)
					deleteActions = append(deleteActions, deleteAction)
				}
			}
			require.Len(t, deleteActions, 2)
			assertGuardedDeleteAction(t, deleteActions[0], "persistentvolumeclaims", uploadPVC.Name, uploadPVC.UID)
			assertGuardedDeleteAction(t, deleteActions[1], "jobs", "orphan-tarball-delete", "orphan-delete-uid")
		})
	}
}

func TestEnsureVolumeSnapshotsWaitsForForegroundUploadTerminationBeforeArchiveDeletion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, provider := range orphanSnapshotProviderCases() {
		t.Run(provider.name, func(t *testing.T) {
			reconciler, chainNode, clientSet, uploadJob, _ := newOrphanUploadTestReconciler(
				t, now, provider.exporter, provider.export,
			)
			clientSet.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				createAction, ok := action.(k8stesting.CreateAction)
				require.True(t, ok)
				job, ok := createAction.GetObject().(*batchv1.Job)
				require.True(t, ok)
				if job.Labels["type"] == "delete" {
					job.UID = "orphan-delete-uid"
				}
				return false, nil, nil
			})
			clientSet.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				deleteAction, ok := action.(k8stesting.DeleteAction)
				require.True(t, ok)
				if deleteAction.GetName() != uploadJob.Name {
					return false, nil, nil
				}
				terminating := uploadJob.DeepCopy()
				deletionTimestamp := metav1.Now()
				terminating.DeletionTimestamp = &deletionTimestamp
				terminating.Finalizers = []string{"foregroundDeletion"}
				require.NoError(t, clientSet.Tracker().Update(
					batchv1.SchemeGroupVersion.WithResource("jobs"), terminating, terminating.Namespace,
				))
				return true, nil, nil
			})

			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			deleteJob, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), "orphan-tarball-delete", metav1.GetOptions{})
			require.NoError(t, err)
			require.NotNil(t, deleteJob.Spec.Suspend)
			assert.True(t, *deleteJob.Spec.Suspend, "external deletion must remain suspended while the upload Job exists")
			storedUpload, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), uploadJob.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.NotNil(t, storedUpload.DeletionTimestamp)

			require.NoError(t, clientSet.Tracker().Delete(
				batchv1.SchemeGroupVersion.WithResource("jobs"), chainNode.Namespace, uploadJob.Name,
			))
			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			deleteJob, err = clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), deleteJob.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.NotNil(t, deleteJob.Spec.Suspend)
			assert.False(t, *deleteJob.Spec.Suspend, "external deletion may start only after the upload Job is absent")
		})
	}
}

type orphanSnapshotProviderCase struct {
	name     string
	exporter string
	export   *appsv1.ExportTarballConfig
}

func orphanSnapshotProviderCases() []orphanSnapshotProviderCase {
	return []orphanSnapshotProviderCase{
		{
			name:     "GCS",
			exporter: "gcs-exporter",
			export:   &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}},
		},
		{
			name:     "S3",
			exporter: "s3-exporter",
			export: &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{
				Bucket: "snapshots", Region: "eu-west-1",
			}},
		},
	}
}

func newOrphanUploadTestReconciler(
	t *testing.T,
	now time.Time,
	exporter string,
	export *appsv1.ExportTarballConfig,
) (*Reconciler, *appsv1.ChainNode, *fake.Clientset, *batchv1.Job, *corev1.PersistentVolumeClaim) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	chainNode := orphanSnapshotTestChainNode(now, export)
	ownerReference := metav1.OwnerReference{
		APIVersion: chainNode.APIVersion,
		Kind:       chainNode.Kind,
		Name:       chainNode.Name,
		UID:        chainNode.UID,
		Controller: ptr.To(true),
	}
	uploadJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "orphan-tarball-upload",
		Namespace: chainNode.Namespace,
		UID:       "orphan-upload-uid",
		Labels: map[string]string{
			"exporter": exporter,
			"owner":    chainNode.Name,
			"type":     "upload",
		},
		OwnerReferences: []metav1.OwnerReference{ownerReference},
	}}
	uploadPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      uploadJob.Name,
		Namespace: uploadJob.Namespace,
		UID:       "orphan-pvc-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(),
			Kind:       "Job",
			Name:       uploadJob.Name,
			UID:        uploadJob.UID,
			Controller: ptr.To(true),
		}},
	}}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode).
		Build()
	clientSet := fake.NewSimpleClientset(uploadJob, uploadPVC)
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}
	return reconciler, chainNode, clientSet, uploadJob, uploadPVC
}

func TestEnsureVolumeSnapshotsResumesPendingDeletionWhenExportsAreDisabled(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct {
		name    string
		disable func(*appsv1.ChainNode)
	}{
		{
			name: "tarball export disabled",
			disable: func(chainNode *appsv1.ChainNode) {
				chainNode.Spec.Persistence.Snapshots.ExportTarball = nil
			},
		},
		{
			name: "snapshots disabled",
			disable: func(chainNode *appsv1.ChainNode) {
				chainNode.Spec.Persistence.Snapshots = nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, snapshotv1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, batchv1.AddToScheme(scheme))

			chainNode := orphanSnapshotTestChainNode(now, &appsv1.ExportTarballConfig{
				S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"},
			})
			test.disable(chainNode)
			suspended := true
			deleteJob := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-tarball-delete",
					Namespace: chainNode.Namespace,
					UID:       "orphan-delete-uid",
					Labels: map[string]string{
						"exporter":           "s3-exporter",
						"owner":              chainNode.Name,
						"type":               "delete",
						"cleanup-exporter":   "s3-exporter",
						"cleanup-owner":      chainNode.Name,
						"cleanup-type":       "upload",
						"cleanup-upload-uid": "orphan-upload-uid",
						"cleanup-pvc-uid":    "orphan-pvc-uid",
					},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: chainNode.APIVersion,
						Kind:       chainNode.Kind,
						Name:       chainNode.Name,
						UID:        chainNode.UID,
						Controller: ptr.To(true),
					}},
				},
				Spec: batchv1.JobSpec{Suspend: &suspended},
			}
			controllerClient := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(chainNode).
				Build()
			clientSet := fake.NewSimpleClientset(deleteJob)
			reconciler := &Reconciler{
				Client:            controllerClient,
				snapshotClientSet: clientSet,
				Scheme:            scheme,
				opts:              &controllers.ControllerRunOptions{},
				recorder:          record.NewFakeRecorder(10),
			}

			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			stored, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), deleteJob.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.NotNil(t, stored.Spec.Suspend)
			assert.False(t, *stored.Spec.Suspend)
		})
	}
}

func TestEnsureVolumeSnapshotsPersistsPendingDeletionSuccessWhenSnapshotsAreDisabled(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	chainNode := orphanSnapshotTestChainNode(now, &appsv1.ExportTarballConfig{
		S3: &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"},
	})
	chainNode.Spec.Persistence.Snapshots = nil
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Name:              "snapshot",
		Namespace:         chainNode.Namespace,
		CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		Labels:            map[string]string{controllers.LabelChainNode: chainNode.Name},
	}}
	tarballName := fmt.Sprintf("%s-%s-archive", chainNode.Status.ChainID, snapshot.CreationTimestamp.UTC().Format(timeLayout))
	deleteJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tarballName + "-delete",
			Namespace: chainNode.Namespace,
			UID:       "delete-uid",
			Labels: map[string]string{
				"exporter": "s3-exporter",
				"owner":    chainNode.Name,
				"type":     "delete",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: chainNode.APIVersion,
				Kind:       chainNode.Kind,
				Name:       chainNode.Name,
				UID:        chainNode.UID,
				Controller: ptr.To(true),
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, snapshot).
		Build()
	clientSet := fake.NewSimpleClientset(deleteJob)
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(snapshot), stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationTarballDeletionComplete])
	_, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), deleteJob.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
}

func TestSnapshotKubernetesClientFallsBackToProductionClientSet(t *testing.T) {
	production := &kubernetes.Clientset{}
	override := fake.NewSimpleClientset()
	reconciler := &Reconciler{ClientSet: production}
	assert.Same(t, production, reconciler.snapshotKubernetesClient())
	reconciler.snapshotClientSet = override
	assert.Same(t, override, reconciler.snapshotKubernetesClient())
}

func TestReconcilePendingTarballDeletionsBlocksFailedWorkflow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	chainNode := orphanSnapshotTestChainNode(now, &appsv1.ExportTarballConfig{
		GCS: &appsv1.GcsExportConfig{Bucket: "new-bucket"},
	})
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "old-tarball-delete", Namespace: chainNode.Namespace, UID: "delete-uid",
			Labels: map[string]string{"exporter": "s3-exporter", "owner": chainNode.Name, "type": "delete"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: chainNode.APIVersion, Kind: chainNode.Kind, Name: chainNode.Name,
				UID: chainNode.UID, Controller: ptr.To(true),
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		}}},
	}
	clientSet := fake.NewSimpleClientset(failedJob)
	reconciler := &Reconciler{snapshotClientSet: clientSet, recorder: record.NewFakeRecorder(10)}

	pending, err := reconciler.reconcilePendingTarballDeletions(context.Background(), chainNode)
	require.NoError(t, err)
	assert.True(t, pending)
	stored, err := clientSet.BatchV1().Jobs(chainNode.Namespace).Get(context.Background(), failedJob.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, failedJob.UID, stored.UID)
}

func TestEnsureVolumeSnapshotsProcessesReadySnapshotBehindFailedDeletion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	chainNode := orphanSnapshotTestChainNode(now, nil)
	chainNode.Annotations[controllers.AnnotationPvcSnapshotInProgress] = "true"
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "snapshot", Namespace: chainNode.Namespace, CreationTimestamp: metav1.NewTime(now),
			Labels:      map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "false"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{
			ReadyToUse:  ptr.To(true),
			RestoreSize: resource.NewQuantity(1, resource.BinarySI),
		},
	}
	failedJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "old-delete", Namespace: chainNode.Namespace, UID: "delete-uid",
		Labels:          map[string]string{"exporter": "s3-exporter", "owner": chainNode.Name, "type": "delete"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: chainNode.APIVersion, Kind: chainNode.Kind, Name: chainNode.Name, UID: chainNode.UID, Controller: ptr.To(true)}},
	}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	controllerClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode, snapshot).Build()
	reconciler := &Reconciler{Client: controllerClient, snapshotClientSet: fake.NewSimpleClientset(failedJob), recorder: record.NewFakeRecorder(10), opts: &controllers.ControllerRunOptions{}}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(snapshot), stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotReady])
}

func TestPersistPendingTarballDeletionSuccessScopesMarkerToName(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	chainNode := orphanSnapshotTestChainNode(now, nil)
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Name: "snapshot", Namespace: chainNode.Namespace, CreationTimestamp: metav1.NewTime(now),
		Labels: map[string]string{controllers.LabelChainNode: chainNode.Name},
	}}
	controllerClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).Build()
	reconciler := &Reconciler{Client: controllerClient}
	oldName := fmt.Sprintf("%s-%s-old", chainNode.Status.ChainID, now.Format(timeLayout))
	require.NoError(t, reconciler.persistPendingTarballDeletionSuccess(context.Background(), chainNode, oldName))
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(snapshot), stored))
	assert.Equal(t, oldName, stored.Annotations[controllers.AnnotationTarballDeletionName])

	chainNode.Spec.Persistence.Snapshots.ExportTarball = &appsv1.ExportTarballConfig{Suffix: ptr.To("new")}
	assert.False(t, tarballDeletionComplete(stored, getTarballName(chainNode, stored), true))
}

func orphanSnapshotTestChainNode(now time.Time, export *appsv1.ExportTarballConfig) *appsv1.ChainNode {
	return &appsv1.ChainNode{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node",
			Namespace:         "default",
			UID:               "node-uid",
			CreationTimestamp: metav1.NewTime(now),
			Annotations: map[string]string{
				controllers.AnnotationLastPvcSnapshot: now.Format(timeLayout),
			},
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency:     "24h",
			ExportTarball: export,
		}}},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeRunning,
			ChainID:      "chain-1",
			PvcSize:      "1Gi",
			LatestHeight: 1,
		},
	}
}

func assertGuardedDeleteAction(t *testing.T, action k8stesting.DeleteAction, resource, name string, uid types.UID) {
	t.Helper()
	assert.Equal(t, resource, action.GetResource().Resource)
	assert.Equal(t, name, action.GetName())
	options := action.GetDeleteOptions()
	require.NotNil(t, options.Preconditions)
	require.NotNil(t, options.Preconditions.UID)
	assert.Equal(t, uid, *options.Preconditions.UID)
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

func TestEnsureVolumeSnapshotsRepairsSnapshotInProgressAnnotation(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name             string
		snapshots        []snapshotv1.VolumeSnapshot
		wantSnapshotting string
	}{
		{
			name: "retained ready snapshot does not keep stale annotation",
			snapshots: []snapshotv1.VolumeSnapshot{{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node-ready",
					Namespace:         "default",
					CreationTimestamp: now,
					Annotations: map[string]string{
						controllers.AnnotationPvcSnapshotReady: "true",
					},
				},
				Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
			}},
			wantSnapshotting: "false",
		},
		{
			name: "pending snapshot alongside retained ready snapshot keeps annotation",
			snapshots: []snapshotv1.VolumeSnapshot{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node-ready",
						Namespace:         "default",
						CreationTimestamp: now,
						Annotations: map[string]string{
							controllers.AnnotationPvcSnapshotReady: "true",
						},
					},
					Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node-pending",
						Namespace:         "default",
						CreationTimestamp: now,
						Annotations: map[string]string{
							controllers.AnnotationPvcSnapshotReady: "false",
						},
					},
					Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(false)},
				},
			},
			wantSnapshotting: "true",
		},
		{
			name: "snapshot error alongside retained ready snapshot keeps annotation while CSI retries",
			snapshots: []snapshotv1.VolumeSnapshot{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node-ready",
						Namespace:         "default",
						CreationTimestamp: now,
						Annotations: map[string]string{
							controllers.AnnotationPvcSnapshotReady: "true",
						},
					},
					Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node-failed",
						Namespace:         "default",
						CreationTimestamp: now,
						Annotations: map[string]string{
							controllers.AnnotationPvcSnapshotReady: "false",
						},
					},
					Status: &snapshotv1.VolumeSnapshotStatus{
						ReadyToUse: ptr.To(false),
						Error:      &snapshotv1.VolumeSnapshotError{Message: ptr.To("snapshot failed")},
					},
				},
			},
			wantSnapshotting: "true",
		},
		{
			name: "deleting snapshot alongside retained ready snapshot does not keep stale annotation",
			snapshots: []snapshotv1.VolumeSnapshot{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node-ready",
						Namespace:         "default",
						CreationTimestamp: now,
						Annotations: map[string]string{
							controllers.AnnotationPvcSnapshotReady: "true",
						},
					},
					Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node-deleting",
						Namespace:         "default",
						CreationTimestamp: now,
						DeletionTimestamp: &now,
						Finalizers:        []string{"snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"},
						Annotations: map[string]string{
							controllers.AnnotationPvcSnapshotReady: "false",
						},
					},
					Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(false)},
				},
			},
			wantSnapshotting: "false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, snapshotv1.AddToScheme(scheme))

			chainNode := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node",
					Namespace:         "default",
					CreationTimestamp: now,
					Annotations: map[string]string{
						controllers.AnnotationPvcSnapshotInProgress: "true",
						controllers.AnnotationLastPvcSnapshot:       now.UTC().Format(timeLayout),
					},
				},
				Spec: appsv1.ChainNodeSpec{
					Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "24h"}},
				},
				Status: appsv1.ChainNodeStatus{
					Phase:        appsv1.PhaseChainNodeSnapshotting,
					PvcSize:      "1Gi",
					LatestHeight: 1,
				},
			}
			objects := make([]client.Object, 0, len(tt.snapshots)+1)
			objects = append(objects, chainNode)
			for i := range tt.snapshots {
				tt.snapshots[i].Labels = map[string]string{controllers.LabelChainNode: chainNode.Name}
				objects = append(objects, &tt.snapshots[i])
			}

			controllerClient := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(objects...).
				Build()
			reconciler := &Reconciler{
				Client:   controllerClient,
				recorder: record.NewFakeRecorder(10),
			}

			require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

			stored := &appsv1.ChainNode{}
			require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
			assert.Equal(t, tt.wantSnapshotting, stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
		})
	}
}

func TestEnsureVolumeSnapshotsRepairsFalseAnnotationFromActiveSnapshot(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	now := metav1.Now()
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "false",
				controllers.AnnotationLastPvcSnapshot:       now.Add(-25 * time.Hour).UTC().Format(timeLayout),
			},
		},
		Spec: appsv1.ChainNodeSpec{
			Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "24h"}},
		},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeRunning,
			PvcSize:      "1Gi",
			LatestHeight: 1,
		},
	}
	pendingSnapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-pending",
			Namespace:         "default",
			CreationTimestamp: now,
			Labels:            map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotReady: "false",
			},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(false)},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, pendingSnapshot).
		Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.Equal(t, appsv1.PhaseChainNodeSnapshotting, stored.Status.Phase)

	snapshots := &snapshotv1.VolumeSnapshotList{}
	require.NoError(t, controllerClient.List(context.Background(), snapshots, client.MatchingLabels{controllers.LabelChainNode: chainNode.Name}))
	assert.Len(t, snapshots.Items, 1, "an active snapshot must prevent scheduling another snapshot")
}

func TestEnsureVolumeSnapshotsClearingStaleAnnotationKeepsSnapshottingUntilPodReconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	now := metav1.Now()
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "true",
				controllers.AnnotationLastPvcSnapshot:       now.UTC().Format(timeLayout),
			},
		},
		Spec: appsv1.ChainNodeSpec{
			Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "24h"}},
		},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeSnapshotting,
			PvcSize:      "1Gi",
			LatestHeight: 1,
		},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode).
		Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, "false", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.Equal(t, appsv1.PhaseChainNodeSnapshotting, stored.Status.Phase)
}

func TestEnsureVolumeSnapshotsClearingStaleAnnotationPreservesSyncingPhase(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	now := metav1.Now()
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "true",
				controllers.AnnotationLastPvcSnapshot:       now.Add(-25 * time.Hour).UTC().Format(timeLayout),
			},
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency:           "24h",
			DisableWhileSyncing: ptr.To(true),
		}}},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeSyncing,
			PvcSize:      "1Gi",
			LatestHeight: 1,
		},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode).
		Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, "false", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.Equal(t, appsv1.PhaseChainNodeSyncing, stored.Status.Phase)
	assert.Equal(t, appsv1.PhaseChainNodeSyncing, chainNode.Status.Phase)

	snapshots := &snapshotv1.VolumeSnapshotList{}
	require.NoError(t, controllerClient.List(context.Background(), snapshots, client.InNamespace(chainNode.Namespace)))
	assert.Empty(t, snapshots.Items, "clearing stale state must not bypass disableWhileSyncing")
}

func TestListNodeSnapshotsScopesResultsToNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	labels := map[string]string{controllers.LabelChainNode: "node"}
	controllerClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(
		&snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "local", Labels: labels}},
		&snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "foreign", Labels: labels}},
	).Build()
	reconciler := &Reconciler{Client: controllerClient}
	chainNode := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "local"}}

	snapshots, err := reconciler.listNodeSnapshots(context.Background(), chainNode)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "local", snapshots[0].Name)
}

func TestEnsureVolumeSnapshotsPreservesActiveStateWhenAnotherSnapshotCompletes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	now := metav1.Now()
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "true",
			},
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "24h"}}},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeSnapshotting,
			PvcSize:      "1Gi",
			LatestHeight: 1,
		},
	}
	ready := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-ready",
			Namespace:         "default",
			CreationTimestamp: now,
			Labels:            map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations:       map[string]string{controllers.AnnotationPvcSnapshotReady: "false"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
	pending := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-pending",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(time.Minute)),
			Labels:            map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations:       map[string]string{controllers.AnnotationPvcSnapshotReady: "false"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(false)},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, ready, pending).
		Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.Equal(t, appsv1.PhaseChainNodeSnapshotting, stored.Status.Phase)
}

func TestEnsureVolumeSnapshotsRepairsTimestampFromAlreadyAnnotatedReadySnapshot(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	completedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "true",
			},
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "24h"}}},
		Status: appsv1.ChainNodeStatus{
			Phase:        appsv1.PhaseChainNodeSnapshotting,
			PvcSize:      "1Gi",
			LatestHeight: 1,
		},
	}
	ready := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-ready",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(completedAt),
			Labels:            map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations:       map[string]string{controllers.AnnotationPvcSnapshotReady: "true"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, ready).
		Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, completedAt.Format(timeLayout), stored.Annotations[controllers.AnnotationLastPvcSnapshot])
	assert.Equal(t, "false", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])

	snapshots := &snapshotv1.VolumeSnapshotList{}
	require.NoError(t, controllerClient.List(context.Background(), snapshots, client.InNamespace(chainNode.Namespace)))
	assert.Len(t, snapshots.Items, 1, "repairing the completion timestamp must prevent an immediate replacement snapshot")
}

func TestEnsureVolumeSnapshotsKeepsNewestReadySnapshotTimestamp(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	olderAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	newerAt := olderAt.Add(time.Hour)
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Spec:       appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "24h"}}},
		Status:     appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeRunning, PvcSize: "1Gi", LatestHeight: 1},
	}
	olderReady := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-older", Namespace: "default", CreationTimestamp: metav1.NewTime(olderAt),
			Labels:      map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "false"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
	newerReady := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-newer", Namespace: "default", CreationTimestamp: metav1.NewTime(newerAt),
			Labels:      map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "true"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, olderReady, newerReady).
		Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, newerAt.Format(timeLayout), stored.Annotations[controllers.AnnotationLastPvcSnapshot])
}

func TestEnsureVolumeSnapshotsDoesNotRewriteSecondPrecisionTimestamp(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	completedAt := time.Date(2026, time.August, 1, 19, 14, 25, 987654321, time.UTC)
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "false",
				controllers.AnnotationLastPvcSnapshot:       completedAt.Format(timeLayout),
			},
		},
		Spec:   appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "24h"}}},
		Status: appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeRunning, PvcSize: "1Gi", LatestHeight: 1},
	}
	ready := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-ready", Namespace: "default", CreationTimestamp: metav1.NewTime(completedAt),
			Labels:      map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "true"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
	baseClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, ready).
		Build()
	countingClient := &countingUpdateClient{Client: baseClient}
	reconciler := &Reconciler{Client: countingClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))
	assert.Zero(t, countingClient.updates, "matching second-resolution timestamps must not trigger a metadata write")
}

func TestEnsureVolumeSnapshotsRepairsSyncingPhaseForStopNodeSnapshot(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "false",
			},
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency: "24h",
			StopNode:  ptr.To(true),
		}}},
		Status: appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeSyncing, PvcSize: "1Gi", LatestHeight: 1},
	}
	pending := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-pending", Namespace: "default",
			Labels:      map[string]string{controllers.LabelChainNode: chainNode.Name},
			Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "false"},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(false)},
	}
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, pending).
		Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, "true", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
	assert.Equal(t, appsv1.PhaseChainNodeSnapshotting, stored.Status.Phase)
}

func TestEnsureVolumeSnapshotsClearsProgressWhenSnapshotsDisabled(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			Annotations: map[string]string{
				controllers.AnnotationPvcSnapshotInProgress: "true",
			},
		},
		Status: appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeSnapshotting},
	}
	controllerClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(chainNode).Build()
	reconciler := &Reconciler{Client: controllerClient, recorder: record.NewFakeRecorder(10)}

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), chainNode, true))

	stored := &appsv1.ChainNode{}
	require.NoError(t, controllerClient.Get(context.Background(), client.ObjectKeyFromObject(chainNode), stored))
	assert.Equal(t, "false", stored.Annotations[controllers.AnnotationPvcSnapshotInProgress])
}

type countingUpdateClient struct {
	client.Client
	updates int
}

func (c *countingUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates++
	return c.Client.Update(ctx, obj, opts...)
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
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
				Status:     appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeSyncing},
			},
			snapshotting: true,
			wantPhase:    appsv1.PhaseChainNodeSyncing,
			wantValue:    "true",
		},
		{
			name: "stop snapshotting",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
				Status:     appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeSnapshotting},
			},
			snapshotting: false,
			wantPhase:    appsv1.PhaseChainNodeSnapshotting,
			wantValue:    "false",
		},
		{
			name: "start with nil annotations",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Annotations: nil},
				Status:     appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeStopped},
			},
			snapshotting: true,
			wantPhase:    appsv1.PhaseChainNodeStopped,
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
