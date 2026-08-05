package datasnapshot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

type SnapshotStatus string

type SnapshotJobPurpose string

type SnapshotJobIdentity struct {
	UID         types.UID
	PVCUID      types.UID
	Terminating bool
}

type SnapshotJob struct {
	Name        string
	UID         types.UID
	Purpose     SnapshotJobPurpose
	Terminating bool
	Upload      *SnapshotJobIdentity
	// Source preserves the listed upload configuration if the live Job vanishes
	// before previous-provider orphan recovery can derive its deletion Job.
	Source *batchv1.Job
	// Exporter is set when a listed deletion Job belongs to a previously configured
	// provider. Deletion Jobs are self-contained, so they can still be resumed and
	// cleaned up after the ChainNode switches providers.
	Exporter string
	Failure  string
	// RequireDestinationIdentity binds status observation to the caller's desired
	// destination. Legacy orphan workflows leave it false and remain self-contained.
	RequireDestinationIdentity bool
}

const (
	SnapshotSucceeded SnapshotStatus = "succeeded"
	SnapshotFailed    SnapshotStatus = "failed"
	SnapshotActive    SnapshotStatus = "active"
	SnapshotNotFound  SnapshotStatus = "notfound"

	labelExporter    = "exporter"
	labelOwner       = "owner"
	labelType        = "type"
	labelDestination = "destination"

	labelCleanupExporter    = "cleanup-exporter"
	labelCleanupOwner       = "cleanup-owner"
	labelCleanupType        = "cleanup-type"
	labelCleanupUploadUID   = "cleanup-upload-uid"
	labelCleanupPVCUID      = "cleanup-pvc-uid"
	labelCleanupAfterUpload = "cleanup-after-upload"
	labelCleanupDeletionUID = "cleanup-deletion-uid"

	SnapshotJobUpload SnapshotJobPurpose = "upload"
	SnapshotJobDelete SnapshotJobPurpose = "delete"

	typeUpload           = string(SnapshotJobUpload)
	typeDelete           = string(SnapshotJobDelete)
	typePostUploadDelete = "post-upload-delete"

	unboundSnapshotDeleteBackoffLimit int32 = 5
)

func snapshotDestinationLabel(values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return fmt.Sprintf("%x", digest[:8])
}

// SnapshotDestinationLabel returns the immutable Job label for an object-store destination.
func SnapshotDestinationLabel(provider, bucket, region, endpoint string, forcePathStyle bool, authentication ...string) string {
	values := []string{provider, bucket}
	if provider == string(appsv1.SnapshotExportProviderGCS) {
		return snapshotDestinationLabel(append(values, authentication...)...)
	}
	values = append(values, region, endpoint, strconv.FormatBool(forcePathStyle))
	return snapshotDestinationLabel(append(values, authentication...)...)
}

// ErrStaleJobReplaced reports that an existing snapshot Job is being deleted because it cannot
// converge to the desired state. Callers should requeue instead of failing the reconcile; the
// replacement Job is created once the stale one is gone.
var ErrStaleJobReplaced = errors.New("stale snapshot job deleted; it will be recreated")

// ErrStaleJobTerminating reports that a previously replaced stale Job still exists with a deletion
// timestamp. Callers should use a delayed requeue while foreground deletion finishes.
var ErrStaleJobTerminating = errors.New("stale snapshot job is still terminating")

// StaleJobReplacedError describes a stale Job whose deletion was successfully requested.
type StaleJobReplacedError struct {
	Purpose          string
	Namespace        string
	Name             string
	UID              types.UID
	ConflictingLabel string
	PreviousValue    string
	DesiredValue     string
}

func (err *StaleJobReplacedError) Error() string {
	if err.ProviderSwitch() {
		return fmt.Sprintf("%s job %s/%s (UID %s) for exporter %q replaced by desired exporter %q: %v",
			err.Purpose, err.Namespace, err.Name, err.UID, err.PreviousValue, err.DesiredValue, ErrStaleJobReplaced)
	}
	return fmt.Sprintf("%s job %s/%s (UID %s) had conflicting label %q: previous value %q, desired value %q: %v",
		err.Purpose, err.Namespace, err.Name, err.UID, err.ConflictingLabel, err.PreviousValue, err.DesiredValue, ErrStaleJobReplaced)
}

func (err *StaleJobReplacedError) Unwrap() error {
	return ErrStaleJobReplaced
}

// ProviderSwitch reports whether the exporter label caused the replacement.
func (err *StaleJobReplacedError) ProviderSwitch() bool {
	return err.ConflictingLabel == labelExporter
}

type SnapshotProvider interface {
	CreateSnapshot(context.Context, string, *snapshotv1.VolumeSnapshot) error
	GetSnapshotStatus(context.Context, string) (SnapshotStatus, error)
	GetSnapshotDeletionStatus(context.Context, SnapshotJob) (SnapshotStatus, error)
	CleanupSnapshot(context.Context, string) error
	DeleteSnapshot(context.Context, string) (SnapshotStatus, error)
	DeleteSnapshotBounded(context.Context, string) (SnapshotStatus, error)
	DeleteSnapshotForUpload(context.Context, SnapshotJob) (SnapshotJob, SnapshotStatus, error)
	CleanupSnapshotDeletion(context.Context, SnapshotJob) error
	ListSnapshots(ctx context.Context) ([]SnapshotJob, error)
}

