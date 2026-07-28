package datasnapshot

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
			Name:            "snapshot-delete",
			Namespace:       "default",
			UID:             "stale-uid",
			Labels:          map[string]string{labelExporter: "s3-exporter", labelOwner: "owner", labelType: typeDelete},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		},
	})

	desired := desiredDeleteJob(owner, "gcs-exporter")
	_, _, err := ensureSnapshotJob(context.Background(), client, owner, desired, "delete")
	require.ErrorIs(t, err, ErrStaleJobReplaced)
	assert.ErrorContains(t, err, labelExporter)

	// The stale Job is gone, so the next reconcile creates the desired one instead of erroring forever.
	_, err = client.BatchV1().Jobs("default").Get(context.Background(), "snapshot-delete", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))

	job, created, err := ensureSnapshotJob(context.Background(), client, owner, desired, "delete")
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "gcs-exporter", job.Labels[labelExporter])
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

func testJobOwner() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"},
	}
}

func ownerReferenceTo(owner *corev1.ConfigMap) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       owner.Name,
		UID:        owner.UID,
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
