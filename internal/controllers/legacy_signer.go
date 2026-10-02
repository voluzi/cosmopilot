package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

// RefuseLegacyTmKMS prevents reconciliation from replacing a legacy signing path or its keys.
func RefuseLegacyTmKMS(ctx context.Context, reader client.Reader, recorder record.EventRecorder, chainNode *appsv1.ChainNode) error {
	pod := &corev1.Pod{}
	err := reader.Get(ctx, client.ObjectKeyFromObject(chainNode), pod)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	var artifacts []string
	if err == nil {
		for _, container := range pod.Spec.Containers {
			if container.Name == "tmkms" {
				artifacts = append(artifacts, fmt.Sprintf("tmkms container in Pod %s/%s", pod.Namespace, pod.Name))
				break
			}
		}
	}
	config := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: chainNode.Namespace, Name: chainNode.Name + "-tmkms"}, config); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	} else if metav1.IsControlledBy(config, chainNode) {
		artifacts = append(artifacts, fmt.Sprintf("owned ConfigMap %s/%s", config.Namespace, config.Name))
	}
	if len(artifacts) == 0 {
		return nil
	}
	err = fmt.Errorf("validator %s/%s cannot be reconciled by Cosmopilot 5: found %s; migrate to cosmosigner on Cosmopilot 4.x before upgrading, or, if already migrated to cosmosigner, delete the stale %s-tmkms ConfigMap", chainNode.Namespace, chainNode.Name, strings.Join(artifacts, " and "), chainNode.Name)
	if recorder != nil {
		recorder.Event(chainNode, corev1.EventTypeWarning, appsv1.ReasonInvalid, err.Error())
	}
	return err
}
