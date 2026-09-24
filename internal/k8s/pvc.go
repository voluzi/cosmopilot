package k8s

import (
	"bytes"
	"context"
	"fmt"
	neturl "net/url"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v4/pkg/images"
)

type PvcHelper struct {
	client           *kubernetes.Clientset
	restConfig       *rest.Config
	pvc              *corev1.PersistentVolumeClaim
	utilityImage     string
	imagePullSecrets []corev1.LocalObjectReference
}

const genesisDownloadTerminationGraceSeconds int64 = 10

func NewPvcHelper(client *kubernetes.Clientset, cfg *rest.Config, pvc *corev1.PersistentVolumeClaim, utilityImage string, imagePullSecrets []corev1.LocalObjectReference) *PvcHelper {
	if utilityImage == "" {
		utilityImage = images.DefaultUtilityImage
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
					ImagePullPolicy: images.PullPolicy(h.utilityImage, ""),
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

func (h *PvcHelper) DownloadGenesis(ctx context.Context, url, path string, sha *string, pc string, af *corev1.Affinity, ns map[string]string) error {
	pod := h.buildDownloadGenesisPod(url, path, sha, pc, af, ns)

	ph := NewPodHelper(h.client, h.restConfig, pod)

	deleteOptions := metav1.DeleteOptions{GracePeriodSeconds: ptr.To(genesisDownloadTerminationGraceSeconds)}
	if err := h.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete previous genesis download pod: %w", err)
	}
	// The grace period and deletion wait ensure the prior writer has stopped before Create.
	// The new download separately reclaims staging left by an earlier SIGKILL.
	if err := ph.WaitForPodDeleted(ctx, time.Minute); err != nil {
		return fmt.Errorf("wait for previous genesis download pod deletion: %w", err)
	}

	if err := ph.Create(ctx); err != nil {
		return err
	}
	defer func() { _ = ph.Delete(ctx) }()

	return ph.WaitForPodSucceeded(ctx, time.Hour)
}

func (h *PvcHelper) buildDownloadGenesisPod(url, path string, sha *string, pc string, af *corev1.Affinity, ns map[string]string) *corev1.Pod {
	destPath := filepath.Join("/pvc", path)
	args := buildGenesisDownloadCommand(url, destPath, sha)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-download-genesis", h.pvc.GetName()),
			Namespace: h.pvc.GetNamespace(),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To(genesisDownloadTerminationGraceSeconds),
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
					ImagePullPolicy: images.PullPolicy(h.utilityImage, ""),
					Command:         []string{"/bin/sh"},
					SecurityContext: RestrictedSecurityContext(),
					Args:            args,
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

const genesisDownloadScript = `set -eu
url=$1
destination=$2
compression=$3
verify_sha=$4
expected_sha=$5

destination_dir=${destination%/*}
destination_name=${destination##*/}
stage_prefix="${destination_dir}/.${destination_name}.download."
for stale_stage in "${stage_prefix}"*; do
	if [ -e "$stale_stage" ] || [ -L "$stale_stage" ]; then
		rm -rf -- "$stale_stage"
	fi
done
stage_dir=$(mktemp -d "${stage_prefix}XXXXXX")
cleanup() {
	status=$?
	trap - EXIT
	rm -rf "$stage_dir" || true
	exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

download_path="$stage_dir/download"
candidate_path="$stage_dir/genesis"
if ! wget -q -O "$download_path" "$url"; then
	echo "genesis download failed" >&2
	exit 1
fi

case "$compression" in
	gzip)
		if ! gunzip -c "$download_path" > "$candidate_path"; then
			echo "genesis gzip decompression failed" >&2
			exit 1
		fi
		;;
	zstd)
		if ! zstd -d -c "$download_path" > "$candidate_path"; then
			echo "genesis zstd decompression failed" >&2
			exit 1
		fi
		;;
	plain)
		mv "$download_path" "$candidate_path"
		;;
esac

if [ "$verify_sha" = "1" ]; then
	checksum_output=$(sha256sum "$candidate_path")
	actual_sha=${checksum_output%% *}
	if [ "$actual_sha" != "$expected_sha" ]; then
		printf 'genesis SHA256 mismatch: expected %s, got %s\n' "$expected_sha" "$actual_sha" >&2
		exit 1
	fi
fi

if [ -d "$destination" ] || [ -L "$destination" ]; then
	printf 'genesis destination must not be a directory or symlink: %s\n' "$destination" >&2
	exit 1
fi
mv -fT "$candidate_path" "$destination"
`

func buildGenesisDownloadCommand(url, destPath string, sha *string) []string {
	compression := "plain"
	switch lowerPath := strings.ToLower(genesisURLPath(url)); {
	case strings.HasSuffix(lowerPath, ".gz"):
		compression = "gzip"
	case strings.HasSuffix(lowerPath, ".zst"):
		compression = "zstd"
	}

	verifySHA := "0"
	expectedSHA := ""
	if sha != nil {
		verifySHA = "1"
		expectedSHA = *sha
	}

	return []string{"-c", genesisDownloadScript, "genesis-download", url, destPath, compression, verifySHA, expectedSHA}
}

// genesisURLPath returns the path component of a genesis URL, so a query string or fragment (as on a
// signed object-store URL) does not hide the compression extension.
func genesisURLPath(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Path
}
