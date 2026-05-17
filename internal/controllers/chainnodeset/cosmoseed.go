package chainnodeset

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

func (r *Reconciler) ensureSeedNodes(ctx context.Context, nodeSet *v1.ChainNodeSet) error {
	logger := log.FromContext(ctx)

	if !nodeSet.Spec.Cosmoseed.IsEnabled() {
		return r.maybeCleanupSeedNodes(ctx, nodeSet)
	}

	configHash, err := r.ensureCosmoseedConfig(ctx, nodeSet)
	if err != nil {
		return err
	}

	seedStatus := make([]v1.SeedStatus, nodeSet.Spec.Cosmoseed.GetInstances())

	ids, err := r.ensureCosmoseedNodeKeys(ctx, nodeSet)
	if err != nil {
		return err
	}

	publicAddresses, err := r.ensureSeedServices(ctx, nodeSet, ids)
	if err != nil {
		return err
	}

	for i, id := range ids {
		seedName := fmt.Sprintf("%s-seed-%d", nodeSet.Name, i)
		seedStatus[i] = v1.SeedStatus{
			Name:          seedName,
			ID:            id,
			PublicAddress: publicAddresses[i],
		}
	}

	// Filter out empty entries — when the Gateway has not yet been assigned an address,
	// publicAddresses[i] is empty and would otherwise produce a malformed EXTERNAL_ADDRESS
	// like ",," in the StatefulSet env var, forcing a redundant rollout once the Gateway
	// is ready.
	knownPublicAddresses := make([]string, 0, len(publicAddresses))
	for _, addr := range publicAddresses {
		if addr != "" {
			knownPublicAddresses = append(knownPublicAddresses, addr)
		}
	}

	ss, err := r.getStatefulSet(nodeSet, configHash, RemoveIdFromFullAddresses(knownPublicAddresses))
	if err != nil {
		return err
	}

	if err = r.ensureStatefulSet(ctx, ss); err != nil {
		return err
	}

	seedRouteName := fmt.Sprintf("%s-seed", nodeSet.GetName())
	if nodeSet.Spec.Cosmoseed.Ingress != nil {
		ing, err := r.getCosmoseedIngress(nodeSet)
		if err != nil {
			return err
		}
		if err = r.ensureIngress(ctx, ing); err != nil {
			return err
		}
		// Clean up any lingering HTTPRoute from a previous Gateway config
		if err = r.Delete(ctx, &gwapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      seedRouteName,
				Namespace: nodeSet.GetNamespace(),
			},
		}); err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
			return err
		}
	} else if nodeSet.Spec.Cosmoseed.Gateway != nil {
		route, err := r.getCosmoseedHTTPRoute(nodeSet)
		if err != nil {
			return err
		}
		if err = controllers.EnsureHTTPRoute(ctx, r.Client, route); err != nil {
			return err
		}
		// Clean up any lingering Ingress from a previous Ingress config
		if err = r.Delete(ctx, &netv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      seedRouteName,
				Namespace: nodeSet.GetNamespace(),
			},
		}); err != nil && !errors.IsNotFound(err) {
			return err
		}
	} else {
		// Neither ingress nor gateway configured — clean up both
		if err := r.Delete(ctx, &netv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      seedRouteName,
				Namespace: nodeSet.GetNamespace(),
			},
		}); err != nil && !errors.IsNotFound(err) {
			return err
		}
		if err := r.Delete(ctx, &gwapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      seedRouteName,
				Namespace: nodeSet.GetNamespace(),
			},
		}); err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
			return err
		}
	}

	if !reflect.DeepEqual(nodeSet.Status.Seeds, seedStatus) {
		logger.Info("updating seeds status")
		nodeSet.Status.Seeds = seedStatus
		return r.Status().Update(ctx, nodeSet)
	}

	return nil
}

