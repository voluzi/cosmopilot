package chainnode

import (
	"context"
	"crypto/sha256"
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
	"k8s.io/apimachinery/pkg/types"
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

func TestNewSnapshotExportStatusCapturesDestinationAndSafeAuthenticationReferences(t *testing.T) {
	snapshot := destinationTestSnapshot()
	tests := []struct {
		name     string
		export   *appsv1.ExportTarballConfig
		provider appsv1.SnapshotExportProvider
		assert   func(*testing.T, appsv1.SnapshotExportStatus)
	}{
		{
			name: "S3",
			export: &appsv1.ExportTarballConfig{
				Compression: ptr.To(appsv1.TarballCompression("zstd")),
				S3: &appsv1.S3ExportConfig{
					Bucket:            "old-s3",
					Region:            "eu-west-1",
					Endpoint:          ptr.To("https://minio.example.com"),
					ForcePathStyle:    ptr.To(true),
					CredentialsSecret: &corev1.LocalObjectReference{Name: "old-aws"},
				},
			},
			provider: appsv1.SnapshotExportProviderS3,
			assert: func(t *testing.T, status appsv1.SnapshotExportStatus) {
				assert.Equal(t, "old-s3", status.Destination.Bucket)
				assert.Equal(t, "eu-west-1", status.Destination.Region)
				assert.Equal(t, "https://minio.example.com", status.Destination.Endpoint)
				assert.True(t, status.Destination.ForcePathStyle)
				require.NotNil(t, status.Destination.CredentialsSecret)
				assert.Equal(t, "old-aws", status.Destination.CredentialsSecret.Name)
				assert.Empty(t, status.Destination.CredentialsSecret.Key)
			},
		},
		{
			name: "GCS",
			export: &appsv1.ExportTarballConfig{
				GCS: &appsv1.GcsExportConfig{
					Bucket: "old-gcs",
					CredentialsSecret: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "old-gcp"},
						Key:                  "credentials.json",
					},
				},
			},
			provider: appsv1.SnapshotExportProviderGCS,
			assert: func(t *testing.T, status appsv1.SnapshotExportStatus) {
				assert.Equal(t, "old-gcs", status.Destination.Bucket)
				require.NotNil(t, status.Destination.CredentialsSecret)
				assert.Equal(t, "old-gcp", status.Destination.CredentialsSecret.Name)
				assert.Equal(t, "credentials.json", status.Destination.CredentialsSecret.Key)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := destinationTestChainNode(tt.export)
			status, err := newSnapshotExportStatus(node, snapshot)
			require.NoError(t, err)
			assert.NotEmpty(t, status.ID)
			assert.Equal(t, snapshot.Name, status.SnapshotName)
			assert.Equal(t, snapshot.UID, status.SnapshotUID)
			assert.Equal(t, getTarballName(node, snapshot), status.ObjectName)
			assert.Equal(t, appsv1.SnapshotExportPhaseUploading, status.Phase)
			assert.Equal(t, tt.provider, status.Destination.Provider)
			tt.assert(t, status)
		})
	}
}

func TestTarballProviderForExportUsesRecordedDestinationAfterConfigurationChanges(t *testing.T) {
	tests := []struct {
		name         string
		current      *appsv1.ExportTarballConfig
		recorded     appsv1.SnapshotExportDestination
		wantProvider string
		wantBucket   string
	}{
		{
			name:         "S3 to GCS",
			current:      &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "new-gcs"}},
			recorded:     appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderS3, Bucket: "old-s3", Region: "eu-west-1"},
			wantProvider: "s3",
			wantBucket:   "old-s3",
		},
		{
			name:         "GCS to S3",
			current:      &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "new-s3", Region: "us-east-1"}},
			recorded:     appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-gcs"},
			wantProvider: "gcs",
			wantBucket:   "old-gcs",
		},
		{
			name:         "S3 bucket change",
			current:      &appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "new-s3", Region: "eu-west-1"}},
			recorded:     appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderS3, Bucket: "old-s3", Region: "eu-west-1"},
			wantProvider: "s3",
			wantBucket:   "old-s3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := destinationTestChainNode(tt.current)
			reconciler := destinationTestReconciler(t, node, nil)
			export := &appsv1.SnapshotExportStatus{Destination: tt.recorded}
			provider, err := reconciler.tarballProviderForExport(node, export)
			require.NoError(t, err)
			switch tt.wantProvider {
			case "s3":
				actual, ok := provider.(*datasnapshot.S3)
				require.True(t, ok)
				assert.Equal(t, tt.wantBucket, actual.Config.Bucket)
			case "gcs":
				actual, ok := provider.(*datasnapshot.GCS)
				require.True(t, ok)
				assert.Equal(t, tt.wantBucket, actual.Config.Bucket)
			}
		})
	}
}

