package datasnapshot

import (
	"context"
	"errors"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
)

func TestGCSCreateSnapshotAuthModes(t *testing.T) {
	tests := []struct {
		name                 string
		config               *appsv1.GcsExportConfig
		wantServiceAccount   string
		wantCredentialsEnv   bool
		wantCredentialsVol   bool
		wantCredentialsMount bool
	}{
		{
			name: "credentials secret",
			config: &appsv1.GcsExportConfig{
				Bucket: "snapshots",
				CredentialsSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-creds"},
					Key:                  "credentials.json",
				},
			},
			wantCredentialsEnv:   true,
			wantCredentialsVol:   true,
			wantCredentialsMount: true,
		},
		{
			name: "workload identity service account",
			config: &appsv1.GcsExportConfig{
				Bucket:             "snapshots",
				ServiceAccountName: ptrTo("snapshot-publisher"),
			},
			wantServiceAccount: "snapshot-publisher",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{
				Compression: ptr.To(appsv1.TarballCompression(dataexporter.CompressionZstd)),
				GCS:         tt.config,
			})
			vs := &snapshotv1.VolumeSnapshot{
				TypeMeta:   metav1.TypeMeta{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot"},
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default"},
				Status: &snapshotv1.VolumeSnapshotStatus{
					RestoreSize: resource.NewQuantity(1024, resource.BinarySI),
				},
			}

			err := provider.CreateSnapshot(context.Background(), "snapshot", vs)
			require.NoError(t, err)

			job := getJob(t, provider, "snapshot-upload")
			assert.Nil(t, job.Spec.TTLSecondsAfterFinished)
			podSpec := job.Spec.Template.Spec
			container := podSpec.Containers[0]
			assert.Equal(t, tt.wantServiceAccount, podSpec.ServiceAccountName)
			assert.Equal(t, tt.wantCredentialsVol, hasVolume(podSpec.Volumes, "credentials"))
			assert.Equal(t, tt.wantCredentialsMount, hasVolumeMount(container.VolumeMounts, "credentials"))
			assert.Equal(t, tt.wantCredentialsEnv, hasEnv(container.Env, "GOOGLE_APPLICATION_CREDENTIALS"))
			assert.Equal(t, "zstd", envValue(container.Env, "COMPRESSION"))
		})
	}
}

func TestGCSJobsUseConfiguredDataExporterImage(t *testing.T) {
	const image = "registry.example.com/dataexporter:custom"
	pullSecrets := []corev1.LocalObjectReference{{Name: "registry-creds"}}
	provider := newTestGCSProviderWithImage(t, &appsv1.ExportTarballConfig{
		GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"},
	}, image, pullSecrets)

	require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))
	uploadPod := getJob(t, provider, "snapshot-upload").Spec.Template.Spec
	assert.Equal(t, image, uploadPod.Containers[0].Image)
	assert.Equal(t, pullSecrets, uploadPod.ImagePullSecrets)

	_, err := provider.DeleteSnapshot(context.Background(), "snapshot")
	require.NoError(t, err)
	deletePod := getJob(t, provider, "snapshot-delete").Spec.Template.Spec
	assert.Equal(t, image, deletePod.Containers[0].Image)
	assert.Equal(t, pullSecrets, deletePod.ImagePullSecrets)
}

