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

	require.NoError(t, cleanupSnapshotDeletionJob(context.Background(), client, owner, "snapshot", gcsExporter))
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

	err := cleanupSnapshotDeletionJob(context.Background(), client, owner, "snapshot", gcsExporter)
	require.ErrorIs(t, err, ErrStaleJobTerminating)

	for _, action := range client.Actions() {
		assert.False(t, action.GetVerb() == "delete" && action.GetResource().Resource == "jobs")
	}
}

func TestCleanupSnapshotDeletionJobRejectsForeignJobs(t *testing.T) {
	owner := testJobOwner()
	foreignOwner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "other-owner", Namespace: "default", UID: "other-owner-uid"},
	}
	tests := []struct {
		name        string
		mutate      func(*batchv1.Job)
		wantMessage string
	}{
		{
			name: "foreign owner",
			mutate: func(job *batchv1.Job) {
				job.OwnerReferences = []metav1.OwnerReference{ownerReferenceTo(foreignOwner)}
			},
			wantMessage: "not controlled by snapshot owner",
		},
		{
			name: "wrong exporter",
			mutate: func(job *batchv1.Job) {
				job.Labels[labelExporter] = s3Exporter
			},
			wantMessage: `has exporter label "s3-exporter", expected "gcs-exporter"`,
		},
		{
			name: "wrong type",
			mutate: func(job *batchv1.Job) {
				job.Labels[labelType] = typeUpload
			},
			wantMessage: `has type label "upload", expected "delete"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := desiredDeleteJob(owner, gcsExporter)
			job.UID = "existing-job-uid"
			tt.mutate(job)
			client := fake.NewSimpleClientset(job)

			err := cleanupSnapshotDeletionJob(context.Background(), client, owner, "snapshot", gcsExporter)
			require.ErrorContains(t, err, tt.wantMessage)

			_, err = client.BatchV1().Jobs("default").Get(context.Background(), job.Name, metav1.GetOptions{})
			require.NoError(t, err, "foreign or mislabeled Job must survive")
			for _, action := range client.Actions() {
				assert.False(t, action.GetVerb() == "delete" && action.GetResource().Resource == "jobs")
			}
		})
	}
}

func TestCleanupSnapshotDeletionJobReportsGetFailure(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("get refused")
	})

	err := cleanupSnapshotDeletionJob(context.Background(), client, testJobOwner(), "snapshot", gcsExporter)
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

	err := cleanupSnapshotDeletionJob(context.Background(), client, owner, "snapshot", gcsExporter)
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
