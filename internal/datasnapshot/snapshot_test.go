package datasnapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestSnapshotJobStatusUsesTerminalConditions(t *testing.T) {
	tests := []struct {
		name   string
		status batchv1.JobStatus
		want   SnapshotStatus
	}{
		{
			name:   "pod failure while job is retrying",
			status: batchv1.JobStatus{Failed: 1},
			want:   SnapshotActive,
		},
		{
			name: "job failed",
			status: batchv1.JobStatus{
				Failed: 1,
				Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				}},
			},
			want: SnapshotFailed,
		},
		{
			name: "job completed after a retry",
			status: batchv1.JobStatus{
				Failed:    1,
				Succeeded: 1,
				Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				}},
			},
			want: SnapshotSucceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, snapshotJobStatus(&batchv1.Job{Status: tt.status}))
		})
	}
}

func TestEnsureSnapshotJobReplacesJobWithUnconvergeableLabels(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-work",
			Namespace:       "default",
			UID:             "stale-uid",
			Labels:          map[string]string{labelExporter: "s3-exporter", labelOwner: "owner", labelType: typeDelete},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
	})

	desired := desiredDeleteJob(owner, "gcs-exporter")
	desired.Name = "snapshot-work"
	_, _, err := ensureSnapshotJob(context.Background(), client, owner, desired, "delete")
	require.ErrorIs(t, err, ErrStaleJobReplaced)
	var replacement *StaleJobReplacedError
	require.ErrorAs(t, err, &replacement)
	assert.Equal(t, typeDelete, replacement.Purpose)
	assert.ErrorContains(t, err, labelExporter)

	// The stale Job is gone, so the next reconcile creates the desired one instead of erroring forever.
	_, err = client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-work", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))

	job, created, err := ensureSnapshotJob(context.Background(), client, owner, desired, "delete")
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "gcs-exporter", job.Labels[labelExporter])
}

func TestEnsureSnapshotJobReportsActualNonExporterLabelMismatch(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "snapshot-work",
		Namespace: "default",
		UID:       "stale-uid",
		Labels: map[string]string{
			labelExporter: "gcs-exporter",
			labelOwner:    "previous-owner",
			labelType:     typeDelete,
		},
		OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
	}})
	desired := desiredDeleteJob(owner, "gcs-exporter")
	desired.Name = "snapshot-work"

	_, _, err := ensureSnapshotJob(context.Background(), client, owner, desired, typeDelete)
	require.ErrorIs(t, err, ErrStaleJobReplaced)
	var replacement *StaleJobReplacedError
	require.ErrorAs(t, err, &replacement)
	assert.Equal(t, typeDelete, replacement.Purpose)
	assert.Equal(t, labelOwner, replacement.ConflictingLabel)
	assert.Equal(t, "previous-owner", replacement.PreviousValue)
	assert.Equal(t, owner.Name, replacement.DesiredValue)
	assert.ErrorContains(t, err, labelOwner)
	assert.ErrorContains(t, err, "previous-owner")
	assert.ErrorContains(t, err, owner.Name)
}

func TestEnsureSnapshotJobWaitsForTerminatingOwnedJob(t *testing.T) {
	owner := testJobOwner()
	deletionTimestamp := metav1.Now()
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:              "snapshot-work",
		Namespace:         "default",
		UID:               "stale-uid",
		DeletionTimestamp: &deletionTimestamp,
		Labels:            map[string]string{labelExporter: "s3-exporter", labelOwner: owner.Name, labelType: typeDelete},
		OwnerReferences:   []metav1.OwnerReference{ownerReferenceTo(owner)},
	}})
	deleteCalls := 0
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		return false, nil, nil
	})
	desired := desiredDeleteJob(owner, "gcs-exporter")
	desired.Name = "snapshot-work"

	_, _, err := ensureSnapshotJob(context.Background(), client, owner, desired, typeDelete)
	require.ErrorIs(t, err, ErrStaleJobTerminating)
	assert.NotErrorIs(t, err, ErrStaleJobReplaced)
	var replacement *StaleJobReplacedError
	assert.False(t, errors.As(err, &replacement), "a terminating Job was not replaced during this call")
	assert.Zero(t, deleteCalls)
}

