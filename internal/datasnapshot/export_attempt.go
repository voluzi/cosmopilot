package datasnapshot

import (
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
	"github.com/voluzi/cosmopilot/v5/internal/k8s"
	"github.com/voluzi/cosmopilot/v5/pkg/images"
)

func NativeLabels(a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe) map[string]string {
	return map[string]string{exportprotocol.OwnerNameLabel: r.Owner.Name, exportprotocol.OwnerUIDLabel: string(r.Owner.UID), exportprotocol.AttemptLabel: a.ID}
}
func NativeJob(a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe) *batchv1.Job {
	labels := NativeLabels(a, r)
	pullPolicy := images.PullPolicy(r.Image, "")
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}
	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount"
	env := []corev1.EnvVar{{Name: "EXPORT_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}}}
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		env = append(env, corev1.EnvVar{Name: key, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: r.Credentials.Name}, Key: key, Optional: ptr.To(key == "AWS_SESSION_TOKEN")}}})
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: r.ResourceName, Namespace: r.Namespace, Labels: labels, Finalizers: []string{exportprotocol.AttemptFinalizer}, OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: r.Owner.Name, UID: r.Owner.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, Spec: batchv1.JobSpec{
		Completions: ptr.To[int32](1), Parallelism: ptr.To[int32](1), BackoffLimit: ptr.To[int32](0), PodReplacementPolicy: ptr.To(batchv1.Failed),
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, DNSPolicy: corev1.DNSClusterFirst, ServiceAccountName: r.ServiceAccount.Name, AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false), PriorityClassName: r.PriorityClass, ImagePullSecrets: r.ImagePullSecrets,
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532), FSGroup: ptr.To[int64](65532)},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.ResourceName, ReadOnly: true}}},
				{Name: "api", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To[int32](0444), Sources: []corev1.VolumeProjection{
					{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To[int64](3600)}},
					{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
					{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
				}}}},
			},
			Containers: []corev1.Container{{Name: "exporter", Image: r.Image, ImagePullPolicy: pullPolicy, Command: []string{"/app/dataexporter"}, Args: []string{"native-upload", "--namespace", r.Namespace, "--owner", r.Owner.Name, "--owner-uid", string(r.Owner.UID), "--attempt-id", a.ID, "--attempt-sequence", strconv.FormatInt(a.Sequence, 10)}, WorkingDir: "/home/app", Env: env, Resources: r.Resources, SecurityContext: k8s.RestrictedSecurityContext(), TerminationMessagePath: exportprotocol.ResultPath, TerminationMessagePolicy: corev1.TerminationMessageReadFile, VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: exportprotocol.DataPath, ReadOnly: true}, {Name: "api", MountPath: tokenPath, ReadOnly: true}}}},
		}},
	}}
}
func NativePVC(a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe, jobUID types.UID) *corev1.PersistentVolumeClaim {
	size, _ := resource.ParseQuantity(r.RestoreSize)
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: r.ResourceName, Namespace: r.Namespace, Labels: NativeLabels(a, r), Finalizers: []string{exportprotocol.AttemptFinalizer}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: r.ResourceName, UID: jobUID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, VolumeMode: ptr.To(corev1.PersistentVolumeFilesystem), StorageClassName: ptr.To(r.StorageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}}, DataSource: &corev1.TypedLocalObjectReference{APIGroup: ptr.To("snapshot.storage.k8s.io"), Kind: "VolumeSnapshot", Name: r.Snapshot.Name}}}
}