func TestRecordedDestinationDeletionSurvivesControllerRestart(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	retention := "1h"
	current := &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
		Bucket:             "new-gcs",
		ServiceAccountName: ptr.To("new-gcs-sa"),
	}, DeleteOnExpire: ptr.To(true)}
	node := destinationTestChainNode(current)
	node.CreationTimestamp = metav1.NewTime(now)
	node.Spec.Persistence.Snapshots.Retention = &retention
	node.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
	snapshot := destinationTestSnapshot()
	snapshot.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Hour))
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady:  "true",
		controllers.AnnotationExportingTarball:  tarballFinished,
		controllers.AnnotationSnapshotRetention: retention,
	}
	exportRecord := appsv1.SnapshotExportStatus{
		ID:             "export-1",
		SnapshotName:   snapshot.Name,
		SnapshotUID:    snapshot.UID,
		ObjectName:     fmt.Sprintf("chain-1-%s-original", snapshot.CreationTimestamp.UTC().Format(timeLayout)),
		Phase:          appsv1.SnapshotExportPhaseUploaded,
		DeleteOnExpire: true,
		Destination: appsv1.SnapshotExportDestination{
			Provider:          appsv1.SnapshotExportProviderS3,
			Bucket:            "old-s3",
			Region:            "eu-west-1",
			CredentialsSecret: &appsv1.SnapshotExportSecretReference{Name: "old-aws"},
		},
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{exportRecord}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "old-aws", Namespace: node.Namespace},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("access"),
			"AWS_SECRET_ACCESS_KEY": []byte("secret"),
		},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "new-gcs-sa", Namespace: node.Namespace}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot, secret, sa})

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	job, err := reconciler.snapshotKubernetesClient().BatchV1().Jobs(node.Namespace).Get(context.Background(), exportRecord.ObjectName+"-delete", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"s3", "delete", "old-s3", exportRecord.ObjectName}, job.Spec.Template.Spec.Containers[0].Args)
	assert.Equal(t, "old-aws", job.Spec.Template.Spec.Containers[0].EnvFrom[0].SecretRef.Name)
	assert.NotEmpty(t, job.Labels["destination"])

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, err = reconciler.snapshotKubernetesClient().BatchV1().Jobs(node.Namespace).UpdateStatus(context.Background(), job, metav1.UpdateOptions{})
	require.NoError(t, err)

	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	restarted := &Reconciler{
		Client:            reconciler.Client,
		snapshotClientSet: reconciler.snapshotClientSet,
		Scheme:            reconciler.Scheme,
		opts:              reconciler.opts,
		recorder:          record.NewFakeRecorder(10),
	}
	require.NoError(t, restarted.ensureVolumeSnapshots(context.Background(), storedNode, true))
	storedSnapshot := &snapshotv1.VolumeSnapshot{}
	err = restarted.Get(context.Background(), types.NamespacedName{Name: snapshot.Name, Namespace: snapshot.Namespace}, storedSnapshot)
	assert.True(t, apierrors.IsNotFound(err))
	require.NoError(t, restarted.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	assert.Empty(t, storedNode.Status.SnapshotExports)
}

func TestRecordedDestinationDeletionIgnoresVolumeSnapshotDeletionAnnotations(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "current-gcs"}})
	snapshot := destinationTestSnapshot()
	snapshot.Annotations = map[string]string{
		controllers.AnnotationTarballDeletionComplete: "true",
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID:           "recorded-export",
		SnapshotName: snapshot.Name,
		SnapshotUID:  snapshot.UID,
		ObjectName:   "pinned-object",
		Phase:        appsv1.SnapshotExportPhaseUploaded,
		Destination: appsv1.SnapshotExportDestination{
			Provider:           appsv1.SnapshotExportProviderS3,
			Bucket:             "pinned-bucket",
			Region:             "eu-west-1",
			ServiceAccountName: "pinned-s3-sa",
		},
	}}
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "pinned-s3-sa", Namespace: node.Namespace}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot, serviceAccount})

	deleted, err := reconciler.isTarballDeleted(context.Background(), node, snapshot)
	require.NoError(t, err)
	assert.False(t, deleted)
	job, err := reconciler.snapshotKubernetesClient().BatchV1().Jobs(node.Namespace).Get(
		context.Background(), "pinned-object-delete", metav1.GetOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"s3", "delete", "pinned-bucket", "pinned-object"}, job.Spec.Template.Spec.Containers[0].Args)
}

func TestExportTarballDoesNotFallbackWhenChainNodeDisappears(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	snapshot := destinationTestSnapshot()
	restoreSize := resource.MustParse("1Gi")
	snapshot.Status.RestoreSize = &restoreSize

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	controllerClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).Build()
	clientSet := fake.NewSimpleClientset()
	reconciler := &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: clientSet,
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(10),
	}

	err := reconciler.exportTarball(context.Background(), node, snapshot)
	assert.True(t, apierrors.IsNotFound(err))
	jobs, listErr := clientSet.BatchV1().Jobs(node.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, jobs.Items)
}

func TestRemovedRecordedCredentialsRequireExplicitCleanup(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	retention := "1h"
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{
		DeleteOnExpire: ptr.To(true),
		GCS:            &appsv1.GcsExportConfig{Bucket: "new-gcs", ServiceAccountName: ptr.To("new-gcs-sa")},
	})
	node.CreationTimestamp = metav1.NewTime(now)
	node.Spec.Persistence.Snapshots.Retention = &retention
	node.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
	snapshot := destinationTestSnapshot()
	snapshot.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Hour))
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady:  "true",
		controllers.AnnotationExportingTarball:  tarballFinished,
		controllers.AnnotationSnapshotRetention: retention,
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID:             "missing-creds",
		SnapshotName:   snapshot.Name,
		SnapshotUID:    snapshot.UID,
		ObjectName:     "old-object",
		Phase:          appsv1.SnapshotExportPhaseUploaded,
		DeleteOnExpire: true,
		Destination: appsv1.SnapshotExportDestination{
			Provider: appsv1.SnapshotExportProviderS3,
			Bucket:   "old-s3",
			Region:   "eu-west-1",
			CredentialsSecret: &appsv1.SnapshotExportSecretReference{
				Name: "removed-aws",
			},
		},
	}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "new-gcs-sa", Namespace: node.Namespace}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot, sa})

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	jobs, err := reconciler.snapshotKubernetesClient().BatchV1().Jobs(node.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobs.Items)
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, stored.Status.SnapshotExports[0].Phase)
	assert.Contains(t, stored.Status.SnapshotExports[0].Message, "removed-aws")
	condition := findCondition(stored.Status.Conditions, appsv1.ConditionSnapshotExportCleanup)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Contains(t, condition.Message, "old-s3")
	assert.Contains(t, condition.Message, "old-object")
	event := <-reconciler.recorder.(*record.FakeRecorder).Events
	assert.Contains(t, event, appsv1.ReasonTarballCleanupRequired)
	assert.Contains(t, event, "old-s3")

	storedSnapshot := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), storedSnapshot))
	assert.NotEqual(t, "true", storedSnapshot.Annotations[controllers.AnnotationTarballDeletionComplete])

	if stored.Annotations == nil {
		stored.Annotations = make(map[string]string)
	}
	stored.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement] = "missing-creds"
	require.NoError(t, reconciler.Update(context.Background(), stored))
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), stored, true))
	err = reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), storedSnapshot)
	assert.True(t, apierrors.IsNotFound(err))
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	assert.Empty(t, stored.Status.SnapshotExports)
	_, annotationPresent := stored.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement]
	assert.False(t, annotationPresent)
	events := drainRecordedEvents(reconciler.recorder.(*record.FakeRecorder))
	assert.Contains(t, strings.Join(events, "\n"), appsv1.ReasonTarballCleanupAcknowledged)
	assert.NotContains(t, strings.Join(events, "\n"), appsv1.ReasonTarballDeleted)
}