func ensureUploadResources(
	ctx context.Context,
	client kubernetes.Interface,
	scheme *runtime.Scheme,
	owner metav1.Object,
	job *batchv1.Job,
	pvc *corev1.PersistentVolumeClaim,
) error {
	actualJob, created, err := ensureSnapshotJob(ctx, client, owner, job, "upload")
	if err != nil {
		return err
	}

	if err = controllerutil.SetControllerReference(actualJob, pvc, scheme); err != nil {
		return cleanUpNewUploadJob(ctx, client, actualJob, created, fmt.Errorf("set PVC owner reference: %w", err))
	}
	if err = ensureUploadPVC(ctx, client, actualJob, pvc); err != nil {
		return cleanUpNewUploadJob(ctx, client, actualJob, created, err)
	}
	return nil
}

func ensureSnapshotJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	desired *batchv1.Job,
	purpose string,
) (*batchv1.Job, bool, error) {
	jobs := client.BatchV1().Jobs(desired.Namespace)
	job, err := jobs.Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return job, true, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, false, err
	}

	job, err = jobs.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("get existing %s job: %w", purpose, err)
	}
	if !metav1.IsControlledBy(job, owner) {
		return nil, false, fmt.Errorf("%s job %s/%s is not controlled by snapshot owner %s", purpose, job.Namespace, job.Name, owner.GetName())
	}
	if job.DeletionTimestamp != nil {
		return nil, false, fmt.Errorf("stale %s job %s/%s is terminating: %w", purpose, job.Namespace, job.Name, ErrStaleJobTerminating)
	}
	job, err = reconcileSnapshotJobIdentity(ctx, client, owner, job, desired, purpose)
	if err != nil {
		return nil, false, err
	}
	return job, false, nil
}

func reconcileSnapshotJobIdentity(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	job, desired *batchv1.Job,
	purpose string,
) (*batchv1.Job, error) {
	jobs := client.BatchV1().Jobs(job.Namespace)
	var err error
	keys := make([]string, 0, len(desired.Labels))
	seen := make(map[string]struct{}, len(desired.Labels))
	for _, key := range []string{labelExporter, labelOwner, labelType, labelDestination} {
		if _, ok := desired.Labels[key]; ok {
			keys = append(keys, key)
			seen[key] = struct{}{}
		}
	}
	remaining := make([]string, 0, len(desired.Labels)-len(keys))
	for key := range desired.Labels {
		if _, ok := seen[key]; !ok {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	keys = append(keys, remaining...)
	for _, key := range keys {
		value := desired.Labels[key]
		if job.Labels[key] != value {
			if job.DeletionTimestamp != nil {
				return nil, fmt.Errorf("stale %s job %s/%s is terminating: %w", purpose, job.Namespace, job.Name, ErrStaleJobTerminating)
			}
			if key == labelDestination && job.Labels[key] == "" && snapshotJobPodIdentityMatches(job, desired) {
				updated := job.DeepCopy()
				if updated.Labels == nil {
					updated.Labels = make(map[string]string)
				}
				updated.Labels[key] = value
				job, err = jobs.Update(ctx, updated, metav1.UpdateOptions{})
				if err != nil {
					return nil, fmt.Errorf("adopt legacy %s job %s/%s: %w", purpose, updated.Namespace, updated.Name, err)
				}
				continue
			}
			if err = deleteSnapshotJob(ctx, client, job); err != nil {
				return nil, fmt.Errorf("delete stale %s job %s/%s: %w", purpose, job.Namespace, job.Name, err)
			}
			return nil, &StaleJobReplacedError{
				Purpose:          purpose,
				Namespace:        job.Namespace,
				Name:             job.Name,
				UID:              job.UID,
				ConflictingLabel: key,
				PreviousValue:    job.Labels[key],
				DesiredValue:     value,
			}
		}
	}
	return job, nil
}

type snapshotJobPodIdentity struct {
	ServiceAccountName string
	PriorityClassName  string
	RestartPolicy      corev1.RestartPolicy
	SecurityContext    *corev1.PodSecurityContext
	ImagePullSecrets   []corev1.LocalObjectReference
	Volumes            []corev1.Volume
	InitContainers     []snapshotJobContainerIdentity
	Containers         []snapshotJobContainerIdentity
}

type snapshotJobContainerIdentity struct {
	Name            string
	Image           string
	Command         []string
	Args            []string
	WorkingDir      string
	Env             []corev1.EnvVar
	EnvFrom         []corev1.EnvFromSource
	VolumeMounts    []corev1.VolumeMount
	ImagePullPolicy corev1.PullPolicy
	SecurityContext *corev1.SecurityContext
}

func snapshotJobPodIdentityMatches(actual, desired *batchv1.Job) bool {
	return apiequality.Semantic.DeepEqual(snapshotJobIdentity(actual), snapshotJobIdentity(desired))
}

func snapshotJobIdentity(job *batchv1.Job) snapshotJobPodIdentity {
	pod := job.Spec.Template.Spec
	serviceAccountName := pod.ServiceAccountName
	if serviceAccountName == "default" {
		serviceAccountName = ""
	}
	return snapshotJobPodIdentity{
		ServiceAccountName: serviceAccountName,
		PriorityClassName:  pod.PriorityClassName,
		RestartPolicy:      pod.RestartPolicy,
		SecurityContext:    pod.SecurityContext,
		ImagePullSecrets:   pod.ImagePullSecrets,
		Volumes:            normalizedSnapshotJobVolumes(pod.Volumes),
		InitContainers:     snapshotJobContainerIdentities(pod.InitContainers),
		Containers:         snapshotJobContainerIdentities(pod.Containers),
	}
}

func normalizedSnapshotJobVolumes(volumes []corev1.Volume) []corev1.Volume {
	normalized := make([]corev1.Volume, len(volumes))
	for i := range volumes {
		volumes[i].DeepCopyInto(&normalized[i])
		if secret := normalized[i].Secret; secret != nil && secret.DefaultMode != nil && *secret.DefaultMode == corev1.SecretVolumeSourceDefaultMode {
			secret.DefaultMode = nil
		}
	}
	return normalized
}

func snapshotJobContainerIdentities(containers []corev1.Container) []snapshotJobContainerIdentity {
	identities := make([]snapshotJobContainerIdentity, len(containers))
	for i := range containers {
		container := containers[i]
		identities[i] = snapshotJobContainerIdentity{
			Name:            container.Name,
			Image:           container.Image,
			Command:         container.Command,
			Args:            container.Args,
			WorkingDir:      container.WorkingDir,
			Env:             container.Env,
			EnvFrom:         container.EnvFrom,
			VolumeMounts:    container.VolumeMounts,
			ImagePullPolicy: container.ImagePullPolicy,
			SecurityContext: container.SecurityContext,
		}
	}
	return identities
}

// uploadJobStatus reports the status of an existing upload Job, rejecting one that belongs to a
// different exporter. Polling looks the Job up by name, and the name does not encode the provider, so
// without this check an in-flight upload started by the previous provider would be read as progress
// towards the newly configured one — and its success would mark the export finished even though
// nothing was written to the new destination.
//
// The stale Job is removed so the next reconcile starts a real upload against the current provider.
func uploadJobStatus(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	namespace, name, exporter string,
) (SnapshotStatus, error) {
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SnapshotNotFound, nil
		}
		return "", err
	}
	// Tarball names derive from chain ID and a second-resolution timestamp, so two ChainNodes in one
	// namespace can collide on this Job name. Never delete a Job this node does not own — that would
	// terminate another node's active export. Mirrors the ownership guard in ensureSnapshotJob.
	if !metav1.IsControlledBy(job, owner) {
		return "", fmt.Errorf("upload job %s/%s is not controlled by snapshot owner %s",
			job.Namespace, job.Name, owner.GetName())
	}
	if job.Labels[labelExporter] != exporter {
		if job.DeletionTimestamp != nil {
			return "", fmt.Errorf("stale upload job %s/%s is terminating: %w", job.Namespace, job.Name, ErrStaleJobTerminating)
		}
		if err = deleteSnapshotJob(ctx, client, job); err != nil {
			return "", fmt.Errorf("delete stale upload job %s/%s: %w", job.Namespace, job.Name, err)
		}
		return "", &StaleJobReplacedError{
			Purpose:          typeUpload,
			Namespace:        job.Namespace,
			Name:             job.Name,
			UID:              job.UID,
			ConflictingLabel: labelExporter,
			PreviousValue:    job.Labels[labelExporter],
			DesiredValue:     exporter,
		}
	}
	return snapshotJobStatus(job), nil
}

