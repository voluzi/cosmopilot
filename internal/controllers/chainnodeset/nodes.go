package chainnodeset

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/pkg/informer"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

func (r *Reconciler) ensureNodes(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	logger := log.FromContext(ctx)

	if nodeSet.Status.ChainID == "" {
		// let's wait for chainID to be available so we have a genesis to proceed
		return nil
	}

	totalInstances := 0
	nodeSetCopy := nodeSet.DeepCopy()

	// The legacy singleton .spec.validator adds one instance. Group validators are
	// already counted via group.GetInstances() below.
	if nodeSet.Spec.Validator != nil {
		totalInstances += 1
	}

	// Grab list of all nodeset nodes
	chainNodes, err := r.listNodeSetNodes(ctx, nodeSet)
	if err != nil {
		return err
	}

	// Create map of group nodes to track deleted ones
	groupList := map[string]int{}
	for _, node := range chainNodes.Items {
		if group, ok := node.Labels[controllers.LabelChainNodeSetGroup]; ok {
			groupList[group]++
		}
	}
	// Exclude validator group
	delete(groupList, validatorGroupName)

	// Groups that are now validator-only. Their ChainNodes are reconciled by ensureValidator, so
	// they are excluded from the index-based deleted-group cleanup below. They still need any
	// regular ChainNodes left over from a previous regular-group config removed (see further down).
	validatorGroups := map[string]struct{}{}

	for _, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil {
			if err := r.ensureNodeGroup(ctx, nodeSet, group); err != nil {
				return err
			}
		} else {
			validatorGroups[group.Name] = struct{}{}
		}
		totalInstances += group.GetInstances()
		delete(groupList, group.Name)
	}

	// Remove nodes from deleted groups
	for group, count := range groupList {
		for i := 0; i < count; i++ {
			nodeName := fmt.Sprintf("%s-%s-%d", nodeSet.GetName(), group, i)
			logger.Info("removing chainnode", "group", group, "chainnode", nodeName)
			if err := r.removeNode(ctx, nodeSet, group, i); err != nil {
				return err
			}
		}
	}

	// When a group is changed from a regular group to a validator group (e.g. 3 regular instances
	// to a single validator), ensureValidator reconciles the validator ChainNodes and removes stale
	// ones labelled validator=true, but it leaves behind the old regular ChainNodes (which carry no
	// validator label). Remove those here so they do not linger. The validator ChainNodes for these
	// groups are kept: they are labelled validator=true and are the desired state.
	for _, node := range chainNodes.Items {
		group, ok := node.Labels[controllers.LabelChainNodeSetGroup]
		if !ok {
			continue
		}
		if _, isValidatorGroup := validatorGroups[group]; !isValidatorGroup {
			continue
		}
		if node.Labels[controllers.LabelChainNodeSetValidator] == controllers.StringValueTrue {
			continue
		}
		logger.Info("removing stale regular chainnode from validator group", "group", group, "chainnode", node.Name)
		if err := r.maybeDeleteNode(ctx, nodeSet, node.Name); err != nil {
			return err
		}
	}

	// Assign the instance count before the comparison so a validator-only change (which does not
	// touch node status here) is still detected and persisted.
	nodeSet.Status.Instances = totalInstances
	if !reflect.DeepEqual(nodeSet.Status, nodeSetCopy.Status) {
		log.FromContext(ctx).Info("updating .status.instances", "instances", totalInstances)
		return r.Status().Update(ctx, nodeSet)
	}
	return nil
}

func (r *Reconciler) listNodeSetNodes(ctx context.Context, nodeSet *appsv1.ChainNodeSet, l ...string) (*appsv1.ChainNodeList, error) {
	if len(l)%2 != 0 {
		return nil, fmt.Errorf("list of labels must contain pairs of key-value")
	}

	selectorMap := map[string]string{controllers.LabelChainNodeSet: nodeSet.GetName()}
	for i := 0; i < len(l); i += 2 {
		selectorMap[l[i]] = l[i+1]
	}

	chainNodeList := &appsv1.ChainNodeList{}
	return chainNodeList, r.List(ctx, chainNodeList, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(selectorMap),
	})
}