func TestSnapshotExportAcknowledgementIsConsumedBeforeItCanAuthorizeFutureCleanup(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	node.Annotations = map[string]string{controllers.AnnotationSnapshotExportCleanupAcknowledgement: "future-export"}
	reconciler := destinationTestReconciler(t, node, nil)

	require.NoError(t, reconciler.reconcileSnapshotExportAcknowledgements(context.Background(), node))
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	_, annotationPresent := stored.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement]
	assert.False(t, annotationPresent)

	stored.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID:           "future-export",
		SnapshotName: "snapshot",
		ObjectName:   "object",
		Phase:        appsv1.SnapshotExportPhaseCleanupRequired,
		Destination:  appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown},
	}}
	require.NoError(t, reconciler.Status().Update(context.Background(), stored))
	require.NoError(t, reconciler.reconcileSnapshotExportAcknowledgements(context.Background(), stored))
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, stored.Status.SnapshotExports[0].Phase)
}

func TestSnapshotExportAcknowledgementStatusFailurePreservesAnnotation(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	node.Annotations = map[string]string{controllers.AnnotationSnapshotExportCleanupAcknowledgement: "cleanup"}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID:           "cleanup",
		SnapshotName: "snapshot",
		ObjectName:   "object",
		Phase:        appsv1.SnapshotExportPhaseCleanupRequired,
		Destination:  appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown},
	}}
	reconciler := destinationTestReconciler(t, node, nil)
	baseClient := reconciler.Client
	reconciler.Client = &statusUpdateFailingClient{Client: baseClient, err: errors.New("status update failed")}

	err := reconciler.reconcileSnapshotExportAcknowledgements(context.Background(), node)
	require.ErrorContains(t, err, "status update failed")
	stored := &appsv1.ChainNode{}
	require.NoError(t, baseClient.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	assert.Equal(t, "cleanup", stored.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement])
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, stored.Status.SnapshotExports[0].Phase)
}

func TestSnapshotExportAcknowledgementPatchFailureRetriesMetadataConsumption(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	node.Annotations = map[string]string{controllers.AnnotationSnapshotExportCleanupAcknowledgement: "cleanup"}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID:           "cleanup",
		SnapshotName: "snapshot",
		ObjectName:   "object",
		Phase:        appsv1.SnapshotExportPhaseCleanupRequired,
		Destination:  appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown},
	}}
	reconciler := destinationTestReconciler(t, node, nil)
	baseClient := reconciler.Client
	reconciler.Client = &patchFailingClient{Client: baseClient, err: errors.New("metadata patch failed")}

	err := reconciler.reconcileSnapshotExportAcknowledgements(context.Background(), node)
	require.ErrorContains(t, err, "metadata patch failed")
	stored := &appsv1.ChainNode{}
	require.NoError(t, baseClient.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	assert.Equal(t, "cleanup", stored.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement])
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportPhaseAcknowledged, stored.Status.SnapshotExports[0].Phase)

	reconciler.Client = baseClient
	require.NoError(t, reconciler.reconcileSnapshotExportAcknowledgements(context.Background(), stored))
	require.NoError(t, baseClient.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	_, annotationPresent := stored.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement]
	assert.False(t, annotationPresent)
	assert.Equal(t, appsv1.SnapshotExportPhaseAcknowledged, stored.Status.SnapshotExports[0].Phase)
}

func TestCleanupRequiredWaitsForFailedDeleteJobRemovalBeforeRetrying(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "current", Region: "eu-west-1"}})
	snapshot := destinationTestSnapshot()
	export := appsv1.SnapshotExportStatus{
		ID:           "retry-export",
		SnapshotName: snapshot.Name,
		SnapshotUID:  snapshot.UID,
		ObjectName:   "recorded-object",
		Phase:        appsv1.SnapshotExportPhaseCleanupRequired,
		Message:      "failed delete requires cleanup",
		Destination: appsv1.SnapshotExportDestination{
			Provider: appsv1.SnapshotExportProviderS3,
			Bucket:   "recorded-bucket",
			Region:   "eu-west-1",
			CredentialsSecret: &appsv1.SnapshotExportSecretReference{
				Name: "aws-creds",
			},
		},
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{export}
	syncSnapshotExportCleanupCondition(node)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-creds", Namespace: node.Namespace},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("access"),
			"AWS_SECRET_ACCESS_KEY": []byte("secret"),
		},
	}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot, secret})
	provider, err := reconciler.tarballProviderForExport(node, &export)
	require.NoError(t, err)
	_, err = provider.DeleteSnapshot(context.Background(), export.ObjectName)
	require.NoError(t, err)
	clientSet := reconciler.snapshotKubernetesClient().(*fake.Clientset)
	job, err := clientSet.BatchV1().Jobs(node.Namespace).Get(context.Background(), export.ObjectName+"-delete", metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	_, err = clientSet.BatchV1().Jobs(node.Namespace).UpdateStatus(context.Background(), job, metav1.UpdateOptions{})
	require.NoError(t, err)
	clientSet.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		storedJob, getErr := clientSet.Tracker().Get(batchv1.SchemeGroupVersion.WithResource("jobs"), node.Namespace, job.Name)
		if getErr != nil {
			return true, nil, getErr
		}
		terminating := storedJob.(*batchv1.Job).DeepCopy()
		deletionTimestamp := metav1.Now()
		terminating.DeletionTimestamp = &deletionTimestamp
		return true, nil, clientSet.Tracker().Update(batchv1.SchemeGroupVersion.WithResource("jobs"), terminating, node.Namespace)
	})

	deleted, err := reconciler.isTarballDeleted(context.Background(), node, snapshot)
	assert.False(t, deleted)
	require.ErrorIs(t, err, datasnapshot.ErrStaleJobTerminating)
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	require.Len(t, storedNode.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, storedNode.Status.SnapshotExports[0].Phase)
	condition := findCondition(storedNode.Status.Conditions, appsv1.ConditionSnapshotExportCleanup)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
}