// TestUploadJobStatusRejectsOtherExportersJob covers the polling path. Upload Jobs are looked up by
// name, and the name does not encode the provider, so a Job left in flight by the previous exporter
// would otherwise be read as progress towards the newly configured one — and its success would mark the
// export finished with nothing written to the new destination.
func TestUploadJobStatusRejectsOtherExportersJob(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-work",
			Namespace:       "default",
			UID:             "stale-uid",
			Labels:          map[string]string{labelExporter: "s3-exporter", labelType: typeUpload},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
		// The old provider's Job completed successfully — the case that would be misreported.
		Status: batchv1.JobStatus{
			Succeeded:  1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		},
	})

	_, err := uploadJobStatus(context.Background(), client, owner, "default", "snapshot-work", "gcs-exporter")
	require.ErrorIs(t, err, ErrStaleJobReplaced)
	var replacement *StaleJobReplacedError
	require.ErrorAs(t, err, &replacement)
	assert.Equal(t, typeUpload, replacement.Purpose)
	assert.ErrorContains(t, err, "s3-exporter")

	// Removed, so the next reconcile starts a real upload against the configured provider.
	_, err = client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-work", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestUploadJobStatusWaitsForTerminatingOwnedJob(t *testing.T) {
	owner := testJobOwner()
	deletionTimestamp := metav1.Now()
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:              "snapshot-work",
		Namespace:         "default",
		UID:               "stale-uid",
		DeletionTimestamp: &deletionTimestamp,
		Labels:            map[string]string{labelExporter: "s3-exporter", labelType: typeUpload},
		OwnerReferences:   []metav1.OwnerReference{ownerReferenceTo(owner)},
	}})
	deleteCalls := 0
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		return false, nil, nil
	})

	_, err := uploadJobStatus(context.Background(), client, owner, "default", "snapshot-work", "gcs-exporter")
	require.ErrorIs(t, err, ErrStaleJobTerminating)
	assert.NotErrorIs(t, err, ErrStaleJobReplaced)
	var replacement *StaleJobReplacedError
	assert.False(t, errors.As(err, &replacement), "a terminating Job was not replaced during this call")
	assert.Zero(t, deleteCalls)
}

func TestUploadJobStatusDoesNotTreatCurrentExporterTerminationAsReplacement(t *testing.T) {
	owner := testJobOwner()
	deletionTimestamp := metav1.Now()
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:              "snapshot-work",
		Namespace:         "default",
		UID:               "current-uid",
		DeletionTimestamp: &deletionTimestamp,
		Labels:            map[string]string{labelExporter: "gcs-exporter", labelType: typeUpload},
		OwnerReferences:   []metav1.OwnerReference{ownerReferenceTo(owner)},
	}})

	status, err := uploadJobStatus(context.Background(), client, owner, "default", "snapshot-work", "gcs-exporter")
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
}

// TestUploadJobStatusNeverDeletesAnotherOwnersJob guards a cross-node hazard: tarball names derive from
// chain ID plus a second-resolution timestamp, so two ChainNodes in one namespace can collide on the
// Job name. Deleting on an exporter mismatch alone would let one node terminate another's live export.
func TestUploadJobStatusNeverDeletesAnotherOwnersJob(t *testing.T) {
	owner := testJobOwner()
	stranger := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "other-node", Namespace: "default", UID: "other-uid"},
	}
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "snapshot-upload",
			Namespace: "default",
			UID:       "other-job-uid",
			// A different exporter AND a different owner: the mismatch must not authorise a delete.
			Labels:          map[string]string{labelExporter: "s3-exporter", labelType: typeUpload},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(stranger)},
		},
	})

	_, err := uploadJobStatus(context.Background(), client, owner, "default", "snapshot-upload", "gcs-exporter")
	require.ErrorContains(t, err, "not controlled by snapshot owner")
	assert.NotErrorIs(t, err, ErrStaleJobReplaced)

	_, err = client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-upload", metav1.GetOptions{})
	require.NoError(t, err, "another owner's export job must survive")
}

func TestUploadJobStatusReportsDeleteFailureOfStaleJob(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-upload",
			Namespace:       "default",
			UID:             "stale-uid",
			Labels:          map[string]string{labelExporter: "s3-exporter", labelType: typeUpload},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
	})
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})

	// A failed delete must surface, not be masked as the sentinel: the caller would otherwise treat the
	// stale Job as replaced when it is still there.
	_, err := uploadJobStatus(context.Background(), client, owner, "default", "snapshot-upload", "gcs-exporter")
	require.ErrorContains(t, err, "delete refused")
	assert.NotErrorIs(t, err, ErrStaleJobReplaced)
}

func TestUploadJobStatusReportsMatchingExportersJob(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-upload",
			Namespace:       "default",
			Labels:          map[string]string{labelExporter: "gcs-exporter", labelType: typeUpload},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
		Status: batchv1.JobStatus{
			Succeeded:  1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		},
	})

	status, err := uploadJobStatus(context.Background(), client, owner, "default", "snapshot-upload", "gcs-exporter")
	require.NoError(t, err)
	assert.Equal(t, SnapshotSucceeded, status)
}

func TestUploadJobStatusReportsMissingJob(t *testing.T) {
	status, err := uploadJobStatus(context.Background(), fake.NewSimpleClientset(), testJobOwner(), "default", "snapshot-upload", "gcs-exporter")
	require.NoError(t, err)
	assert.Equal(t, SnapshotNotFound, status)
}

func TestReconcileSnapshotDeletionJobReportsMissingJobWithoutCreatingIt(t *testing.T) {
	client := fake.NewSimpleClientset()

	status, err := reconcileSnapshotDeletionJob(context.Background(), client, testJobOwner(), SnapshotJob{
		Name: "snapshot", Purpose: SnapshotJobDelete,
	}, gcsExporter)
	require.NoError(t, err)
	assert.Equal(t, SnapshotNotFound, status)
	for _, action := range client.Actions() {
		assert.False(t, action.GetVerb() == "create" && action.GetResource().Resource == "jobs")
	}
}