func uploadJobStatusForDesired(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	desired *batchv1.Job,
) (SnapshotStatus, error) {
	job, err := client.BatchV1().Jobs(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SnapshotNotFound, nil
		}
		return "", err
	}
	if !metav1.IsControlledBy(job, owner) {
		return "", fmt.Errorf("upload job %s/%s is not controlled by snapshot owner %s",
			job.Namespace, job.Name, owner.GetName())
	}
	job, err = reconcileSnapshotJobIdentity(ctx, client, owner, job, desired, typeUpload)
	if err != nil {
		return "", err
	}
	return snapshotJobStatus(job), nil
}

func cleanupSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) error {
	if expected.Purpose != SnapshotJobDelete {
		return fmt.Errorf("snapshot job %q has purpose %q, expected %q", expected.Name, expected.Purpose, SnapshotJobDelete)
	}
	namespace := owner.GetNamespace()
	jobName := fmt.Sprintf("%s-delete", expected.Name)
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get snapshot deletion job %s/%s: %w", namespace, jobName, err)
	}
	if expected.UID != "" && job.UID != expected.UID {
		return fmt.Errorf("snapshot deletion job %s/%s has UID %s, expected listed UID %s",
			job.Namespace, job.Name, job.UID, expected.UID)
	}
	if !metav1.IsControlledBy(job, owner) {
		return fmt.Errorf("snapshot deletion job %s/%s is not controlled by snapshot owner %s",
			job.Namespace, job.Name, owner.GetName())
	}
	if job.DeletionTimestamp != nil {
		return fmt.Errorf("snapshot deletion job %s/%s is terminating: %w",
			job.Namespace, job.Name, ErrStaleJobTerminating)
	}
	for _, expected := range []struct {
		label   string
		desired string
	}{
		{label: labelExporter, desired: exporter},
		{label: labelOwner, desired: owner.GetName()},
		{label: labelType, desired: typeDelete},
	} {
		actual := job.Labels[expected.label]
		if actual == expected.desired {
			continue
		}
		if err = deleteSnapshotJob(ctx, client, job); err != nil {
			return fmt.Errorf("delete stale snapshot deletion job %s/%s: %w", job.Namespace, job.Name, err)
		}
		return &StaleJobReplacedError{
			Purpose:          typeDelete,
			Namespace:        job.Namespace,
			Name:             job.Name,
			UID:              job.UID,
			ConflictingLabel: expected.label,
			PreviousValue:    actual,
			DesiredValue:     expected.desired,
		}
	}
	if err = deleteSnapshotJob(ctx, client, job); err != nil {
		return fmt.Errorf("delete snapshot deletion job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

func deleteSnapshotJob(ctx context.Context, client kubernetes.Interface, job *batchv1.Job) error {
	propagation := metav1.DeletePropagationForeground
	err := client.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
		Preconditions:     &metav1.Preconditions{UID: &job.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func snapshotJobStatus(job *batchv1.Job) SnapshotStatus {
	failed := false
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return SnapshotSucceeded
		case batchv1.JobFailed:
			failed = true
		}
	}
	if failed {
		return SnapshotFailed
	}
	return SnapshotActive
}

func snapshotNameFromJob(job *batchv1.Job) string {
	switch job.Labels[labelType] {
	case typeUpload:
		return strings.TrimSuffix(job.Name, "-upload")
	case typeDelete:
		return strings.TrimSuffix(job.Name, "-delete")
	default:
		return job.Name
	}
}

func snapshotJobFromJob(job *batchv1.Job) SnapshotJob {
	snapshotJob := SnapshotJob{
		Name:        snapshotNameFromJob(job),
		UID:         job.UID,
		Purpose:     SnapshotJobPurpose(job.Labels[labelType]),
		Terminating: job.DeletionTimestamp != nil,
		Failure:     snapshotJobFailure(job),
	}
	if snapshotJob.Purpose == SnapshotJobDelete && job.Labels[labelCleanupUploadUID] != "" {
		snapshotJob.Upload = &SnapshotJobIdentity{
			UID:    types.UID(job.Labels[labelCleanupUploadUID]),
			PVCUID: types.UID(job.Labels[labelCleanupPVCUID]),
		}
	}
	return snapshotJob
}

func snapshotJobFailure(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type != batchv1.JobFailed || condition.Status != corev1.ConditionTrue {
			continue
		}
		reason := strings.TrimSpace(condition.Reason)
		message := strings.TrimSpace(condition.Message)
		switch {
		case reason != "" && message != "":
			return reason + ": " + message
		case reason != "":
			return reason
		case message != "":
			return message
		default:
			return "delete Job failed"
		}
	}
	return ""
}

func snapshotJobsFromJobs(jobs []batchv1.Job, currentIdentity ...string) []SnapshotJob {
	type groupedSnapshotJobs struct {
		preferred         SnapshotJob
		preferredExporter string
		upload            *SnapshotJobIdentity
		uploadExporter    string
		uploadSource      *batchv1.Job
	}

	uniqueJobs := make(map[string]groupedSnapshotJobs, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		snapshotJob := snapshotJobFromJob(job)
		group := uniqueJobs[snapshotJob.Name]
		if snapshotJob.Purpose == SnapshotJobUpload {
			group.upload = &SnapshotJobIdentity{UID: snapshotJob.UID, Terminating: snapshotJob.Terminating}
			group.uploadExporter = job.Labels[labelExporter]
			group.uploadSource = job.DeepCopy()
		}
		if group.preferred.Purpose != SnapshotJobDelete || snapshotJob.Purpose == SnapshotJobDelete {
			group.preferred = snapshotJob
			group.preferredExporter = job.Labels[labelExporter]
		}
		uniqueJobs[snapshotJob.Name] = group
	}

	result := make([]SnapshotJob, 0, len(uniqueJobs))
	for _, group := range uniqueJobs {
		if group.preferred.Purpose == SnapshotJobDelete && group.preferred.Upload == nil &&
			group.preferredExporter == group.uploadExporter {
			group.preferred.Upload = group.upload
		} else if group.preferred.Purpose == SnapshotJobDelete && group.upload != nil &&
			group.preferredExporter == group.uploadExporter &&
			group.preferred.Upload.UID == group.upload.UID {
			group.preferred.Upload.Terminating = group.upload.Terminating
		}
		previousExporter := len(currentIdentity) > 0 && group.preferredExporter != "" &&
			group.preferredExporter != currentIdentity[0]
		uploadDestination := ""
		if group.uploadSource != nil {
			uploadDestination = group.uploadSource.Labels[labelDestination]
		}
		previousDestination := len(currentIdentity) > 1 && group.preferred.Purpose == SnapshotJobUpload &&
			(uploadDestination != "" || snapshotUploadJobHasRecordedExecution(group.uploadSource)) &&
			uploadDestination != currentIdentity[1]
		if previousExporter || previousDestination {
			group.preferred.Exporter = group.preferredExporter
			if group.preferred.Purpose == SnapshotJobUpload {
				group.preferred.Source = group.uploadSource
			}
		}
		result = append(result, group.preferred)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func snapshotUploadJobHasRecordedExecution(job *batchv1.Job) bool {
	return job != nil && len(job.Spec.Template.Spec.Containers) == 1
}

func listSnapshotJobs(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	exporter string,
	destination ...string,
) ([]SnapshotJob, error) {
	selector := labels.SelectorFromSet(map[string]string{labelOwner: owner.GetName()}).String()
	list, err := client.BatchV1().Jobs(owner.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}

	jobs := make([]batchv1.Job, 0, len(list.Items))
	for i := range list.Items {
		job := list.Items[i]
		// The owner label is name-based and can survive a delete/recreate race.
		// Ignore Jobs controlled by a previous object with the same name; the old
		// owner's garbage collection must handle them.
		if !metav1.IsControlledBy(&job, owner) {
			continue
		}
		// Jobs remain discoverable after a provider switch. Upload Jobs retain the
		// previous provider's pod configuration, which is enough to derive a matching
		// deletion workflow without using the newly configured credentials.
		if job.Labels[labelType] == typeUpload || job.Labels[labelType] == typeDelete {
			jobs = append(jobs, job)
		}
	}
	identity := []string{exporter}
	identity = append(identity, destination...)
	return snapshotJobsFromJobs(jobs, identity...), nil
}

// ListSnapshotDeletionJobs returns owner-labelled deletion Jobs regardless of
// the currently configured exporter. Deletion Jobs are self-contained and may
// need to finish after tarball exports or snapshots have been disabled.
func ListSnapshotDeletionJobs(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
) ([]SnapshotJob, error) {
	jobs, err := listSnapshotJobs(ctx, client, owner, "")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(jobs, func(job SnapshotJob) bool {
		return job.Purpose != SnapshotJobDelete
	}), nil
}

// ReconcileSnapshotDeletionJob resumes and reports a previously created
// deletion Job using the exporter identity persisted on the Job itself.
func ReconcileSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	job SnapshotJob,
) (SnapshotStatus, error) {
	if job.Exporter == "" {
		return "", fmt.Errorf("snapshot deletion job %q is missing its exporter identity", job.Name)
	}
	return reconcileSnapshotDeletionJob(ctx, client, owner, job, job.Exporter)
}

// CleanupSnapshotDeletionResources removes a completed deletion Job and any
// paired upload resources using the identities persisted on the deletion Job.
func CleanupSnapshotDeletionResources(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	job SnapshotJob,
) error {
	if job.Exporter == "" {
		return fmt.Errorf("snapshot deletion job %q is missing its exporter identity", job.Name)
	}
	return cleanupSnapshotDeletionResources(ctx, client, owner, job, job.Exporter)
}

// DeleteSnapshotForUpload creates a deletion workflow from an upload Job that
// belongs to a previously configured exporter.
func DeleteSnapshotForUpload(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	upload SnapshotJob,
) (SnapshotJob, SnapshotStatus, error) {
	if upload.Purpose != SnapshotJobUpload || upload.Exporter == "" {
		return SnapshotJob{}, "", fmt.Errorf("snapshot upload job %q is missing its previous exporter identity", upload.Name)
	}
	identity := SnapshotJobIdentity{UID: upload.UID, Terminating: upload.Terminating}
	uploadJob, pvc, err := getSnapshotUploadResources(ctx, client, owner, upload.Name, identity, upload.Exporter)
	if err != nil {
		return SnapshotJob{}, "", err
	}
	if uploadJob == nil {
		if pvc == nil || upload.Source == nil || upload.Source.UID != upload.UID {
			return SnapshotJob{}, SnapshotNotFound, nil
		}
		uploadJob = upload.Source.DeepCopy()
	}
	if pvc != nil {
		identity.PVCUID = pvc.UID
	}
	deletionJob, err := deletionJobFromUpload(uploadJob, owner, upload.Exporter, identity)
	if err != nil {
		return SnapshotJob{}, "", err
	}
	deletionJob, _, err = ensureSnapshotJob(ctx, client, owner, deletionJob, typeDelete)
	if err != nil {
		return SnapshotJob{}, "", err
	}
	deletion := snapshotJobFromJob(deletionJob)
	deletion.Exporter = upload.Exporter
	status, err := reconcileSnapshotDeletionJob(ctx, client, owner, deletion, upload.Exporter)
	return deletion, status, err
}

func deletionJobFromUpload(
	upload *batchv1.Job,
	owner metav1.Object,
	exporter string,
	identity SnapshotJobIdentity,
) (*batchv1.Job, error) {
	if len(upload.Spec.Template.Spec.Containers) != 1 {
		return nil, fmt.Errorf("snapshot upload job %s/%s has %d containers, expected 1",
			upload.Namespace, upload.Name, len(upload.Spec.Template.Spec.Containers))
	}
	container := upload.Spec.Template.Spec.Containers[0]
	provider := strings.TrimSuffix(exporter, "-exporter")
	if len(container.Args) < 5 || container.Args[0] != provider || container.Args[1] != typeUpload {
		return nil, fmt.Errorf("snapshot upload job %s/%s has unexpected exporter arguments", upload.Namespace, upload.Name)
	}
	name := snapshotNameFromJob(upload)
	container.Args = []string{provider, typeDelete, container.Args[3], name}
	container.WorkingDir = "/app"
	container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
		return mount.Name == "data"
	})
	template := *upload.Spec.Template.DeepCopy()
	for _, label := range []string{
		"controller-uid",
		"job-name",
		"batch.kubernetes.io/controller-uid",
		"batch.kubernetes.io/job-name",
	} {
		delete(template.Labels, label)
	}
	template.Spec.Containers = []corev1.Container{container}
	template.Spec.Volumes = slices.DeleteFunc(template.Spec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == "data"
	})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-delete",
			Namespace:       upload.Namespace,
			Labels:          map[string]string{labelExporter: exporter, labelOwner: owner.GetName(), labelType: typeDelete},
			OwnerReferences: append([]metav1.OwnerReference(nil), upload.OwnerReferences...),
		},
		Spec: batchv1.JobSpec{BackoffLimit: ptr.To(unboundSnapshotDeleteBackoffLimit), Template: template},
	}
	if destination := upload.Labels[labelDestination]; destination != "" {
		job.Labels[labelDestination] = destination
	}
	setSnapshotDeletionUploadIdentity(job, owner, exporter, identity)
	return job, nil
}

