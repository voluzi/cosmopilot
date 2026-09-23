package tmkms

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	testRootAnnotation  = "test/root"
	testClassAnnotation = "test/class"
)

// testAttribution attributes objects to a named root, standing in for the controller's scheme.
type testAttribution struct{ root string }

func (a testAttribution) Stamp(object metav1.Object, class string) bool {
	annotations := object.GetAnnotations()
	if annotations[testRootAnnotation] == a.root && annotations[testClassAnnotation] == class {
		return false
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[testRootAnnotation] = a.root
	annotations[testClassAnnotation] = class
	object.SetAnnotations(annotations)
	return true
}

func (a testAttribution) Attributed(object metav1.Object, class string) (bool, bool) {
	annotations := object.GetAnnotations()
	root, stamped := annotations[testRootAnnotation]
	return stamped, stamped && root == a.root && annotations[testClassAnnotation] == class
}

type ownershipFixture struct {
	t       *testing.T
	client  *k8sfake.Clientset
	scheme  *runtime.Scheme
	owner   *corev1.ConfigMap
	foreign *corev1.ConfigMap
	deletes map[string]*metav1.Preconditions
}

func newOwnershipFixture(t *testing.T, objects ...runtime.Object) *ownershipFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f := &ownershipFixture{
		t:       t,
		client:  k8sfake.NewClientset(objects...),
		scheme:  scheme,
		owner:   &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}},
		foreign: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "foreign-uid"}},
		deletes: map[string]*metav1.Preconditions{},
	}
	f.client.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteAction)
		f.deletes[action.GetResource().Resource+"/"+del.GetName()] = del.GetDeleteOptions().Preconditions
		return false, nil, nil
	})
	return f
}

func (f *ownershipFixture) kms(opts ...Option) *KMS {
	opts = append([]Option{WithAttribution(testAttribution{root: "validator"})}, opts...)
	return New(f.client, f.scheme, "validator-tmkms", f.owner, opts...)
}

func (f *ownershipFixture) controlledBy(object metav1.Object, owner metav1.Object) {
	f.t.Helper()
	if err := controllerutil.SetControllerReference(owner, object, f.scheme); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ownershipFixture) create(object runtime.Object) {
	f.t.Helper()
	if err := f.client.Tracker().Add(object); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ownershipFixture) secret() *corev1.Secret {
	f.t.Helper()
	s, err := f.client.CoreV1().Secrets("default").Get(context.Background(), "validator-tmkms", metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *ownershipFixture) pvc() *corev1.PersistentVolumeClaim {
	f.t.Helper()
	p, err := f.client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "validator-tmkms", metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *ownershipFixture) exists(kind string) bool {
	f.t.Helper()
	var err error
	switch kind {
	case "configmap":
		_, err = f.client.CoreV1().ConfigMaps("default").Get(context.Background(), "validator-tmkms", metav1.GetOptions{})
	case "secret":
		_, err = f.client.CoreV1().Secrets("default").Get(context.Background(), "validator-tmkms", metav1.GetOptions{})
	case "pvc":
		_, err = f.client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "validator-tmkms", metav1.GetOptions{})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		f.t.Fatal(err)
	}
	return err == nil
}

func legacyIdentitySecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms", Namespace: "default", UID: "secret-uid"},
		Immutable:  ptr.To(true),
		Data:       map[string][]byte{identityKeyName: []byte("identity")},
	}
}

func legacyStatePVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms", Namespace: "default", UID: "pvc-uid"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(tmkmsPvcSize),
			}},
		},
	}
}

func ownerPodMounting(f *ownershipFixture, secret, claim string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"}}
	if secret != "" {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "tmkms-identity", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secret},
		}})
	}
	if claim != "" {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "tmkms-data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		}})
	}
	f.controlledBy(pod, f.owner)
	return pod
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

func TestDeployConfigRefusesIdentitySecretsItCannotClaim(t *testing.T) {
	cases := map[string]func(*ownershipFixture) *corev1.Secret{
		"controlled by another owner": func(f *ownershipFixture) *corev1.Secret {
			s := legacyIdentitySecret()
			f.controlledBy(s, f.foreign)
			return s
		},
		"unowned with another shape": func(*ownershipFixture) *corev1.Secret {
			s := legacyIdentitySecret()
			s.Data["token"] = []byte("unrelated")
			return s
		},
		"attributed to another root": func(*ownershipFixture) *corev1.Secret {
			s := legacyIdentitySecret()
			s.Annotations = map[string]string{testRootAnnotation: "other", testClassAnnotation: ClassIdentity}
			return s
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOwnershipFixture(t)
			original := build(f)
			f.create(original)

			requireErrorContains(t, f.kms().DeployConfig(context.Background()), "not managed by this ChainNode")
			got := f.secret()
			if got.Annotations[testRootAnnotation] != original.Annotations[testRootAnnotation] {
				t.Fatalf("refused Secret was re-attributed: %v", got.Annotations)
			}
			if f.exists("configmap") {
				t.Fatal("ConfigMap was created although the identity Secret was refused")
			}
		})
	}
}

