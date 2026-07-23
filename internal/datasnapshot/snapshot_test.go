package datasnapshot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
