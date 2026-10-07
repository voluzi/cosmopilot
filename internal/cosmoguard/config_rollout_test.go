package cosmoguard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	guardconfig "github.com/voluzi/cosmoguard/v5/pkg/cosmoguard"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

func TestConfigRolloutState(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "owner"}}
	encryptionKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "guard-cluster", Namespace: "ns"}, Data: map[string][]byte{EncryptionKeySecretKey: []byte(encryptionKey)}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rules", Namespace: "ns", UID: "rules"}, Data: map[string]string{"rules.yaml": ""}}
	p := Params{Name: "guard", Namespace: "ns", Image: "guard:5.0.0", UpstreamHost: "node.ns.svc.cluster.local", ConfigMap: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "rules"}, Key: "rules.yaml"}, EncryptionKeySecret: secret.Name, PeerServiceName: "guard-peer", Autoscaling: &AutoscalingParams{MaxReplicas: 10}, PodAnnotations: map[string]string{"user": "preserved"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, secret, cm).Build()
	apply := func(c client.Client) error { return ApplyStatefulSet(ctx, c, scheme, owner, p, p.StatefulSet()) }
	read := func() *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: p.Name}, sts))
		return sts
	}
	change := func(raw string, binary bool) {
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cm), cm))
		if binary {
			cm.Data = nil
			cm.BinaryData = map[string][]byte{"rules.yaml": []byte(raw)}
		} else {
			cm.BinaryData = nil
			cm.Data = map[string]string{"rules.yaml": raw}
		}
		require.NoError(t, c.Update(ctx, cm))
	}
	require.NoError(t, apply(c))
	initial := read()
	require.NotEmpty(t, initial.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint])
	require.NotEmpty(t, initial.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme])
	require.Empty(t, initial.Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart])

	t.Run("fingerprint is keyed against the rendered environment", func(t *testing.T) {
		env := map[string]string{"COSMOGUARD_METRICS_ENABLE": "true", "COSMOGUARD_METRICS_PORT": "9001", "COSMOGUARD_CLUSTER_ENABLE": "true", "COSMOGUARD_CLUSTER_BIND_ADDR": "192.0.2.1", "COSMOGUARD_CLUSTER_DISCOVERY_MODE": "dns", "COSMOGUARD_CLUSTER_DISCOVERY_DNS_HOST": "guard-peer.ns.svc.cluster.local", "COSMOGUARD_CLUSTER_ENCRYPTION_KEY": encryptionKey, "COSMOGUARD_NODE_HOST": p.UpstreamHost, "COSMOGUARD_DASHBOARD_ENABLE": "false"}
		cfg, err := guardconfig.ParseConfig(nil, func(name string) (string, bool) { value, ok := env[name]; return value, ok })
		require.NoError(t, err)
		raw, err := guardconfig.RestartFingerprint(cfg)
		require.NoError(t, err)
		mac := hmac.New(sha256.New, []byte(encryptionKey))
		_, err = mac.Write([]byte(raw))
		require.NoError(t, err)
		assert.Equal(t, hex.EncodeToString(mac.Sum(nil)), initial.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint])
		assert.NotEqual(t, raw, initial.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint])
		for _, value := range initial.Annotations {
			assert.NotContains(t, value, raw)
		}
	})
	t.Run("rules and overridden declarations preserve template", func(t *testing.T) {
		t.Setenv("COSMOGUARD_ENABLE_EVM", "true")
		for _, raw := range []string{"lcd: {rules: [{paths: [/new], action: deny}]}", "# comment\nlcd: {rules: [{paths: [/new], action: deny}]}", "metrics: {enable: false, port: 9999}\nnode: {host: ignored}\nlcd: {rules: [{paths: [/other], action: allow}]}"} {
			change(raw, false)
			require.NoError(t, apply(c))
			assert.Equal(t, initial.Spec.Template, read().Spec.Template)
		}
	})
	t.Run("auth rolls once and HPA replicas survive", func(t *testing.T) {
		live := read()
		live.Spec.Replicas = ptr.To(int32(7))
		require.NoError(t, c.Update(ctx, live))
		change("auth: {enable: true}", true)
		require.NoError(t, apply(c))
		rolled := read()
		assert.NotEqual(t, initial.Spec.Template, rolled.Spec.Template)
		assert.Equal(t, int32(7), *rolled.Spec.Replicas)
		require.NoError(t, apply(c))
		assert.Equal(t, rolled.ResourceVersion, read().ResourceVersion)
		assert.Equal(t, map[string]string{"user": "preserved"}, p.PodAnnotations)
	})
	t.Run("invalid and missing inputs preserve baseline", func(t *testing.T) {
		previous := read()
		for _, raw := range []string{"auth: [invalid", "lcd: {rules: [{paths: [/broken], action: unknown}]}"} {
			change(raw, false)
			require.NoError(t, apply(c))
			assert.Equal(t, previous.Annotations, read().Annotations)
			assert.Equal(t, previous.Spec.Template, read().Spec.Template)
		}
		cm.Data = nil
		require.NoError(t, c.Update(ctx, cm))
		require.NoError(t, apply(c))
		assert.Equal(t, previous.ResourceVersion, read().ResourceVersion)
		require.NoError(t, c.Delete(ctx, cm))
		require.NoError(t, apply(c))
		assert.Equal(t, previous.ResourceVersion, read().ResourceVersion)
		cm.ResourceVersion = ""
		require.NoError(t, c.Create(ctx, cm))
	})
	t.Run("reversion produces a distinct rollout marker", func(t *testing.T) {
		first := read().Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart]
		change("", false)
		require.NoError(t, apply(c))
		second := read().Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart]
		change("auth: {enable: true}", false)
		require.NoError(t, apply(c))
		third := read().Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart]
		assert.NotEqual(t, first, second)
		assert.NotEqual(t, first, third)
		assert.NotEqual(t, second, third)
	})
	t.Run("module upgrade rebaselines even when file also changes", func(t *testing.T) {
		live := read()
		live.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme] = "older-module"
		require.NoError(t, c.Update(ctx, live))
		change("", false)
		require.NoError(t, apply(c))
		adopted := read()
		assert.Equal(t, live.Spec.Template, adopted.Spec.Template)
		assert.NotEqual(t, live.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme], adopted.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme])
		require.NoError(t, apply(c))
		assert.Equal(t, adopted.ResourceVersion, read().ResourceVersion)
	})
	t.Run("conflict leaves stored baseline untouched and retry rolls", func(t *testing.T) {
		before := read()
		change("auth: {enable: true}", false)
		conflicting := interceptor.NewClient(c, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return apierrors.NewConflict(appsv1.Resource("statefulsets"), p.Name, nil)
		}})
		require.True(t, apierrors.IsConflict(apply(conflicting)))
		assert.Equal(t, before.Annotations, read().Annotations)
		assert.Equal(t, before.Spec.Template, read().Spec.Template)
		require.NoError(t, apply(c))
		assert.NotEqual(t, before.Spec.Template, read().Spec.Template)
	})
	t.Run("owner collision is rejected", func(t *testing.T) {
		other := owner.DeepCopy()
		other.UID = "other"
		require.ErrorContains(t, ApplyStatefulSet(ctx, c, scheme, other, p, p.StatefulSet()), "another owner")
	})
	t.Run("adoption preserves HPA count and exact template", func(t *testing.T) {
		live := read()
		for _, key := range []string{controllers.AnnotationCosmoGuardRestartFingerprint, controllers.AnnotationCosmoGuardConfigDigest, controllers.AnnotationCosmoGuardFingerprintScheme} {
			delete(live.Annotations, key)
		}
		require.NoError(t, controllerutil.SetControllerReference(owner, live, scheme))
		require.NoError(t, c.Update(ctx, live))
		require.NoError(t, apply(c))
		adopted := read()
		assert.Equal(t, live.Spec.Template, adopted.Spec.Template)
		assert.Equal(t, live.Spec.Replicas, adopted.Spec.Replicas)
		assert.NotEmpty(t, adopted.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint])
	})
}