func (r *Reconciler) maybeCleanupSeedNodes(ctx context.Context, nodeSet *v1.ChainNodeSet) error {
	// Cleanup statefulset
	logger := log.FromContext(ctx)

	// Cleanup statefulset
	if err := r.Delete(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
		},
	}); err != nil && !errors.IsNotFound(err) {
		return err
	}

	// Cleanup ingress
	if err := r.Delete(ctx, &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
		},
	}); err != nil && !errors.IsNotFound(err) {
		return err
	}

	// Cleanup httproute
	if err := r.Delete(ctx, &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
		},
	}); err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
		return err
	}

	// Cleanup TCPRoutes by label (can't use GetInstances() since it returns 0 when disabled)
	tcpRouteList := &gwapiv1a2.TCPRouteList{}
	if err := r.List(ctx, tcpRouteList, client.InNamespace(nodeSet.GetNamespace()), client.MatchingLabels{
		controllers.LabelApp:          controllers.CosmoseedName,
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}); err != nil {
		if !controllers.IsCRDNotInstalled(err) {
			return err
		}
	} else {
		for _, route := range tcpRouteList.Items {
			if err := r.Delete(ctx, &route); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}

	// Cleanup services
	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, client.InNamespace(nodeSet.GetNamespace()), client.MatchingLabels{
		controllers.LabelApp:          controllers.CosmoseedName,
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}); err != nil {
		return err
	}
	for _, svc := range svcList.Items {
		logger.Info("deleting stale service", "name", svc.Name)
		if err := r.Delete(ctx, &svc); err != nil {
			return err
		}
	}

	if len(nodeSet.Status.Seeds) != 0 {
		nodeSet.Status.Seeds = nil
		return r.Status().Update(ctx, nodeSet)
	}
	return nil
}

func (r *Reconciler) ensureCosmoseedNodeKeys(ctx context.Context, nodeSet *v1.ChainNodeSet) ([]string, error) {
	logger := log.FromContext(ctx)

	ids := make([]string, nodeSet.Spec.Cosmoseed.GetInstances())

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-cosmoseed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
		},
	}

	err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	if err != nil {
		if errors.IsNotFound(err) {
			secret.Data = make(map[string][]byte, nodeSet.Spec.Cosmoseed.GetInstances())
			for i := 0; i < nodeSet.Spec.Cosmoseed.GetInstances(); i++ {
				keyName := fmt.Sprintf("%s-seed-%d", nodeSet.GetName(), i)

				id, key, err := cometbft.GenerateNodeKey()
				if err != nil {
					return nil, err
				}
				secret.Data[keyName] = key
				ids[i] = id
			}

			logger.Info("creating secret")
			return ids, r.Create(ctx, secret)
		}
		return nil, err
	}

	needsUpdate := false
	if secret.Data == nil {
		secret.Data = make(map[string][]byte, nodeSet.Spec.Cosmoseed.GetInstances())
	}

	for i := 0; i < nodeSet.Spec.Cosmoseed.GetInstances(); i++ {
		keyName := fmt.Sprintf("%s-seed-%d", nodeSet.GetName(), i)
		generateNew := false

		key, ok := secret.Data[keyName]
		if !ok {
			generateNew = true
		}
		if ids[i], err = cometbft.GetNodeID(key); err != nil {
			logger.Info("error getting node id. generating new", "key", keyName, "err", err)
			generateNew = true
		}
		if generateNew {
			needsUpdate = true
			ids[i], key, err = cometbft.GenerateNodeKey()
			if err != nil {
				return nil, err
			}
			secret.Data[keyName] = key
		}
	}

	if needsUpdate {
		logger.Info("updating secret")
		return ids, r.Update(ctx, secret)
	}

	return ids, nil
}

func (r *Reconciler) ensureCosmoseedConfig(ctx context.Context, nodeSet *v1.ChainNodeSet) (string, error) {
	hash, cm, err := r.getCosmoseedConfigMap(ctx, nodeSet)
	if err != nil {
		return "", err
	}
	return hash, r.ensureConfigMap(ctx, cm)
}

func (r *Reconciler) getCosmoseedConfigMap(ctx context.Context, nodeSet *v1.ChainNodeSet) (string, *corev1.ConfigMap, error) {
	peers, err := r.listChainPeers(ctx, nodeSet.Status.ChainID)
	if err != nil {
		return "", nil, err
	}

	var publicPeers []v1.Peer
	for _, node := range nodeSet.Status.Nodes {
		if node.Public {
			publicPeers = append(publicPeers, v1.Peer{
				ID:      node.ID,
				Address: node.PublicAddress,
				Port:    &node.PublicPort,
			})
		}
	}

	cfg, err := nodeSet.Spec.Cosmoseed.GetCosmoseedConfig(nodeSet.Status.ChainID, peers.ExcludeSeeds().Append(publicPeers).String())
	if err != nil {
		return "", nil, err
	}

	b, err := yaml.Marshal(cfg)
	if err != nil {
		return "", nil, err
	}

	spec := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-cosmoseed", nodeSet.Name),
			Namespace: nodeSet.GetNamespace(),
		},
		Data: map[string]string{cosmoseedConfigFileName: string(b)},
	}
	return utils.Sha256(string(b)), spec, controllerutil.SetControllerReference(nodeSet, spec, r.Scheme)
}

