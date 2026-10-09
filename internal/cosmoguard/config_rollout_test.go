package cosmoguard

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	guardconfig "github.com/voluzi/cosmoguard/v6/pkg/cosmoguard"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	v1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

func TestConfigRolloutState(t *testing.T) {
	t.Setenv("COSMOGUARD_ENABLE_EVM", "false")
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
	t.Run("operator process environment does not affect rollout inputs", func(t *testing.T) {
		before := read()
		// The operator's environment must not enter the guard's closed comparison lookup.
		t.Setenv("COSMOGUARD_ENABLE_EVM", "true")
		require.NoError(t, apply(c))
		assert.Equal(t, before.ResourceVersion, read().ResourceVersion)
		change("# force classification with unchanged guard configuration\n", false)
		require.NoError(t, apply(c))
		after := read()
		assert.Equal(t, before.Spec.Template, after.Spec.Template)
		assert.Equal(t, before.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint], after.Annotations[controllers.AnnotationCosmoGuardRestartFingerprint])
		assert.NotEqual(t, before.Annotations[controllers.AnnotationCosmoGuardConfigDigest], after.Annotations[controllers.AnnotationCosmoGuardConfigDigest])
	})
	t.Run("rules and overridden declarations preserve template", func(t *testing.T) {
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
	t.Run("module upgrade rebaselines unchanged inputs", func(t *testing.T) {
		for _, marker := range []string{"", "previous-restart"} {
			live := read()
			live.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme] = "v5.1.0:old-scheme"
			if marker == "" {
				delete(live.Spec.Template.Annotations, controllers.AnnotationCosmoGuardRestart)
			} else {
				live.Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart] = marker
			}
			require.NoError(t, c.Update(ctx, live))
			require.NoError(t, apply(c))
			adopted := read()
			assert.Equal(t, live.Spec.Template, adopted.Spec.Template)
			assert.NotEqual(t, live.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme], adopted.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme])
			require.NoError(t, apply(c))
			assert.Equal(t, adopted.ResourceVersion, read().ResourceVersion)
		}
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

func TestConfigRolloutUnclassifiableConfig(t *testing.T) {
	t.Setenv("HOSTNAME", "operator-host-value")
	cases := []struct{ name, raw, privateValue string }{
		{"parse failure", "auth: [private-config-value", "private-config-value"},
		{"unresolved variable", "auth: {identities: [{name: test, apiKey: '${HOSTNAME:?private-message}'}]}", "private-message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "owner"}}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rules", Namespace: "ns"}, Data: map[string]string{"cosmoguard.yaml": ""}}
			p := baseParams()
			p.UpstreamHost = "node.ns.svc.cluster.local"
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: p.EncryptionKeySecret, Namespace: "ns"}, Data: map[string][]byte{EncryptionKeySecretKey: []byte(base64.StdEncoding.EncodeToString(make([]byte, 32)))}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, cm, secret).Build()
			require.NoError(t, ApplyStatefulSet(ctx, c, scheme, owner, p, p.StatefulSet()))
			before := &appsv1.StatefulSet{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: p.Name}, before))
			cm.Data["cosmoguard.yaml"] = tc.raw
			require.NoError(t, c.Update(ctx, cm))
			var output bytes.Buffer
			logger := zap.New(zap.UseDevMode(false), zap.WriteTo(&output))
			require.NoError(t, ApplyStatefulSet(log.IntoContext(ctx, logger), c, scheme, owner, p, p.StatefulSet()))
			var entry map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry))
			assert.Equal(t, "error", entry["level"])
			assert.Equal(t, cm.Name, entry["configMap"])
			assert.Equal(t, p.ConfigMap.Key, entry["key"])
			assert.NotContains(t, output.String(), tc.privateValue)
			assert.NotContains(t, output.String(), "HOSTNAME")
			assert.NotContains(t, output.String(), "operator-host-value")
			after := &appsv1.StatefulSet{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(before), after))
			assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
		})
	}
}

func TestConfigRolloutUpgradeTemplate(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "owner"}}
	p := baseParams()
	p.UpstreamHost = "node.ns.svc.cluster.local"
	cfg := &v1.Config{}
	p.Image, p.Resources = cfg.GetCosmoGuardImage(""), cfg.GetCosmoGuardResources()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: p.EncryptionKeySecret, Namespace: "ns"}, Data: map[string][]byte{EncryptionKeySecretKey: []byte(base64.StdEncoding.EncodeToString(make([]byte, 32)))}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rules", Namespace: "ns"}, Data: map[string]string{"cosmoguard.yaml": "lcd: {rules: [{paths: [/status], action: allow}]}"}}
	old := p.StatefulSet()
	old.Spec.Template.Spec.TopologySpreadConstraints = nil
	old.Spec.Template.Spec.TerminationGracePeriodSeconds = nil
	old.Spec.Template.Spec.Containers[0].Image = "ghcr.io/voluzi/cosmoguard:5.1.0"
	old.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("250Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("250Mi")},
	}
	old.Annotations = map[string]string{controllers.AnnotationCosmoGuardFingerprintScheme: "v5.1.0:old-scheme", controllers.AnnotationCosmoGuardRestartFingerprint: "old-fingerprint", controllers.AnnotationCosmoGuardConfigDigest: "old-digest"}
	require.NoError(t, controllerutil.SetControllerReference(owner, old, scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, secret, cm, old).Build()
	apply := func() { require.NoError(t, ApplyStatefulSet(ctx, c, scheme, owner, p, p.StatefulSet())) }
	read := func() *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(old), sts))
		return sts
	}
	apply()
	upgraded := read()
	container := upgraded.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "ghcr.io/voluzi/cosmoguard:6.0.0", container.Image)
	for _, quantities := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
		assert.Equal(t, 0, quantities.Cpu().Cmp(resource.MustParse("500m")))
		assert.Equal(t, 0, quantities.Memory().Cmp(resource.MustParse("500Mi")))
	}
	require.Len(t, upgraded.Spec.Template.Spec.TopologySpreadConstraints, 1)
	assert.Equal(t, corev1.ScheduleAnyway, upgraded.Spec.Template.Spec.TopologySpreadConstraints[0].WhenUnsatisfiable)
	require.NotNil(t, upgraded.Spec.Template.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(30), *upgraded.Spec.Template.Spec.TerminationGracePeriodSeconds)
	assert.NotEqual(t, old.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme], upgraded.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme])
	assert.True(t, strings.HasPrefix(upgraded.Annotations[controllers.AnnotationCosmoGuardFingerprintScheme], "v6.0.0:"),
		"scheme must carry the cosmoguard module version read from build info")
	assert.Empty(t, upgraded.Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart])
	apply()
	assert.Equal(t, upgraded.ResourceVersion, read().ResourceVersion)
}