func TestGCSDeleteSnapshotAuthModes(t *testing.T) {
	tests := []struct {
		name                 string
		config               *appsv1.GcsExportConfig
		wantServiceAccount   string
		wantCredentialsEnv   bool
		wantCredentialsVol   bool
		wantCredentialsMount bool
	}{
		{
			name: "credentials secret",
			config: &appsv1.GcsExportConfig{
				Bucket: "snapshots",
				CredentialsSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-creds"},
					Key:                  "credentials.json",
				},
			},
			wantCredentialsEnv:   true,
			wantCredentialsVol:   true,
			wantCredentialsMount: true,
		},
		{
			name: "workload identity service account",
			config: &appsv1.GcsExportConfig{
				Bucket:             "snapshots",
				ServiceAccountName: ptrTo("snapshot-publisher"),
			},
			wantServiceAccount: "snapshot-publisher",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: tt.config})

			status, err := provider.DeleteSnapshot(context.Background(), "snapshot")
			require.NoError(t, err)
			assert.Equal(t, SnapshotActive, status)

			job := getJob(t, provider, "snapshot-delete")
			assert.Nil(t, job.Spec.TTLSecondsAfterFinished)
			require.NotNil(t, job.Spec.BackoffLimit)
			assert.Equal(t, unboundSnapshotDeleteBackoffLimit, *job.Spec.BackoffLimit)
			podSpec := job.Spec.Template.Spec
			container := podSpec.Containers[0]
			assert.Equal(t, tt.wantServiceAccount, podSpec.ServiceAccountName)
			assert.Equal(t, tt.wantCredentialsVol, hasVolume(podSpec.Volumes, "credentials"))
			assert.Equal(t, tt.wantCredentialsMount, hasVolumeMount(container.VolumeMounts, "credentials"))
			assert.Equal(t, tt.wantCredentialsEnv, hasEnv(container.Env, "GOOGLE_APPLICATION_CREDENTIALS"))
			assert.Equal(t, "/app", container.WorkingDir)
		})
	}
}

func TestGCSDestinationIdentityIncludesAuthenticationReference(t *testing.T) {
	withFirstKey := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
		Bucket: "snapshots",
		CredentialsSecret: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-creds"},
			Key:                  "first.json",
		},
	}})
	withSecondKey := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
		Bucket: "snapshots",
		CredentialsSecret: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-creds"},
			Key:                  "second.json",
		},
	}})
	withServiceAccount := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
		Bucket:             "snapshots",
		ServiceAccountName: ptr.To("snapshot-publisher"),
	}})

	assert.NotEqual(t, withFirstKey.destinationLabel(), withSecondKey.destinationLabel())
	assert.NotEqual(t, withFirstKey.destinationLabel(), withServiceAccount.destinationLabel())
}

func TestGCSDeleteSnapshotReportsTerminalStatus(t *testing.T) {
	tests := []struct {
		name       string
		jobStatus  batchv1.JobStatus
		wantStatus SnapshotStatus
	}{
		{
			name: "failed",
			jobStatus: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			}}},
			wantStatus: SnapshotFailed,
		},
		{
			name: "succeeded",
			jobStatus: batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}}},
			wantStatus: SnapshotSucceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
			status, err := provider.DeleteSnapshot(context.Background(), "snapshot")
			require.NoError(t, err)
			assert.Equal(t, SnapshotActive, status)

			job := getJob(t, provider, "snapshot-delete")
			job.Status = tt.jobStatus
			_, err = provider.Client.BatchV1().Jobs("default").Update(context.Background(), job, metav1.UpdateOptions{})
			require.NoError(t, err)

			status, err = provider.DeleteSnapshot(context.Background(), "snapshot")
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, status)
			_ = getJob(t, provider, "snapshot-delete")
		})
	}
}

func TestGCSCleanupSnapshotDeletionUsesForegroundUIDPrecondition(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-delete",
			Namespace:       "default",
			UID:             "gcs-delete-uid",
			Labels:          map[string]string{labelExporter: gcsExporter, labelOwner: provider.Owner.GetName(), labelType: typeDelete},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceToObject(provider.Owner)},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		}}},
	}
	_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), job, metav1.CreateOptions{})
	require.NoError(t, err)
	client := provider.Client.(*fake.Clientset)
	actionCount := len(client.Actions())

	require.NoError(t, provider.CleanupSnapshotDeletion(context.Background(), SnapshotJob{
		Name: "snapshot", UID: job.UID, Purpose: SnapshotJobDelete,
	}))

	deleteAction := requireJobDeleteAction(t, client.Actions()[actionCount:])
	assert.Equal(t, job.Name, deleteAction.GetName())
	options := deleteAction.GetDeleteOptions()
	require.NotNil(t, options.PropagationPolicy)
	assert.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)
	require.NotNil(t, options.Preconditions)
	require.NotNil(t, options.Preconditions.UID)
	assert.Equal(t, job.UID, *options.Preconditions.UID)
}