func (r *Reconciler) listChainPeers(ctx context.Context, chainID string) (v1.PeerList, error) {
	listOption := client.MatchingLabels{
		controllers.LabelChainID: chainID,
		controllers.LabelPeer:    controllers.StringValueTrue,
	}
	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, listOption); err != nil {
		return nil, err
	}

	peers := make([]v1.Peer, 0)

	for _, svc := range svcList.Items {
		peer := v1.Peer{
			ID:            svc.Labels[controllers.LabelNodeID],
			Address:       svc.Name,
			Port:          ptr.To(chainutils.P2pPort),
			Unconditional: ptr.To(true),
		}

		if svc.Labels[controllers.LabelSeed] == controllers.StringValueTrue {
			peer.Seed = ptr.To(true)
		}

		if svc.Labels[controllers.LabelValidator] == controllers.StringValueTrue {
			peer.Private = ptr.To(true)
		}

		peers = append(peers, peer)
	}

	sort.Slice(peers, func(i, j int) bool {
		return peers[i].ID < peers[j].ID
	})

	return peers, nil
}

func (r *Reconciler) getStatefulSet(nodeSet *v1.ChainNodeSet, configHash string, publicAddresses []string) (*appsv1.StatefulSet, error) {
	replicas := int32(nodeSet.Spec.Cosmoseed.GetInstances())

	labels := map[string]string{
		controllers.LabelApp:          controllers.CosmoseedName,
		controllers.LabelChainNodeSet: nodeSet.GetName(),
		controllers.LabelChainID:      nodeSet.Status.ChainID,
	}

	keysVolumeMounts := make([]corev1.VolumeMount, replicas)
	for i := range keysVolumeMounts {
		podName := fmt.Sprintf("%s-seed-%d", nodeSet.GetName(), i)
		keysVolumeMounts[i] = corev1.VolumeMount{
			Name:      "nodekey",
			ReadOnly:  true,
			MountPath: path.Join(cosmoseedMountPoint, podName),
			SubPath:   podName,
		}
	}

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						controllers.AnnotationConfigHash: configHash,
					},
				},
				Spec: corev1.PodSpec{
					PriorityClassName: r.opts.GetNodesPriorityClassName(),
					SecurityContext:   k8s.RestrictedPodSecurityContext(),
					Containers: []corev1.Container{
						{
							Name:            controllers.CosmoseedName,
							Image:           r.opts.CosmoseedImage,
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: k8s.RestrictedSecurityContext(),
							Args: []string{
								"--home", cosmoseedMountPoint,
								"--log-level", nodeSet.Spec.Cosmoseed.GetLogLevel(),
								"--config-read-only",
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "p2p",
									ContainerPort: 26656,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "http",
									ContainerPort: 8080,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name:  "EXTERNAL_ADDRESS",
									Value: strings.Join(publicAddresses, ","),
								},
							},
							Resources: nodeSet.Spec.Cosmoseed.Resources,
							VolumeMounts: append([]corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: path.Join(cosmoseedMountPoint, cosmoseedAddrBookDir),
								},
								{
									Name:      "config",
									MountPath: path.Join(cosmoseedMountPoint, cosmoseedConfigFileName),
									SubPath:   cosmoseedConfigFileName,
								},
							}, keysVolumeMounts...),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: fmt.Sprintf("%s-cosmoseed", nodeSet.Name),
									},
								},
							},
						},
						{
							Name: "nodekey",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: fmt.Sprintf("%s-cosmoseed", nodeSet.GetName()),
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			ServiceName:         fmt.Sprintf("%s-seed-headless", nodeSet.GetName()),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
		},
	}

	return ss, controllerutil.SetControllerReference(nodeSet, ss, r.Scheme)
}