func (r *Reconciler) ensureNodeGroup(ctx context.Context, nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec) error {
	logger := log.FromContext(ctx)

	chainNodeList, err := r.listNodeSetNodes(ctx, nodeSet, controllers.LabelChainNodeSetGroup, group.Name)
	if err != nil {
		return err
	}

	currentSize := len(chainNodeList.Items)
	desiredSize := group.GetInstances()

	// Remove ChainNodes if necessary
	for i := currentSize - 1; i >= desiredSize; i-- {
		nodeName := fmt.Sprintf("%s-%s-%d", nodeSet.GetName(), group.Name, i)
		logger.Info("removing chainnode", "group", group.Name, "chainnode", nodeName)
		if err := r.removeNode(ctx, nodeSet, group.Name, i); err != nil {
			return err
		}
	}

	for i := 0; i < desiredSize; i++ {
		node, err := r.getNodeSpec(nodeSet, group, i)
		if err != nil {
			return err
		}
		if err := r.ensureNode(ctx, nodeSet, node, waitNone); err != nil {
			return err
		}

		nodeStatus := appsv1.ChainNodeSetNodeStatus{
			Name:    node.Name,
			ID:      node.Status.NodeID,
			Address: node.Status.IP,
			Port:    chainutils.P2pPort,
			Seed:    node.Status.SeedMode,
			Group:   group.Name,
		}
		if host, port, ok := parsePublicAddress(node.Status.PublicAddress); ok {
			nodeStatus.Public = true
			nodeStatus.PublicAddress = host
			nodeStatus.PublicPort = port
		}
		AddOrUpdateNodeStatus(nodeSet, nodeStatus)
	}
	return nil
}

// chainNodeWait selects what condition (if any) ensureNode blocks on after creating or
// updating a ChainNode.
type chainNodeWait int

const (
	// waitNone returns as soon as the ChainNode is reconciled.
	waitNone chainNodeWait = iota

	// waitRunningOrSyncing blocks until the ChainNode is running, syncing or snapshotting.
	waitRunningOrSyncing

	// waitGenesisReady blocks only until the ChainNode has produced a genesis (its chainID is
	// populated). This is used for genesis-initializing validators in a multi-validator group:
	// such a chain only produces blocks once every group validator is online, so blocking on
	// "running" would deadlock (the remaining validators are created only after this wait
	// returns). The genesis (and its ConfigMap) exists as soon as the chainID is set.
	waitGenesisReady
)

func (r *Reconciler) ensureNode(ctx context.Context, nodeSet *appsv1.ChainNodeSet, node *appsv1.ChainNode, wait chainNodeWait) error {
	logger := log.FromContext(ctx)

	currentNode := &appsv1.ChainNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), currentNode); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating chainnode", "chainnode", node.GetName())
			if err := r.Create(ctx, node); err != nil {
				return err
			}
			r.recorder.Eventf(nodeSet,
				corev1.EventTypeNormal,
				appsv1.ReasonNodeCreated,
				"ChainNode %s created",
				node.GetName(),
			)
			return r.waitForChainNode(node, wait)
		}
		return err
	}

	// Require overrideVersion to be removed individually from each node
	if currentNode.Spec.OverrideVersion != nil && node.Spec.OverrideVersion == nil {
		node.Spec.OverrideVersion = currentNode.Spec.OverrideVersion
	}

	if !currentNode.Equal(node) {
		logger.Info("updating chainnode", "chainnode", node.GetName())
		node.ObjectMeta.ResourceVersion = currentNode.ObjectMeta.ResourceVersion
		node.Annotations = currentNode.Annotations
		if err := r.Update(ctx, node); err != nil {
			return err
		}
		r.recorder.Eventf(nodeSet,
			corev1.EventTypeNormal,
			appsv1.ReasonNodeUpdated,
			"ChainNode %s updated",
			node.GetName(),
		)
	} else {
		*node = *currentNode
	}

	return r.waitForChainNode(node, wait)
}

// waitForChainNode blocks until the ChainNode satisfies the given wait condition, returning
// immediately for waitNone.
func (r *Reconciler) waitForChainNode(node *appsv1.ChainNode, wait chainNodeWait) error {
	switch wait {
	case waitRunningOrSyncing:
		return r.waitChainNode(node, func(cn *appsv1.ChainNode) bool {
			return cn.Status.Phase == appsv1.PhaseChainNodeRunning ||
				cn.Status.Phase == appsv1.PhaseChainNodeSyncing ||
				cn.Status.Phase == appsv1.PhaseChainNodeSnapshotting
		})
	case waitGenesisReady:
		return r.waitChainNode(node, func(cn *appsv1.ChainNode) bool {
			return cn.Status.ChainID != ""
		})
	default:
		return nil
	}
}

func (r *Reconciler) removeNode(ctx context.Context, nodeSet *appsv1.ChainNodeSet, group string, index int) error {
	nodeName := fmt.Sprintf("%s-%s-%d", nodeSet.GetName(), group, index)
	if err := r.Delete(ctx, &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Namespace: nodeSet.GetNamespace()}}); err != nil {
		return err
	}
	DeleteNodeStatus(nodeSet, nodeName)

	r.recorder.Eventf(nodeSet,
		corev1.EventTypeNormal,
		appsv1.ReasonNodeDeleted,
		"ChainNode %s removed",
		nodeName,
	)

	return nil
}