func snapshotJobExporter(job SnapshotJob, currentExporter string) string {
	if job.Exporter != "" {
		return job.Exporter
	}
	return currentExporter
}

func getSnapshotUploadResources(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	name string,
	expected SnapshotJobIdentity,
	exporter string,
) (*batchv1.Job, *corev1.PersistentVolumeClaim, error) {
	if expected.UID == "" {
		return nil, nil, fmt.Errorf("snapshot upload job %q is missing its listed UID", name)
	}

	namespace := owner.GetNamespace()
	jobName := fmt.Sprintf("%s-upload", name)
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("get orphan upload job %s/%s: %w", namespace, jobName, err)
		}
		job = nil
	} else {
		if job.UID != expected.UID {
			return nil, nil, fmt.Errorf("orphan upload job %s/%s has UID %s, expected listed UID %s",
				job.Namespace, job.Name, job.UID, expected.UID)
		}
		if !metav1.IsControlledBy(job, owner) {
			return nil, nil, fmt.Errorf("orphan upload job %s/%s is not controlled by snapshot owner %s",
				job.Namespace, job.Name, owner.GetName())
		}
		for _, expectedLabel := range []struct {
			name  string
			value string
		}{
			{name: labelExporter, value: exporter},
			{name: labelOwner, value: owner.GetName()},
			{name: labelType, value: typeUpload},
		} {
			if job.Labels[expectedLabel.name] != expectedLabel.value {
				return nil, nil, fmt.Errorf("orphan upload job %s/%s has %s label %q, expected %q",
					job.Namespace, job.Name, expectedLabel.name, job.Labels[expectedLabel.name], expectedLabel.value)
			}
		}
	}

	pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return job, nil, nil
		}
		return nil, nil, fmt.Errorf("get orphan upload PVC %s/%s: %w", namespace, jobName, err)
	}
	controller := metav1.GetControllerOfNoCopy(pvc)
	if controller == nil || controller.APIVersion != batchv1.SchemeGroupVersion.String() || controller.Kind != "Job" ||
		controller.Name != jobName || controller.UID != expected.UID {
		return nil, nil, fmt.Errorf("orphan upload PVC %s/%s is not controlled by upload job UID %s",
			pvc.Namespace, pvc.Name, expected.UID)
	}
	if expected.PVCUID != "" && pvc.UID != expected.PVCUID {
		return nil, nil, fmt.Errorf("orphan upload PVC %s/%s has UID %s, expected recorded UID %s",
			pvc.Namespace, pvc.Name, pvc.UID, expected.PVCUID)
	}
	return job, pvc, nil
}