func ValidateNativePodSpec(actual *corev1.PodSpec, a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe) error {
	expected := NativeJob(a, r).Spec.Template.Spec
	p := &corev1.Pod{Spec: *actual.DeepCopy()}
	e := &corev1.Pod{Spec: expected}
	scheme.Scheme.Default(p)
	scheme.Scheme.Default(e)
	s, w := &p.Spec, &e.Spec
	if err := normalizeNativeResources(s.Containers, w.Containers); err != nil {
		return err
	}
	if len(s.InitContainers) != 0 || len(s.EphemeralContainers) != 0 || s.HostNetwork || s.HostPID || s.HostIPC || s.ShareProcessNamespace != nil && *s.ShareProcessNamespace || s.ServiceAccountName != w.ServiceAccountName || s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken || s.RestartPolicy != w.RestartPolicy || !apiequality.Semantic.DeepEqual(s.Containers, w.Containers) || !apiequality.Semantic.DeepEqual(s.Volumes, w.Volumes) || !apiequality.Semantic.DeepEqual(s.SecurityContext, w.SecurityContext) || !apiequality.Semantic.DeepEqual(s.ImagePullSecrets, w.ImagePullSecrets) || s.PriorityClassName != w.PriorityClassName || s.RuntimeClassName != nil || s.OS != nil && s.OS.Name != corev1.Linux || len(s.ResourceClaims) > 0 || len(s.HostAliases) > 0 || s.DNSConfig != nil || s.DNSPolicy != w.DNSPolicy || s.Hostname != "" || s.Subdomain != "" || !apiequality.Semantic.DeepEqual(s.EnableServiceLinks, w.EnableServiceLinks) {
		return fmt.Errorf("admitted native worker differs from frozen execution recipe")
	}
	return nil
}
func ValidateNativeJob(j *batchv1.Job, a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe) error {
	if j.Name != r.ResourceName || j.Namespace != r.Namespace || !j.DeletionTimestamp.IsZero() || !apiequality.Semantic.DeepEqual(j.OwnerReferences, NativeJob(a, r).OwnerReferences) || !containsFinalizer(j.Finalizers, exportprotocol.AttemptFinalizer) || ptr.Deref(j.Spec.Completions, 1) != 1 || ptr.Deref(j.Spec.Parallelism, 1) != 1 || ptr.Deref(j.Spec.BackoffLimit, 6) != 0 || j.Spec.TTLSecondsAfterFinished != nil || !apiequality.Semantic.DeepEqual(j.Spec.PodFailurePolicy, NativeJob(a, r).Spec.PodFailurePolicy) || ptr.Deref(j.Spec.PodReplacementPolicy, batchv1.TerminatingOrFailed) != batchv1.Failed || j.Spec.CompletionMode != nil && *j.Spec.CompletionMode != batchv1.NonIndexedCompletion || ptr.Deref(j.Spec.Suspend, false) {
		return fmt.Errorf("native Job identity or execution policy changed")
	}
	return ValidateNativePodSpec(&j.Spec.Template.Spec, a, r)
}
func ValidateNativePVC(p *corev1.PersistentVolumeClaim, a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe, jobUID types.UID) error {
	expected := NativePVC(a, r, jobUID)
	if p.Name != r.ResourceName || p.Namespace != r.Namespace || !p.DeletionTimestamp.IsZero() || !apiequality.Semantic.DeepEqual(p.OwnerReferences, expected.OwnerReferences) || !containsFinalizer(p.Finalizers, exportprotocol.AttemptFinalizer) {
		return fmt.Errorf("native PVC identity changed")
	}
	if ref := p.Spec.DataSourceRef; ref != nil {
		if ref.APIGroup == nil || *ref.APIGroup != "snapshot.storage.k8s.io" || ref.Kind != "VolumeSnapshot" || ref.Name != r.Snapshot.Name || (ref.Namespace != nil && *ref.Namespace != r.Namespace) {
			return fmt.Errorf("native PVC dataSourceRef changed")
		}
	}
	actual := p.Spec.DeepCopy()
	actual.VolumeName = ""
	actual.DataSourceRef = nil
	if !apiequality.Semantic.DeepEqual(actual, &expected.Spec) {
		return fmt.Errorf("native PVC recipe changed")
	}
	return nil
}
func containsFinalizer(fs []string, f string) bool {
	for _, v := range fs {
		if v == f {
			return true
		}
	}
	return false
}

