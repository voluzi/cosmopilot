package chainnode

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/chainutils"
	"github.com/voluzi/cosmopilot/v5/pkg/images"
)

func (r *Reconciler) buildDataInitPod(app *chainutils.App, chainNode *appsv1.ChainNode, pvc *corev1.PersistentVolumeClaim) (*corev1.Pod, error) {
	pod, err := app.BuildInitPod(pvc, r.buildAdditionalVolumes(chainNode), r.buildInitCommands(chainNode)...)
	if err != nil {
		return nil, err
	}
	if timeout := chainNode.GetPersistenceInitTimeout(); timeout > 0 {
		seconds := int64(timeout.Seconds())
		pod.Spec.ActiveDeadlineSeconds = &seconds
	}
	if chainNode.Spec.Persistence == nil || chainNode.Spec.Persistence.Restore == nil {
		return pod, nil
	}
	restore := chainNode.Spec.Persistence.Restore
	source := restore.Snapshot
	container := &pod.Spec.InitContainers[0]
	container.Name = "data-restore"
	container.Image = r.opts.GetDataExporterImage()
	container.ImagePullPolicy = images.PullPolicy(container.Image, "")
	container.Command = nil
	container.Args = []string{source.Provider, "restore"}
	if restore.Verification != nil && restore.Verification.SHA256 != "" {
		container.Args = append(container.Args, "--sha256", restore.Verification.SHA256)
	}
	container.Args = append(container.Args, "--", "/home/app/data", source.Bucket, source.Name)
	container.Resources = restore.Resources
	container.Env = nil
	pod.Spec.ServiceAccountName = source.ServiceAccountName
	switch source.Provider {
	case "s3":
		container.Env = []corev1.EnvVar{
			{Name: "S3_ENDPOINT", Value: source.Endpoint},
			{Name: "S3_FORCE_PATH_STYLE", Value: strconv.FormatBool(source.ForcePathStyle)},
		}
		if source.Region != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "AWS_REGION", Value: source.Region})
		}
		if source.CredentialsSecret != nil {
			container.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: source.CredentialsSecret.Name}}}}
		}
	case "gcs":
		if source.CredentialsSecret != nil {
			key := source.CredentialsSecret.Key
			if key == "" {
				key = "credentials.json"
			}
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: "restore-credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: source.CredentialsSecret.Name,
					Items:      []corev1.KeyToPath{{Key: key, Path: "credentials.json"}},
				}},
			})
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "restore-credentials", MountPath: "/creds", ReadOnly: true})
			container.Env = []corev1.EnvVar{{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: "/creds/credentials.json"}}
		}
	default:
		return nil, fmt.Errorf("unsupported restore provider %q", source.Provider)
	}
	return pod, nil
}