func cleanupSnapshotUploadResources(
	ctx context.Context,
	client kubernetes.Interface,
	job *batchv1.Job,
	pvc *corev1.PersistentVolumeClaim,
) error {
	if job != nil && job.DeletionTimestamp == nil {
		if err := deleteSnapshotJob(ctx, client, job); err != nil {
			return fmt.Errorf("delete orphan upload job %s/%s: %w", job.Namespace, job.Name, err)
		}
	}
	if pvc != nil {
		err := client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &pvc.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete orphan upload PVC %s/%s: %w", pvc.Namespace, pvc.Name, err)
		}
	}
	return nil
}

func setSnapshotDeletionUploadIdentity(
	job *batchv1.Job,
	owner metav1.Object,
	exporter string,
	upload SnapshotJobIdentity,
) {
	setSnapshotDeletionUploadIdentityLabels(job, owner, exporter, upload)
	suspend := true
	job.Spec.Suspend = &suspend
}

func setSnapshotDeletionUploadIdentityLabels(
	job *batchv1.Job,
	owner metav1.Object,
	exporter string,
	upload SnapshotJobIdentity,
) {
	if job.Labels == nil {
		job.Labels = make(map[string]string)
	}
	job.Labels[labelCleanupExporter] = exporter
	job.Labels[labelCleanupOwner] = owner.GetName()
	job.Labels[labelCleanupType] = typeUpload
	job.Labels[labelCleanupUploadUID] = string(upload.UID)
	if upload.PVCUID != "" {
		job.Labels[labelCleanupPVCUID] = string(upload.PVCUID)
	} else {
		delete(job.Labels, labelCleanupPVCUID)
	}
}