func TestDeployConfigAdoptsLegacyIdentitySecret(t *testing.T) {
	cases := map[string]func(*ownershipFixture){
		"exact shape": func(f *ownershipFixture) {
			f.create(legacyIdentitySecret())
		},
		"mounted by the owner pod": func(f *ownershipFixture) {
			s := legacyIdentitySecret()
			s.Immutable = nil
			f.create(s)
			f.create(ownerPodMounting(f, "validator-tmkms", ""))
		},
		"attributed to the same root": func(f *ownershipFixture) {
			s := legacyIdentitySecret()
			s.Immutable = nil
			s.Annotations = map[string]string{testRootAnnotation: "validator", testClassAnnotation: ClassIdentity}
			f.create(s)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOwnershipFixture(t)
			setup(f)

			if err := f.kms().DeployConfig(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := f.secret()
			if string(got.Data[identityKeyName]) != "identity" {
				t.Fatalf("identity key was replaced: %q", got.Data[identityKeyName])
			}
			if _, owned := (testAttribution{root: "validator"}).Attributed(got, ClassIdentity); !owned {
				t.Fatalf("adopted Secret is not attributed: %v", got.Annotations)
			}
		})
	}
}

func TestDeployConfigRefusesOwnedIdentitySecretWithoutKey(t *testing.T) {
	f := newOwnershipFixture(t)
	s := legacyIdentitySecret()
	s.Data = map[string][]byte{"other": []byte("x")}
	s.Annotations = map[string]string{testRootAnnotation: "validator", testClassAnnotation: ClassIdentity}
	f.create(s)

	requireErrorContains(t, f.kms().DeployConfig(context.Background()), "has no "+identityKeyName)
}

func TestDeployConfigRefusesForeignConfigMap(t *testing.T) {
	f := newOwnershipFixture(t)
	f.create(legacyIdentitySecret())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms", Namespace: "default"},
		Data:       map[string]string{configFileName: "foreign"},
	}
	f.controlledBy(cm, f.foreign)
	f.create(cm)

	requireErrorContains(t, f.kms().DeployConfig(context.Background()), "not managed by this ChainNode")
	got, err := f.client.CoreV1().ConfigMaps("default").Get(context.Background(), "validator-tmkms", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data[configFileName] != "foreign" {
		t.Fatalf("foreign ConfigMap was rewritten: %q", got.Data[configFileName])
	}
}

func TestDeployConfigUpdatesOwnedConfigMap(t *testing.T) {
	f := newOwnershipFixture(t)
	f.create(legacyIdentitySecret())
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms", Namespace: "default"},
		Data:       map[string]string{configFileName: "stale"},
	}
	f.controlledBy(cm, f.owner)
	f.create(cm)

	if err := f.kms().DeployConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := f.client.CoreV1().ConfigMaps("default").Get(context.Background(), "validator-tmkms", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data[configFileName] == "stale" {
		t.Fatal("owned ConfigMap was not updated")
	}
}