func TestGCSDeleteSnapshotForUploadReturnsPairedActivatedDeletion(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	uploadJob, uploadPVC := testOrphanUploadResources(provider.Owner, gcsExporter)
	_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), uploadJob, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), uploadPVC, metav1.CreateOptions{})
	require.NoError(t, err)
	client := provider.Client.(*fake.Clientset)
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		require.True(t, ok)
		job, ok := createAction.GetObject().(*batchv1.Job)
		require.True(t, ok)
		if job.Labels[labelType] == typeDelete {
			job.UID = "delete-uid"
		}
		return false, nil, nil
	})

	deletion, status, err := provider.DeleteSnapshotForUpload(context.Background(), SnapshotJob{
		Name: "snapshot", UID: uploadJob.UID, Purpose: SnapshotJobUpload,
	})
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
	assert.Equal(t, SnapshotJob{
		Name: "snapshot", UID: "delete-uid", Purpose: SnapshotJobDelete,
		Upload: &SnapshotJobIdentity{UID: uploadJob.UID, PVCUID: uploadPVC.UID},
	}, deletion)

	deleteJob := getJob(t, provider, "snapshot-delete")
	require.NotNil(t, deleteJob.Spec.Suspend)
	assert.False(t, *deleteJob.Spec.Suspend)
	require.NotNil(t, deleteJob.Spec.BackoffLimit)
	assert.Equal(t, unboundSnapshotDeleteBackoffLimit, *deleteJob.Spec.BackoffLimit)
	assert.Equal(t, gcsExporter, deleteJob.Labels[labelCleanupExporter])
	assert.Equal(t, provider.Owner.GetName(), deleteJob.Labels[labelCleanupOwner])
	assert.Equal(t, typeUpload, deleteJob.Labels[labelCleanupType])
	assert.Equal(t, string(uploadJob.UID), deleteJob.Labels[labelCleanupUploadUID])
	assert.Equal(t, string(uploadPVC.UID), deleteJob.Labels[labelCleanupPVCUID])
}

func TestGCSListSnapshotsDistinguishesDeletionJobs(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	status, err := provider.DeleteSnapshot(context.Background(), "snapshot")
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)

	jobs, err := provider.ListSnapshots(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []SnapshotJob{{Name: "snapshot", Purpose: SnapshotJobDelete}}, jobs)
}

func TestGCSResumesS3DeletionWorkflowAfterProviderSwitch(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	uploadJob, uploadPVC := testOrphanUploadResources(provider.Owner, s3Exporter)
	deleteJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snapshot-delete",
			Namespace: "default",
			UID:       "s3-delete-uid",
			Labels: map[string]string{
				labelExporter:         s3Exporter,
				labelOwner:            provider.Owner.GetName(),
				labelType:             typeDelete,
				labelCleanupExporter:  s3Exporter,
				labelCleanupOwner:     provider.Owner.GetName(),
				labelCleanupType:      typeUpload,
				labelCleanupUploadUID: string(uploadJob.UID),
				labelCleanupPVCUID:    string(uploadPVC.UID),
			},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceToObject(provider.Owner)},
		},
		Spec: batchv1.JobSpec{Suspend: ptr.To(true)},
	}
	_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), uploadJob, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), uploadPVC, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = provider.Client.BatchV1().Jobs("default").Create(context.Background(), deleteJob, metav1.CreateOptions{})
	require.NoError(t, err)

	jobs, err := provider.ListSnapshots(context.Background())
	require.NoError(t, err)
	require.Equal(t, []SnapshotJob{{
		Name: "snapshot", UID: deleteJob.UID, Purpose: SnapshotJobDelete, Exporter: s3Exporter,
		Upload: &SnapshotJobIdentity{UID: uploadJob.UID, PVCUID: uploadPVC.UID},
	}}, jobs)

	status, err := provider.GetSnapshotDeletionStatus(context.Background(), jobs[0])
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
	activated := getJob(t, provider, deleteJob.Name)
	require.NotNil(t, activated.Spec.Suspend)
	assert.False(t, *activated.Spec.Suspend)
	assert.Equal(t, s3Exporter, activated.Labels[labelExporter])
	_, err = provider.Client.BatchV1().Jobs("default").Get(context.Background(), uploadJob.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), uploadPVC.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestGCSListSnapshotsPreservesDeleteSuffixInArchiveName(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot-delete", testVolumeSnapshot()))

	jobs, err := provider.ListSnapshots(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []SnapshotJob{{Name: "snapshot-delete", Purpose: SnapshotJobUpload}}, jobs)
}