func TestSnapshotJobsFromJobsRetainsUploadIdentityAlongsidePreferredDeletionJob(t *testing.T) {
	deletionTimestamp := metav1.Now()
	jobs := []batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Name: "snapshot-delete", UID: "delete-uid", Labels: map[string]string{labelType: typeDelete}}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:              "snapshot-upload",
			UID:               "upload-uid",
			DeletionTimestamp: &deletionTimestamp,
			Labels:            map[string]string{labelType: typeUpload},
		}},
	}

	assert.Equal(t, []SnapshotJob{{
		Name:    "snapshot",
		UID:     "delete-uid",
		Purpose: SnapshotJobDelete,
		Upload:  &SnapshotJobIdentity{UID: "upload-uid", Terminating: true},
	}}, snapshotJobsFromJobs(jobs))
}

func TestSnapshotJobsFromJobsRetainsRecordedUploadIdentityAfterUploadJobIsGone(t *testing.T) {
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "snapshot-delete",
		UID:  "delete-uid",
		Labels: map[string]string{
			labelType:             typeDelete,
			labelCleanupUploadUID: "upload-uid",
			labelCleanupPVCUID:    "pvc-uid",
		},
	}}

	assert.Equal(t, []SnapshotJob{{
		Name:    "snapshot",
		UID:     "delete-uid",
		Purpose: SnapshotJobDelete,
		Upload:  &SnapshotJobIdentity{UID: "upload-uid", PVCUID: "pvc-uid"},
	}}, snapshotJobsFromJobs([]batchv1.Job{job}))
}

func TestSnapshotJobsFromJobsCarriesTerminatingUploadIdentity(t *testing.T) {
	deletionTimestamp := metav1.Now()
	jobs := []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{
		Name:              "snapshot-upload",
		UID:               "upload-uid",
		DeletionTimestamp: &deletionTimestamp,
		Labels:            map[string]string{labelType: typeUpload},
	}}}

	assert.Equal(t, []SnapshotJob{{
		Name:        "snapshot",
		UID:         "upload-uid",
		Purpose:     SnapshotJobUpload,
		Terminating: true,
	}}, snapshotJobsFromJobs(jobs))
}

func TestListSnapshotJobsIncludesPreviousExporterDeletionWorkflow(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name:      "previous-delete",
			Namespace: owner.Namespace,
			UID:       "previous-delete-uid",
			Labels: map[string]string{
				labelExporter:         s3Exporter,
				labelOwner:            owner.Name,
				labelType:             typeDelete,
				labelCleanupExporter:  s3Exporter,
				labelCleanupOwner:     owner.Name,
				labelCleanupType:      typeUpload,
				labelCleanupUploadUID: "previous-upload-uid",
				labelCleanupPVCUID:    "previous-pvc-uid",
			},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name:      "current-upload",
			Namespace: owner.Namespace,
			UID:       "current-upload-uid",
			Labels: map[string]string{
				labelExporter: gcsExporter,
				labelOwner:    owner.Name,
				labelType:     typeUpload,
			},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name:      "old-upload-only-upload",
			Namespace: owner.Namespace,
			UID:       "old-upload-only-uid",
			Labels: map[string]string{
				labelExporter: s3Exporter,
				labelOwner:    owner.Name,
				labelType:     typeUpload,
			},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		}},
	)

	jobs, err := listSnapshotJobs(context.Background(), client, owner, gcsExporter)
	require.NoError(t, err)
	assert.Equal(t, []SnapshotJob{
		{Name: "current", UID: "current-upload-uid", Purpose: SnapshotJobUpload},
		{Name: "old-upload-only", UID: "old-upload-only-uid", Purpose: SnapshotJobUpload, Exporter: s3Exporter},
		{
			Name: "previous", UID: "previous-delete-uid", Purpose: SnapshotJobDelete, Exporter: s3Exporter,
			Upload: &SnapshotJobIdentity{UID: "previous-upload-uid", PVCUID: "previous-pvc-uid"},
		},
	}, jobs)
}

func TestDeleteSnapshotForPreviousExporterUploadDerivesDeletionJob(t *testing.T) {
	owner := testJobOwner()
	upload, pvc := testOrphanUploadResources(owner, s3Exporter)
	upload.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:         "dataexporter",
		Image:        "dataexporter:test",
		Args:         []string{"s3", "upload", "data", "snapshots", "snapshot"},
		WorkingDir:   "/home/app",
		VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/home/app/data"}},
	}}
	upload.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "data"}}
	upload.Spec.Template.Labels = map[string]string{
		"application":                        "dataexporter",
		"controller-uid":                     "upload-uid",
		"job-name":                           upload.Name,
		"batch.kubernetes.io/controller-uid": "upload-uid",
		"batch.kubernetes.io/job-name":       upload.Name,
	}
	client := fake.NewSimpleClientset(upload, pvc)

	deletion, status, err := DeleteSnapshotForUpload(context.Background(), client, owner, SnapshotJob{
		Name: "snapshot", UID: upload.UID, Purpose: SnapshotJobUpload, Exporter: s3Exporter,
	})
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
	assert.Equal(t, s3Exporter, deletion.Exporter)

	job, err := client.BatchV1().Jobs(owner.Namespace).Get(context.Background(), "snapshot-delete", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, s3Exporter, job.Labels[labelExporter])
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, []string{"s3", "delete", "snapshots", "snapshot"}, job.Spec.Template.Spec.Containers[0].Args)
	assert.Equal(t, "/app", job.Spec.Template.Spec.Containers[0].WorkingDir)
	assert.Empty(t, job.Spec.Template.Spec.Containers[0].VolumeMounts)
	assert.Empty(t, job.Spec.Template.Spec.Volumes)
	assert.Equal(t, "dataexporter", job.Spec.Template.Labels["application"])
	assert.NotContains(t, job.Spec.Template.Labels, "controller-uid")
	assert.NotContains(t, job.Spec.Template.Labels, "job-name")
	assert.NotContains(t, job.Spec.Template.Labels, "batch.kubernetes.io/controller-uid")
	assert.NotContains(t, job.Spec.Template.Labels, "batch.kubernetes.io/job-name")
	require.NotNil(t, job.Spec.Suspend)
	assert.False(t, *job.Spec.Suspend)
}

