package chainnode

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

type disruptionBudgetExhaustedError struct {
	Unavailable int
	Maximum     int
	Namespace   string
	Labels      string
}

func (e *disruptionBudgetExhaustedError) Error() string {
	return fmt.Sprintf("disruption budget exhausted: %d/%d pods unavailable in namespace %q for labels %q",
		e.Unavailable, e.Maximum, e.Namespace, e.Labels)
}

type lockManager struct {
	active map[string]struct{}
	mu     sync.Mutex
}

func newLockManager() *lockManager {
	return &lockManager{active: make(map[string]struct{})}
}

func generateLockKey(l map[string]string) string {
	var keys []string
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for _, k := range keys {
		builder.WriteString(k)
		builder.WriteByte('=')
		builder.WriteString(l[k])
		builder.WriteByte(',')
	}
	return builder.String()
}

func (lm *lockManager) tryAcquire(namespace string, labels map[string]string) (func(), bool) {
	key := namespace + "\x00" + generateLockKey(labels)
	lm.mu.Lock()
	if _, exists := lm.active[key]; exists {
		lm.mu.Unlock()
		return nil, false
	}
	lm.active[key] = struct{}{}
	lm.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			lm.mu.Lock()
			delete(lm.active, key)
			lm.mu.Unlock()
		})
	}, true
}

func (r *Reconciler) checkDisruptionAllowance(ctx context.Context, namespace string, l map[string]string) error {
	logger := log.FromContext(ctx)

	podsList, err := r.listPodsWithLabels(ctx, namespace, l)
	if err != nil {
		return err
	}
	unavailable := unavailablePodCount(podsList)

	logger.V(1).Info("disruption check", "unavailable", unavailable, "labels", l)
	if unavailable >= r.opts.DisruptionMaxUnavailable {
		return &disruptionBudgetExhaustedError{
			Unavailable: unavailable, Maximum: r.opts.DisruptionMaxUnavailable,
			Namespace: namespace, Labels: generateLockKey(l),
		}
	}
	return nil
}

func (r *Reconciler) freshDisruptionTarget(ctx context.Context, current *corev1.Pod) (*corev1.Pod, error) {
	fresh := &corev1.Pod{}
	err := r.reservationReader().Get(ctx, client.ObjectKeyFromObject(current), fresh)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fresh.UID != current.UID {
		return nil, nil
	}
	return fresh, nil
}

func (r *Reconciler) setPodRecreationDeferred(ctx context.Context, node *appsv1.ChainNode, reason, message string) error {
	previous := apiMeta.FindStatusCondition(node.Status.Conditions, appsv1.ConditionPodRecreationDeferred)
	transition := previous == nil || previous.Reason != reason
	changed := apiMeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type: appsv1.ConditionPodRecreationDeferred, Status: metav1.ConditionTrue,
		Reason: reason, Message: message, ObservedGeneration: node.Generation,
	})
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, node); err != nil {
		return err
	}
	if transition {
		r.recorder.Event(node, corev1.EventTypeNormal, reason, message)
	}
	return nil
}

func (r *Reconciler) clearPodRecreationDeferred(ctx context.Context, node *appsv1.ChainNode) error {
	if !apiMeta.RemoveStatusCondition(&node.Status.Conditions, appsv1.ConditionPodRecreationDeferred) {
		return nil
	}
	return r.Status().Update(ctx, node)
}

func (r *Reconciler) listPodsWithLabels(ctx context.Context, namespace string, l map[string]string) (*corev1.PodList, error) {
	podList := &corev1.PodList{}
	return podList, r.reservationReader().List(ctx, podList,
		client.InNamespace(namespace), client.MatchingLabels(l))
}

func unavailablePodCount(podList *corev1.PodList) int {
	unavailable := 0
	for _, pod := range podList.Items {
		if !isPodRunningAndReady(&pod) {
			unavailable++
		}
	}

	return unavailable
}

func isPodRunningAndReady(pod *corev1.Pod) bool {
	// Check if the pod's phase is "Running"
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	// Check the pod's "Ready" condition is "True"
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
			break
		}
	}

	if !ready {
		return false
	}

	// Check if all containers are ready
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if !containerStatus.Ready || containerStatus.State.Running == nil {
			return false
		}
	}

	// Check if node-utils sidecar is ready
	for _, containerStatus := range pod.Status.InitContainerStatuses {
		if containerStatus.Name == nodeUtilsContainerName {
			if !containerStatus.Ready || containerStatus.State.Running == nil {
				return false
			}
		}
	}

	return true
}