func NativeCleanupJob(a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe) *batchv1.Job {
	j := NativeJob(a, r)
	j.Name = "delete-" + a.ID
	j.Spec.Template.Spec.Containers[0].Args[0] = "native-cleanup"
	j.Spec.Template.Spec.Containers[0].WorkingDir = "/app"
	j.Spec.Template.Spec.Containers[0].VolumeMounts = j.Spec.Template.Spec.Containers[0].VolumeMounts[1:]
	j.Spec.Template.Spec.Volumes = j.Spec.Template.Spec.Volumes[1:]
	return j
}
func ValidateNativeCleanupPodSpec(actual *corev1.PodSpec, a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe) error {
	if len(actual.Containers) != 1 {
		return fmt.Errorf("native cleanup requires one fixed container")
	}
	expected := NativeCleanupJob(a, r).Spec.Template.Spec
	p := &corev1.Pod{Spec: *actual.DeepCopy()}
	e := &corev1.Pod{Spec: expected}
	scheme.Scheme.Default(p)
	scheme.Scheme.Default(e)
	// Reuse the same strict worker validator by restoring only the fixed data-free differences.
	normalized := p.Spec.DeepCopy()
	base := NativeJob(a, r).Spec.Template.Spec
	normalized.Containers[0].Args = base.Containers[0].Args
	normalized.Containers[0].WorkingDir = base.Containers[0].WorkingDir
	normalized.Containers[0].VolumeMounts = append([]corev1.VolumeMount{base.Containers[0].VolumeMounts[0]}, normalized.Containers[0].VolumeMounts...)
	normalized.Volumes = append([]corev1.Volume{base.Volumes[0]}, normalized.Volumes...)
	if err := normalizeNativeResources(p.Spec.Containers, e.Spec.Containers); err != nil {
		return err
	}
	if !apiequality.Semantic.DeepEqual(p.Spec.Containers, e.Spec.Containers) || !apiequality.Semantic.DeepEqual(p.Spec.Volumes, e.Spec.Volumes) {
		return fmt.Errorf("native cleanup worker changed")
	}
	return ValidateNativePodSpec(normalized, a, r)
}
func ValidateNativeCleanupJob(j *batchv1.Job, a *appsv1.SnapshotExportAttempt, r exportprotocol.Recipe) error {
	if j.Name != "delete-"+a.ID {
		return fmt.Errorf("native cleanup Job name changed")
	}
	if err := ValidateNativeCleanupPodSpec(&j.Spec.Template.Spec, a, r); err != nil {
		return err
	}
	normalized := j.DeepCopy()
	normalized.Name = r.ResourceName
	normalized.Spec.Template.Spec = NativeJob(a, r).Spec.Template.Spec
	return ValidateNativeJob(normalized, a, r)
}

func normalizeNativeResources(actual, expected []corev1.Container) error {
	if len(actual) != len(expected) {
		return fmt.Errorf("native container count changed")
	}
	for i := range actual {
		got, want := &actual[i].Resources, &expected[i].Resources
		if !apiequality.Semantic.DeepEqual(got.Claims, want.Claims) {
			return fmt.Errorf("native resource claims changed")
		}
		for _, pair := range [][2]corev1.ResourceList{{got.Requests, want.Requests}, {got.Limits, want.Limits}} {
			for key, minimum := range pair[1] {
				quantity, ok := pair[0][key]
				if !ok || quantity.Cmp(minimum) < 0 {
					return fmt.Errorf("native explicit resource %s lowered or removed", key)
				}
			}
			for key, quantity := range pair[0] {
				switch key {
				case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
				default:
					frozen, ok := pair[1][key]
					if !ok || quantity.Cmp(frozen) != 0 {
						return fmt.Errorf("unsupported native resource mutation %s", key)
					}
				}
			}
		}
		*got = *want.DeepCopy()
	}
	return nil
}