func TestReconcileLegacyDeletionReplacesActiveUnpairedUploadWorkflow(t *testing.T) {
	owner := testJobOwner()
	deleteJob := desiredDeleteJob(owner, gcsExporter)
	deleteJob.UID = "delete-uid"
	uploadJob, _ := testOrphanUploadResources(owner, gcsExporter)
	client := fake.NewSimpleClientset(deleteJob, uploadJob)
	expected := snapshotJobsFromJobs([]batchv1.Job{*deleteJob, *uploadJob})[0]

	_, err := reconcileSnapshotDeletionJob(context.Background(), client, owner, expected, gcsExporter)
	require.ErrorIs(t, err, ErrStaleJobReplaced)
	_, getErr := client.BatchV1().Jobs(owner.Namespace).Get(context.Background(), deleteJob.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr))
}

func TestCleanupLegacyDeletionRetainsUnattributedDanglingUploadPVC(t *testing.T) {
	owner := testJobOwner()
	deleteJob := desiredDeleteJob(owner, gcsExporter)
	deleteJob.UID = "delete-uid"
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "snapshot-upload", Namespace: owner.Namespace, UID: "pvc-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: "snapshot-upload", UID: "upload-uid", Controller: ptr.To(true),
		}},
	}}
	client := fake.NewSimpleClientset(deleteJob, pvc)

	require.NoError(t, cleanupSnapshotDeletionResources(context.Background(), client, owner, SnapshotJob{
		Name: "snapshot", UID: deleteJob.UID, Purpose: SnapshotJobDelete,
	}, gcsExporter))
	storedPVC, err := client.CoreV1().PersistentVolumeClaims(owner.Namespace).Get(context.Background(), pvc.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, pvc.UID, storedPVC.UID)
}

func TestListSnapshotJobsIgnoresSameNamePreviousOwner(t *testing.T) {
	owner := testJobOwner()
	previousOwner := owner.DeepCopy()
	previousOwner.UID = "previous-owner-uid"
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "previous-delete",
		Namespace: owner.Namespace,
		UID:       "previous-delete-uid",
		Labels: map[string]string{
			labelExporter: s3Exporter,
			labelOwner:    owner.Name,
			labelType:     typeDelete,
		},
		OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(previousOwner)},
	}})

	jobs, err := listSnapshotJobs(context.Background(), client, owner, gcsExporter)
	require.NoError(t, err)
	assert.Empty(t, jobs)
}

func TestSnapshotJobsFromJobsDoesNotPairDifferentExporterUpload(t *testing.T) {
	jobs := []batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{
			Name: "snapshot-delete", UID: "delete-uid",
			Labels: map[string]string{labelExporter: s3Exporter, labelType: typeDelete},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name: "snapshot-upload", UID: "upload-uid",
			Labels: map[string]string{labelExporter: gcsExporter, labelType: typeUpload},
		}},
	}

	assert.Equal(t, []SnapshotJob{{
		Name: "snapshot", UID: "delete-uid", Purpose: SnapshotJobDelete, Exporter: s3Exporter,
	}}, snapshotJobsFromJobs(jobs, gcsExporter))
}