func TestGCSDeleteSnapshotBoundedReplacesExistingLegacyBackoffJob(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	status, err := provider.DeleteSnapshot(context.Background(), "snapshot")
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
	legacy, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Get(context.Background(), "snapshot-delete", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, legacy.Spec.BackoffLimit)
	assert.Equal(t, unboundSnapshotDeleteBackoffLimit, *legacy.Spec.BackoffLimit)

	status, err = provider.DeleteSnapshotBounded(context.Background(), "snapshot")
	assert.Empty(t, status)
	require.ErrorIs(t, err, ErrStaleJobReplaced)
	var replacement *StaleJobReplacedError
	require.ErrorAs(t, err, &replacement)
	assert.Equal(t, "spec.backoffLimit", replacement.ConflictingLabel)
	assert.Equal(t, "5", replacement.PreviousValue)
	assert.Equal(t, "0", replacement.DesiredValue)

	_, getErr := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Get(context.Background(), "snapshot-delete", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr))
}

func TestGCSGetSnapshotDeletionStatusReplacesPreUpgradeBackoffJob(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	status, err := provider.DeleteSnapshot(context.Background(), "snapshot")
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
	job := getJob(t, provider, "snapshot-delete")
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, unboundSnapshotDeleteBackoffLimit, *job.Spec.BackoffLimit)

	status, err = provider.GetSnapshotDeletionStatus(context.Background(), SnapshotJob{
		Name: "snapshot", UID: job.UID, Purpose: SnapshotJobDelete, RequireDestinationIdentity: true,
	})
	assert.Empty(t, status)
	require.ErrorIs(t, err, ErrStaleJobReplaced)
	var replacement *StaleJobReplacedError
	require.ErrorAs(t, err, &replacement)
	assert.Equal(t, "spec.backoffLimit", replacement.ConflictingLabel)
	assert.Equal(t, "5", replacement.PreviousValue)
	assert.Equal(t, "0", replacement.DesiredValue)

	_, getErr := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Get(
		context.Background(), job.Name, metav1.GetOptions{},
	)
	assert.True(t, apierrors.IsNotFound(getErr))
}

func TestGCSGetSnapshotDeletionStatusDoesNotCreateMissingJob(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})

	status, err := provider.GetSnapshotDeletionStatus(context.Background(), SnapshotJob{
		Name: "snapshot", Purpose: SnapshotJobDelete,
	})
	require.NoError(t, err)
	assert.Equal(t, SnapshotNotFound, status)
	for _, action := range provider.Client.(*fake.Clientset).Actions() {
		assert.False(t, action.GetVerb() == "create" && action.GetResource().Resource == "jobs")
	}
}