func TestDeployConfigStatePVCOwnership(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*ownershipFixture)
		err   string
	}{
		{name: "legacy shape is adopted", setup: func(f *ownershipFixture) { f.create(legacyStatePVC()) }},
		{name: "other shape mounted by the owner pod is adopted", setup: func(f *ownershipFixture) {
			p := legacyStatePVC()
			p.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("5Gi")
			f.create(p)
			f.create(ownerPodMounting(f, "", "validator-tmkms"))
		}},
		{name: "other shape not mounted is refused", err: "not managed by this ChainNode", setup: func(f *ownershipFixture) {
			p := legacyStatePVC()
			p.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("5Gi")
			f.create(p)
		}},
		{name: "mounted by a pod the owner does not control is refused", err: "not managed by this ChainNode", setup: func(f *ownershipFixture) {
			p := legacyStatePVC()
			p.Spec.AccessModes = append(p.Spec.AccessModes, corev1.ReadOnlyMany)
			f.create(p)
			pod := ownerPodMounting(f, "", "validator-tmkms")
			pod.OwnerReferences = nil
			f.controlledBy(pod, f.foreign)
			f.create(pod)
		}},
		{name: "controlled by another owner is refused", err: "not managed by this ChainNode", setup: func(f *ownershipFixture) {
			p := legacyStatePVC()
			f.controlledBy(p, f.foreign)
			f.create(p)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOwnershipFixture(t)
			f.create(legacyIdentitySecret())
			tc.setup(f)

			err := f.kms(PersistState(true)).DeployConfig(context.Background())
			if tc.err != "" {
				requireErrorContains(t, err, tc.err)
				if _, stamped := (testAttribution{}).Attributed(f.pvc(), ClassState); stamped {
					t.Fatal("refused PVC was attributed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, owned := (testAttribution{root: "validator"}).Attributed(f.pvc(), ClassState); !owned {
				t.Fatalf("adopted PVC is not attributed: %v", f.pvc().Annotations)
			}
		})
	}
}

func TestDeployConfigStampsNewStatePVC(t *testing.T) {
	f := newOwnershipFixture(t)
	f.create(legacyIdentitySecret())

	if err := f.kms(PersistState(true)).DeployConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.pvc()
	if _, owned := (testAttribution{root: "validator"}).Attributed(got, ClassState); !owned {
		t.Fatalf("new PVC is not attributed: %v", got.Annotations)
	}
	if len(got.OwnerReferences) != 0 {
		t.Fatalf("new PVC has owner references %v; it must outlive the ChainNode", got.OwnerReferences)
	}
}

func TestUndeployConfigKeepsObjectsItDoesNotOwn(t *testing.T) {
	f := newOwnershipFixture(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms", Namespace: "default"}}
	f.controlledBy(cm, f.foreign)
	f.create(cm)
	secret := legacyIdentitySecret()
	secret.Data["token"] = []byte("unrelated")
	f.create(secret)

	if err := f.kms().UndeployConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.exists("configmap") || !f.exists("secret") {
		t.Fatal("UndeployConfig deleted an object it does not own")
	}
	if len(f.deletes) != 0 {
		t.Fatalf("unexpected delete requests: %v", f.deletes)
	}
}

func TestUndeployConfigDeletesOwnedObjectsExactly(t *testing.T) {
	f := newOwnershipFixture(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms", Namespace: "default", UID: "cm-uid"}}
	f.controlledBy(cm, f.owner)
	f.create(cm)
	f.create(legacyIdentitySecret())
	f.create(legacyStatePVC())

	if err := f.kms().UndeployConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.exists("configmap") || f.exists("secret") {
		t.Fatal("owned ConfigMap and identity Secret must be deleted")
	}
	if !f.exists("pvc") {
		t.Fatal("the signing state PVC must never be deleted")
	}
	for key, uid := range map[string]string{"configmaps/validator-tmkms": "cm-uid", "secrets/validator-tmkms": "secret-uid"} {
		pre := f.deletes[key]
		if pre == nil || pre.UID == nil || string(*pre.UID) != uid {
			t.Fatalf("delete of %s preconditions = %v, want UID %s", key, pre, uid)
		}
	}
}

func TestUndeployConfigAttemptsSecretDeleteAfterConfigMapError(t *testing.T) {
	f := newOwnershipFixture(t)
	f.create(legacyIdentitySecret())
	f.client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("forced ConfigMap failure")
	})

	requireErrorContains(t, f.kms().UndeployConfig(context.Background()), "forced ConfigMap failure")
	if f.exists("secret") {
		t.Fatal("identity Secret delete was not attempted after the ConfigMap error")
	}
}

func TestUndeployConfigRequiresAttribution(t *testing.T) {
	f := newOwnershipFixture(t)
	f.create(legacyIdentitySecret())

	err := New(f.client, f.scheme, "validator-tmkms", f.owner).UndeployConfig(context.Background())
	requireErrorContains(t, err, "no resource attribution")
	if !f.exists("secret") {
		t.Fatal("Secret deleted without attribution")
	}
}

func TestReplaceHelperPodOnlyDeletesOwnedPods(t *testing.T) {
	f := newOwnershipFixture(t)
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms-generate-identity", Namespace: "default"}}
	f.controlledBy(foreign, f.foreign)
	f.create(foreign)
	owned := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms-vault-upload", Namespace: "default", UID: "pod-uid"}}
	f.controlledBy(owned, f.owner)
	f.create(owned)
	kms := f.kms()

	requireErrorContains(t, kms.replaceHelperPod(context.Background(), foreign.Name), "managed by another owner")
	if _, err := f.client.CoreV1().Pods("default").Get(context.Background(), foreign.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("foreign helper pod was deleted: %v", err)
	}

	if err := kms.replaceHelperPod(context.Background(), owned.Name); err != nil {
		t.Fatal(err)
	}
	if pre := f.deletes["pods/"+owned.Name]; pre == nil || pre.UID == nil || *pre.UID != "pod-uid" {
		t.Fatalf("owned helper pod delete preconditions = %v, want UID pod-uid", pre)
	}
}