func TestGetSnapshotUploadResourcesRejectsChangedIdentityOrOwnership(t *testing.T) {
	owner := testJobOwner()
	foreignOwner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "default", UID: "foreign-owner-uid"},
	}
	tests := []struct {
		name      string
		mutateJob func(*batchv1.Job)
		mutatePVC func(*corev1.PersistentVolumeClaim)
		wantError string
	}{
		{
			name: "same-name replacement Job",
			mutateJob: func(job *batchv1.Job) {
				job.UID = "replacement-job-uid"
			},
			wantError: "UID",
		},
		{
			name: "foreign controller",
			mutateJob: func(job *batchv1.Job) {
				job.OwnerReferences = []metav1.OwnerReference{ownerReferenceTo(foreignOwner)}
			},
			wantError: "not controlled by snapshot owner",
		},
		{
			name: "changed provider",
			mutateJob: func(job *batchv1.Job) {
				job.Labels[labelExporter] = s3Exporter
			},
			wantError: "exporter label",
		},
		{
			name: "changed type",
			mutateJob: func(job *batchv1.Job) {
				job.Labels[labelType] = typeDelete
			},
			wantError: "type label",
		},
		{
			name: "same-name replacement PVC",
			mutatePVC: func(pvc *corev1.PersistentVolumeClaim) {
				pvc.UID = "replacement-pvc-uid"
				pvc.OwnerReferences[0].UID = "replacement-job-uid"
			},
			wantError: "not controlled by upload job",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:      "snapshot-upload",
				Namespace: "default",
				UID:       "listed-job-uid",
				Labels: map[string]string{
					labelExporter: gcsExporter,
					labelOwner:    owner.Name,
					labelType:     typeUpload,
				},
				OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
			}}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name:      job.Name,
				Namespace: job.Namespace,
				UID:       "listed-pvc-uid",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       "Job",
					Name:       job.Name,
					UID:        job.UID,
					Controller: ptr.To(true),
				}},
			}}
			if tt.mutateJob != nil {
				tt.mutateJob(job)
			}
			if tt.mutatePVC != nil {
				tt.mutatePVC(pvc)
			}
			client := fake.NewSimpleClientset(job, pvc)

			_, _, err := getSnapshotUploadResources(
				context.Background(),
				client,
				owner,
				"snapshot",
				SnapshotJobIdentity{UID: "listed-job-uid", PVCUID: "listed-pvc-uid"},
				gcsExporter,
			)
			require.ErrorContains(t, err, tt.wantError)
			for _, action := range client.Actions() {
				assert.NotEqual(t, "delete", action.GetVerb())
			}
		})
	}
}

func TestReconcileSnapshotDeletionJobRejectsChangedDeletionOrCleanupIdentity(t *testing.T) {
	owner := testJobOwner()
	foreignOwner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "default", UID: "foreign-owner-uid"},
	}
	tests := []struct {
		name      string
		mutate    func(*batchv1.Job)
		wantError string
	}{
		{name: "delete UID", mutate: func(job *batchv1.Job) { job.UID = "replacement-delete-uid" }, wantError: "UID"},
		{name: "delete owner", mutate: func(job *batchv1.Job) {
			job.OwnerReferences = []metav1.OwnerReference{ownerReferenceTo(foreignOwner)}
		}, wantError: "not controlled by snapshot owner"},
		{name: "delete provider", mutate: func(job *batchv1.Job) { job.Labels[labelExporter] = s3Exporter }, wantError: labelExporter},
		{name: "delete owner label", mutate: func(job *batchv1.Job) { job.Labels[labelOwner] = foreignOwner.Name }, wantError: labelOwner},
		{name: "delete type", mutate: func(job *batchv1.Job) { job.Labels[labelType] = typeUpload }, wantError: labelType},
		{name: "cleanup provider", mutate: func(job *batchv1.Job) { job.Labels[labelCleanupExporter] = s3Exporter }, wantError: labelCleanupExporter},
		{name: "cleanup owner", mutate: func(job *batchv1.Job) { job.Labels[labelCleanupOwner] = foreignOwner.Name }, wantError: labelCleanupOwner},
		{name: "cleanup type", mutate: func(job *batchv1.Job) { job.Labels[labelCleanupType] = typeDelete }, wantError: labelCleanupType},
		{name: "upload UID", mutate: func(job *batchv1.Job) { job.Labels[labelCleanupUploadUID] = "replacement-upload-uid" }, wantError: "records upload UID"},
		{name: "PVC UID", mutate: func(job *batchv1.Job) { job.Labels[labelCleanupPVCUID] = "replacement-pvc-uid" }, wantError: "records upload PVC UID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := desiredDeleteJob(owner, gcsExporter)
			job.UID = "listed-delete-uid"
			setSnapshotDeletionUploadIdentity(job, owner, gcsExporter, SnapshotJobIdentity{
				UID: "listed-upload-uid", PVCUID: "listed-pvc-uid",
			})
			expected := snapshotJobFromJob(job)
			tt.mutate(job)
			client := fake.NewSimpleClientset(job)

			_, err := reconcileSnapshotDeletionJob(context.Background(), client, owner, expected, gcsExporter)
			require.ErrorContains(t, err, tt.wantError)
			for _, action := range client.Actions() {
				assert.NotContains(t, []string{"delete", "update"}, action.GetVerb())
			}
		})
	}
}