func TestCleanupRequiredSnapshotDoesNotBlockUnrelatedCountRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	retain := int32(1)
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{
		DeleteOnExpire: ptr.To(true),
		S3:             &appsv1.S3ExportConfig{Bucket: "snapshots", Region: "eu-west-1"},
	})
	node.CreationTimestamp = metav1.NewTime(now)
	node.Spec.Persistence.Snapshots.Retain = &retain
	node.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)

	snapshots := []*snapshotv1.VolumeSnapshot{
		destinationTestSnapshot(),
		destinationTestSnapshot(),
		destinationTestSnapshot(),
	}
	for i, snapshot := range snapshots {
		snapshot.Name = fmt.Sprintf("snapshot-%d", i)
		snapshot.UID = types.UID(fmt.Sprintf("snapshot-%d-uid", i))
		snapshot.CreationTimestamp = metav1.NewTime(now.Add(time.Duration(i-3) * time.Hour))
		snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
		snapshot.Annotations = map[string]string{
			controllers.AnnotationPvcSnapshotReady: "true",
			controllers.AnnotationExportingTarball: tarballFinished,
		}
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{
		{
			ID:             "blocked",
			SnapshotName:   snapshots[0].Name,
			SnapshotUID:    snapshots[0].UID,
			ObjectName:     "blocked-object",
			Phase:          appsv1.SnapshotExportPhaseCleanupRequired,
			DeleteOnExpire: true,
			Message:        "operator cleanup required",
			Destination: appsv1.SnapshotExportDestination{
				Provider: appsv1.SnapshotExportProviderS3,
				Bucket:   "old-bucket",
				Region:   "eu-west-1",
				CredentialsSecret: &appsv1.SnapshotExportSecretReference{
					Name: "missing-creds",
				},
			},
		},
		{
			ID:           "acknowledged",
			SnapshotName: snapshots[1].Name,
			SnapshotUID:  snapshots[1].UID,
			ObjectName:   "acknowledged-object",
			Phase:        appsv1.SnapshotExportPhaseAcknowledged,
			Destination:  appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown},
		},
	}
	syncSnapshotExportCleanupCondition(node)
	objects := []client.Object{snapshots[0], snapshots[1], snapshots[2]}
	reconciler := destinationTestReconciler(t, node, objects)

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	oldest := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshots[0]), oldest))
	acknowledged := &snapshotv1.VolumeSnapshot{}
	err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshots[1]), acknowledged)
	assert.True(t, apierrors.IsNotFound(err))
	newest := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshots[2]), newest))
}

func TestSnapshotExportReferenceValidationFailsClosedWithoutKubernetesClient(t *testing.T) {
	reconciler := &Reconciler{}
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "bucket", Region: "region"}})
	export := &appsv1.SnapshotExportStatus{
		ID:         "export",
		ObjectName: "object",
		Destination: appsv1.SnapshotExportDestination{
			Provider: appsv1.SnapshotExportProviderS3,
			Bucket:   "bucket",
			Region:   "region",
		},
	}

	err := reconciler.validateSnapshotExportReferences(context.Background(), node, export)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Kubernetes client")
}

func TestSnapshotExportReferenceValidationRequiresS3CredentialKeys(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "bucket", Region: "region"}})
	export := &appsv1.SnapshotExportStatus{
		ID:         "export",
		ObjectName: "object",
		Destination: appsv1.SnapshotExportDestination{
			Provider: appsv1.SnapshotExportProviderS3,
			Bucket:   "bucket",
			Region:   "region",
			CredentialsSecret: &appsv1.SnapshotExportSecretReference{
				Name: "aws-creds",
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-creds", Namespace: node.Namespace},
		Data:       map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("access")},
	}
	reconciler := destinationTestReconciler(t, node, []client.Object{secret})

	err := reconciler.validateSnapshotExportReferences(context.Background(), node, export)
	var unavailable *snapshotExportReferenceUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Contains(t, err.Error(), "AWS_SECRET_ACCESS_KEY")
}

func TestSnapshotExportStatusMutationPreservesCallerSpec(t *testing.T) {
	caller := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "caller-bucket"}})
	caller.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID:           "export",
		SnapshotName: "snapshot",
		ObjectName:   "object",
		Phase:        appsv1.SnapshotExportPhaseUploading,
		Destination:  appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "recorded"},
	}}
	stored := caller.DeepCopy()
	stored.Spec.Persistence = nil
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(stored).
		Build()
	reconciler := &Reconciler{Client: controllerClient}

	_, err := reconciler.setSnapshotExportPhase(context.Background(), caller, "export", appsv1.SnapshotExportPhaseUploaded, "")
	require.NoError(t, err)
	require.NotNil(t, caller.Spec.Persistence)
	require.NotNil(t, caller.Spec.Persistence.Snapshots)
	assert.Equal(t, "caller-bucket", caller.Spec.Persistence.Snapshots.ExportTarball.GCS.Bucket)
}

func TestSnapshotExportStatusMutationPreservesUnrelatedPendingStatus(t *testing.T) {
	caller := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	caller.Status.LatestHeight = 99
	caller.Status.Conditions = []metav1.Condition{{
		Type:    "PendingUnrelatedCondition",
		Status:  metav1.ConditionTrue,
		Reason:  "Pending",
		Message: "not persisted yet",
	}}
	caller.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID:           "export",
		SnapshotName: "snapshot",
		ObjectName:   "object",
		Phase:        appsv1.SnapshotExportPhaseUploading,
		Destination:  appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "snapshots"},
	}}
	stored := caller.DeepCopy()
	stored.Status.LatestHeight = 1
	stored.Status.Conditions = nil
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(stored).
		Build()
	reconciler := &Reconciler{Client: controllerClient}

	_, err := reconciler.setSnapshotExportPhase(context.Background(), caller, "export", appsv1.SnapshotExportPhaseUploaded, "")
	require.NoError(t, err)
	assert.Equal(t, int64(99), caller.Status.LatestHeight)
	require.Len(t, caller.Status.Conditions, 1)
	assert.Equal(t, "PendingUnrelatedCondition", caller.Status.Conditions[0].Type)
	assert.Equal(t, appsv1.SnapshotExportPhaseUploaded, caller.Status.SnapshotExports[0].Phase)
}