func (r *Reconciler) ensureSeedServices(ctx context.Context, nodeSet *v1.ChainNodeSet, ids []string) ([]string, error) {
	// Track expected resources for cleanup
	expected := map[string]bool{}
	expectedTCPRoutes := map[string]bool{}

	// List of public addresses (empty if not exposed)
	publicAddresses := make([]string, len(ids))

	headlessSvc, err := r.getCosmoseedHeadlessServiceSpec(nodeSet)
	if err != nil {
		return nil, err
	}
	if err = r.ensureService(ctx, headlessSvc); err != nil {
		return nil, err
	}
	expected[headlessSvc.Name] = true

	mainSvc, err := r.getSeedHttpServiceSpec(nodeSet)
	if err != nil {
		return nil, err
	}
	if err = r.ensureService(ctx, mainSvc); err != nil {
		return nil, err
	}
	expected[mainSvc.Name] = true

	for i, id := range ids {
		internalSvc, err := r.getSeedInternalServiceSpec(nodeSet, id, i)
		if err != nil {
			return nil, err
		}
		if err = r.ensureService(ctx, internalSvc); err != nil {
			return nil, err
		}
		expected[internalSvc.Name] = true

		if nodeSet.Spec.Cosmoseed.Expose.Enabled() {
			if nodeSet.Spec.Cosmoseed.Expose.UsesGateway() {
				// Gateway mode: ensure TCPRoute first, then delete the stale expose service.
				// This ordering avoids a window with no P2P exposure if the Gateway API
				// call fails transiently.
				tcpRoute, err := r.getSeedTCPRouteSpec(nodeSet, internalSvc.Name, i)
				if err != nil {
					return nil, err
				}
				if err = controllers.EnsureTCPRoute(ctx, r.Client, tcpRoute); err != nil {
					return nil, err
				}
				expectedTCPRoutes[tcpRoute.Name] = true

				staleSvc, err := r.getSeedExposeServiceSpec(nodeSet, i)
				if err != nil {
					return nil, err
				}
				if err = r.Delete(ctx, staleSvc); err != nil && !errors.IsNotFound(err) {
					return nil, err
				}

				// Discover public address from Gateway status
				gwRef := nodeSet.Spec.Cosmoseed.Expose.Gateway
				gwNamespace := nodeSet.GetNamespace()
				if gwRef.Namespace != nil {
					gwNamespace = *gwRef.Namespace
				}
				gw := &gwapiv1.Gateway{}
				if err = r.Get(ctx, client.ObjectKey{Name: gwRef.Name, Namespace: gwNamespace}, gw); err != nil {
					if controllers.IsCRDNotInstalled(err) {
						log.FromContext(ctx).Info("gateway api crds not installed, skipping public address update")
					} else {
						return nil, fmt.Errorf("failed to get Gateway %s: %w", gwRef.Name, err)
					}
				} else if len(gw.Status.Addresses) > 0 {
					listenerPort := nodeSet.Spec.Cosmoseed.Expose.GetGatewayPort() + int32(i)
					publicAddresses[i] = fmt.Sprintf("%s@%s:%d", id, gw.Status.Addresses[0].Value, listenerPort)
				}
			} else {
				// LoadBalancer/NodePort mode: delete any stale TCPRoute
				if err = r.Delete(ctx, &gwapiv1a2.TCPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-seed-%d-p2p", nodeSet.GetName(), i),
						Namespace: nodeSet.GetNamespace(),
					},
				}); err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
					return nil, err
				}

				exposeSvc, err := r.getSeedExposeServiceSpec(nodeSet, i)
				if err != nil {
					return nil, err
				}
				if err = r.ensureService(ctx, exposeSvc); err != nil {
					return nil, err
				}
				expected[exposeSvc.Name] = true

				publicAddresses[i], err = r.getSeedPublicAddress(ctx, nodeSet, exposeSvc, id)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	// Cleanup
	svcList := &corev1.ServiceList{}
	if err = r.List(ctx, svcList, client.InNamespace(nodeSet.GetNamespace()), client.MatchingLabels{
		controllers.LabelApp:          controllers.CosmoseedName,
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}); err != nil {
		return nil, err
	}

	for _, svc := range svcList.Items {
		if !expected[svc.Name] {
			log.FromContext(ctx).Info("deleting stale service", "name", svc.Name)
			if err := r.Delete(ctx, &svc); err != nil {
				return nil, err
			}
		}
	}

	// Cleanup stale TCPRoutes (e.g., after scale-down or switching from gateway to service mode)
	tcpRouteList := &gwapiv1a2.TCPRouteList{}
	if err = r.List(ctx, tcpRouteList, client.InNamespace(nodeSet.GetNamespace()), client.MatchingLabels{
		controllers.LabelApp:          controllers.CosmoseedName,
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}); err != nil {
		if !controllers.IsCRDNotInstalled(err) {
			return nil, err
		}
	} else {
		for _, route := range tcpRouteList.Items {
			if !expectedTCPRoutes[route.Name] {
				log.FromContext(ctx).Info("deleting stale tcproute", "name", route.Name)
				if err := r.Delete(ctx, &route); err != nil && !errors.IsNotFound(err) {
					return nil, err
				}
			}
		}
	}

	return publicAddresses, nil
}