func (r *Reconciler) getNodeSpec(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec, index int) (*appsv1.ChainNode, error) {
	var genesisConfig *appsv1.GenesisConfig
	if nodeSet.Spec.Genesis.ShouldDownloadUsingContainer() || nodeSet.Spec.Genesis.HasConfigMapSource() {
		genesisConfig = nodeSet.Spec.Genesis
	} else {
		genesisConfig = &appsv1.GenesisConfig{
			ConfigMap: ptr.To(nodeSet.Spec.Genesis.GetConfigMapName(nodeSet.Status.ChainID)),
		}
	}

	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%d", nodeSet.GetName(), group.Name, index),
			Namespace: nodeSet.GetNamespace(),
			// Stamp the internal validator label false explicitly. WithChainNodeSetLabels copies the
			// ChainNodeSet's user labels onto this ChainNode; if the parent carries a user label
			// "validator=true", it would otherwise make the validator cleanup selector treat this
			// regular node as a stale validator and delete it. Setting it here ensures the internal
			// semantics win over any inherited user label (MergeMaps lets the explicit value override).
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet:          nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup:     group.Name,
				controllers.LabelChainNodeSetValidator: strconv.FormatBool(false),
			}),
		},
		Spec: appsv1.ChainNodeSpec{
			Genesis:                       genesisConfig,
			App:                           nodeSet.GetAppSpecWithUpgrades(),
			Config:                        group.Config,
			Persistence:                   group.Persistence.DeepCopy(),
			Peers:                         group.Peers,
			Expose:                        exposeForInstance(group.Expose, index),
			Resources:                     group.Resources,
			Affinity:                      group.Affinity,
			NodeSelector:                  group.NodeSelector,
			StateSyncRestore:              group.StateSyncRestore,
			StateSyncResources:            group.StateSyncResources,
			IgnoreGroupOnDisruptionChecks: group.IgnoreGroupOnDisruptionChecks,
			VPA:                           group.VPA,
			OverrideVersion:               group.OverrideVersion,
		},
	}

	// Mark nodes of groups targeted by a managed cosmosigner deployment as remote-signer targets, so
	// they listen for the signer and carry its discovery-service selector label (valued with the signer
	// name so that signer's service selects the targeted group's pods).
	if signerName, ok := signerNameForNode(nodeSet, group.Name, index); ok {
		node.Spec.RemoteSignerTarget = true
		node.Labels = utils.MergeMaps(node.Labels, map[string]string{
			controllers.LabelCosmosignerTarget: signerName,
		})
	}

	if group.IndividualIngresses != nil {
		node.Spec.Ingress = group.IndividualIngresses.DeepCopy()
		node.Spec.Ingress.Host = fmt.Sprintf("%d.%s", index, group.IndividualIngresses.Host)
	}

	if group.IndividualGatewayRoutes != nil {
		node.Spec.Gateway = group.IndividualGatewayRoutes.DeepCopy()
		node.Spec.Gateway.Host = fmt.Sprintf("%d.%s", index, group.IndividualGatewayRoutes.Host)
	}

	if nodeSet.HasValidator() && group.ShouldInheritValidatorGasPrice() {
		price := nodeSet.GetValidatorMinimumGasPrices()
		if price != "" {
			data := map[string]string{
				controllers.MinimumGasPricesKey: price,
			}
			dataBytes, err := json.Marshal(data)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal minimum gas prices: %w", err)
			}

			if node.Spec.Config == nil {
				node.Spec.Config = &appsv1.Config{
					Override: &map[string]runtime.RawExtension{
						controllers.AppTomlFile: {Raw: dataBytes},
					},
				}
			} else if node.Spec.Config.Override == nil {
				node.Spec.Config.Override = &map[string]runtime.RawExtension{
					controllers.AppTomlFile: {Raw: dataBytes},
				}
			} else {
				cfg := *node.Spec.Config.Override
				// The override may exist for other files only (no app.toml entry yet). Start from an
				// empty object in that case instead of unmarshaling a nil Raw, which would error.
				cfgData := map[string]interface{}{}
				if raw := cfg[controllers.AppTomlFile].Raw; len(raw) > 0 {
					if err := json.Unmarshal(raw, &cfgData); err != nil {
						return nil, fmt.Errorf("unmarshaling app config: %w", err)
					}
				}

				cfgData[controllers.MinimumGasPricesKey] = price
				newDataBytes, err := json.Marshal(cfgData)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal config data: %w", err)
				}
				cfg[controllers.AppTomlFile] = runtime.RawExtension{Raw: newDataBytes}
			}
		}
	}

	globalIngressLabels := map[string]string{}
	for _, ingress := range nodeSet.Spec.Ingresses {
		if ingress.HasGroup(group.Name) {
			globalIngressLabels[ingress.GetName(nodeSet)] = strconv.FormatBool(true)
		}
	}

	if len(globalIngressLabels) > 0 {
		node.Labels = utils.MergeMaps(node.Labels, globalIngressLabels)
	}

	globalGatewayLabels := map[string]string{}
	for _, gw := range nodeSet.Spec.GatewayRoutes {
		if gw.HasGroup(group.Name) {
			globalGatewayLabels[gw.GetName(nodeSet)] = strconv.FormatBool(true)
		}
	}

	if len(globalGatewayLabels) > 0 {
		node.Labels = utils.MergeMaps(node.Labels, globalGatewayLabels)
	}

	// When enabling snapshots on a group, we only do it on one node, the onde with the index indicated
	// by `.nodes[].snapshotNodeIndex`.
	if node.Spec.Persistence != nil && index != group.GetSnapshotNodeIndex() {
		node.Spec.Persistence.Snapshots = nil
	}

	return node, controllerutil.SetControllerReference(nodeSet, node, r.Scheme)
}