func TestExportTarballPersistsDestinationBeforeJobCreation(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{
		Bucket:            "durable-bucket",
		Region:            "eu-west-1",
		CredentialsSecret: &corev1.LocalObjectReference{Name: "aws-creds"},
	}})
	snapshot := destinationTestSnapshot()
	restoreSize := resource.MustParse("1Gi")
	snapshot.Status.RestoreSize = &restoreSize
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "aws-creds", Namespace: node.Namespace}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot, secret})
	reconciler.snapshotClientSet.(*fake.Clientset).PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated job create failure")
	})

	err := reconciler.exportTarball(context.Background(), node, snapshot)
	require.ErrorContains(t, err, "simulated job create failure")
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportPhaseUploading, stored.Status.SnapshotExports[0].Phase)
	assert.Equal(t, "durable-bucket", stored.Status.SnapshotExports[0].Destination.Bucket)
	assert.Equal(t, "aws-creds", stored.Status.SnapshotExports[0].Destination.CredentialsSecret.Name)
}

func TestVolumeSnapshotMetadataCannotRedirectRecordedDeletion(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "current"}})
	snapshot := destinationTestSnapshot()
	snapshot.Annotations = map[string]string{
		"cosmopilot.voluzi.com/tarball-destination": `{"provider":"s3","bucket":"victim","endpoint":"https://attacker.example","object":"victim"}`,
	}
	export := &appsv1.SnapshotExportStatus{
		ObjectName: "recorded-object",
		Destination: appsv1.SnapshotExportDestination{
			Provider: appsv1.SnapshotExportProviderGCS,
			Bucket:   "recorded-bucket",
		},
	}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})
	provider, err := reconciler.tarballProviderForExport(node, export)
	require.NoError(t, err)
	gcs, ok := provider.(*datasnapshot.GCS)
	require.True(t, ok)
	assert.Equal(t, "recorded-bucket", gcs.Config.Bucket)
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func drainRecordedEvents(recorder *record.FakeRecorder) []string {
	events := make([]string, 0)
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func TestPruneSnapshotExportsRemovesAbsentTerminalDeleteRecords(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{
		{ID: "deleted", SnapshotName: "gone", SnapshotUID: "gone-uid", ObjectName: "deleted-object", Phase: appsv1.SnapshotExportPhaseDeleted, DeleteOnExpire: true},
		{ID: "acknowledged", SnapshotName: "also-gone", SnapshotUID: "also-gone-uid", ObjectName: "acknowledged-object", Phase: appsv1.SnapshotExportPhaseAcknowledged, DeleteOnExpire: true},
		{ID: "deleting", SnapshotName: "pending", SnapshotUID: "pending-uid", ObjectName: "pending-object", Phase: appsv1.SnapshotExportPhaseDeleting, DeleteOnExpire: true},
	}
	reconciler := destinationTestReconciler(t, node, nil)

	require.NoError(t, reconciler.pruneRetainedSnapshotExports(context.Background(), node, nil))
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, "deleting", stored.Status.SnapshotExports[0].ID)
}

func TestAcknowledgedExportCompletesWhenSnapshotsAreDisabled(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	node.Spec.Persistence.Snapshots = nil
	node.Annotations = map[string]string{controllers.AnnotationSnapshotExportCleanupAcknowledgement: "cleanup"}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "cleanup", SnapshotName: "snapshot", SnapshotUID: "snapshot-uid", ObjectName: "recorded-object",
		Phase:       appsv1.SnapshotExportPhaseCleanupRequired,
		Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-bucket"},
	}}
	reconciler := destinationTestReconciler(t, node, nil)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "recorded-object-delete", Namespace: node.Namespace, UID: "job-uid",
		Labels:          map[string]string{"exporter": "gcs-exporter", "owner": node.Name, "type": "delete"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: node.APIVersion, Kind: node.Kind, Name: node.Name, UID: node.UID, Controller: ptr.To(true)}},
	}}
	reconciler.snapshotClientSet = fake.NewSimpleClientset(job)

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	_, err := reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	assert.Empty(t, stored.Status.SnapshotExports)
	_, present := stored.Annotations[controllers.AnnotationSnapshotExportCleanupAcknowledgement]
	assert.False(t, present)
}

func TestPendingDeletionSuccessRemovesAcknowledgedExport(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	snapshot := destinationTestSnapshot()
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "cleanup", SnapshotName: snapshot.Name, SnapshotUID: snapshot.UID, ObjectName: "recorded-object",
		Phase:       appsv1.SnapshotExportPhaseAcknowledged,
		Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-bucket"},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})

	require.NoError(t, reconciler.persistPendingTarballDeletionSuccess(context.Background(), node, "recorded-object"))
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	assert.Empty(t, stored.Status.SnapshotExports)
}

func destinationTestChainNode(export *appsv1.ExportTarballConfig) *appsv1.ChainNode {
	return &appsv1.ChainNode{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node",
			Namespace: "default",
			UID:       "node-uid",
		},
		Spec: appsv1.ChainNodeSpec{Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{
			Frequency:     "240h",
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

func destinationTestSnapshot() *snapshotv1.VolumeSnapshot {
	return &snapshotv1.VolumeSnapshot{
		TypeMeta: metav1.TypeMeta{APIVersion: snapshotv1.SchemeGroupVersion.String(), Kind: "VolumeSnapshot"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "snapshot",
			Namespace:         "default",
			UID:               "snapshot-uid",
			CreationTimestamp: metav1.NewTime(time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)),
		},
		Status: &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)},
	}
}