func (r *Reconciler) getCosmoseedHeadlessServiceSpec(nodeSet *v1.ChainNodeSet) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed-headless", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
			Labels: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				controllers.LabelChainID:      nodeSet.Status.ChainID,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				controllers.LabelChainID:      nodeSet.Status.ChainID,
			},
		},
	}
	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getSeedHttpServiceSpec(nodeSet *v1.ChainNodeSet) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
			Labels: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				controllers.LabelChainID:      nodeSet.Status.ChainID,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       cosmoseedHttpPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       cosmoseedHttpPort,
					TargetPort: intstr.FromInt32(cosmoseedHttpPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				controllers.LabelChainID:      nodeSet.Status.ChainID,
			},
		},
	}
	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getSeedInternalServiceSpec(nodeSet *v1.ChainNodeSet, id string, index int) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed-%d-internal", nodeSet.GetName(), index),
			Namespace: nodeSet.GetNamespace(),
			Labels: map[string]string{
				controllers.LabelPeer:         controllers.StringValueTrue,
				controllers.LabelNodeID:       id,
				controllers.LabelSeed:         controllers.StringValueTrue,
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				controllers.LabelChainID:      nodeSet.Status.ChainID,
			},
		},
		Spec: corev1.ServiceSpec{
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.P2pPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       cosmoseedP2pPort,
					TargetPort: intstr.FromInt32(cosmoseedP2pPort),
				},
				{
					Name:       cosmoseedHttpPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       cosmoseedHttpPort,
					TargetPort: intstr.FromInt32(cosmoseedHttpPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelApp:                     controllers.CosmoseedName,
				controllers.LabelChainNodeSet:            nodeSet.GetName(),
				controllers.LabelChainID:                 nodeSet.Status.ChainID,
				controllers.AnnotationStatefulSetPodName: fmt.Sprintf("%s-seed-%d", nodeSet.GetName(), index),
			},
		},
	}
	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getSeedExposeServiceSpec(nodeSet *v1.ChainNodeSet, index int) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed-%d", nodeSet.GetName(), index),
			Namespace: nodeSet.GetNamespace(),
			Labels: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                     nodeSet.Spec.Cosmoseed.Expose.GetServiceType(),
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.P2pPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       cosmoseedP2pPort,
					TargetPort: intstr.FromInt32(cosmoseedP2pPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelApp:                     controllers.CosmoseedName,
				controllers.LabelChainNodeSet:            nodeSet.GetName(),
				controllers.LabelChainID:                 nodeSet.Status.ChainID,
				controllers.AnnotationStatefulSetPodName: fmt.Sprintf("%s-seed-%d", nodeSet.GetName(), index),
			},
		},
	}
	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getSeedPublicAddress(ctx context.Context, nodeSet *v1.ChainNodeSet, svc *corev1.Service, id string) (string, error) {
	logger := log.FromContext(ctx)

	sh := k8s.NewServiceHelper(r.ClientSet, r.RestConfig, svc)

	switch nodeSet.Spec.Cosmoseed.Expose.GetServiceType() {
	case corev1.ServiceTypeNodePort:
		// Wait for NodePort to be available
		logger.V(1).Info("waiting for nodePort to be available", "svc", svc.GetName())
		if err := sh.WaitForCondition(ctx, func(svc *corev1.Service) (bool, error) {
			return svc.Spec.Ports[0].NodePort > 0, nil
		}, timeoutWaitServiceIP); err != nil {
			return "", err
		}
		port := svc.Spec.Ports[0].NodePort

		var node *corev1.Node
		nodes, err := r.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", err
		}
		if len(nodes.Items) > 0 {
			node = &nodes.Items[0]
		}

		if node == nil {
			return "", fmt.Errorf("no node found")
		}

		var address string
		addressPriority := []corev1.NodeAddressType{
			corev1.NodeExternalIP,
			corev1.NodeExternalDNS,
			corev1.NodeInternalIP,
			corev1.NodeInternalDNS,
			corev1.NodeHostName,
		}

		for _, addrType := range addressPriority {
			for _, addr := range node.Status.Addresses {
				if addr.Type == addrType {
					address = addr.Address
					break
				}
			}
			if address != "" {
				break
			}
		}

		if address == "" {
			return "", fmt.Errorf("no address found for nodeport")
		}

		return fmt.Sprintf("%s@%s:%d", id, address, port), nil

	case corev1.ServiceTypeLoadBalancer:
		// Wait for LoadBalancer to be available
		logger.V(1).Info("waiting for load balancer address to be available", "svc", svc.GetName())
		if err := sh.WaitForCondition(ctx, func(svc *corev1.Service) (bool, error) {
			return len(svc.Status.LoadBalancer.Ingress) > 0 &&
				k8s.LoadBalancerAddress(svc.Status.LoadBalancer.Ingress[0]) != "", nil
		}, timeoutWaitServiceIP); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s@%s:%d", id, k8s.LoadBalancerAddress(svc.Status.LoadBalancer.Ingress[0]), chainutils.P2pPort), nil
	}

	return "", fmt.Errorf("unsupported service type")
}

