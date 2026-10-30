package chainnode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

type nativeRoundTripper func(*http.Request) (*http.Response, error)

func (f nativeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeExportAttemptAPIBoundary(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires verified local envtest assets")
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "helm", "cosmopilot", "crds"), filepath.Join("..", "..", "..", "test", "testdata", "crds")}, ErrorIfCRDPathMissing: true}
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	cfg, err := environment.Start()
	require.NoError(t, err)
	require.NoError(t, appsv1.AddToScheme(scheme.Scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme.Scheme))
	admin, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	ctx := t.Context()
	var objectMu sync.Mutex
	var storedObject []byte
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case req.Method == http.MethodPost && req.URL.Query().Has("uploads"):
			_, _ = io.WriteString(w, `<CreateMultipartUploadResult><UploadId>fixture-upload</UploadId></CreateMultipartUploadResult>`)
		case req.Method == http.MethodPut && req.URL.Query().Get("partNumber") == "1":
			data, err := io.ReadAll(req.Body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			objectMu.Lock()
			storedObject = data
			objectMu.Unlock()
			w.Header().Set("ETag", `"fixture-part"`)
		case req.Method == http.MethodPost && req.URL.Query().Get("uploadId") == "fixture-upload":
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>fixture-object</ETag></CompleteMultipartUploadResult>`)
		default:
			http.Error(w, "unexpected fixture request", 400)
		}
	}))
	t.Cleanup(store.Close)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "native-attempt-"}}
	require.NoError(t, admin.Create(ctx, ns))
	t.Run("protocol API compatibility", func(t *testing.T) { nativeProtocolAPIMatrix(t, admin, ns.Name) })
	c := destinationTestChainNode(&appsv1.ExportTarballConfig{Protocol: "source-v1", DeleteOnExpire: ptr.To(true), S3: &appsv1.S3ExportConfig{Bucket: "bucket", Region: "region", Endpoint: ptr.To(store.URL), ForcePathStyle: ptr.To(true), CredentialsSecret: &corev1.LocalObjectReference{Name: "credentials"}}})
	c.UID = ""
	c.Namespace = ns.Name
	c.Spec.App = appsv1.AppSpec{Image: "application:test", App: "app"}
	c.Status = appsv1.ChainNodeStatus{}
	require.NoError(t, admin.Create(ctx, c))
	credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: ns.Name}, Data: map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("key"), "AWS_SECRET_ACCESS_KEY": []byte("secret")}}
	require.NoError(t, admin.Create(ctx, credentials))
	sourcePVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: ns.Name}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("native-csi"), AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	require.NoError(t, admin.Create(ctx, sourcePVC))
	s := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: ns.Name, Annotations: map[string]string{controllers.AnnotationDataHeight: "123"}}, Spec: snapshotv1.VolumeSnapshotSpec{Source: snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: ptr.To("source")}}}
	require.NoError(t, admin.Create(ctx, s))
	content := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: ns.Name + "-content"}, Spec: snapshotv1.VolumeSnapshotContentSpec{DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete, Driver: "native.test.csi", Source: snapshotv1.VolumeSnapshotContentSource{SnapshotHandle: ptr.To("test-handle")}, VolumeSnapshotRef: corev1.ObjectReference{Kind: "VolumeSnapshot", APIVersion: snapshotv1.SchemeGroupVersion.String(), Name: s.Name, Namespace: s.Namespace, UID: s.UID}}}
	require.NoError(t, admin.Create(ctx, content))
	content.Status = &snapshotv1.VolumeSnapshotContentStatus{ReadyToUse: ptr.To(true), SnapshotHandle: ptr.To("test-handle")}
	require.NoError(t, admin.Status().Update(ctx, content))
	s.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true), BoundVolumeSnapshotContentName: ptr.To(content.Name), RestoreSize: ptr.To(resource.MustParse("1Gi"))}
	require.NoError(t, admin.Status().Update(ctx, s))
	var posts atomic.Int32
	var dropStatusACK atomic.Bool
	var failStatusWrite atomic.Bool
	var dropCreateACK atomic.Bool
	var dropCleanupACK atomic.Bool
	var pauseJob <-chan struct{}
	var jobArrived chan<- struct{}
	var orderingFailure atomic.Bool
	var denyAuthorityRead atomic.Bool
	transportCfg := rest.CopyConfig(cfg)
	transportCfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/status") && failStatusWrite.CompareAndSwap(true, false) {
				if req.Body != nil {
					_ = req.Body.Close()
				}
				return nil, fmt.Errorf("test unavailable status write")
			}
			if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/jobs") && pauseJob != nil {
				select {
				case jobArrived <- struct{}{}:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				select {
				case <-pauseJob:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}

			if req.Method == http.MethodPost && (strings.HasSuffix(req.URL.Path, "/jobs") || strings.HasSuffix(req.URL.Path, "/persistentvolumeclaims")) {
				posts.Add(1)
				held := &snapshotv1.VolumeSnapshot{}
				owner := &appsv1.ChainNode{}
				if admin.Get(ctx, client.ObjectKeyFromObject(s), held) != nil || admin.Get(ctx, client.ObjectKeyFromObject(c), owner) != nil || !hasString(held.Finalizers, exportprotocol.SourceFinalizer) || owner.Status.SnapshotExportAuthority == nil {
					orderingFailure.Store(true)
				}
			}
			response, err := base.RoundTrip(req)
			if err == nil && response.StatusCode < 300 && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/jobs") && dropCleanupACK.CompareAndSwap(true, false) {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				return nil, fmt.Errorf("test lost cleanup POST acknowledgement")
			}
			if err == nil && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/persistentvolumeclaims") && dropCreateACK.CompareAndSwap(true, false) {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				return nil, fmt.Errorf("test lost PVC acknowledgement")
			}
			if err == nil && response.StatusCode < 300 && req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/status") && dropStatusACK.CompareAndSwap(true, false) {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				return nil, fmt.Errorf("test lost status acknowledgement")
			}
			return response, err
		})
	}
	readerCfg := rest.CopyConfig(cfg)
	readerCfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
			if denyAuthorityRead.Load() && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/chainnodes/"+c.Name) {
				return nil, fmt.Errorf("test authority API unavailable")
			}
			return base.RoundTrip(req)
		})
	}
	authorityReader, err := client.New(readerCfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	controlled, err := client.New(transportCfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	r := &Reconciler{Client: controlled, APIReader: authorityReader, RestConfig: cfg, Scheme: scheme.Scheme, opts: &controllers.ControllerRunOptions{DataExporterImage: "exporter:test"}}
	refresh := func() { require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c)) }
	attempt := func() *appsv1.SnapshotExportAttempt {
		refresh()
		require.Len(t, c.Status.SnapshotExportAuthority.Attempts, 1)
		return c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
	}
	require.ErrorContains(t, r.reserveNativeAttempt(ctx, c, s), "deferred")
	require.NoError(t, r.reserveNativeAttempt(ctx, c, s))
	a := attempt()
	require.Equal(t, "Reserved", a.Phase)
	require.Zero(t, posts.Load())
	recipe, err := exportprotocol.DecodeRecipe(a)
	require.NoError(t, err)
	dropStatusACK.Store(true)
	require.NoError(t, r.holdNativeSource(ctx, c, a, recipe))
	a = attempt()
	require.Equal(t, "Held", a.Phase)
	require.NotEmpty(t, a.SourceHoldResourceVersion)
	require.Zero(t, posts.Load())
	// Both writers use the same observed attempt; only one may persist its random issuer.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := c.DeepCopy()
			errs <- r.issueNativeResource(ctx, local, a, recipe, true)
		}()
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	require.EqualValues(t, 1, posts.Load())
	require.False(t, orderingFailure.Load())
	a = attempt()
	require.NotEmpty(t, a.JobCreateIssuer)
	observedJob := &batchv1.Job{}
	require.NoError(t, admin.Get(ctx, nativeKey(recipe), observedJob))
	require.NoError(t, datasnapshot.ValidateNativeJob(observedJob, a, recipe))
	denyAuthorityRead.Store(true)
	require.Error(t, r.cleanupNativeOrphans(ctx, client.ObjectKeyFromObject(c)))
	denyAuthorityRead.Store(false)
	require.NoError(t, admin.Get(ctx, nativeKey(recipe), observedJob))

	c.Status.SnapshotExportAuthority = nil
	require.NoError(t, r.cleanupNativeOrphans(ctx, client.ObjectKeyFromObject(c)))
	require.NoError(t, admin.Get(ctx, nativeKey(recipe), observedJob))
	stillHeld := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), stillHeld))
	require.Contains(t, stillHeld.Finalizers, exportprotocol.SourceFinalizer)
	refresh()

	require.NoError(t, r.bindNativeResources(ctx, c, a, recipe))
	a = attempt()
	require.NotNil(t, a.Job, "phase=%s reason=%s", a.Phase, a.Reason)
	dropCreateACK.Store(true)
	require.Error(t, r.bindNativeResources(ctx, c, a, recipe))
	a = attempt()
	require.True(t, a.PVCCreateIssued)
	require.EqualValues(t, 2, posts.Load())
	require.NoError(t, r.bindNativeResources(ctx, c, a, recipe))
	a = attempt()
	require.NotNil(t, a.ScratchPVC)
	t.Run("RBAC exact worker identity", func(t *testing.T) {
		workerCfg := rest.CopyConfig(cfg)
		workerCfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + ns.Name + ":" + recipe.ServiceAccount.Name, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + ns.Name, "system:authenticated"}}
		worker, err := client.New(workerCfg, client.Options{Scheme: scheme.Scheme})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return worker.Get(ctx, client.ObjectKeyFromObject(c), &appsv1.ChainNode{}) == nil }, 5*time.Second, 50*time.Millisecond)
		require.True(t, apierrors.IsForbidden(worker.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "other"}, &appsv1.ChainNode{})))
		require.True(t, apierrors.IsForbidden(worker.Get(ctx, client.ObjectKeyFromObject(credentials), &corev1.Secret{})))
		require.True(t, apierrors.IsForbidden(worker.Status().Update(ctx, c.DeepCopy())))
	})
	t.Run("schema prevents stale resurrection and witness resets", func(t *testing.T) {
		fresh := c.DeepCopy()
		fresh.Status.SnapshotExportAuthority.LastSequence = 0
		require.Error(t, admin.Status().Update(ctx, fresh))
		fresh = c.DeepCopy()
		fresh.Status.SnapshotExportAuthority.Attempts[0].JobCreateIssued = false
		require.Error(t, admin.Status().Update(ctx, fresh))
		fresh = c.DeepCopy()
		fresh.Status.SnapshotExportAuthority.Attempts[0].Recipe += " "
		require.Error(t, admin.Status().Update(ctx, fresh))
	})
	// Envtest simulates CSI binding, kubelet execution and Job completion; no physical mount is claimed.
	pvc := &corev1.PersistentVolumeClaim{}
	require.NoError(t, admin.Get(ctx, nativeKey(recipe), pvc))
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: ns.Name + "-pv"}, Spec: corev1.PersistentVolumeSpec{StorageClassName: recipe.StorageClass, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: recipe.Driver, VolumeHandle: "clone-handle"}}, ClaimRef: &corev1.ObjectReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID}}}
	pvc.Spec.VolumeName = pv.Name
	require.NoError(t, admin.Update(ctx, pvc))
	pvc.Status.Phase = corev1.ClaimBound
	require.NoError(t, admin.Status().Update(ctx, pvc))
	t.Run("initial bound PVC waits for its unobserved PV", func(t *testing.T) {
		require.NoError(t, r.bindNativeResources(ctx, c, a, recipe))
		require.Equal(t, a, attempt())
		require.Nil(t, a.ScratchPV)
		require.EqualValues(t, 2, posts.Load())
	})
	require.NoError(t, admin.Create(ctx, pv))
	pv.Status.Phase = corev1.VolumeBound
	require.NoError(t, admin.Status().Update(ctx, pv))
	job := &batchv1.Job{}
	require.NoError(t, admin.Get(ctx, nativeKey(recipe), job))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: recipe.ResourceName + "-pod", Namespace: ns.Name, Labels: job.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
	require.NoError(t, admin.Create(ctx, pod))
	originalController := pod.OwnerReferences[0]
	for _, wrong := range []metav1.OwnerReference{
		{APIVersion: "batch/v1", Kind: "Job", Name: "other-job", UID: job.UID, Controller: ptr.To(true)},
		{APIVersion: "v1", Kind: "ConfigMap", Name: job.Name, UID: job.UID, Controller: ptr.To(true)},
	} {
		pod.OwnerReferences[0] = wrong
		require.NoError(t, admin.Update(ctx, pod))
		require.NoError(t, r.bindNativeResources(ctx, c, a, recipe))
		a = attempt()
		require.Equal(t, "ResourcesRequested", a.Phase)
	}
	pod.OwnerReferences[0] = originalController
	require.NoError(t, admin.Update(ctx, pod))

	t.Run("later binding proofs defer without authorizing", func(t *testing.T) {
		for _, path := range []string{"/volumesnapshots/" + s.Name, "/volumesnapshotcontents/" + content.Name, "/secrets/" + recipe.Credentials.Name, "/serviceaccounts/" + recipe.ServiceAccount.Name} {
			t.Run(path, func(t *testing.T) {
				reader, fired := nativeFaultReader(t, cfg, path, 503, 1)
				local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts}
				require.Error(t, local.recoverNativeExports(ctx, c))
				require.Positive(t, fired.Load())
				require.Equal(t, a, attempt())
				require.Nil(t, a.AuthorizedAt)
				require.EqualValues(t, 2, posts.Load())
			})
		}
	})
	require.NoError(t, r.bindNativeResources(ctx, c, a, recipe))
	a = attempt()
	require.Equal(t, "Uploading", a.Phase)
	_, _, err = exportprotocol.Authorize(c, c.UID, a.Sequence, a.ID, pod.UID)
	require.NoError(t, err)
	envelope := exportprotocol.EnvelopeFor(a, recipe)
	uploader, err := dataexporter.NewS3Exporter(ctx, dataexporter.S3Config{Region: recipe.Destination.Region, Endpoint: recipe.Destination.Endpoint, ForcePathStyle: recipe.Destination.ForcePathStyle, ExplicitCredentials: &aws.Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	require.NoError(t, err)
	archiveDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(archiveDir, "state.db"), []byte("actual archive byte fixture"), 0600))
	envelope.Result, err = uploader.UploadWithResult(ctx, archiveDir, recipe.Destination.Bucket, recipe.ObjectName, recipe.UploadOptions()...)
	require.NoError(t, err)
	objectMu.Lock()
	actualDigest := fmt.Sprintf("%x", sha256.Sum256(storedObject))
	actualSize := len(storedObject)
	objectMu.Unlock()
	require.Equal(t, actualDigest, envelope.Result.SHA256)
	require.EqualValues(t, actualSize, envelope.Result.SizeBytes)
	encoded, err := exportprotocol.EncodeEnvelope(envelope)
	require.NoError(t, err)
	require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "exporter", Image: recipe.Image, ImageID: "fixture", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(encoded)}}}}
	require.NoError(t, admin.Status().Update(ctx, pod))
	job.Status.StartTime = ptr.To(metav1.Now())
	job.Status.CompletionTime = ptr.To(metav1.Now())
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}, {Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, admin.Status().Update(ctx, job))
	t.Run("readiness loss defers the same completed upload", func(t *testing.T) {
		for _, change := range []string{"snapshot ready", "snapshot binding unavailable", "snapshot size unavailable", "content ready", "content handle unavailable"} {
			t.Run(change, func(t *testing.T) {
				snapshot := &snapshotv1.VolumeSnapshot{}
				currentContent := &snapshotv1.VolumeSnapshotContent{}
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), snapshot))
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(content), currentContent))
				savedSnapshot, savedContent := snapshot.Status.DeepCopy(), currentContent.Status.DeepCopy()
				switch change {
				case "snapshot ready":
					snapshot.Status.ReadyToUse = ptr.To(false)
				case "snapshot binding unavailable":
					snapshot.Status.BoundVolumeSnapshotContentName = nil
				case "snapshot size unavailable":
					snapshot.Status.RestoreSize = nil
				case "content ready":
					currentContent.Status.ReadyToUse = ptr.To(false)
				case "content handle unavailable":
					currentContent.Status.SnapshotHandle = nil
				}
				require.NoError(t, admin.Status().Update(ctx, snapshot))
				require.NoError(t, admin.Status().Update(ctx, currentContent))
				require.Error(t, r.recoverNativeExports(ctx, c))
				require.Equal(t, a, attempt())
				require.Nil(t, c.Status.SnapshotExports[0].Archive)
				require.EqualValues(t, 2, posts.Load())
				snapshot.Status, currentContent.Status = savedSnapshot, savedContent
				require.NoError(t, admin.Status().Update(ctx, snapshot))
				require.NoError(t, admin.Status().Update(ctx, currentContent))
			})
		}
	})
	t.Run("operational reads preserve completed upload", func(t *testing.T) {
		for _, fault := range []struct {
			name, path string
			code, skip int
		}{
			{"snapshot throttled", "/volumesnapshots/" + s.Name, 429, 0},
			{"content unavailable", "/volumesnapshotcontents/" + content.Name, 503, 0},
			{"secret forbidden", "/secrets/" + recipe.Credentials.Name, 403, 0},
			{"service account forbidden", "/serviceaccounts/" + recipe.ServiceAccount.Name, 403, 0},
			{"job server timeout", "/jobs/" + recipe.ResourceName, 504, 0},
			{"pvc internal error", "/persistentvolumeclaims/" + recipe.ResourceName, 500, 0},
			{"pv unauthorized", "/persistentvolumes/" + pv.Name, 401, 0},
			{"pod transport", "/pods/" + pod.Name, 0, 0},
			{"worker list unavailable", "/pods", 503, 0},
			{"publication snapshot", "/volumesnapshots/" + s.Name, 503, 1},
			{"publication content", "/volumesnapshotcontents/" + content.Name, 503, 1},
			{"publication secret", "/secrets/" + recipe.Credentials.Name, 503, 1},
			{"publication service account", "/serviceaccounts/" + recipe.ServiceAccount.Name, 503, 1},
		} {
			if !t.Run(fault.name, func(t *testing.T) {
				reader, fired := nativeFaultReader(t, cfg, fault.path, fault.code, fault.skip)
				local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts}
				before := posts.Load()
				err := local.recoverNativeExports(ctx, c)
				require.Positive(t, fired.Load(), "the selected wire read must be reached")
				require.Error(t, err)
				handled, fatal := local.handleNativeOutcome(ctx, c, err)
				require.True(t, handled)
				require.NoError(t, fatal)
				require.Equal(t, a, attempt(), "a read failure must preserve the exact authorized attempt")
				require.Nil(t, c.Status.SnapshotExports[0].Archive)
				require.Equal(t, before, posts.Load())
				held := &snapshotv1.VolumeSnapshot{}
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), held))
				require.Contains(t, held.Finalizers, exportprotocol.SourceFinalizer)
				holders, err := nativeHolders(held)
				require.NoError(t, err)
				require.Equal(t, nativeHolder(recipe, a), holders[a.ID])
				for _, obj := range []client.Object{&batchv1.Job{}, &corev1.PersistentVolumeClaim{}} {
					require.NoError(t, admin.Get(ctx, nativeKey(recipe), obj))
				}
				resultPod := &corev1.Pod{}
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(pod), resultPod))
				require.Equal(t, string(encoded), resultPod.Status.ContainerStatuses[0].State.Terminated.Message)
			}) {
				t.FailNow()
			}
		}
	})
	failStatusWrite.Store(true)
	require.Error(t, r.publishNativeArchive(ctx, c, a, recipe))
	a = attempt()
	require.Equal(t, "Uploading", a.Phase)
	require.Nil(t, c.Status.SnapshotExports[0].Archive)
	retained := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), retained))
	require.Contains(t, retained.Finalizers, exportprotocol.SourceFinalizer)
	require.NoError(t, admin.Get(ctx, nativeKey(recipe), &batchv1.Job{}))
	dropStatusACK.Store(true)
	require.NoError(t, r.publishNativeArchive(ctx, c, a, recipe))
	a = attempt()
	require.Equal(t, "Succeeded", a.Phase)
	require.NotNil(t, c.Status.SnapshotExports[0].Archive)
	descriptor := c.Status.SnapshotExports[0].Archive
	require.Equal(t, actualDigest, descriptor.SHA256)
	require.EqualValues(t, actualSize, descriptor.SizeBytes)
	require.Equal(t, envelope.Result.ObjectKey, descriptor.ObjectKey)
	require.EqualValues(t, 2, posts.Load(), "same attempt issued exactly one upload Job and PVC")
	require.Equal(t, s.UID, descriptor.SourceSnapshot.UID)
	require.Equal(t, content.UID, descriptor.SourceContent.UID)
	require.Equal(t, recipe.Driver, descriptor.CSIDriver)
	require.Equal(t, credentials.UID, descriptor.ReadIdentity.CredentialsSecret.UID)
	require.Equal(t, recipe.ServiceAccount, descriptor.ReadIdentity.ExecutionServiceAccount)
	require.Equal(t, "Secret", descriptor.ReadIdentity.Mode)
	require.Equal(t, "123", descriptor.SourceHeight)
	require.Equal(t, "CaptureObserved", descriptor.SourceHeightKind)

	t.Run("availability corrections", func(t *testing.T) {
		nativeReviewFixesAPI(t, admin, r, c, s, a, recipe, &dropCleanupACK, &posts, &failStatusWrite, &dropStatusACK)
	})
	held := &snapshotv1.VolumeSnapshot{}
	require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), held))
	require.Contains(t, held.Finalizers, exportprotocol.SourceFinalizer)
	require.NoError(t, r.cleanupNativeAttempt(ctx, c, a, recipe))
	require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), held))
	require.NotContains(t, held.Finalizers, exportprotocol.SourceFinalizer)
	t.Run("descriptor is immutable", func(t *testing.T) {
		refresh()
		fresh := c.DeepCopy()
		fresh.Status.SnapshotExports[0].Archive.SHA256 = strings.Repeat("b", 64)
		require.Error(t, admin.Status().Update(ctx, fresh))
		fresh = c.DeepCopy()
		fresh.Status.SnapshotExports[0].Archive = nil
		require.True(t, apierrors.IsInvalid(admin.Status().Update(ctx, fresh)))
	})
	t.Run("delayed POST cannot resurrect cancelled source authority", func(t *testing.T) {
		refresh()
		late := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "late-source", Namespace: ns.Name}, Spec: s.Spec}
		require.NoError(t, admin.Create(ctx, late))
		lateContent := content.DeepCopy()
		lateContent.ObjectMeta = metav1.ObjectMeta{Name: ns.Name + "-late-content"}
		lateContent.Spec.VolumeSnapshotRef.Name = late.Name
		lateContent.Spec.VolumeSnapshotRef.UID = late.UID
		require.NoError(t, admin.Create(ctx, lateContent))
		lateContent.Status = &snapshotv1.VolumeSnapshotContentStatus{ReadyToUse: ptr.To(true), SnapshotHandle: ptr.To("late-handle")}
		require.NoError(t, admin.Status().Update(ctx, lateContent))
		late.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true), BoundVolumeSnapshotContentName: ptr.To(lateContent.Name), RestoreSize: ptr.To(resource.MustParse("1Gi"))}
		require.NoError(t, admin.Status().Update(ctx, late))
		require.NoError(t, r.reserveNativeAttempt(ctx, c, late))
		refresh()
		latest := c.Status.SnapshotExportAuthority.Attempts[len(c.Status.SnapshotExportAuthority.Attempts)-1].DeepCopy()
		lateRecipe, err := exportprotocol.DecodeRecipe(latest)
		require.NoError(t, err)
		require.NoError(t, r.holdNativeSource(ctx, c, latest, lateRecipe))
		refresh()
		latest = exportprotocol.FindAttempt(c, latest.Sequence, latest.ID).DeepCopy()
		release := make(chan struct{})
		arrived := make(chan struct{}, 1)
		pauseJob = release
		jobArrived = arrived
		var releaseOnce sync.Once
		postCtx, cancelPost := context.WithCancel(t.Context())
		result := make(chan error, 1)
		joined := make(chan struct{})
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			cancelPost()
			select {
			case <-joined:
			case <-time.After(10 * time.Second):
				t.Error("delayed native POST did not join during cleanup")
			}
		})
		caller := c.DeepCopy()
		go func() {
			defer close(joined)
			result <- r.issueNativeResource(postCtx, caller, latest, lateRecipe, true)
		}()
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("native POST did not reach delay fixture")
		}
		refresh()
		current := exportprotocol.FindAttempt(c, latest.Sequence, latest.ID).DeepCopy()
		require.NoError(t, r.invalidateNativeAttempt(ctx, c, current, "PolicyDisabled"))
		refresh()
		current = exportprotocol.FindAttempt(c, latest.Sequence, latest.ID).DeepCopy()
		require.NoError(t, r.cleanupNativeAttempt(ctx, c, current, lateRecipe))
		require.NoError(t, admin.Delete(ctx, late))
		replacement := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: late.Name, Namespace: ns.Name}, Spec: late.Spec}
		require.NoError(t, admin.Create(ctx, replacement))
		require.NotEqual(t, late.UID, replacement.UID)
		releaseOnce.Do(func() { close(release) })
		select {
		case err := <-result:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("delayed native POST did not join")
		}
		require.NoError(t, admin.Get(ctx, nativeKey(lateRecipe), &batchv1.Job{}))
		refresh()
		_, _, err = exportprotocol.Authorize(c, c.UID, latest.Sequence, latest.ID, "late-pod")
		require.Error(t, err)
		current = exportprotocol.FindAttempt(c, latest.Sequence, latest.ID)
		require.Equal(t, "Invalidated", current.Phase)
		require.Equal(t, "Complete", current.RemoteCleanupState)
		require.NoError(t, r.cleanupNativeAttempt(ctx, c, current.DeepCopy(), lateRecipe))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(replacement), replacement))
		require.NotContains(t, replacement.Finalizers, exportprotocol.SourceFinalizer)
	})
	t.Run("native cleanup acknowledgement and watermark compaction", func(t *testing.T) {
		pauseJob = nil
		refresh()
		original := exportprotocol.FindAttempt(c, a.Sequence, a.ID).DeepCopy()
		_, err := r.setSnapshotExportPhase(ctx, c, original.ExportID, appsv1.SnapshotExportPhaseDeleting, "")
		require.NoError(t, err)
		refresh()
		original = exportprotocol.FindAttempt(c, a.Sequence, a.ID).DeepCopy()
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, original, recipe))
		refresh()
		original = exportprotocol.FindAttempt(c, a.Sequence, a.ID).DeepCopy()
		require.NotEmpty(t, original.CleanupIssuer)
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, original, recipe))
		refresh()
		original = exportprotocol.FindAttempt(c, a.Sequence, a.ID).DeepCopy()
		require.NotNil(t, original.CleanupJob)
		cleanupJob := &batchv1.Job{}
		cleanupKey := client.ObjectKey{Namespace: ns.Name, Name: "delete-" + original.ID}
		require.NoError(t, admin.Get(ctx, cleanupKey, cleanupJob))
		cleanupPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cleanupJob.Name + "-pod", Namespace: ns.Name, Labels: cleanupJob.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: cleanupJob.Name, UID: cleanupJob.UID, Controller: ptr.To(true)}}}, Spec: *cleanupJob.Spec.Template.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, cleanupPod))
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, original, recipe))
		refresh()
		original = exportprotocol.FindAttempt(c, a.Sequence, a.ID).DeepCopy()
		require.NotNil(t, original.CleanupPod)
		_, _, _, err = exportprotocol.AuthorizeCleanup(c, c.UID, original.Sequence, original.ID, cleanupPod.UID)
		require.NoError(t, err)
		_, _, _, err = exportprotocol.AuthorizeCleanup(c, "replaced-owner", original.Sequence, original.ID, cleanupPod.UID)
		require.Error(t, err)
		_, _, _, err = exportprotocol.AuthorizeCleanup(c, c.UID, original.Sequence, original.ID, "stale-pod")
		require.Error(t, err)
		deleting := c.DeepCopy()
		deleting.DeletionTimestamp = ptr.To(metav1.Now())
		_, _, _, err = exportprotocol.AuthorizeCleanup(deleting, c.UID, original.Sequence, original.ID, cleanupPod.UID)
		require.Error(t, err)
		cleanupPod.Status.Phase = corev1.PodSucceeded
		cleanupPod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "exporter", Image: recipe.Image, ImageID: "fixture", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
		require.NoError(t, admin.Status().Update(ctx, cleanupPod))
		cleanupJob.Status.StartTime = ptr.To(metav1.Now())
		cleanupJob.Status.CompletionTime = ptr.To(metav1.Now())
		cleanupJob.Status.Succeeded = 1
		cleanupJob.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}, {Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		require.NoError(t, admin.Status().Update(ctx, cleanupJob))
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, original, recipe))
		refresh()
		original = exportprotocol.FindAttempt(c, a.Sequence, a.ID).DeepCopy()
		require.True(t, original.CleanupComplete)
		require.Equal(t, appsv1.SnapshotExportPhaseDeleted, c.Status.SnapshotExports[0].Phase)
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, original, recipe))
		// Envtest has no PVC-protection controller; simulate its unused-claim release through the API.
		remainingPVC := &corev1.PersistentVolumeClaim{}
		if err := admin.Get(ctx, nativeKey(recipe), remainingPVC); err == nil {
			remainingPVC.Finalizers = nil
			require.NoError(t, admin.Update(ctx, remainingPVC))
		} else {
			require.True(t, apierrors.IsNotFound(err))
		}
		require.NoError(t, r.cleanupNativeAttempt(ctx, c, original, recipe))
		refresh()
		original = exportprotocol.FindAttempt(c, a.Sequence, a.ID).DeepCopy()
		require.Equal(t, "Complete", original.LocalCleanupState)
		require.NoError(t, r.compactNativeAttempts(ctx, c))
		refresh()
		require.NotNil(t, exportprotocol.FindAttempt(c, original.Sequence, original.ID), "source still exists")
		require.NotNil(t, snapshotExportByID(c, original.ExportID).Archive)
		require.NoError(t, admin.Delete(ctx, s))
		stale := c.DeepCopy()
		watermark := c.Status.SnapshotExportAuthority.LastSequence
		require.NoError(t, r.compactNativeAttempts(ctx, c))
		refresh()
		require.Equal(t, watermark, c.Status.SnapshotExportAuthority.LastSequence)
		require.Nil(t, exportprotocol.FindAttempt(c, original.Sequence, original.ID))
		require.Nil(t, snapshotExportByID(c, original.ExportID))
		require.True(t, apierrors.IsConflict(admin.Status().Update(ctx, stale)))
		resurrect := c.DeepCopy()
		resurrect.Status.SnapshotExportAuthority.Attempts = append(resurrect.Status.SnapshotExportAuthority.Attempts, *original)
		require.True(t, apierrors.IsInvalid(admin.Status().Update(ctx, resurrect)))
		_, _, err = exportprotocol.Authorize(c, c.UID, original.Sequence, original.ID, original.Pod.UID)
		require.Error(t, err)
		delayed := datasnapshot.NativeJob(original, recipe)
		require.NoError(t, admin.Create(ctx, delayed))
		require.NoError(t, r.cleanupNativeOrphans(ctx, client.ObjectKeyFromObject(c)))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, nativeKey(recipe), &batchv1.Job{})))
	})

	t.Run("policy removed before reservation does not strand authority finalizer", func(t *testing.T) {
		owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "finalizer-only-", Namespace: ns.Name, Finalizers: []string{exportprotocol.AuthorityFinalizer}}, Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{Image: "application:test", App: "app"}}}
		require.NoError(t, admin.Create(ctx, owner))
		require.NoError(t, admin.Delete(ctx, owner))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(owner), owner))
		require.NoError(t, r.recoverNativeExports(ctx, owner))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(owner), &appsv1.ChainNode{})))
	})

	t.Run("escaped descriptor budget rejects reservation before resource POST", func(t *testing.T) {
		owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "oversize-", Namespace: ns.Name}, Spec: *c.Spec.DeepCopy()}
		budgetSource := s.DeepCopy()
		budgetSource.ObjectMeta = metav1.ObjectMeta{Name: "budget-source", Namespace: ns.Name}
		require.NoError(t, admin.Create(ctx, budgetSource))
		budgetSource.Status = s.Status.DeepCopy()
		require.NoError(t, admin.Status().Update(ctx, budgetSource))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(content), content))
		content.Spec.VolumeSnapshotRef.Name = budgetSource.Name
		content.Spec.VolumeSnapshotRef.UID = budgetSource.UID
		require.NoError(t, admin.Update(ctx, content))
		owner.Spec.Persistence.Snapshots.ExportTarball.S3.Endpoint = ptr.To("https://store.example/" + strings.Repeat("a", 8000))
		require.NoError(t, admin.Create(ctx, owner))
		before := posts.Load()
		require.ErrorContains(t, r.reserveNativeAttempt(ctx, owner, budgetSource), "deferred")
		err := r.reserveNativeAttempt(ctx, owner, budgetSource)
		require.ErrorContains(t, err, "descriptor exceeds")
		require.Equal(t, before, posts.Load())
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(owner), owner))
		require.Nil(t, owner.Status.SnapshotExportAuthority)
	})

}
func hasString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func nativeProtocolAPIMatrix(t *testing.T, admin client.Client, namespace string) {
	t.Helper()
	modes := []struct {
		name, protocol, provider, auth string
		valid                          bool
	}{
		{"legacy omitted ambient", "", "s3", "ambient", true},
		{"legacy explicit ambient", "legacy", "s3", "ambient", true},
		{"legacy secret", "", "s3", "secret", true},
		{"legacy service account", "legacy", "s3", "sa", true},
		{"legacy gcs", "", "gcs", "secret", true},
		{"legacy explicit gcs", "legacy", "gcs", "secret", true},
		{"native secret", "source-v1", "s3", "secret", true},
		{"native ambient", "source-v1", "s3", "ambient", false},
		{"native service account", "source-v1", "s3", "sa", false},
		{"native gcs", "source-v1", "gcs", "secret", false},
		{"native secret plus service account", "source-v1", "s3", "both", false},
	}
	for _, mode := range modes {
		for _, placement := range []string{"node", "group", "validator"} {
			t.Run(mode.name+"/"+placement, func(t *testing.T) {
				cfg := &appsv1.ExportTarballConfig{Protocol: mode.protocol}
				if mode.provider == "s3" {
					cfg.S3 = &appsv1.S3ExportConfig{Bucket: "bucket", Region: "region"}
					if mode.auth == "secret" || mode.auth == "both" {
						cfg.S3.CredentialsSecret = &corev1.LocalObjectReference{Name: "credentials"}
					}
					if mode.auth == "sa" || mode.auth == "both" {
						cfg.S3.ServiceAccountName = ptr.To("identity")
					}
				} else {
					cfg.GCS = &appsv1.GcsExportConfig{Bucket: "bucket", CredentialsSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "gcs"}, Key: "credentials.json"}}
				}
				persistence := &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "1h", ExportTarball: cfg}}
				var obj client.Object
				app := appsv1.AppSpec{Image: "application:test", App: "app"}
				if placement == "node" {
					obj = &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "matrix-", Namespace: namespace}, Spec: appsv1.ChainNodeSpec{App: app, Persistence: persistence}}
				} else {
					group := appsv1.NodeGroupSpec{Name: "group", Persistence: persistence}
					if placement == "validator" {
						group.Persistence = nil
						group.Validator = &appsv1.NodeSetValidatorConfig{Persistence: persistence}
					}
					obj = &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{GenerateName: "matrix-", Namespace: namespace}, Spec: appsv1.ChainNodeSetSpec{App: app, Nodes: []appsv1.NodeGroupSpec{group}}}
				}
				err := admin.Create(t.Context(), obj)
				if mode.valid {
					require.NoError(t, err)
				} else {
					require.True(t, apierrors.IsInvalid(err), "expected schema denial, got %v", err)
					if mode.name == "native gcs" {
						require.ErrorContains(t, err, "source-v1 requires S3 Secret credentials")
						require.NotContains(t, err.Error(), "no such key")
					}
				}
			})
		}
	}
	t.Run("legacy multi-record status remains writable", func(t *testing.T) {
		node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "legacy-", Namespace: namespace}, Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{Image: "application:test", App: "app"}}}
		require.NoError(t, admin.Create(t.Context(), node))
		node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{ID: "one", SnapshotName: "snapshot-one", ObjectName: "one", Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown}, Phase: appsv1.SnapshotExportPhaseCleanupRequired}, {ID: "two", SnapshotName: "snapshot-two", ObjectName: "two", Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderUnknown}, Phase: appsv1.SnapshotExportPhaseCleanupRequired}}
		require.NoError(t, admin.Status().Update(t.Context(), node))
		node.Status.SnapshotExports[1].Message = "updated"
		require.NoError(t, admin.Status().Update(t.Context(), node))
		node.Status.SnapshotExports = append(node.Status.SnapshotExports, node.Status.SnapshotExports[0])
		require.True(t, apierrors.IsInvalid(admin.Status().Update(t.Context(), node)))
	})
}

func nativeFaultReader(t *testing.T, cfg *rest.Config, path string, code, skip int) (client.Client, *atomic.Int32) {
	t.Helper()
	var reads, fired atomic.Int32
	faultConfig := rest.CopyConfig(cfg)
	faultConfig.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, path) || int(reads.Add(1)) <= skip {
				return base.RoundTrip(req)
			}
			fired.Add(1)
			if code == 0 {
				return nil, fmt.Errorf("test native read transport unavailable")
			}
			reason := metav1.StatusReasonInternalError
			switch code {
			case 401:
				reason = metav1.StatusReasonUnauthorized
			case 403:
				reason = metav1.StatusReasonForbidden
			case 429:
				reason = metav1.StatusReasonTooManyRequests
			case 503:
				reason = metav1.StatusReasonServiceUnavailable
			case 504:
				reason = metav1.StatusReasonTimeout
			}
			body, err := json.Marshal(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: int32(code), Reason: reason, Message: "test native read unavailable"})
			if err != nil {
				return nil, err
			}
			// No Retry-After: client-go must expose the fault to reconciliation.
			return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
		})
	}
	reader, err := client.New(faultConfig, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	return reader, &fired
}