func destinationTestReconciler(t *testing.T, node *appsv1.ChainNode, objects []client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	allObjects := append([]client.Object{node}, objects...)
	controllerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(allObjects...).
		Build()
	clientSetObjects := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		switch object := object.(type) {
		case *corev1.Secret:
			clientSetObjects = append(clientSetObjects, object.DeepCopy())
		case *corev1.ServiceAccount:
			clientSetObjects = append(clientSetObjects, object.DeepCopy())
		}
	}
	return &Reconciler{
		Client:            controllerClient,
		snapshotClientSet: fake.NewSimpleClientset(clientSetObjects...),
		Scheme:            scheme,
		opts:              &controllers.ControllerRunOptions{},
		recorder:          record.NewFakeRecorder(20),
	}
}

type snapshotDeleteFailingClient struct {
	client.Client
	err error
}

func (failing *snapshotDeleteFailingClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	if _, ok := object.(*snapshotv1.VolumeSnapshot); ok {
		return failing.err
	}
	return failing.Client.Delete(ctx, object, opts...)
}

type statusUpdateFailingClient struct {
	client.Client
	err error
}

func (failing *statusUpdateFailingClient) Status() client.SubResourceWriter {
	return &statusUpdateFailingWriter{SubResourceWriter: failing.Client.Status(), err: failing.err}
}

type statusUpdateFailingWriter struct {
	client.SubResourceWriter
	err error
}

func (failing *statusUpdateFailingWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return failing.err
}

type patchFailingClient struct {
	client.Client
	err error
}

func (failing *patchFailingClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return failing.err
}

func TestIsTarballReadyRequiresAcknowledgementForLegacyInFlightExport(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "current-bucket"}})
	snapshot := destinationTestSnapshot()
	snapshot.Annotations = map[string]string{controllers.AnnotationExportingTarball: strconv.FormatBool(true)}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})

	ready, err := reconciler.isTarballReady(context.Background(), node, snapshot)
	require.NoError(t, err)
	assert.False(t, ready)
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportProviderUnknown, stored.Status.SnapshotExports[0].Destination.Provider)
	assert.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, stored.Status.SnapshotExports[0].Phase)
	jobs, listErr := reconciler.snapshotKubernetesClient().BatchV1().Jobs(node.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, jobs.Items)
}

func TestEnsureVolumeSnapshotsDefersOrphanCleanupForUnknownLegacyUpload(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{
		Suffix: ptr.To("new"),
		GCS:    &appsv1.GcsExportConfig{Bucket: "new-bucket"},
	})
	snapshot := destinationTestSnapshot()
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady: "true",
		controllers.AnnotationExportingTarball: "true",
	}
	legacyJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "chain-1-20260801T000000-old-upload", Namespace: node.Namespace, UID: "legacy-upload-uid",
		Labels: map[string]string{"exporter": "gcs-exporter", "owner": node.Name, "type": "upload"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: node.APIVersion, Kind: node.Kind, Name: node.Name, UID: node.UID, Controller: ptr.To(true),
		}},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})
	reconciler.snapshotClientSet = fake.NewSimpleClientset(legacyJob)

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	_, err := reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Get(context.Background(), legacyJob.Name, metav1.GetOptions{})
	require.NoError(t, err, "unknown legacy upload must not be treated as orphaned")
	stored := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), stored))
	require.Len(t, stored.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportProviderUnknown, stored.Status.SnapshotExports[0].Destination.Provider)
}

func TestEnsureVolumeSnapshotsFinishesRecordedUploadAfterExportRemoval(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "old-bucket"}})
	snapshot := destinationTestSnapshot()
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady: "true",
		controllers.AnnotationExportingTarball: "true",
	}
	restoreSize := resource.MustParse("1Gi")
	snapshot.Status.RestoreSize = &restoreSize
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})
	require.NoError(t, reconciler.exportTarball(context.Background(), node, snapshot))
	require.Len(t, node.Status.SnapshotExports, 1)
	export := node.Status.SnapshotExports[0]
	job, err := reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Get(context.Background(), export.ObjectName+"-upload", metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, err = reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).UpdateStatus(context.Background(), job, metav1.UpdateOptions{})
	require.NoError(t, err)
	storedBeforeRemoval := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedBeforeRemoval))
	storedBeforeRemoval.Spec.Persistence.Snapshots.ExportTarball = nil
	require.NoError(t, reconciler.Update(context.Background(), storedBeforeRemoval))
	node = storedBeforeRemoval

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	storedSnapshot := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), storedSnapshot))
	assert.Equal(t, tarballFinished, storedSnapshot.Annotations[controllers.AnnotationExportingTarball])
	_, err = reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = reconciler.snapshotClientSet.CoreV1().PersistentVolumeClaims(node.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	require.Len(t, storedNode.Status.SnapshotExports, 1)
	assert.Equal(t, appsv1.SnapshotExportPhaseUploaded, storedNode.Status.SnapshotExports[0].Phase)
}

func TestEnsureVolumeSnapshotsFailsClosedForLegacyUploadAfterExportRemoval(t *testing.T) {
	node := destinationTestChainNode(nil)
	retention := "1h"
	node.Spec.Persistence.Snapshots.Retention = &retention
	node.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
	snapshot := destinationTestSnapshot()
	snapshot.CreationTimestamp = metav1.NewTime(time.Now().UTC().Add(-2 * time.Hour))
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady:  "true",
		controllers.AnnotationExportingTarball:  "true",
		controllers.AnnotationSnapshotRetention: retention,
	}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	storedSnapshot := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), storedSnapshot))
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	require.Len(t, storedNode.Status.SnapshotExports, 1)
	assert.True(t, storedNode.Status.SnapshotExports[0].DeleteOnExpire)
	assert.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, storedNode.Status.SnapshotExports[0].Phase)

	// Exercise retention directly after the legacy in-flight branch has recorded
	// the unknown destination. Without deletion proof or operator acknowledgement,
	// the expired VolumeSnapshot must remain present.
	storedSnapshot.Annotations[controllers.AnnotationExportingTarball] = tarballFinished
	require.NoError(t, reconciler.Update(context.Background(), storedSnapshot))
	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), storedNode, true))
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), &snapshotv1.VolumeSnapshot{}),
		"unknown remote cleanup must gate expired snapshot deletion")
}