// waitChainNode blocks until the given ChainNode satisfies validateFunc, the wait times out, or
// the ChainNode is deleted. On success, node is updated in place with the latest observed state.
func (r *Reconciler) waitChainNode(node *appsv1.ChainNode, validateFunc func(*appsv1.ChainNode) bool) error {
	if validateFunc(node) {
		return nil
	}

	nodesInformer, err := informer.GetChainNodesInformer(r.RestConfig)
	if err != nil {
		return err
	}

	var exitErr error
	stopCh := make(chan struct{})
	once := sync.Once{}

	go func() {
		time.Sleep(ChainNodeWaitTimeout)
		exitErr = fmt.Errorf("timeout waiting for chainnode %s running or syncing status", node.GetName())
		once.Do(func() { close(stopCh) })
	}()

	if _, err := nodesInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			chainNode := &appsv1.ChainNode{}
			uns, ok := obj.(*unstructured.Unstructured)
			if !ok {
				exitErr = fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
				once.Do(func() { close(stopCh) })
				return
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.UnstructuredContent(), chainNode); err != nil {
				exitErr = fmt.Errorf("error converting unstructured to chainnode: %w", err)
				once.Do(func() { close(stopCh) })
				return
			}
			if chainNode.GetName() == node.GetName() && chainNode.GetNamespace() == node.GetNamespace() {
				*node = *chainNode
				if validateFunc(node) {
					once.Do(func() { close(stopCh) })
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			chainNode := &appsv1.ChainNode{}
			uns, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				exitErr = fmt.Errorf("expected *unstructured.Unstructured, got %T", newObj)
				once.Do(func() { close(stopCh) })
				return
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.UnstructuredContent(), chainNode); err != nil {
				exitErr = fmt.Errorf("error converting unstructured to chainnode: %w", err)
				once.Do(func() { close(stopCh) })
				return
			}
			if chainNode.GetName() == node.GetName() && chainNode.GetNamespace() == node.GetNamespace() {
				*node = *chainNode
				if validateFunc(node) {
					once.Do(func() { close(stopCh) })
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			chainNode := &appsv1.ChainNode{}
			uns, ok := obj.(*unstructured.Unstructured)
			if !ok {
				exitErr = fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
				once.Do(func() { close(stopCh) })
				return
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.UnstructuredContent(), chainNode); err != nil {
				exitErr = fmt.Errorf("error converting unstructured to chainnode: %w", err)
				once.Do(func() { close(stopCh) })
				return
			}
			if chainNode.GetName() == node.GetName() && chainNode.GetNamespace() == node.GetNamespace() {
				exitErr = fmt.Errorf("chainnode was deleted")
				once.Do(func() { close(stopCh) })
			}
		},
	}); err != nil {
		return err
	}
	nodesInformer.Informer().Run(stopCh)
	return exitErr
}

func (r *Reconciler) maybeDeleteNode(ctx context.Context, nodeSet *appsv1.ChainNodeSet, name string) error {
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nodeSet.GetNamespace(),
		},
	}
	if err := r.Delete(ctx, node); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	DeleteNodeStatus(nodeSet, name)
	return nil
}

// exposeForInstance returns the ExposeConfig that should be applied to the i-th
// instance of a group. When Gateway-based P2P exposure is used, the gateway
// listener port is offset by the instance index so each instance attaches to a
// distinct TCP listener (Port + i). All other Expose fields are shared.
func exposeForInstance(src *appsv1.ExposeConfig, index int) *appsv1.ExposeConfig {
	if src == nil || src.Gateway == nil {
		return src
	}
	out := src.DeepCopy()
	base := out.GetGatewayPort()
	port := base + int32(index)
	out.Gateway.Port = &port
	return out
}