func TestReconcileSnapshotDeletionJobKeepsSuspendedMarkerForSameNameUploadReplacement(t *testing.T) {
	owner := testJobOwner()
	deletionJob := desiredDeleteJob(owner, gcsExporter)
	deletionJob.UID = "delete-uid"
	setSnapshotDeletionUploadIdentity(deletionJob, owner, gcsExporter, SnapshotJobIdentity{
		UID: "original-upload-uid", PVCUID: "original-pvc-uid",
	})
	replacementJob, replacementPVC := testOrphanUploadResources(owner, gcsExporter)
	replacementJob.UID = "replacement-upload-uid"
	replacementPVC.UID = "replacement-pvc-uid"
	replacementPVC.OwnerReferences[0].UID = replacementJob.UID
	client := fake.NewSimpleClientset(deletionJob, replacementJob, replacementPVC)

	_, err := reconcileSnapshotDeletionJob(
		context.Background(), client, owner, snapshotJobFromJob(deletionJob), gcsExporter,
	)
	require.ErrorContains(t, err, "UID")

	storedDeletion, err := client.BatchV1().Jobs("default").Get(context.Background(), deletionJob.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, storedDeletion.Spec.Suspend)
	assert.True(t, *storedDeletion.Spec.Suspend)
	storedReplacement, err := client.BatchV1().Jobs("default").Get(context.Background(), replacementJob.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, replacementJob.UID, storedReplacement.UID)
	for _, action := range client.Actions() {
		assert.NotContains(t, []string{"delete", "update"}, action.GetVerb())
	}
}

func TestReconcileSnapshotDeletionJobRetriesActivationAfterCrash(t *testing.T) {
	owner := testJobOwner()
	deletionJob := desiredDeleteJob(owner, gcsExporter)
	deletionJob.UID = "delete-uid"
	setSnapshotDeletionUploadIdentity(deletionJob, owner, gcsExporter, SnapshotJobIdentity{UID: "upload-uid"})
	client := fake.NewSimpleClientset(deletionJob)
	crashErr := errors.New("controller crashed before activation was persisted")
	updateCalls := 0
	client.PrependReactor("update", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		if updateCalls == 1 {
			return true, nil, crashErr
		}
		return false, nil, nil
	})
	expected := snapshotJobFromJob(deletionJob)

	_, err := reconcileSnapshotDeletionJob(context.Background(), client, owner, expected, gcsExporter)
	require.ErrorIs(t, err, crashErr)
	stored, err := client.BatchV1().Jobs("default").Get(context.Background(), deletionJob.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, stored.Spec.Suspend)
	assert.True(t, *stored.Spec.Suspend)

	status, err := reconcileSnapshotDeletionJob(context.Background(), client, owner, expected, gcsExporter)
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
	stored, err = client.BatchV1().Jobs("default").Get(context.Background(), deletionJob.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, stored.Spec.Suspend)
	assert.False(t, *stored.Spec.Suspend)
	assert.Equal(t, 2, updateCalls)
}

func TestReconcileSnapshotDeletionJobRecoversWhenActivationResponseIsLost(t *testing.T) {
	owner := testJobOwner()
	deletionJob := desiredDeleteJob(owner, gcsExporter)
	deletionJob.UID = "delete-uid"
	setSnapshotDeletionUploadIdentity(deletionJob, owner, gcsExporter, SnapshotJobIdentity{UID: "upload-uid"})
	client := fake.NewSimpleClientset(deletionJob)
	crashErr := errors.New("controller crashed after activation was persisted")
	updateCalls := 0
	client.PrependReactor("update", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		if updateCalls != 1 {
			return false, nil, nil
		}
		updateAction, ok := action.(k8stesting.UpdateAction)
		require.True(t, ok)
		updated, ok := updateAction.GetObject().(*batchv1.Job)
		require.True(t, ok)
		require.NoError(t, client.Tracker().Update(
			batchv1.SchemeGroupVersion.WithResource("jobs"), updated.DeepCopy(), updated.Namespace,
		))
		return true, nil, crashErr
	})
	expected := snapshotJobFromJob(deletionJob)

	_, err := reconcileSnapshotDeletionJob(context.Background(), client, owner, expected, gcsExporter)
	require.ErrorIs(t, err, crashErr)
	stored, err := client.BatchV1().Jobs("default").Get(context.Background(), deletionJob.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, stored.Spec.Suspend)
	assert.False(t, *stored.Spec.Suspend)

	status, err := reconcileSnapshotDeletionJob(context.Background(), client, owner, expected, gcsExporter)
	require.NoError(t, err)
	assert.Equal(t, SnapshotActive, status)
	assert.Equal(t, 1, updateCalls, "the activated deletion Job must not be started again")
}