func TestUnknownLegacyExportPreservesCleanupIntentWithoutGuessingObjectName(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{
		GCS:            &appsv1.GcsExportConfig{Bucket: "current-bucket"},
		Suffix:         ptr.To("new-suffix"),
		DeleteOnExpire: ptr.To(true),
	})
	snapshot := destinationTestSnapshot()
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})

	export, err := reconciler.ensureUnknownSnapshotExportStatus(context.Background(), node, snapshot)
	require.NoError(t, err)
	assert.Empty(t, export.ObjectName)
	assert.True(t, export.DeleteOnExpire)
	assert.NotContains(t, export.Message, "new-suffix")
}

func TestAcknowledgedUnknownInFlightExportUnblocksSnapshotLifecycle(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "current-bucket"}})
	snapshot := destinationTestSnapshot()
	snapshot.Annotations = map[string]string{controllers.AnnotationExportingTarball: strconv.FormatBool(true)}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "legacy", SnapshotName: snapshot.Name, SnapshotUID: snapshot.UID,
		Phase:       appsv1.SnapshotExportPhaseAcknowledged,
		Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})

	ready, err := reconciler.isTarballReady(context.Background(), node, snapshot)
	require.NoError(t, err)
	assert.False(t, ready)
	stored := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), stored))
	assert.Equal(t, tarballFinished, stored.Annotations[controllers.AnnotationExportingTarball])
}

func TestRecordedDeletionProofIsHonoredBeforeReferenceValidation(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "current-bucket"}})
	snapshot := destinationTestSnapshot()
	snapshot.Annotations = map[string]string{
		controllers.AnnotationTarballDeletionComplete: "true",
		controllers.AnnotationTarballDeletionName:     "recorded-object",
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "recorded", SnapshotName: snapshot.Name, SnapshotUID: snapshot.UID, ObjectName: "recorded-object",
		Phase: appsv1.SnapshotExportPhaseUploaded,
		Destination: appsv1.SnapshotExportDestination{
			Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-bucket",
			CredentialsSecret: &appsv1.SnapshotExportSecretReference{Name: "missing", Key: "credentials.json"},
		},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})

	deleted, err := reconciler.isTarballDeleted(context.Background(), node, snapshot)
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.Equal(t, appsv1.SnapshotExportPhaseDeleted, node.Status.SnapshotExports[0].Phase)
}

func TestAcknowledgedKnownExportCleansDeletionJobBeforeRemovingStatus(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "current-bucket"}})
	snapshot := destinationTestSnapshot()
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "recorded", SnapshotName: snapshot.Name, SnapshotUID: snapshot.UID, ObjectName: "recorded-object",
		Phase:       appsv1.SnapshotExportPhaseAcknowledged,
		Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-bucket"},
	}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "recorded-object-delete", Namespace: node.Namespace, UID: "job-uid",
		Labels:          map[string]string{"exporter": "gcs-exporter", "owner": node.Name, "type": "delete"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: node.APIVersion, Kind: node.Kind, Name: node.Name, UID: node.UID, Controller: ptr.To(true)}},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})
	reconciler.snapshotClientSet = fake.NewSimpleClientset(job)

	require.NoError(t, reconciler.cleanUpTarballDeletion(context.Background(), node, snapshot))
	_, err := reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	assert.Empty(t, node.Status.SnapshotExports)
}

func TestSnapshotExportDeletePolicyRemainsBoundToRecordedExport(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "old"}, DeleteOnExpire: ptr.To(true)})
	snapshot := destinationTestSnapshot()
	export, err := newSnapshotExportStatus(node, snapshot)
	require.NoError(t, err)
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{export}
	node.Spec.Persistence.Snapshots.ExportTarball = nil

	assert.True(t, shouldDeleteSnapshotTarballOnExpire(node, snapshot))
	node.Status.SnapshotExports[0].DeleteOnExpire = false
	assert.False(t, shouldDeleteSnapshotTarballOnExpire(node, snapshot))
}

func TestUploadingSnapshotExportCanConvergeToDeleted(t *testing.T) {
	assert.True(t, snapshotExportPhaseTransitionAllowed(appsv1.SnapshotExportPhaseUploading, appsv1.SnapshotExportPhaseDeleted))
}

func TestSnapshotExportAcknowledgementEventsOnlyReportNewTransitions(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	node.Annotations = map[string]string{controllers.AnnotationSnapshotExportCleanupAcknowledgement: "already,new"}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{
		{ID: "already", SnapshotName: "old", ObjectName: "old", Phase: appsv1.SnapshotExportPhaseAcknowledged, Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown}},
		{ID: "new", SnapshotName: "new", ObjectName: "new", Phase: appsv1.SnapshotExportPhaseCleanupRequired, Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown}},
	}
	reconciler := destinationTestReconciler(t, node, nil)

	require.NoError(t, reconciler.reconcileSnapshotExportAcknowledgements(context.Background(), node))
	events := strings.Join(drainRecordedEvents(reconciler.recorder.(*record.FakeRecorder)), "\n")
	assert.Contains(t, events, "cleanup new")
	assert.NotContains(t, events, "cleanup already")
}

func TestEnsureVolumeSnapshotsDefersOrphanCleanupForPersistedUnknownLegacyUpload(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{
		Suffix: ptr.To("new"),
		GCS:    &appsv1.GcsExportConfig{Bucket: "new-bucket"},
	})
	snapshot := destinationTestSnapshot()
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady: "true",
		controllers.AnnotationExportingTarball: "true",
	}
	legacyJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "chain-1-20260801T000000-old-upload", Namespace: node.Namespace, UID: "legacy-upload-uid",
		Labels: map[string]string{"exporter": "gcs-exporter", "owner": node.Name, "type": "upload"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: node.APIVersion, Kind: node.Kind, Name: node.Name, UID: node.UID, Controller: ptr.To(true),
		}},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})
	reconciler.snapshotClientSet = fake.NewSimpleClientset(legacyJob)

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	require.Len(t, node.Status.SnapshotExports, 1)
	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	_, err := reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Get(context.Background(), legacyJob.Name, metav1.GetOptions{})
	require.NoError(t, err, "persisted unknown export must continue suppressing orphan cleanup")
}