func postUploadDeletionJob(
	marker *batchv1.Job,
	owner metav1.Object,
	exporter string,
	upload SnapshotJobIdentity,
) *batchv1.Job {
	spec := *marker.Spec.DeepCopy()
	spec.Selector = nil
	spec.ManualSelector = nil
	for _, label := range []string{
		"controller-uid",
		"job-name",
		"batch.kubernetes.io/controller-uid",
		"batch.kubernetes.io/job-name",
	} {
		delete(spec.Template.Labels, label)
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSuffix(marker.Name, "-delete") + "-purge",
			Namespace: marker.Namespace,
			Labels: map[string]string{
				labelExporter:           exporter,
				labelOwner:              owner.GetName(),
				labelType:               typePostUploadDelete,
				labelCleanupDeletionUID: string(marker.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job",
				Name:       marker.Name,
				UID:        marker.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: spec,
	}
	setSnapshotDeletionUploadIdentity(job, owner, exporter, upload)
	return job
}

func snapshotDeletionUploadIdentity(
	job *batchv1.Job,
	owner metav1.Object,
	exporter string,
) (*SnapshotJobIdentity, error) {
	hasCleanupIdentity := false
	for _, label := range []string{
		labelCleanupExporter,
		labelCleanupOwner,
		labelCleanupType,
		labelCleanupUploadUID,
		labelCleanupPVCUID,
	} {
		if job.Labels[label] != "" {
			hasCleanupIdentity = true
			break
		}
	}
	if !hasCleanupIdentity {
		if job.Spec.Suspend != nil && *job.Spec.Suspend {
			return nil, fmt.Errorf("suspended snapshot deletion job %s/%s is missing its upload cleanup identity",
				job.Namespace, job.Name)
		}
		return nil, nil
	}

	for _, expectedLabel := range []struct {
		name  string
		value string
	}{
		{name: labelCleanupExporter, value: exporter},
		{name: labelCleanupOwner, value: owner.GetName()},
		{name: labelCleanupType, value: typeUpload},
	} {
		if job.Labels[expectedLabel.name] != expectedLabel.value {
			return nil, fmt.Errorf("snapshot deletion job %s/%s has %s label %q, expected %q",
				job.Namespace, job.Name, expectedLabel.name, job.Labels[expectedLabel.name], expectedLabel.value)
		}
	}
	uploadUID := types.UID(job.Labels[labelCleanupUploadUID])
	if uploadUID == "" {
		return nil, fmt.Errorf("snapshot deletion job %s/%s is missing %s label",
			job.Namespace, job.Name, labelCleanupUploadUID)
	}
	return &SnapshotJobIdentity{
		UID:    uploadUID,
		PVCUID: types.UID(job.Labels[labelCleanupPVCUID]),
	}, nil
}

func getSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) (*batchv1.Job, *SnapshotJobIdentity, error) {
	if expected.Purpose != SnapshotJobDelete {
		return nil, nil, fmt.Errorf("snapshot job %q has purpose %q, expected %q", expected.Name, expected.Purpose, SnapshotJobDelete)
	}

	namespace := owner.GetNamespace()
	jobName := fmt.Sprintf("%s-delete", expected.Name)
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, expected.Upload, nil
		}
		return nil, nil, fmt.Errorf("get snapshot deletion job %s/%s: %w", namespace, jobName, err)
	}
	if expected.UID != "" && job.UID != expected.UID {
		return nil, nil, fmt.Errorf("snapshot deletion job %s/%s has UID %s, expected listed UID %s",
			job.Namespace, job.Name, job.UID, expected.UID)
	}
	if !metav1.IsControlledBy(job, owner) {
		return nil, nil, fmt.Errorf("snapshot deletion job %s/%s is not controlled by snapshot owner %s",
			job.Namespace, job.Name, owner.GetName())
	}
	if job.DeletionTimestamp != nil {
		return nil, nil, fmt.Errorf("snapshot deletion job %s/%s is terminating: %w",
			job.Namespace, job.Name, ErrStaleJobTerminating)
	}
	for _, expectedLabel := range []struct {
		name  string
		value string
	}{
		{name: labelExporter, value: exporter},
		{name: labelOwner, value: owner.GetName()},
		{name: labelType, value: typeDelete},
	} {
		if job.Labels[expectedLabel.name] != expectedLabel.value {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s has %s label %q, expected %q",
				job.Namespace, job.Name, expectedLabel.name, job.Labels[expectedLabel.name], expectedLabel.value)
		}
	}
	upload, err := snapshotDeletionUploadIdentity(job, owner, exporter)
	if err != nil {
		return nil, nil, err
	}
	if upload == nil && expected.Upload != nil {
		identity := *expected.Upload
		_, pvc, resourceErr := getSnapshotUploadResources(ctx, client, owner, expected.Name, identity, exporter)
		if resourceErr != nil {
			return nil, nil, resourceErr
		}
		if pvc != nil {
			identity.PVCUID = pvc.UID
		}
		updated := job.DeepCopy()
		if snapshotJobStatus(job) == SnapshotActive {
			setSnapshotDeletionUploadIdentity(updated, owner, exporter, identity)
		} else {
			// A completed Job cannot run again. Keep it as the durable marker for a
			// hidden suspended successor that is activated after the upload vanishes.
			setSnapshotDeletionUploadIdentityLabels(updated, owner, exporter, identity)
			updated.Labels[labelCleanupAfterUpload] = "true"
			// Terminal deletion Jobs normally have a TTL. Disable it atomically with
			// the cleanup identity so the marker cannot disappear and cascade-delete
			// its post-upload worker before that worker finishes.
			updated.Spec.TTLSecondsAfterFinished = nil
		}
		updated, err = client.BatchV1().Jobs(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("pair legacy snapshot deletion job %s/%s with upload UID %s: %w",
				job.Namespace, job.Name, expected.Upload.UID, err)
		}
		if updated.UID != job.UID {
			return nil, nil, fmt.Errorf("paired snapshot deletion job %s/%s has UID %s, expected %s",
				updated.Namespace, updated.Name, updated.UID, job.UID)
		}
		job = updated
		upload = &identity
	}
	if expected.Upload != nil && upload != nil {
		if expected.Upload.UID != upload.UID {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s records upload UID %s, expected %s",
				job.Namespace, job.Name, upload.UID, expected.Upload.UID)
		}
		if expected.Upload.PVCUID != "" && upload.PVCUID != expected.Upload.PVCUID {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s records upload PVC UID %s, expected %s",
				job.Namespace, job.Name, upload.PVCUID, expected.Upload.PVCUID)
		}
	}
	if afterUpload := job.Labels[labelCleanupAfterUpload]; afterUpload != "" {
		if afterUpload != "true" {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s has %s label %q, expected %q",
				job.Namespace, job.Name, labelCleanupAfterUpload, afterUpload, "true")
		}
		if upload == nil {
			return nil, nil, fmt.Errorf("snapshot deletion job %s/%s is missing its post-upload cleanup identity",
				job.Namespace, job.Name)
		}
		worker, _, workerErr := ensureSnapshotJob(
			ctx, client, job, postUploadDeletionJob(job, owner, exporter, *upload), typePostUploadDelete,
		)
		if workerErr != nil {
			return nil, nil, workerErr
		}
		return worker, upload, nil
	}
	return job, upload, nil
}