func (r *Reconciler) getCosmoseedIngress(nodeSet *v1.ChainNodeSet) (*netv1.Ingress, error) {
	pathType := netv1.PathTypeImplementationSpecific

	ingress := &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
			Labels: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				controllers.LabelChainID:      nodeSet.Status.ChainID,
			},
			Annotations: nodeSet.Spec.Cosmoseed.Ingress.Annotations,
		},
		Spec: netv1.IngressSpec{
			IngressClassName: ptr.To(nodeSet.Spec.Cosmoseed.Ingress.GetIngressClass()),
			Rules: []netv1.IngressRule{
				{
					Host: nodeSet.Spec.Cosmoseed.Ingress.Host,
					IngressRuleValue: netv1.IngressRuleValue{
						HTTP: &netv1.HTTPIngressRuleValue{
							Paths: []netv1.HTTPIngressPath{
								{
									PathType: &pathType,
									Backend: netv1.IngressBackend{
										Service: &netv1.IngressServiceBackend{
											Name: fmt.Sprintf("%s-seed", nodeSet.GetName()),
											Port: netv1.ServiceBackendPort{
												Number: cosmoseedHttpPort,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if !nodeSet.Spec.Cosmoseed.Ingress.DisableTLS {
		secretName := fmt.Sprintf("%s-seed-tls", nodeSet.GetName())
		if nodeSet.Spec.Cosmoseed.Ingress.TlsSecretName != nil {
			secretName = *nodeSet.Spec.Cosmoseed.Ingress.TlsSecretName
		}

		ingress.Spec.TLS = []netv1.IngressTLS{
			{
				Hosts:      []string{nodeSet.Spec.Cosmoseed.Ingress.Host},
				SecretName: secretName,
			},
		}
	}

	return ingress, controllerutil.SetControllerReference(nodeSet, ingress, r.Scheme)
}

func (r *Reconciler) getSeedTCPRouteSpec(nodeSet *v1.ChainNodeSet, backendSvcName string, index int) (*gwapiv1a2.TCPRoute, error) {
	port := gwapiv1.PortNumber(cosmoseedP2pPort)
	gwRef := nodeSet.Spec.Cosmoseed.Expose.Gateway
	var namespace *gwapiv1.Namespace
	if gwRef.Namespace != nil {
		ns := gwapiv1.Namespace(*gwRef.Namespace)
		namespace = &ns
	}
	// Each seed instance attaches to a distinct Gateway listener at base+index so multiple
	// seeds can coexist on a single Gateway (L4 routes have no SNI to disambiguate).
	listenerPort := nodeSet.Spec.Cosmoseed.Expose.GetGatewayPort() + int32(index)
	parentRef := gwapiv1.ParentReference{
		Name:      gwapiv1.ObjectName(gwRef.Name),
		Namespace: namespace,
		Port:      ptr.To(gwapiv1.PortNumber(listenerPort)),
	}

	route := &gwapiv1a2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed-%d-p2p", nodeSet.GetName(), index),
			Namespace: nodeSet.GetNamespace(),
			Labels: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
			},
		},
		Spec: gwapiv1a2.TCPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{parentRef},
			},
			Rules: []gwapiv1a2.TCPRouteRule{
				{
					BackendRefs: []gwapiv1a2.BackendRef{
						{
							BackendObjectReference: gwapiv1.BackendObjectReference{
								Name: gwapiv1.ObjectName(backendSvcName),
								Port: ptr.To(port),
							},
						},
					},
				},
			},
		},
	}
	return route, controllerutil.SetControllerReference(nodeSet, route, r.Scheme)
}
