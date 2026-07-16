package cosmosigner

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

const (
	// labelAppName / appNameCosmosigner / labelInstance identify signer pods and back the
	// StatefulSet selector. They are separate from the caller-provided labels so the selector is
	// stable regardless of what the owner stamps on the resources.
	labelAppName       = "app.kubernetes.io/name"
	labelInstance      = "app.kubernetes.io/instance"
	appNameCosmosigner = "cosmosigner"

	// labelOwnerUID stamps the owning CR's UID on the per-pod PVCs so teardown can tell OUR raft-state
	// claims apart from those of a same-name signer owned by a different CR (a ChainNode and a
	// ChainNodeSet with the same name both derive "<name>-signer"). The instance label alone shares
	// the collided name, so it cannot distinguish owners.
	labelOwnerUID = "cosmopilot.voluzi.com/cosmosigner-owner"

	// containerName is the name of the signer container.
	containerName = "cosmosigner"

	// discoveryServiceSuffix names the headless service the signer uses to discover target nodes.
	discoveryServiceSuffix = "-privval"

	// dataVolumeName is the volumeClaimTemplate name for the per-pod state PVC. The StatefulSet
	// controller derives PVC names from it as `<dataVolumeName>-<sts>-<ordinal>`; DeletePVCs matches
	// that exact pattern, so both must stay in sync through this constant.
	dataVolumeName = "data"
)

// Params carries everything needed to render a cosmosigner deployment's resources. The owner
// (ChainNode or ChainNodeSet controller) sets owner references and applies the returned objects.
type Params struct {
	// Name is the base name for owned resources (e.g. "<owner>-signer") and the StatefulSet's
	// governing service name.
	Name      string
	Namespace string

	// OwnerUID is the owning CR's UID, stamped on the per-pod PVCs so teardown distinguishes them from
	// a same-name signer owned by a different CR. Empty in unit tests that don't exercise teardown.
	OwnerUID types.UID

	ChainID  string
	Image    string
	Replicas int32
	LogLevel string

	StateStorageSize   string
	StorageClassName   *string
	Resources          corev1.ResourceRequirements
	RaftTLSSecret      *string
	ServiceAccountName string

	Backend Backend

	// Labels are stamped on every owned resource and on signer pods.
	Labels map[string]string

	// TargetSelector selects the pods of the targeted node groups for the discovery service.
	TargetSelector map[string]string
}

// raftServiceDNS is the namespaced DNS name of the StatefulSet's governing headless service, used to
// build stable per-replica raft peer addresses. The `.svc` (no cluster domain) form resolves via the
// pod's DNS search domain, so it works regardless of the cluster's DNS domain (not just cluster.local).
func (p Params) raftServiceDNS() string {
	return fmt.Sprintf("%s.%s.svc", p.Name, p.Namespace)
}

// DiscoveryServiceName is the name of the headless service the signer points node_service at.
func (p Params) DiscoveryServiceName() string {
	return p.Name + discoveryServiceSuffix
}

// nodeServiceEndpoint is the host:port the signer dials to discover target nodes.
func (p Params) nodeServiceEndpoint() string {
	return fmt.Sprintf("%s.%s.svc:%d", p.DiscoveryServiceName(), p.Namespace, chainutils.PrivValPort)
}

// selectorLabels are the immutable labels that identify signer pods.
func (p Params) selectorLabels() map[string]string {
	return InstanceLabels(p.Name)
}

// pvcTemplateLabels are the labels stamped on the per-pod raft-state PVCs: the instance labels plus
// the owning CR's UID, so teardown can select OUR claims and never a same-name signer's owned by a
// different CR. The owner label is omitted when no UID is set (unit tests that don't touch teardown).
func (p Params) pvcTemplateLabels() map[string]string {
	return pvcOwnerLabels(p.Name, p.OwnerUID)
}

// pvcOwnerLabels returns InstanceLabels plus the owner-UID label when ownerUID is non-empty. Shared
// by the builder and the teardown selectors so they stay in lockstep.
func pvcOwnerLabels(name string, ownerUID types.UID) map[string]string {
	labels := InstanceLabels(name)
	if ownerUID != "" {
		labels[labelOwnerUID] = string(ownerUID)
	}
	return labels
}

// InstanceLabels returns the immutable labels identifying a signer instance's pods and PVCs, so the
// per-pod raft-state PVCs can be selected for cleanup on teardown.
func InstanceLabels(name string) map[string]string {
	return map[string]string{
		labelAppName:  appNameCosmosigner,
		labelInstance: name,
	}
}

// podLabels merges the caller labels with the immutable selector labels.
func (p Params) podLabels() map[string]string {
	return utils.MergeMaps(p.Labels, p.selectorLabels())
}

// BuildConfig assembles the cosmosigner config for the given replica count.
func (p Params) BuildConfig() *Config {
	cfg := &Config{
		ChainID:     p.ChainID,
		NodeService: p.nodeServiceEndpoint(),
		ConnKey:     connKeyPath,
		StateDir:    dataMountPath,
		LogLevel:    p.LogLevel,
		Backend:     p.Backend.backendConfig(),
		Raft: RaftConfig{
			BindAddr:  raftBindAddr,
			DataDir:   raftDataDir,
			Bootstrap: true,
		},
	}

	// A multi-replica cluster needs the full, identical member list on every pod. A single-replica
	// signer bootstraps a one-node cluster of itself (members omitted).
	if p.Replicas > 1 {
		members := make([]Member, 0, p.Replicas)
		for i := int32(0); i < p.Replicas; i++ {
			id := fmt.Sprintf("%s-%d", p.Name, i)
			members = append(members, Member{
				ID:      id,
				Address: fmt.Sprintf("%s.%s:%d", id, p.raftServiceDNS(), raftPort),
			})
		}
		cfg.Raft.Members = members
	}

	if p.RaftTLSSecret != nil {
		cfg.Raft.TLSCert = raftTLSMountDir + "/tls.crt"
		cfg.Raft.TLSKey = raftTLSMountDir + "/tls.key"
		cfg.Raft.TLSCA = raftTLSMountDir + "/ca.crt"
	}

	return cfg
}