func TestCleanupSnapshotDeletionResourcesRejectsChangedDeletionIdentityBeforeUploadCleanup(t *testing.T) {
	owner := testJobOwner()
	foreignOwner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "default", UID: "foreign-owner-uid"},
	}
	tests := []struct {
		name      string
		mutate    func(*batchv1.Job)
		wantError string
	}{
		{
			name: "same-name replacement",
			mutate: func(job *batchv1.Job) {
				job.UID = "replacement-delete-uid"
			},
			wantError: "UID",
		},
		{
			name: "foreign replacement",
			mutate: func(job *batchv1.Job) {
				job.OwnerReferences = []metav1.OwnerReference{ownerReferenceTo(foreignOwner)}
			},
			wantError: "not controlled by snapshot owner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uploadJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:      "snapshot-upload",
				Namespace: "default",
				UID:       "upload-job-uid",
				Labels: map[string]string{
					labelExporter: gcsExporter,
					labelOwner:    owner.Name,
					labelType:     typeUpload,
				},
				OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
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
			deleteJob := desiredDeleteJob(owner, gcsExporter)
			deleteJob.UID = "listed-delete-uid"
			deleteJob.OwnerReferences = []metav1.OwnerReference{ownerReferenceTo(owner)}
			tt.mutate(deleteJob)
			client := fake.NewSimpleClientset(uploadJob, uploadPVC, deleteJob)

			err := cleanupSnapshotDeletionResources(context.Background(), client, owner, SnapshotJob{
				Name:    "snapshot",
				UID:     "listed-delete-uid",
				Purpose: SnapshotJobDelete,
				Upload:  &SnapshotJobIdentity{UID: uploadJob.UID},
			}, gcsExporter)
			require.ErrorContains(t, err, tt.wantError)
			for _, action := range client.Actions() {
				assert.NotEqual(t, "delete", action.GetVerb(), "upload resources must survive an unverified deletion Job")
			}
			_, err = client.BatchV1().Jobs("default").Get(context.Background(), uploadJob.Name, metav1.GetOptions{})
			require.NoError(t, err)
			_, err = client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), uploadPVC.Name, metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestEnsureSnapshotJobKeepsJobWithMatchingLabels(t *testing.T) {
	owner := testJobOwner()
	existing := desiredDeleteJob(owner, "s3-exporter")
	existing.UID = "existing-uid"
	existing.OwnerReferences = []metav1.OwnerReference{ownerReferenceTo(owner)}
	client := fake.NewSimpleClientset(existing)

	job, created, err := ensureSnapshotJob(context.Background(), client, owner, desiredDeleteJob(owner, "s3-exporter"), "delete")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, "existing-uid", string(job.UID))
}

func TestEnsureSnapshotJobReportsDeleteFailureOfStaleJob(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-delete",
			Namespace:       "default",
			UID:             "stale-uid",
			Labels:          map[string]string{labelExporter: "s3-exporter", labelOwner: "owner", labelType: typeDelete},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
	})
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})

	_, _, err := ensureSnapshotJob(context.Background(), client, owner, desiredDeleteJob(owner, "gcs-exporter"), "delete")
	require.ErrorContains(t, err, "delete refused")
	assert.NotErrorIs(t, err, ErrStaleJobReplaced)
}

func TestEnsureSnapshotJobReportsUIDPreconditionConflict(t *testing.T) {
	owner := testJobOwner()
	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-work",
			Namespace:       "default",
			UID:             "stale-uid",
			Labels:          map[string]string{labelExporter: "s3-exporter", labelOwner: owner.Name, labelType: typeDelete},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
	})
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: batchv1.GroupName, Resource: "jobs"},
			"snapshot-work",
			errors.New("UID precondition did not match"),
		)
	})
	desired := desiredDeleteJob(owner, "gcs-exporter")
	desired.Name = "snapshot-work"

	_, _, err := ensureSnapshotJob(context.Background(), client, owner, desired, typeDelete)
	require.True(t, apierrors.IsConflict(err))
	assert.NotErrorIs(t, err, ErrStaleJobReplaced)
}

func TestCleanupSnapshotDeletionJobTreatsNotFoundAsSuccess(t *testing.T) {
	client := fake.NewSimpleClientset()
	owner := testJobOwner()

	require.NoError(t, cleanupSnapshotDeletionJob(context.Background(), client, owner, testDeletionSnapshotJob("snapshot"), gcsExporter))
	for _, action := range client.Actions() {
		assert.False(t, action.GetVerb() == "delete" && action.GetResource().Resource == "jobs")
	}
}

func TestCleanupSnapshotDeletionJobWaitsForTerminatingJob(t *testing.T) {
	owner := testJobOwner()
	job := desiredDeleteJob(owner, gcsExporter)
	job.UID = "existing-job-uid"
	job.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	client := fake.NewSimpleClientset(job)

	err := cleanupSnapshotDeletionJob(context.Background(), client, owner, testDeletionSnapshotJob("snapshot"), gcsExporter)
	require.ErrorIs(t, err, ErrStaleJobTerminating)

	for _, action := range client.Actions() {
		assert.False(t, action.GetVerb() == "delete" && action.GetResource().Resource == "jobs")
	}
}

func TestCleanupSnapshotDeletionJobRejectsForeignJob(t *testing.T) {
	owner := testJobOwner()
	foreignOwner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "other-owner", Namespace: "default", UID: "other-owner-uid"},
	}
	job := desiredDeleteJob(owner, gcsExporter)
	job.UID = "existing-job-uid"
	job.OwnerReferences = []metav1.OwnerReference{ownerReferenceTo(foreignOwner)}
	client := fake.NewSimpleClientset(job)

	err := cleanupSnapshotDeletionJob(context.Background(), client, owner, testDeletionSnapshotJob("snapshot"), gcsExporter)
	require.ErrorContains(t, err, "not controlled by snapshot owner")

	_, err = client.BatchV1().Jobs("default").Get(context.Background(), job.Name, metav1.GetOptions{})
	require.NoError(t, err, "foreign Job must survive")
	for _, action := range client.Actions() {
		assert.False(t, action.GetVerb() == "delete" && action.GetResource().Resource == "jobs")
	}
}