func TestEnsureVolumeSnapshotsRetriesRecordedUploadAfterExportRemoval(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "old-bucket"}})
	snapshot := destinationTestSnapshot()
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady: "true",
		controllers.AnnotationExportingTarball: "true",
	}
	restoreSize := resource.MustParse("1Gi")
	snapshot.Status.RestoreSize = &restoreSize
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})
	require.NoError(t, reconciler.exportTarball(context.Background(), node, snapshot))
	require.Len(t, node.Status.SnapshotExports, 1)
	export := node.Status.SnapshotExports[0]
	require.NoError(t, reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Delete(
		context.Background(), export.ObjectName+"-upload", metav1.DeleteOptions{},
	))
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	storedNode.Spec.Persistence.Snapshots.ExportTarball = nil
	require.NoError(t, reconciler.Update(context.Background(), storedNode))
	node = storedNode

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	storedSnapshot := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), storedSnapshot))
	assert.Empty(t, storedSnapshot.Annotations[controllers.AnnotationExportingTarball])

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	_, err := reconciler.snapshotClientSet.BatchV1().Jobs(node.Namespace).Get(
		context.Background(), export.ObjectName+"-upload", metav1.GetOptions{},
	)
	require.NoError(t, err, "recorded uploading export must retry with its persisted destination")
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), storedSnapshot))
	assert.Equal(t, strconv.FormatBool(true), storedSnapshot.Annotations[controllers.AnnotationExportingTarball])
}

func TestEnsureVolumeSnapshotsKeepsRetainedExportRecordWhenSnapshotDeleteFails(t *testing.T) {
	node := destinationTestChainNode(nil)
	retention := "1h"
	node.Spec.Persistence.Snapshots.Retention = &retention
	node.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
	snapshot := destinationTestSnapshot()
	snapshot.CreationTimestamp = metav1.NewTime(time.Now().UTC().Add(-2 * time.Hour))
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady:  "true",
		controllers.AnnotationExportingTarball:  tarballFinished,
		controllers.AnnotationSnapshotRetention: retention,
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "retained", SnapshotName: snapshot.Name, SnapshotUID: snapshot.UID, ObjectName: "retained-object",
		Phase: appsv1.SnapshotExportPhaseUploaded, DeleteOnExpire: false,
		Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-bucket"},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})
	deleteErr := errors.New("snapshot delete failed")
	reconciler.Client = &snapshotDeleteFailingClient{Client: reconciler.Client, err: deleteErr}

	err := reconciler.ensureVolumeSnapshots(context.Background(), node, true)
	require.ErrorIs(t, err, deleteErr)
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), &snapshotv1.VolumeSnapshot{}))
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	require.Len(t, storedNode.Status.SnapshotExports, 1, "failed snapshot deletion must preserve historical export identity")
	assert.Equal(t, "retained-object", storedNode.Status.SnapshotExports[0].ObjectName)
}

func TestEnsureVolumeSnapshotsKeepsRetainedExportRecordWhileSnapshotTerminates(t *testing.T) {
	node := destinationTestChainNode(nil)
	retention := "1h"
	node.Spec.Persistence.Snapshots.Retention = &retention
	node.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
	snapshot := destinationTestSnapshot()
	snapshot.CreationTimestamp = metav1.NewTime(time.Now().UTC().Add(-2 * time.Hour))
	snapshot.Finalizers = []string{"snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"}
	snapshot.Labels = map[string]string{controllers.LabelChainNode: node.Name}
	snapshot.Annotations = map[string]string{
		controllers.AnnotationPvcSnapshotReady:  "true",
		controllers.AnnotationExportingTarball:  tarballFinished,
		controllers.AnnotationSnapshotRetention: retention,
	}
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "retained", SnapshotName: snapshot.Name, SnapshotUID: snapshot.UID, ObjectName: "retained-object",
		Phase: appsv1.SnapshotExportPhaseUploaded, DeleteOnExpire: false,
		Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-bucket"},
	}}
	reconciler := destinationTestReconciler(t, node, []client.Object{snapshot})

	require.NoError(t, reconciler.ensureVolumeSnapshots(context.Background(), node, true))
	terminating := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(snapshot), terminating))
	require.NotNil(t, terminating.DeletionTimestamp)
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	require.Len(t, storedNode.Status.SnapshotExports, 1, "terminating snapshot must retain historical export identity")
	assert.Equal(t, "retained-object", storedNode.Status.SnapshotExports[0].ObjectName)
}

func TestPruneRetainedSnapshotExportsRetriesAfterSnapshotDeletion(t *testing.T) {
	node := destinationTestChainNode(nil)
	node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{
		ID: "retained", SnapshotName: "deleted-snapshot", SnapshotUID: "deleted-uid", ObjectName: "retained-object",
		Phase: appsv1.SnapshotExportPhaseUploaded, DeleteOnExpire: false,
		Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderGCS, Bucket: "old-bucket"},
	}}
	reconciler := destinationTestReconciler(t, node, nil)

	require.NoError(t, reconciler.pruneRetainedSnapshotExports(context.Background(), node, nil))
	storedNode := &appsv1.ChainNode{}
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(node), storedNode))
	assert.Empty(t, storedNode.Status.SnapshotExports)
}

func TestSnapshotExportStatusIDIsStableAndDNSLabelSafe(t *testing.T) {
	node := destinationTestChainNode(&appsv1.ExportTarballConfig{S3: &appsv1.S3ExportConfig{Bucket: "bucket", Region: "region"}})
	snapshot := destinationTestSnapshot()
	first, err := newSnapshotExportStatus(node, snapshot)
	require.NoError(t, err)
	second, err := newSnapshotExportStatus(node, snapshot)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.LessOrEqual(t, len(first.ID), 63)
	assert.Equal(t, first.ID, strings.ToLower(first.ID))
	assert.NotContains(t, first.ID, "/")
	assert.NotContains(t, first.ID, "_")
	digest := sha256.Sum256([]byte(strings.Join([]string{
		string(node.UID), snapshot.Name, string(snapshot.UID), first.ObjectName,
		string(first.Destination.Provider), first.Destination.Bucket, first.Destination.Endpoint,
	}, "\x00")))
	assert.Equal(t, fmt.Sprintf("export-%x", digest[:8]), first.ID)
}
