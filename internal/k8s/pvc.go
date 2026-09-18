package k8s

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
)

type PvcHelper struct {
	client           *kubernetes.Clientset
	restConfig       *rest.Config
	pvc              *corev1.PersistentVolumeClaim
	utilityImage     string
	imagePullSecrets []corev1.LocalObjectReference
}

func NewPvcHelper(client *kubernetes.Clientset, cfg *rest.Config, pvc *corev1.PersistentVolumeClaim, utilityImage string, imagePullSecrets []corev1.LocalObjectReference) *PvcHelper {
	if utilityImage == "" {
		utilityImage = DefaultUtilityImage
	}
	return &PvcHelper{
		client:           client,
		restConfig:       cfg,
		pvc:              pvc,
		utilityImage:     utilityImage,
		imagePullSecrets: cloneLocalObjectReferences(imagePullSecrets),
	}
}

func (h *PvcHelper) WriteToFile(ctx context.Context, content, path, pc string, af *corev1.Affinity, ns map[string]string) error {
	pod := h.buildWriteFilePod(path, pc, af, ns)

	ph := NewPodHelper(h.client, h.restConfig, pod)

	// Delete the pod if it already exists
	_ = ph.Delete(ctx)

	// Delete the pod independently of the result
	defer func() { _ = ph.Delete(ctx) }()

	// Create the pod
	if err := ph.Create(ctx); err != nil {
		return err
	}

	// Wait for container to be running
	if err := ph.WaitForContainerStarted(ctx, 4*time.Minute, "busybox"); err != nil {
		return err
	}

	// Attach to container to push file content
	var input bytes.Buffer
	input.WriteString(content)
	if _, _, err := ph.Attach(ctx, "busybox", &input); err != nil {
		return err
	}

	// Wait for the pod to be running
	if err := ph.WaitForPodSucceeded(ctx, time.Minute); err != nil {
		return err
	}

	return nil
}

func (h *PvcHelper) buildWriteFilePod(path, pc string, af *corev1.Affinity, ns map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-write-file", h.pvc.GetName()),
			Namespace: h.pvc.GetNamespace(),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			// Kubelet reaps the pod after 5 min even if cosmopilot dies mid-call
			// (SIGKILL prevents `defer ph.Delete` from running).
			ActiveDeadlineSeconds: ptr.To[int64](300),
			PriorityClassName:     pc,
			Affinity:              af,
			NodeSelector:          ns,
			ImagePullSecrets:      cloneLocalObjectReferences(h.imagePullSecrets),
			SecurityContext:       RestrictedPodSecurityContext(),
			Volumes: []corev1.Volume{
				{
					Name: "pvc",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: h.pvc.GetName(),
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "busybox",
					Image:           h.utilityImage,
					Command:         []string{"/bin/sh"},
					SecurityContext: RestrictedSecurityContext(),
					Args: []string{
						"-c",
						fmt.Sprintf("cp /dev/stdin %s", filepath.Join("/pvc", path)),
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "pvc",
							MountPath: "/pvc",
						},
					},
					Stdin:     true,
					StdinOnce: true,
				},
			},
		},
	}
}

func (h *PvcHelper) DownloadGenesis(ctx context.Context, url, path, pc string, af *corev1.Affinity, ns map[string]string) error {
	pod := h.buildDownloadGenesisPod(url, path, pc, af, ns)

	ph := NewPodHelper(h.client, h.restConfig, pod)

	// Delete the pod if it already exists
	_ = ph.Delete(ctx)

	// Delete the pod independently of the result
	defer func() { _ = ph.Delete(ctx) }()

	// Create the pod
	if err := ph.Create(ctx); err != nil {
		return err
	}

	return ph.WaitForPodSucceeded(ctx, time.Hour)
}

func (h *PvcHelper) buildDownloadGenesisPod(url, path, pc string, af *corev1.Affinity, ns map[string]string) *corev1.Pod {
	destPath := filepath.Join("/pvc", path)
	cmd := buildGenesisDownloadCommand(url, destPath)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-download-genesis", h.pvc.GetName()),
			Namespace: h.pvc.GetNamespace(),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			// Kubelet reaps the pod after 75 min (download wait is 1h) even if
			// cosmopilot dies mid-call (SIGKILL prevents `defer ph.Delete`).
			ActiveDeadlineSeconds: ptr.To[int64](4500),
			PriorityClassName:     pc,
			Affinity:              af,
			NodeSelector:          ns,
			ImagePullSecrets:      cloneLocalObjectReferences(h.imagePullSecrets),
			SecurityContext:       RestrictedPodSecurityContext(),
			Volumes: []corev1.Volume{
				{
					Name: "pvc",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: h.pvc.GetName(),
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "downloader",
					Image:           h.utilityImage,
					Command:         []string{"/bin/sh"},
					SecurityContext: RestrictedSecurityContext(),
					Args:            []string{"-c", cmd},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "pvc",
							MountPath: "/pvc",
						},
					},
				},
			},
		},
	}
}

func cloneLocalObjectReferences(refs []corev1.LocalObjectReference) []corev1.LocalObjectReference {
	if refs == nil {
		return nil
	}
	return append([]corev1.LocalObjectReference(nil), refs...)
}

// buildGenesisDownloadCommand returns the appropriate download command based on URL extension.
// It auto-detects compression format (.gz, .zst) and decompresses accordingly.
func buildGenesisDownloadCommand(url, destPath string) string {
	lowerURL := strings.ToLower(url)

	switch {
	case strings.HasSuffix(lowerURL, ".gz"):
		return fmt.Sprintf("wget -qO- '%s' | gunzip > %s", url, destPath)
	case strings.HasSuffix(lowerURL, ".zst"):
		return fmt.Sprintf("wget -qO- '%s' | zstd -d > %s", url, destPath)
	default:
		return fmt.Sprintf("wget -O %s '%s'", destPath, url)
	}
}