func reconcileSnapshotDeletionJob(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) (SnapshotStatus, error) {
	deletionJob, upload, err := getSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	if err != nil {
		return "", err
	}
	if deletionJob == nil {
		return SnapshotNotFound, nil
	}
	if upload == nil {
		return snapshotJobStatus(deletionJob), nil
	}

	uploadJob, pvc, err := getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return "", err
	}
	suspended := deletionJob.Spec.Suspend != nil && *deletionJob.Spec.Suspend
	if !suspended {
		if uploadJob != nil {
			return "", fmt.Errorf("snapshot deletion job %s/%s is active while upload job %s/%s UID %s still exists",
				deletionJob.Namespace, deletionJob.Name, uploadJob.Namespace, uploadJob.Name, uploadJob.UID)
		}
		if pvc != nil {
			if err = cleanupSnapshotUploadResources(ctx, client, nil, pvc); err != nil {
				return "", err
			}
		}
		return snapshotJobStatus(deletionJob), nil
	}

	if err = cleanupSnapshotUploadResources(ctx, client, uploadJob, pvc); err != nil {
		return "", err
	}
	uploadJob, _, err = getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return "", err
	}
	if uploadJob != nil {
		return SnapshotActive, nil
	}

	updated := deletionJob.DeepCopy()
	activate := false
	updated.Spec.Suspend = &activate
	updated, err = client.BatchV1().Jobs(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return "", fmt.Errorf("activate snapshot deletion job %s/%s UID %s: %w",
			deletionJob.Namespace, deletionJob.Name, deletionJob.UID, err)
	}
	if updated.UID != deletionJob.UID {
		return "", fmt.Errorf("activated snapshot deletion job %s/%s has UID %s, expected %s",
			updated.Namespace, updated.Name, updated.UID, deletionJob.UID)
	}
	return snapshotJobStatus(updated), nil
}

