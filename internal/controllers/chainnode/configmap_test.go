package chainnode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func TestConfigGenerationCacheKeyIncludesAppEnv(t *testing.T) {
	base := &appsv1.ChainNode{
		Spec: appsv1.ChainNodeSpec{
			App: appsv1.AppSpec{Image: "example/app", App: "appd"},
			Config: &appsv1.Config{Env: []corev1.EnvVar{
				{Name: "HOME", Value: "/home/app"},
				{
					Name: "FROM_SECRET",
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "app-secret"},
						Key:                  "token",
					}},
				},
			}},
		},
	}

	baseKey, err := configGenerationCacheKey(base)
	require.NoError(t, err)
	require.NotEmpty(t, baseKey)

	same := base.DeepCopy()
	sameKey, err := configGenerationCacheKey(same)
	require.NoError(t, err)
	assert.Equal(t, baseKey, sameKey)

	differentValue := base.DeepCopy()
	differentValue.Spec.Config.Env[0].Value = "/different-home"
	differentValueKey, err := configGenerationCacheKey(differentValue)
	require.NoError(t, err)
	assert.NotEqual(t, baseKey, differentValueKey)

	differentValueFrom := base.DeepCopy()
	differentValueFrom.Spec.Config.Env[1].ValueFrom.SecretKeyRef.Name = "different-secret"
	differentValueFromKey, err := configGenerationCacheKey(differentValueFrom)
	require.NoError(t, err)
	assert.NotEqual(t, baseKey, differentValueFromKey)

	differentOrder := base.DeepCopy()
	differentOrder.Spec.Config.Env[0], differentOrder.Spec.Config.Env[1] =
		differentOrder.Spec.Config.Env[1], differentOrder.Spec.Config.Env[0]
	differentOrderKey, err := configGenerationCacheKey(differentOrder)
	require.NoError(t, err)
	assert.NotEqual(t, baseKey, differentOrderKey)

	withDivisor := base.DeepCopy()
	withDivisor.Spec.Config.Env = []corev1.EnvVar{{
		Name: "CPU_LIMIT",
		ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
			Resource: "limits.cpu",
			Divisor:  resource.MustParse("1"),
		}},
	}}
	firstDivisorKey, err := configGenerationCacheKey(withDivisor)
	require.NoError(t, err)
	withDivisor.Spec.Config.Env[0].ValueFrom.ResourceFieldRef.Divisor = resource.MustParse("2")
	secondDivisorKey, err := configGenerationCacheKey(withDivisor)
	require.NoError(t, err)
	assert.NotEqual(t, firstDivisorKey, secondDivisorKey)
}

func TestGetConfigHash(t *testing.T) {
	tests := []struct {
		name      string
		configs   map[string]interface{}
		wantError bool
	}{
		{
			name: "simple config",
			configs: map[string]interface{}{
				"key1": "value1",
				"key2": "value2",
			},
			wantError: false,
		},
		{
			name: "nested config",
			configs: map[string]interface{}{
				"app": map[string]interface{}{
					"setting1": "value1",
					"setting2": 123,
				},
			},
			wantError: false,
		},
		{
			name:      "empty config",
			configs:   map[string]interface{}{},
			wantError: false,
		},
		{
			name: "config with slices",
			configs: map[string]interface{}{
				"list": []string{"item1", "item2", "item3"},
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := getConfigHash(tt.configs)
			if tt.wantError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.NotEmpty(t, hash)
		})
	}
}

func TestGetConfigHash_Consistency(t *testing.T) {
	config1 := map[string]interface{}{
		"key1": "value1",
		"key2": "value2",
	}
	config2 := map[string]interface{}{
		"key1": "value1",
		"key2": "value2",
	}

	hash1, err1 := getConfigHash(config1)
	hash2, err2 := getConfigHash(config2)

	assert.NoError(t, err1)
	assert.NoError(t, err2)
	assert.Equal(t, hash1, hash2)
}

func TestGetConfigHash_DifferentForDifferentConfigs(t *testing.T) {
	config1 := map[string]interface{}{
		"key1": "value1",
	}
	config2 := map[string]interface{}{
		"key1": "value2",
	}

	hash1, err1 := getConfigHash(config1)
	hash2, err2 := getConfigHash(config2)

	assert.NoError(t, err1)
	assert.NoError(t, err2)
	assert.NotEqual(t, hash1, hash2)
}