func TestGCSGetSnapshotStatusPreservesUploadResources(t *testing.T) {
	tests := []struct {
		name       string
		createJob  bool
		jobStatus  batchv1.JobStatus
		wantStatus SnapshotStatus
	}{
		{
			name:      "failed job",
			createJob: true,
			jobStatus: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			}}},
			wantStatus: SnapshotFailed,
		},
		{
			name:      "successful job",
			createJob: true,
			jobStatus: batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}}},
			wantStatus: SnapshotSucceeded,
		},
		{name: "missing job", wantStatus: SnapshotNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
				Bucket:             "snapshots",
				ServiceAccountName: ptr.To("snapshot-exporter"),
			}})
			if tt.createJob {
				job := provider.uploadJob("snapshot")
				job.OwnerReferences = []metav1.OwnerReference{ownerReferenceToObject(provider.Owner)}
				job.Status = tt.jobStatus
				_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), job, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			_, err := provider.Client.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			status, err := provider.GetSnapshotStatus(context.Background(), "snapshot")
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, status)

			_, err = provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			if tt.createJob {
				require.NoError(t, err)
			} else {
				assert.True(t, apierrors.IsNotFound(err))
			}
			_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestGCSCleanupSnapshotDeletesUploadResources(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))

	require.NoError(t, provider.CleanupSnapshot(context.Background(), "snapshot"))

	_, err := provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestGCSCreateSnapshotIsIdempotent(t *testing.T) {
	tests := []struct {
		name      string
		deletePVC bool
	}{
		{name: "existing job and PVC"},
		{name: "existing job with missing PVC", deletePVC: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
			require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))
			if tt.deletePVC {
				require.NoError(t, provider.Client.CoreV1().PersistentVolumeClaims("default").Delete(context.Background(), "snapshot-upload", metav1.DeleteOptions{}))
			}

			require.NoError(t, provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot()))
			_, err := provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestGCSCreateSnapshotRejectsForeignPVCWithoutDeletingIt(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	_, err := provider.Client.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	err = provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot())
	require.ErrorContains(t, err, "not controlled by upload job")

	_, err = provider.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestGCSCreateSnapshotRejectsForeignJob(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{Bucket: "snapshots"}})
	_, err := provider.Client.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-upload", Namespace: "default"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	err = provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot())
	require.ErrorContains(t, err, "not controlled by snapshot owner")

	_, err = provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestGCSCreateSnapshotCleansJobWhenPVCCreationFails(t *testing.T) {
	provider := newTestGCSProvider(t, &appsv1.ExportTarballConfig{GCS: &appsv1.GcsExportConfig{
		Bucket:             "snapshots",
		ServiceAccountName: ptr.To("snapshot-exporter"),
	}})
	client := provider.Client.(*fake.Clientset)
	client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("PVC create failed")
	})

	err := provider.CreateSnapshot(context.Background(), "snapshot", testVolumeSnapshot())
	require.ErrorContains(t, err, "PVC create failed")

	_, err = provider.Client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func newTestGCSProvider(t *testing.T, cfg *appsv1.ExportTarballConfig) *GCS {
	return newTestGCSProviderWithImage(t, cfg, "ghcr.io/voluzi/dataexporter:test", nil)
}

func newTestGCSProviderWithImage(t *testing.T, cfg *appsv1.ExportTarballConfig, image string, pullSecrets []corev1.LocalObjectReference) *GCS {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))

	owner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"},
	}

	return NewGcsSnapshotProvider(fake.NewSimpleClientset(), scheme, owner, "", image, pullSecrets, cfg).(*GCS)
}

func getJob(t *testing.T, provider *GCS, name string) *batchv1.Job {
	t.Helper()

	job, err := provider.Client.BatchV1().Jobs(provider.Owner.GetNamespace()).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return job
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, volume := range volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

func hasVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	for _, mount := range mounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

func hasEnv(envs []corev1.EnvVar, name string) bool {
	for _, env := range envs {
		if env.Name == name {
			return true
		}
	}
	return false
}

func ptrTo[T any](v T) *T {
	return &v
}