func reconcileSnapshotDeletionJobForDesired(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
	desired *batchv1.Job,
) (SnapshotStatus, error) {
	job, err := client.BatchV1().Jobs(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SnapshotNotFound, nil
		}
		return "", err
	}
	if expected.UID != "" && job.UID != expected.UID {
		return "", fmt.Errorf("snapshot deletion job %s/%s has UID %s, expected listed UID %s",
			job.Namespace, job.Name, job.UID, expected.UID)
	}
	if !int32PointersEqual(job.Spec.BackoffLimit, desired.Spec.BackoffLimit) {
		if job.DeletionTimestamp != nil {
			return "", fmt.Errorf("stale %s job %s/%s is terminating: %w",
				typeDelete, job.Namespace, job.Name, ErrStaleJobTerminating)
		}
		if err = deleteSnapshotJob(ctx, client, job); err != nil {
			return "", fmt.Errorf("delete stale %s job %s/%s: %w", typeDelete, job.Namespace, job.Name, err)
		}
		return "", &StaleJobReplacedError{
			Purpose:          typeDelete,
			Namespace:        job.Namespace,
			Name:             job.Name,
			UID:              job.UID,
			ConflictingLabel: "spec.backoffLimit",
			PreviousValue:    int32PointerString(job.Spec.BackoffLimit),
			DesiredValue:     int32PointerString(desired.Spec.BackoffLimit),
		}
	}
	if _, err = reconcileSnapshotJobIdentity(ctx, client, owner, job, desired, typeDelete); err != nil {
		return "", err
	}
	return reconcileSnapshotDeletionJob(ctx, client, owner, expected, exporter)
}

func int32PointersEqual(left, right *int32) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func int32PointerString(value *int32) string {
	if value == nil {
		return "<nil>"
	}
	return strconv.FormatInt(int64(*value), 10)
}

func cleanupSnapshotDeletionResources(
	ctx context.Context,
	client kubernetes.Interface,
	owner metav1.Object,
	expected SnapshotJob,
	exporter string,
) error {
	if expected.Upload == nil {
		// Legacy deletion Jobs did not persist an upload UID. A same-name PVC
		// controlled by an already-absent Job cannot be tied to this ChainNode, so
		// retain it rather than risk deleting another node's colliding upload PVC.
		return cleanupSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	}

	deletionJob, upload, err := getSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	if err != nil {
		return err
	}
	if upload == nil {
		upload = expected.Upload
	}
	if upload == nil {
		return cleanupSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	}
	if deletionJob != nil && deletionJob.Spec.Suspend != nil && *deletionJob.Spec.Suspend {
		return fmt.Errorf("snapshot deletion job %s/%s is still suspended", deletionJob.Namespace, deletionJob.Name)
	}
	job, pvc, err := getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return err
	}
	if err = cleanupSnapshotUploadResources(ctx, client, job, pvc); err != nil {
		return err
	}
	job, _, err = getSnapshotUploadResources(ctx, client, owner, expected.Name, *upload, exporter)
	if err != nil {
		return err
	}
	if job != nil {
		return nil
	}
	if deletionJob == nil {
		return nil
	}
	if deletionJob.Name != fmt.Sprintf("%s-delete", expected.Name) {
		return cleanupSnapshotDeletionJob(ctx, client, owner, expected, exporter)
	}
	if err = deleteSnapshotJob(ctx, client, deletionJob); err != nil {
		return fmt.Errorf("delete snapshot deletion job %s/%s: %w", deletionJob.Namespace, deletionJob.Name, err)
	}
	return nil
}

func ensureUploadPVC(
	ctx context.Context,
	client kubernetes.Interface,
	job *batchv1.Job,
	desired *corev1.PersistentVolumeClaim,
) error {
	claims := client.CoreV1().PersistentVolumeClaims(desired.Namespace)
	pvc, err := claims.Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create upload PVC: %w", err)
	}

	pvc, err = claims.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get existing upload PVC: %w", err)
	}
	if !metav1.IsControlledBy(pvc, job) {
		return fmt.Errorf("upload PVC %s/%s is not controlled by upload job %s", pvc.Namespace, pvc.Name, job.Name)
	}
	if !apiequality.Semantic.DeepEqual(pvc.Spec.DataSource, desired.Spec.DataSource) {
		return fmt.Errorf("upload PVC %s/%s has a conflicting snapshot data source", pvc.Namespace, pvc.Name)
	}
	return nil
}

func cleanUpNewUploadJob(
	ctx context.Context,
	client kubernetes.Interface,
	job *batchv1.Job,
	created bool,
	cause error,
) error {
	if !created {
		return cause
	}
	propagation := metav1.DeletePropagationForeground
	if err := client.BatchV1().Jobs(job.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("%w; clean up upload job: %v", cause, err)
	}
	return cause
}