func TestGetMostRecentHeightFromServicesAnnotations(t *testing.T) {
	tests := []struct {
		name            string
		annotationsList []map[string]string
		wantHeight      int64
		wantHash        string
	}{
		{
			name: "single annotation",
			annotationsList: []map[string]string{
				{
					controllers.AnnotationStateSyncTrustHeight: "100",
					controllers.AnnotationStateSyncTrustHash:   "hash100",
				},
			},
			wantHeight: 100,
			wantHash:   "hash100",
		},
		{
			name: "multiple annotations, find max",
			annotationsList: []map[string]string{
				{
					controllers.AnnotationStateSyncTrustHeight: "100",
					controllers.AnnotationStateSyncTrustHash:   "hash100",
				},
				{
					controllers.AnnotationStateSyncTrustHeight: "200",
					controllers.AnnotationStateSyncTrustHash:   "hash200",
				},
				{
					controllers.AnnotationStateSyncTrustHeight: "150",
					controllers.AnnotationStateSyncTrustHash:   "hash150",
				},
			},
			wantHeight: 200,
			wantHash:   "hash200",
		},
		{
			name:            "empty annotations",
			annotationsList: []map[string]string{},
			wantHeight:      0,
			wantHash:        "",
		},
		{
			name: "missing height annotations",
			annotationsList: []map[string]string{
				{
					"other-annotation": "value",
				},
			},
			wantHeight: 0,
			wantHash:   "",
		},
		{
			name: "invalid height format",
			annotationsList: []map[string]string{
				{
					controllers.AnnotationStateSyncTrustHeight: "not-a-number",
					controllers.AnnotationStateSyncTrustHash:   "hash",
				},
			},
			wantHeight: 0,
			wantHash:   "",
		},
		{
			name: "mixed valid and invalid heights",
			annotationsList: []map[string]string{
				{
					controllers.AnnotationStateSyncTrustHeight: "invalid",
					controllers.AnnotationStateSyncTrustHash:   "hash1",
				},
				{
					controllers.AnnotationStateSyncTrustHeight: "100",
					controllers.AnnotationStateSyncTrustHash:   "hash100",
				},
			},
			wantHeight: 100,
			wantHash:   "hash100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			height, hash := getMostRecentHeightFromServicesAnnotations(tt.annotationsList)
			assert.Equal(t, tt.wantHeight, height)
			assert.Equal(t, tt.wantHash, hash)
		})
	}
}

func TestGetExternalAddress(t *testing.T) {
	tests := []struct {
		name        string
		chainNode   *appsv1.ChainNode
		wantAddress string
		wantOK      bool
	}{
		{
			name: "returns public address when PublicAddress is set",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mynode",
					Namespace: "cosmos",
				},
				Status: appsv1.ChainNodeStatus{
					PublicAddress: "nodeid@example.com:26656",
				},
			},
			wantAddress: "example.com:26656",
			wantOK:      true,
		},
		{
			name: "returns false without PublicAddress",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mynode",
					Namespace: "cosmos",
				},
				Status: appsv1.ChainNodeStatus{
					PublicAddress: "",
				},
			},
			wantAddress: "",
			wantOK:      false,
		},
		{
			name: "returns false when PublicAddress has no @",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mynode",
					Namespace: "cosmos",
				},
				Status: appsv1.ChainNodeStatus{
					PublicAddress: "invalid-no-at-sign",
				},
			},
			wantAddress: "",
			wantOK:      false,
		},
		{
			name: "returns false when PublicAddress has empty host",
			chainNode: &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mynode",
					Namespace: "cosmos",
				},
				Status: appsv1.ChainNodeStatus{
					PublicAddress: "nodeid@",
				},
			},
			wantAddress: "",
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address, ok := getExternalAddress(tt.chainNode)
			assert.Equal(t, tt.wantAddress, address)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

func TestGetExternalAddress_PublicNodeFormat(t *testing.T) {
	// Public nodes advertise their public address (host:port stripped of the node id).
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gaia-sentry-0",
			Namespace: "mainnet",
		},
		Status: appsv1.ChainNodeStatus{
			PublicAddress: "abc123@203.0.113.50:26656",
		},
	}

	address, ok := getExternalAddress(chainNode)
	assert.True(t, ok)
	assert.Equal(t, "203.0.113.50:26656", address)
}

func TestNewConfigLockManager(t *testing.T) {
	clm := newConfigLockManager()
	assert.NotNil(t, clm)
	assert.NotNil(t, clm.locks)
}

func TestConfigLockManager_GetLockForVersion(t *testing.T) {
	clm := newConfigLockManager()

	// Get lock for version1
	lock1 := clm.getLockForVersion("v1.0.0")
	assert.NotNil(t, lock1)

	// Get lock for same version
	lock2 := clm.getLockForVersion("v1.0.0")
	assert.NotNil(t, lock2)

	// Should return same lock instance for same version
	assert.Same(t, lock1, lock2)

	// Get lock for different version
	lock3 := clm.getLockForVersion("v2.0.0")
	assert.NotNil(t, lock3)

	// Should return different lock for different version
	assert.NotSame(t, lock1, lock3)
}

func TestConfigLockManager_CapacityLimit(t *testing.T) {
	clm := newConfigLockManager()

	// Fill up to capacity
	locks := make(map[string]interface{})
	for i := 0; i < maxConfigLocks; i++ {
		version := string(rune('a' + i))
		lock := clm.getLockForVersion(version)
		locks[version] = lock
	}

	// Should have maxConfigLocks entries
	assert.Equal(t, maxConfigLocks, len(clm.locks))

	// Try to add one more
	extraLock := clm.getLockForVersion("extra-version")
	assert.NotNil(t, extraLock)

	// Should still have maxConfigLocks entries (reused existing lock)
	assert.LessOrEqual(t, len(clm.locks), maxConfigLocks)
}