// ConfigYAML renders the cosmosigner config.yaml. Callers render once per reconcile and pass the
// result to ConfigMap and StatefulSet, so the ConfigMap contents and the pod-template ROLLME hash
// always come from the same render.
func (p Params) ConfigYAML() (string, error) {
	return p.BuildConfig().Render()
}

// ConfigMap builds the ConfigMap holding the rendered config.yaml.
func (p Params) ConfigMap(configYAML string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Data: map[string]string{configFileName: configYAML},
	}
}

// RaftService builds the headless governing service that provides stable per-replica DNS for the
// raft transport.
func (p Params) RaftService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 p.selectorLabels(),
			Ports: []corev1.ServicePort{
				{
					Name:       raftPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       raftPort,
					TargetPort: intstr.FromInt32(raftPort),
				},
			},
		},
	}
}

// DiscoveryService builds the headless service the signer resolves to find target nodes. It must
// publish not-ready addresses: a targeted node blocks at startup waiting for the signer to dial
// in, so gating discovery on readiness would deadlock.
func (p Params) DiscoveryService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.DiscoveryServiceName(),
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 p.TargetSelector,
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.PrivValPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.PrivValPort,
					TargetPort: intstr.FromInt32(chainutils.PrivValPort),
				},
			},
		},
	}
}

// StatefulSet builds the signer StatefulSet. configYAML is the rendered config (from ConfigYAML),
// hashed into the pod template so a config change rolls the signer.
func (p Params) StatefulSet(configYAML string) (*appsv1.StatefulSet, error) {
	storageQty, err := resource.ParseQuantity(p.StateStorageSize)
	if err != nil {
		return nil, fmt.Errorf("bad cosmosigner stateStorageSize: %w", err)
	}

	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: p.Name},
				},
			},
		},
	}
	volumes = append(volumes, p.Backend.volumes()...)

	volumeMounts := []corev1.VolumeMount{
		{Name: dataVolumeName, MountPath: dataMountPath},
		{Name: "config", MountPath: configMountPath},
	}
	volumeMounts = append(volumeMounts, p.Backend.volumeMounts()...)

	if p.RaftTLSSecret != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "raft-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: *p.RaftTLSSecret},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "raft-tls", ReadOnly: true, MountPath: raftTLSMountDir,
		})
	}

	signer := corev1.Container{
		Name:            containerName,
		Image:           p.Image,
		SecurityContext: k8s.RestrictedSecurityContext(),
		Args:            []string{"start", "--config", configMountPath + "/" + configFileName},
		Env: []corev1.EnvVar{
			{
				Name: "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
			// node_id is the pod name; advertise resolves to this pod's stable raft DNS. Both
			// override the config file (env has higher precedence in cosmosigner).
			{Name: "COSMOSIGNER_RAFT_NODE_ID", Value: "$(POD_NAME)"},
			{Name: "COSMOSIGNER_RAFT_ADVERTISE", Value: fmt.Sprintf("$(POD_NAME).%s:%d", p.raftServiceDNS(), raftPort)},
			// Force a rollout when the rendered config changes.
			{Name: "ROLLME", Value: utils.Sha256(configYAML)},
		},
		Ports: []corev1.ContainerPort{
			{Name: raftPortName, ContainerPort: raftPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: volumeMounts,
		Resources:    p.Resources,
		// cosmosigner exposes no HTTP health endpoint, so probes are TCP against the raft
		// transport, which listens regardless of leadership.
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(raftPort)},
			},
			FailureThreshold: 60,
			PeriodSeconds:    5,
			TimeoutSeconds:   5,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(raftPort)},
			},
			FailureThreshold: 3,
			PeriodSeconds:    10,
			TimeoutSeconds:   5,
		},
	}

	containers := append([]corev1.Container{signer}, p.Backend.sidecars()...)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.Labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(p.Replicas),
			// Delete the per-pod state PVCs when the StatefulSet is deleted (including via owner
			// garbage collection when the ChainNode/ChainNodeSet is deleted), so a recreated signer
			// never reuses stale raft state. Retained on scale-down, which cannot happen anyway —
			// replicas are webhook-immutable.
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			// Raft needs a quorum to elect a leader; with OrderedReady a fresh multi-replica cluster
			// would deadlock waiting for pod-0 readiness before pod-1 starts. Parallel lets the
			// quorum form.
			PodManagementPolicy: appsv1.ParallelPodManagement,
			ServiceName:         p.Name,
			Selector:            &metav1.LabelSelector{MatchLabels: p.selectorLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: p.podLabels(),
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: p.ServiceAccountName,
					SecurityContext:    k8s.RestrictedPodSecurityContext(),
					Containers:         containers,
					Volumes:            volumes,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					// Label the per-pod PVCs so they can be selected for cleanup when the signer is
					// removed (StatefulSet PVCs are not garbage-collected automatically), including the
					// owner UID so a same-name signer owned by a different CR is never conflated.
					ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName, Labels: p.pvcTemplateLabels()},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: p.StorageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: storageQty},
						},
					},
				},
			},
		},
	}
	return sts, nil
}