func TestCleanupSnapshotDeletionJobReplacesMislabeledOwnedJob(t *testing.T) {
	owner := testJobOwner()
	tests := []struct {
		name         string
		mutate       func(*batchv1.Job)
		wantLabel    string
		wantPrevious string
		wantDesired  string
	}{
		{
			name: "wrong exporter",
			mutate: func(job *batchv1.Job) {
				job.Labels[labelExporter] = s3Exporter
			},
			wantLabel:    labelExporter,
			wantPrevious: s3Exporter,
			wantDesired:  gcsExporter,
		},
		{
			name: "wrong type",
			mutate: func(job *batchv1.Job) {
				job.Labels[labelType] = typeUpload
			},
			wantLabel:    labelType,
			wantPrevious: typeUpload,
			wantDesired:  typeDelete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := desiredDeleteJob(owner, gcsExporter)
			job.UID = "existing-job-uid"
			tt.mutate(job)
			client := fake.NewSimpleClientset(job)

			err := cleanupSnapshotDeletionJob(context.Background(), client, owner, testDeletionSnapshotJob("snapshot"), gcsExporter)
			require.ErrorIs(t, err, ErrStaleJobReplaced)
			var replacement *StaleJobReplacedError
			require.ErrorAs(t, err, &replacement)
			assert.Equal(t, typeDelete, replacement.Purpose)
			assert.Equal(t, tt.wantLabel, replacement.ConflictingLabel)
			assert.Equal(t, tt.wantPrevious, replacement.PreviousValue)
			assert.Equal(t, tt.wantDesired, replacement.DesiredValue)

			_, err = client.BatchV1().Jobs("default").Get(context.Background(), job.Name, metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err), "mislabeled owned Job must be deleted")
		})
	}
}

func TestCleanupSnapshotDeletionJobReportsGetFailure(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("get refused")
	})

	err := cleanupSnapshotDeletionJob(context.Background(), client, testJobOwner(), testDeletionSnapshotJob("snapshot"), gcsExporter)
	require.ErrorContains(t, err, "get snapshot deletion job default/snapshot-delete")
	require.ErrorContains(t, err, "get refused")
}

func TestCleanupSnapshotDeletionJobReportsUIDPreconditionConflict(t *testing.T) {
	owner := testJobOwner()
	job := desiredDeleteJob(owner, gcsExporter)
	job.UID = "existing-job-uid"
	client := fake.NewSimpleClientset(job)
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: batchv1.GroupName, Resource: "jobs"},
			job.Name,
			errors.New("UID precondition did not match"),
		)
	})

	err := cleanupSnapshotDeletionJob(context.Background(), client, owner, testDeletionSnapshotJob("snapshot"), gcsExporter)
	require.True(t, apierrors.IsConflict(err))
	require.ErrorContains(t, err, "delete snapshot deletion job default/snapshot-delete")
	require.ErrorContains(t, err, "UID precondition did not match")

	_, err = client.BatchV1().Jobs("default").Get(context.Background(), job.Name, metav1.GetOptions{})
	require.NoError(t, err, "replacement Job must survive a UID precondition conflict")
}

func testJobOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"},
	}
}

func testDeletionSnapshotJob(name string) SnapshotJob {
	return SnapshotJob{Name: name, Purpose: SnapshotJobDelete}
}

func ownerReferenceTo(owner *corev1.ConfigMap) metav1.OwnerReference {
	return ownerReferenceToObject(owner)
}

// ownerReferenceToObject builds the controller reference the providers set on the Jobs they create, so
// fixtures pass the ownership guard that protects another node's Jobs from deletion.
func ownerReferenceToObject(owner metav1.Object) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       owner.GetName(),
		UID:        owner.GetUID(),
		Controller: ptr.To(true),
	}
}

func testOrphanUploadResources(owner metav1.Object, exporter string) (*batchv1.Job, *corev1.PersistentVolumeClaim) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      "snapshot-upload",
		Namespace: owner.GetNamespace(),
		UID:       "upload-uid",
		Labels: map[string]string{
			labelExporter: exporter,
			labelOwner:    owner.GetName(),
			labelType:     typeUpload,
		},
		OwnerReferences: []metav1.OwnerReference{ownerReferenceToObject(owner)},
	}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      job.Name,
		Namespace: job.Namespace,
		UID:       "pvc-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(),
			Kind:       "Job",
			Name:       job.Name,
			UID:        job.UID,
			Controller: ptr.To(true),
		}},
	}}
	return job, pvc
}

func desiredDeleteJob(owner *corev1.ConfigMap, exporter string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snapshot-delete",
			Namespace:       "default",
			Labels:          map[string]string{labelExporter: exporter, labelOwner: owner.Name, labelType: typeDelete},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
	}
}

func requireJobDeleteAction(t *testing.T, actions []k8stesting.Action) k8stesting.DeleteAction {
	t.Helper()
	for _, action := range actions {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "jobs" {
			deleteAction, ok := action.(k8stesting.DeleteAction)
			require.True(t, ok)
			return deleteAction
		}
	}
	require.FailNow(t, "job delete action not found")
	return nil
}