func TestConfigRolloutRenderedOverrides(t *testing.T) {
	ctx := context.Background()
	for _, discovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "static", true: "discovery"}[discovery], func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "owner"}}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rules", Namespace: "ns"}, Data: map[string]string{"cosmoguard.yaml": ""}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "ns"}, Data: map[string][]byte{"user": []byte("operator-user"), "password": []byte("operator-password"), EncryptionKeySecretKey: []byte(base64.StdEncoding.EncodeToString(make([]byte, 32)))}}
			p := baseParams()
			p.Name = "guard"
			p.EncryptionKeySecret = secret.Name
			p.EvmEnabled = true
			p.Dashboard = &DashboardParams{Port: 8080, AuthUser: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: "user"}, AuthPassword: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: "password"}}
			if discovery {
				p.DiscoveryHost = "nodes.ns.svc.cluster.local"
			} else {
				p.UpstreamHost = "node.ns.svc.cluster.local"
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, cm, secret).Build()
			apply := func() { require.NoError(t, ApplyStatefulSet(ctx, c, scheme, owner, p, p.StatefulSet())) }
			read := func() *appsv1.StatefulSet {
				sts := &appsv1.StatefulSet{}
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: p.Name}, sts))
				return sts
			}
			apply()
			before := read()
			require.NotEmpty(t, before.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint])
			// These file declarations lose to the same literal/Secret overrides the guard process sees.
			cm.Data["cosmoguard.yaml"] = "enableEvm: false\ndashboard: {enable: false, port: 9099, basicAuthUser: ignored, basicAuthPassword: ignored}\ncache: {cluster: {bindAddr: ignored, encryptionKey: ignored}}"
			require.NoError(t, c.Update(ctx, cm))
			apply()
			assert.Equal(t, before.Spec.Template, read().Spec.Template)
			assert.Equal(t, before.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint], read().Annotations[controllers.AnnotationCosmoGuardRestartFingerprint])
			require.NoError(t, c.Delete(ctx, secret))
			cm.Data["cosmoguard.yaml"] = "auth: {enable: true}"
			require.NoError(t, c.Update(ctx, cm))
			previous := read()
			apply()
			assert.Equal(t, previous.ResourceVersion, read().ResourceVersion)
		})
	}
}
