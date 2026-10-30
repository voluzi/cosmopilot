package chainnode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v5/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
	"github.com/voluzi/cosmopilot/v5/internal/resourcecleanup"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
	"github.com/voluzi/cosmopilot/v5/pkg/utils"
)

func nativeReviewFixesAPI(t *testing.T, admin client.Client, r *Reconciler, template *appsv1.ChainNode, source *snapshotv1.VolumeSnapshot, completed *appsv1.SnapshotExportAttempt, original exportprotocol.Recipe, dropCleanupACK *atomic.Bool, posts *atomic.Int32, failStatusWrite *atomic.Bool, dropStatusACK *atomic.Bool) {
	ctx := t.Context()
	seed := func(t *testing.T, namespace, phase string, customize func(*appsv1.SnapshotExportAttempt, *appsv1.SnapshotExportStatus)) (*appsv1.ChainNode, *appsv1.SnapshotExportAttempt, exportprotocol.Recipe) {
		t.Helper()
		c := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "correction-", Namespace: namespace, Finalizers: []string{exportprotocol.AuthorityFinalizer, "test.example/observe"}}, Spec: *template.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, c))
		id := string(uuid.NewUUID())
		recipe := original
		recipe.Namespace = namespace
		recipe.Owner = identity(c)
		recipe.ExportID = "export-" + id
		recipe.ObjectName = recipe.ExportID
		recipe.ResourceName = recipe.ExportID
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cred-" + id[:8], Namespace: namespace}, Data: map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("key"), "AWS_SECRET_ACCESS_KEY": []byte("secret")}}
		require.NoError(t, admin.Create(ctx, secret))
		recipe.Credentials = identity(secret)
		recipe.Destination.CredentialsSecret = &appsv1.SnapshotExportSecretReference{Name: secret.Name}
		c.Spec.Persistence.Snapshots.ExportTarball.S3.CredentialsSecret.Name = secret.Name
		require.NoError(t, admin.Update(ctx, c))
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "export-" + string(c.UID), Namespace: namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: c.Name, UID: c.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, AutomountServiceAccountToken: ptr.To(false)}
		require.NoError(t, admin.Create(ctx, sa))
		recipe.ServiceAccount = identity(sa)
		encoded, digest, err := exportprotocol.EncodeRecipe(recipe)
		require.NoError(t, err)
		a := completed.DeepCopy()
		a.Sequence = 1
		a.ID = id
		a.ExportID = recipe.ExportID
		a.Recipe = encoded
		a.RecipeSHA256 = digest
		a.Phase = phase
		a.LocalCleanupState = "Complete"
		a.RemoteCleanupState = "Complete"
		a.Job = &appsv1.ExportResourceIdentity{Name: recipe.ResourceName, UID: types.UID("job-" + id)}
		a.ScratchPVC = &appsv1.ExportResourceIdentity{Name: recipe.ResourceName, UID: types.UID("pvc-" + id)}
		a.Pod = &appsv1.ExportResourceIdentity{Name: recipe.ResourceName + "-pod", UID: types.UID("pod-" + id)}
		envelope := exportprotocol.EnvelopeFor(a, recipe)
		previous, err := exportprotocol.DecodeEnvelope([]byte(completed.CompletedEnvelope))
		require.NoError(t, err)
		envelope.Result = previous.Result
		envelope.Result.ObjectKey = recipe.ObjectName + recipe.Compression.Extension()
		body, err := exportprotocol.EncodeEnvelope(envelope)
		require.NoError(t, err)
		a.CompletedEnvelope = string(body)
		archive, err := exportprotocol.NewArchive(a, recipe, envelope, metav1.Now())
		require.NoError(t, err)
		e := appsv1.SnapshotExportStatus{ID: a.ExportID, SnapshotName: recipe.Snapshot.Name, SnapshotUID: recipe.Snapshot.UID, ObjectName: recipe.ObjectName, Destination: recipe.Destination, Phase: appsv1.SnapshotExportPhaseUploaded, DeleteOnExpire: true, ProvenanceVersion: 1, Archive: archive, RemoteCleanupState: "Complete"}
		if phase != "Succeeded" {
			a.CompletedEnvelope = ""
			a.CompletedAt = nil
			e.Archive = nil
			e.Phase = appsv1.SnapshotExportPhaseUploading
		}
		if phase != "Uploading" && phase != "Succeeded" {
			a.AuthorizedAt = nil
		}
		if customize != nil {
			customize(a, &e)
		}
		if a.Recipe != encoded {
			recipe, err = exportprotocol.DecodeRecipe(a)
			require.NoError(t, err)
			if phase == "Succeeded" {
				envelope = exportprotocol.EnvelopeFor(a, recipe)
				envelope.Result = previous.Result
				envelope.Result.ObjectKey = recipe.ObjectName + recipe.Compression.Extension()
				body, err = exportprotocol.EncodeEnvelope(envelope)
				require.NoError(t, err)
				a.CompletedEnvelope = string(body)
				e.Archive, err = exportprotocol.NewArchive(a, recipe, envelope, metav1.Now())
				require.NoError(t, err)
			}
		}
		c.Status.SnapshotExportAuthority = &appsv1.SnapshotExportAuthority{Version: 1, LastSequence: 1, Attempts: []appsv1.SnapshotExportAttempt{*a}}
		c.Status.SnapshotExports = []appsv1.SnapshotExportStatus{e}
		require.NoError(t, admin.Status().Update(ctx, c))
		return c, c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy(), recipe
	}
	refresh := func(t *testing.T, c *appsv1.ChainNode) *appsv1.SnapshotExportAttempt {
		t.Helper()
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		return c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
	}
	t.Run("persisted legacy export completes after switching to native", func(t *testing.T) {
		c := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "legacy-switch-", Namespace: template.Namespace}, Spec: *template.Spec.DeepCopy()}
		c.Spec.Persistence.Snapshots.ExportTarball.Protocol = ""
		c.Spec.Persistence.Snapshots.ExportTarball.S3.Bucket = "recorded-bucket"
		require.NoError(t, admin.Create(ctx, c))
		c.SetEstablishedChainID("legacy-chain")
		require.NoError(t, admin.Status().Update(ctx, c))
		s, _ := nativeReviewSource(t, admin, source, original.Driver)
		s.TypeMeta = metav1.TypeMeta{APIVersion: snapshotv1.SchemeGroupVersion.String(), Kind: "VolumeSnapshot"}
		e, err := newSnapshotExportStatus(c, s)
		require.NoError(t, err)
		c.Status.SnapshotExports = []appsv1.SnapshotExportStatus{e}
		require.NoError(t, admin.Status().Update(ctx, c))
		clients, err := kubernetes.NewForConfig(r.RestConfig)
		require.NoError(t, err)
		local := &Reconciler{Client: admin, APIReader: admin, ClientSet: clients, RestConfig: r.RestConfig, Scheme: r.Scheme, opts: r.opts, recorder: record.NewFakeRecorder(20)}
		require.NoError(t, local.exportTarball(ctx, c, s))
		c.Spec.Persistence.Snapshots.ExportTarball.Protocol = "source-v1"
		c.Spec.Persistence.Snapshots.ExportTarball.S3.Bucket = "current-bucket"
		require.NoError(t, admin.Update(ctx, c))
		t.Run("start recovery", func(t *testing.T) {
			require.NoError(t, local.exportTarball(ctx, c, s), "start recovery must use persisted legacy provenance")
		})
		jobs := &batchv1.JobList{}
		require.NoError(t, admin.List(ctx, jobs, client.InNamespace(c.Namespace)))
		var job *batchv1.Job
		for i := range jobs.Items {
			if metav1.IsControlledBy(&jobs.Items[i], c) {
				job = &jobs.Items[i]
				break
			}
		}
		require.NotNil(t, job)
		require.Contains(t, job.Spec.Template.Spec.Containers[0].Args, "recorded-bucket")
		job.Status.Succeeded = 1
		job.Status.StartTime, job.Status.CompletionTime = ptr.To(metav1.Now()), ptr.To(metav1.Now())
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}, {Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		require.NoError(t, admin.Status().Update(ctx, job))
		ready, err := local.isTarballReady(ctx, c, s)
		require.NoError(t, err)
		require.True(t, ready)
		require.NoError(t, local.finishTarballExport(ctx, c, s))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Nil(t, c.Status.SnapshotExportAuthority)
		require.Equal(t, appsv1.SnapshotExportPhaseUploaded, c.Status.SnapshotExports[0].Phase)
		require.Equal(t, "recorded-bucket", c.Status.SnapshotExports[0].Destination.Bucket)
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), s))
		require.Equal(t, tarballFinished, s.Annotations[controllers.AnnotationExportingTarball])
		err = admin.Get(ctx, client.ObjectKeyFromObject(job), job)
		require.True(t, apierrors.IsNotFound(err) || err == nil && !job.DeletionTimestamp.IsZero(), "legacy cleanup must request deletion")
	})
	t.Run("persisted native export never enters legacy start after configuration change", func(t *testing.T) {
		c, a, _ := seed(t, template.Namespace, "Uploading", nil)
		c.Spec.Persistence.Snapshots.ExportTarball.Protocol = ""
		require.NoError(t, admin.Update(ctx, c))
		local := &Reconciler{Client: admin, APIReader: admin, RestConfig: r.RestConfig, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
		s := source.DeepCopy()
		err := local.exportTarball(ctx, c, s)
		_, fatal := local.handleNativeSnapshotOutcome(ctx, c, s, s.Name, err)
		require.NoError(t, fatal)
		ready, err := local.isTarballReady(ctx, c, s)
		require.NoError(t, err)
		require.False(t, ready)
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Equal(t, a, exportprotocol.FindAttempt(c, a.Sequence, a.ID))
		require.Len(t, c.Status.SnapshotExports, 1)
		jobs := &batchv1.JobList{}
		require.NoError(t, admin.List(ctx, jobs, client.InNamespace(c.Namespace)))
		for _, job := range jobs.Items {
			require.False(t, metav1.IsControlledBy(&job, c))
		}
	})
	// issued seeds a held attempt whose Job and PVC exist, as recovery finds them after both POSTs.
	issued := func(t *testing.T, age time.Duration) (*appsv1.ChainNode, *snapshotv1.VolumeSnapshot, *appsv1.SnapshotExportAttempt, exportprotocol.Recipe, *batchv1.Job, *corev1.PersistentVolumeClaim) {
		t.Helper()
		s, content := nativeReviewSource(t, admin, source, original.Driver)
		c, a, recipe := seed(t, template.Namespace, "ResourcesRequested", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			rec, err := exportprotocol.DecodeRecipe(a)
			require.NoError(t, err)
			rec.Snapshot, rec.Content = identity(s), identity(content)
			a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
			require.NoError(t, err)
			a.Job, a.ScratchPVC, a.ScratchPV, a.Pod = nil, nil, nil, nil
			a.LocalCleanupState, a.RemoteCleanupState = "", ""
			a.CreatedAt = metav1.NewTime(time.Now().Add(-age))
			e.SnapshotName, e.SnapshotUID = s.Name, s.UID
			e.RemoteCleanupState = ""
		})
		holders, err := json.Marshal(map[string]string{a.ID: nativeHolder(recipe, a)})
		require.NoError(t, err)
		s.Annotations[exportprotocol.HoldAnnotation] = string(holders)
		s.Annotations[controllers.AnnotationExportingTarball] = "true"
		s.Finalizers = append(s.Finalizers, exportprotocol.SourceFinalizer)
		require.NoError(t, admin.Update(ctx, s))
		job := datasnapshot.NativeJob(a, recipe)
		require.NoError(t, admin.Create(ctx, job))
		pvc := datasnapshot.NativePVC(a, recipe, job.UID)
		require.NoError(t, admin.Create(ctx, pvc))
		return c, s, a, recipe, job, pvc
	}
	bindScratch := func(t *testing.T, recipe exportprotocol.Recipe, pvc *corev1.PersistentVolumeClaim, createPV bool) {
		t.Helper()
		pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: recipe.ResourceName}, Spec: corev1.PersistentVolumeSpec{StorageClassName: recipe.StorageClass, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: recipe.Driver, VolumeHandle: recipe.ResourceName}}, ClaimRef: &corev1.ObjectReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID}}}
		if createPV {
			require.NoError(t, admin.Create(ctx, pv))
			pv.Status.Phase = corev1.VolumeBound
			require.NoError(t, admin.Status().Update(ctx, pv))
		}
		pvc.Spec.VolumeName = pv.Name
		require.NoError(t, admin.Update(ctx, pvc))
		pvc.Status.Phase = corev1.ClaimBound
		require.NoError(t, admin.Status().Update(ctx, pvc))
	}
	workerPod := func(t *testing.T, job *batchv1.Job, status corev1.PodStatus) *corev1.Pod {
		t.Helper()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace, Labels: job.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, pod))
		pod.Status = status
		require.NoError(t, admin.Status().Update(ctx, pod))
		return pod
	}
	// requireSafeInvalidation asserts zero S3 authority was ever granted and local authority is released.
	requireSafeInvalidation := func(t *testing.T, c *appsv1.ChainNode, s *snapshotv1.VolumeSnapshot, reason string, recipe exportprotocol.Recipe) {
		t.Helper()
		a := refresh(t, c)
		require.Equal(t, "Invalidated", a.Phase)
		require.Equal(t, reason, a.Reason)
		require.Nil(t, a.AuthorizedAt)
		require.Equal(t, "Complete", a.RemoteCleanupState)
		for range 2 {
			if err := r.recoverNativeExports(ctx, c); err != nil {
				_, fatal := r.handleNativeOutcome(ctx, c, err)
				require.NoError(t, fatal)
			}
			// Envtest has no PVC-protection controller; release the unused fixture claim.
			pvc := &corev1.PersistentVolumeClaim{}
			if err := admin.Get(ctx, nativeKey(recipe), pvc); err == nil {
				controllerutil.RemoveFinalizer(pvc, "kubernetes.io/pvc-protection")
				require.NoError(t, admin.Update(ctx, pvc))
			}
		}
		if err := r.recoverNativeExports(ctx, c); err != nil {
			_, fatal := r.handleNativeOutcome(ctx, c, err)
			require.NoError(t, fatal)
		}
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Equal(t, "Complete", c.Status.SnapshotExports[0].RemoteCleanupState)
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), s))
		require.NotContains(t, s.Finalizers, exportprotocol.SourceFinalizer)
		require.Equal(t, tarballNativeFailed, s.Annotations[controllers.AnnotationExportingTarball])
		require.False(t, snapshotExportInFlight(c, s), "count retention must not stay pinned")
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, nativeKey(recipe), &batchv1.Job{})))
	}
	recoverUntil := func(t *testing.T, c *appsv1.ChainNode, ready func() bool) {
		t.Helper()
		for range 6 {
			if err := r.recoverNativeExports(ctx, c); err != nil {
				_, fatal := r.handleNativeOutcome(ctx, c, err)
				require.NoError(t, fatal)
			}
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			if ready() {
				return
			}
		}
		require.True(t, ready(), "recovery did not settle: %+v", c.Status.SnapshotExportAuthority)
	}
	for name, status := range map[string]corev1.PodStatus{
		"pod failed":             {Phase: corev1.PodFailed},
		"pod succeeded":          {Phase: corev1.PodSucceeded},
		"exporter already ended": {Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "exporter", Image: original.Image, ImageID: "fixture", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}},
	} {
		t.Run("terminal worker never reaches authorization "+name, func(t *testing.T) {
			c, s, _, recipe, job, pvc := issued(t, 0)
			bindScratch(t, recipe, pvc, true)
			workerPod(t, job, status)
			recoverUntil(t, c, func() bool { return c.Status.SnapshotExportAuthority.Attempts[0].Phase == "Invalidated" })
			requireSafeInvalidation(t, c, s, "WorkerTerminatedBeforeAuthorization", recipe)
		})
	}
	for name, condition := range map[string]batchv1.JobConditionType{"job failed": batchv1.JobFailed, "job complete": batchv1.JobComplete} {
		t.Run("terminal worker never reaches authorization "+name, func(t *testing.T) {
			c, s, _, recipe, job, _ := issued(t, 0)
			job.Status.StartTime = ptr.To(metav1.Now())
			if condition == batchv1.JobFailed {
				job.Status.Failed = 1
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue}, {Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			} else {
				job.Status.Succeeded = 1
				job.Status.CompletionTime = ptr.To(metav1.Now())
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}, {Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			}
			require.NoError(t, admin.Status().Update(ctx, job))
			recoverUntil(t, c, func() bool { return c.Status.SnapshotExportAuthority.Attempts[0].Phase == "Invalidated" })
			requireSafeInvalidation(t, c, s, "WorkerTerminatedBeforeAuthorization", recipe)
		})
	}
	for _, wait := range []string{"never bound PVC", "unobserved PV", "unobserved Pod", "unready source"} {
		t.Run("preauthorization deadline expires "+wait, func(t *testing.T) {
			c, s, _, recipe, job, pvc := issued(t, nativePreauthorizationDeadline+time.Minute)
			switch wait {
			case "unobserved PV":
				bindScratch(t, recipe, pvc, false)
			case "unobserved Pod":
				bindScratch(t, recipe, pvc, true)
			case "unready source":
				bindScratch(t, recipe, pvc, true)
				workerPod(t, job, corev1.PodStatus{Phase: corev1.PodPending})
				s.Status.ReadyToUse = ptr.To(false)
				require.NoError(t, admin.Status().Update(ctx, s))
			}
			recoverUntil(t, c, func() bool { return c.Status.SnapshotExportAuthority.Attempts[0].Phase == "Invalidated" })
			requireSafeInvalidation(t, c, s, "AuthorizationDeadlineExceeded", recipe)
		})
	}
	for _, age := range []time.Duration{0, 3 * time.Minute, nativePreauthorizationDeadline - time.Minute} {
		t.Run(fmt.Sprintf("pending issued PVC keeps authority age %s", age), func(t *testing.T) {
			s, content := nativeReviewSource(t, admin, source, original.Driver)
			c, a, recipe := seed(t, template.Namespace, "ResourcesRequested", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				rec, err := exportprotocol.DecodeRecipe(a)
				require.NoError(t, err)
				rec.Snapshot, rec.Content = identity(s), identity(content)
				a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
				require.NoError(t, err)
				a.Job, a.ScratchPVC, a.ScratchPV, a.Pod = nil, nil, nil, nil
				a.LocalCleanupState = ""
				a.CreatedAt = metav1.NewTime(time.Now().Add(-age))
				e.SnapshotName, e.SnapshotUID = s.Name, s.UID
			})
			holders, err := json.Marshal(map[string]string{a.ID: nativeHolder(recipe, a)})
			require.NoError(t, err)
			s.Annotations[exportprotocol.HoldAnnotation] = string(holders)
			s.Finalizers = append(s.Finalizers, exportprotocol.SourceFinalizer)
			require.NoError(t, admin.Update(ctx, s))
			job := datasnapshot.NativeJob(a, recipe)
			require.NoError(t, admin.Create(ctx, job))
			pvc := datasnapshot.NativePVC(a, recipe, job.UID)
			require.NoError(t, admin.Create(ctx, pvc))
			for range 2 {
				require.NoError(t, r.recoverNativeExports(ctx, c))
			}
			before := refresh(t, c)
			err = r.recoverNativeExports(ctx, c)
			if age > 2*time.Minute {
				require.Error(t, err)
				deferred, fatal := r.handleNativeOutcome(ctx, c, err)
				require.NoError(t, fatal)
				require.True(t, deferred)
			} else {
				require.NoError(t, err)
			}
			after := refresh(t, c)
			require.Equal(t, before, after, "pending storage must neither invalidate nor reset issuance")
			condition := apiMeta.FindStatusCondition(c.Status.Conditions, appsv1.ConditionSnapshotExportDegraded)
			if age > 2*time.Minute {
				require.NotNil(t, condition)
				require.Equal(t, "RecoveryDeferred", condition.Reason)
				require.LessOrEqual(t, len(condition.Message), 256)
			} else {
				require.Nil(t, condition)
			}
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), s))
			require.Contains(t, s.Finalizers, exportprotocol.SourceFinalizer)
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(pvc), pvc))
			require.Equal(t, after.ScratchPVC.UID, pvc.UID)
		})
	}
	for name, boundary := range map[string]struct {
		phase    string
		stopping bool
		read     int32
	}{
		"after stopped cleanup": {"Succeeded", true, 2},
		"after local cleanup":   {"Invalidated", false, 1},
		"after invalidation":    {"ResourcesRequested", true, 2},
	} {
		t.Run("compacted authority is rejected "+name, func(t *testing.T) {
			c, a, _ := seed(t, template.Namespace, boundary.phase, nil)
			var reads atomic.Int32
			racing := rest.CopyConfig(r.RestConfig)
			racing.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
				return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/chainnodes/"+c.Name) && reads.Add(1) == boundary.read {
						raced := &appsv1.ChainNode{}
						if err := admin.Get(ctx, client.ObjectKeyFromObject(c), raced); err != nil {
							return nil, err
						}
						raced.Status.SnapshotExportAuthority.Attempts = []appsv1.SnapshotExportAttempt{}
						if err := admin.Status().Update(ctx, raced); err != nil {
							return nil, err
						}
					}
					return base.RoundTrip(req)
				})
			}
			reader, err := client.New(racing, client.Options{Scheme: r.Scheme})
			require.NoError(t, err)
			local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
			before := posts.Load()
			err = local.recoverNativeAttempt(ctx, c, a, boundary.stopping, false)
			var operation *nativeOperationError
			require.ErrorAs(t, err, &operation)
			require.Equal(t, "InvalidAuthority", operation.reason)
			require.Equal(t, a.ExportID, operation.exportID)
			require.Equal(t, before, posts.Load())
		})
	}
	pendingStatus := func(c *appsv1.ChainNode) {
		c.Status.SeedMode = true
		c.Status.LatestHeight = 42
		c.Status.Phase = appsv1.PhaseChainNodeRunning
		apiMeta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: "Unrelated", Status: metav1.ConditionTrue, Reason: "Pending"})
	}
	requirePendingStatus := func(t *testing.T, c *appsv1.ChainNode) {
		t.Helper()
		require.True(t, c.Status.SeedMode)
		require.EqualValues(t, 42, c.Status.LatestHeight)
		require.Equal(t, appsv1.PhaseChainNodeRunning, c.Status.Phase)
		require.True(t, apiMeta.IsStatusConditionTrue(c.Status.Conditions, "Unrelated"))
	}
	t.Run("native status writes keep the caller's full Update valid", func(t *testing.T) {
		c := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "status-write-", Namespace: template.Namespace}, Spec: *template.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, c))
		require.NoError(t, r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportDegraded, "RecoveryDeferred", "test recovery deferred"))
		pendingStatus(c)
		require.NoError(t, admin.Update(ctx, c), "a status write the caller merged must not fail its next spec or metadata write")
		c.Annotations = map[string]string{"test.example/pending": "true"}
		require.NoError(t, r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportDegraded, "", ""))
		server := &appsv1.ChainNode{}
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), server))
		require.NotEqual(t, server.ResourceVersion, c.ResourceVersion, "pending metadata keeps the stale version")
		require.True(t, apierrors.IsConflict(admin.Update(ctx, c.DeepCopy())))
	})
	for _, pending := range []string{"none", "annotation", "spec"} {
		t.Run("authority finalizer writes preserve pending state "+pending, func(t *testing.T) {
			mutate := func(c *appsv1.ChainNode) {
				pendingStatus(c)
				switch pending {
				case "annotation":
					c.Annotations = map[string]string{"test.example/pending": "true"}
				case "spec":
					c.Spec.OverrideImage = ptr.To("application:pending")
				}
			}
			requireWritable := func(t *testing.T, c *appsv1.ChainNode, server *appsv1.ChainNode) {
				t.Helper()
				requirePendingStatus(t, c)
				if pending == "none" {
					require.Equal(t, server.ResourceVersion, c.ResourceVersion)
					require.NoError(t, admin.Update(ctx, c))
					return
				}
				require.NotEqual(t, server.ResourceVersion, c.ResourceVersion)
				require.True(t, apierrors.IsConflict(admin.Update(ctx, c.DeepCopy())), "a later full Update must not overwrite the newer object")
			}
			added := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "finalizer-add-", Namespace: template.Namespace}, Spec: *template.Spec.DeepCopy()}
			require.NoError(t, admin.Create(ctx, added))
			mutate(added)
			var deferred *nativeDeferred
			require.ErrorAs(t, r.reserveNativeAttempt(ctx, added, source), &deferred)
			require.Contains(t, added.Finalizers, exportprotocol.AuthorityFinalizer)
			server := &appsv1.ChainNode{}
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(added), server))
			require.Contains(t, server.Finalizers, exportprotocol.AuthorityFinalizer)
			requireWritable(t, added, server)

			removed := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "finalizer-remove-", Namespace: template.Namespace, Finalizers: []string{exportprotocol.AuthorityFinalizer, "test.example/observe"}}, Spec: *template.Spec.DeepCopy()}
			require.NoError(t, admin.Create(ctx, removed))
			require.NoError(t, admin.Delete(ctx, removed))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(removed), removed))
			mutate(removed)
			require.NoError(t, r.recoverNativeExports(ctx, removed))
			require.NotContains(t, removed.Finalizers, exportprotocol.AuthorityFinalizer)
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(removed), server))
			require.NotContains(t, server.Finalizers, exportprotocol.AuthorityFinalizer)
			requireWritable(t, removed, server)
		})
	}
	t.Run("orphan snapshot read errors remain failures", func(t *testing.T) {
		local := &Reconciler{Client: r.Client, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
		var faults *atomic.Int32
		local.APIReader, faults = nativeFaultReader(t, r.RestConfig, "/volumesnapshots", 503, 0)
		_, err := local.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: template.Namespace, Name: "absent-owner"}})
		require.True(t, apierrors.IsServiceUnavailable(err), "unexpected error: %v", err)
		require.Positive(t, faults.Load())
	})

	t.Run("unapplied condition write is not mistaken for acknowledged recovery", func(t *testing.T) {
		c, _, _ := seed(t, template.Namespace, "Succeeded", nil)
		unchangedRV := c.ResourceVersion
		require.NoError(t, r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportDegraded, "", ""))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Equal(t, unchangedRV, c.ResourceVersion, "absent conditions must not trigger a status write")
		failStatusWrite.Store(true)
		require.ErrorContains(t, r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportDegraded, "RecoveryDeferred", "test recovery deferred"), "unavailable status write")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.False(t, apiMeta.IsStatusConditionTrue(c.Status.Conditions, appsv1.ConditionSnapshotExportDegraded))
		dropStatusACK.Store(true)
		require.NoError(t, r.nativeCondition(ctx, c, appsv1.ConditionSnapshotExportDegraded, "RecoveryDeferred", "test recovery deferred"))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.True(t, apiMeta.IsStatusConditionTrue(c.Status.Conditions, appsv1.ConditionSnapshotExportDegraded))
	})
	t.Run("credential loss before cleanup issuance defers until the same name returns", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", nil)
		archive := c.Status.SnapshotExports[0].Archive.DeepCopy()
		postCount := posts.Load()
		require.NoError(t, r.retainNativeSnapshot(ctx, c, source, &c.Status.SnapshotExports[0]))
		a = refresh(t, c)
		require.Equal(t, appsv1.SnapshotExportPhaseDeleting, c.Status.SnapshotExports[0].Phase)
		require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}))
		for range 2 {
			err := r.reconcileNativeDeletion(ctx, c, a, recipe)
			var operation *nativeOperationError
			require.ErrorAs(t, err, &operation)
			require.Equal(t, "ExecutionIdentityUnavailable", operation.reason)
			a = refresh(t, c)
			require.Empty(t, a.CleanupState, "a missing prerequisite is never a permanent failure")
			require.Zero(t, a.CleanupAttempt)
			require.Empty(t, a.CleanupIssuer)
			require.Equal(t, "Required", a.RemoteCleanupState)
			require.Equal(t, "Required", c.Status.SnapshotExports[0].RemoteCleanupState)
			require.Contains(t, c.Status.SnapshotExports[0].Message, "deletion remains unproven")
			require.True(t, apiMeta.IsStatusConditionTrue(c.Status.Conditions, appsv1.ConditionSnapshotExportCleanup))
			require.Equal(t, postCount, posts.Load())
			require.Equal(t, archive, c.Status.SnapshotExports[0].Archive)
		}
		replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}, Data: map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("key"), "AWS_SECRET_ACCESS_KEY": []byte("secret")}}
		require.NoError(t, admin.Create(ctx, replacement))
		require.NotEqual(t, recipe.Credentials.UID, replacement.UID)
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.EqualValues(t, 1, a.CleanupAttempt)
		require.Empty(t, a.CleanupState)
		require.Equal(t, postCount+1, posts.Load())
		job := &batchv1.Job{}
		require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, job))
		require.NoError(t, datasnapshot.ValidateNativeCleanupJob(job, a, recipe))
	})
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual retry survives recovery acknowledged %v", acknowledged), func(t *testing.T) {
			snapshot, content := nativeReviewSource(t, admin, source, original.Driver)
			c, _, recipe := seed(t, template.Namespace, "Invalidated", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				rec, err := exportprotocol.DecodeRecipe(a)
				require.NoError(t, err)
				rec.Snapshot, rec.Content = identity(snapshot), identity(content)
				a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
				require.NoError(t, err)
				a.AuthorizedAt = ptr.To(metav1.Now())
				a.RemoteCleanupState = "Required"
				e.SnapshotName, e.SnapshotUID = snapshot.Name, snapshot.UID
				e.Phase, e.RemoteCleanupState = appsv1.SnapshotExportPhaseCleanupRequired, "Required"
			})
			snapshot.Labels = map[string]string{controllers.LabelChainNode: c.Name}
			snapshot.Annotations[controllers.AnnotationExportingTarball] = "true"
			require.NoError(t, admin.Update(ctx, snapshot))
			require.NoError(t, r.recoverNativeExports(ctx, c))
			a := refresh(t, c)
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
			require.Equal(t, tarballNativeFailed, snapshot.Annotations[controllers.AnnotationExportingTarball])
			if acknowledged {
				c.Annotations = map[string]string{exportprotocol.CleanupAckAnnotation: a.ExportID}
				require.NoError(t, admin.Update(ctx, c))
				require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
				a = refresh(t, c)
			}
			old := a.DeepCopy()
			delete(snapshot.Annotations, controllers.AnnotationExportingTarball)
			delete(snapshot.Annotations, controllers.AnnotationTarballExportAttempts)
			require.NoError(t, admin.Update(ctx, snapshot))
			require.NoError(t, r.recoverNativeExports(ctx, c))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
			require.Empty(t, snapshot.Annotations[controllers.AnnotationExportingTarball], "recovery precedes the normal retry loop")
			require.Empty(t, snapshot.Annotations[controllers.AnnotationTarballExportAttempts])
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			if acknowledged {
				require.Empty(t, c.Status.SnapshotExportAuthority.Attempts, "acknowledgement releases the attempt slot")
			}
			c.Status.LatestHeight, c.Status.PvcSize = 1, "1Gi"
			require.NoError(t, admin.Status().Update(ctx, c))
			clients, err := kubernetes.NewForConfig(r.RestConfig)
			require.NoError(t, err)
			local := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts, recorder: record.NewFakeRecorder(100), ClientSet: clients}
			require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			next := &c.Status.SnapshotExportAuthority.Attempts[len(c.Status.SnapshotExportAuthority.Attempts)-1]
			require.Greater(t, next.Sequence, old.Sequence)
			require.NotEqual(t, old.ID, next.ID)
			nextRecipe, err := exportprotocol.DecodeRecipe(next)
			require.NoError(t, err)
			require.NotEqual(t, recipe.ObjectName, nextRecipe.ObjectName)
			require.EqualValues(t, 1, next.UploadAttempt, "removing both markers requests a new manual cycle")
			require.Equal(t, next.ExportID, snapshotExportFor(c, snapshot).ID)
			if !acknowledged {
				require.Equal(t, old, exportprotocol.FindAttempt(c, old.Sequence, old.ID), "retry does not erase unacknowledged uncertainty")
			}
			require.Equal(t, "Required", snapshotExportByID(c, old.ExportID).RemoteCleanupState)
			require.NoError(t, r.cleanupNativeAttempt(ctx, c, old, recipe))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
			require.Equal(t, "true", snapshot.Annotations[controllers.AnnotationExportingTarball], "old attempt must preserve the new retry marker")
		})
	}
	t.Run("marker repair preserves unrelated progress and API conflicts", func(t *testing.T) {
		snapshot, content := nativeReviewSource(t, admin, source, original.Driver)
		c, a, recipe := seed(t, template.Namespace, "Invalidated", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			rec, err := exportprotocol.DecodeRecipe(a)
			require.NoError(t, err)
			rec.Snapshot, rec.Content = identity(snapshot), identity(content)
			a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
			require.NoError(t, err)
			a.RemoteCleanupState = "Required"
			e.SnapshotName, e.SnapshotUID = snapshot.Name, snapshot.UID
			e.RemoteCleanupState, e.Phase = "Required", appsv1.SnapshotExportPhaseCleanupRequired
		})
		for _, marker := range []string{"", tarballFailed, tarballFinished, tarballUploaded, "operator-value"} {
			snapshot.Annotations[controllers.AnnotationExportingTarball] = marker
			require.NoError(t, admin.Update(ctx, snapshot))
			require.NoError(t, r.cleanupNativeAttempt(ctx, c, a, recipe))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
			require.Equal(t, marker, snapshot.Annotations[controllers.AnnotationExportingTarball])
		}
		snapshot.Annotations[controllers.AnnotationExportingTarball] = "true"
		require.NoError(t, admin.Update(ctx, snapshot))
		var changed atomic.Bool
		raceConfig := rest.CopyConfig(r.RestConfig)
		raceConfig.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
			return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/volumesnapshots/"+snapshot.Name) && changed.CompareAndSwap(false, true) {
					newer := &snapshotv1.VolumeSnapshot{}
					if err := admin.Get(ctx, client.ObjectKeyFromObject(snapshot), newer); err != nil {
						return nil, err
					}
					newer.Annotations[controllers.AnnotationExportingTarball] = tarballFinished
					if err := admin.Update(ctx, newer); err != nil {
						return nil, err
					}
				}
				return base.RoundTrip(req)
			})
		}
		raced, err := client.New(raceConfig, client.Options{Scheme: r.Scheme})
		require.NoError(t, err)
		local := &Reconciler{Client: raced, APIReader: r.APIReader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
		require.True(t, apierrors.IsConflict(local.cleanupNativeAttempt(ctx, c, a, recipe)))
		require.True(t, changed.Load())
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
		require.Equal(t, tarballFinished, snapshot.Annotations[controllers.AnnotationExportingTarball])
		snapshot.Annotations[controllers.AnnotationExportingTarball] = "true"
		snapshot.Finalizers = []string{"test.example/hold"}
		require.NoError(t, admin.Update(ctx, snapshot))
		require.NoError(t, admin.Delete(ctx, snapshot))
		require.NoError(t, r.cleanupNativeAttempt(ctx, c, a, recipe))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
		require.Equal(t, "true", snapshot.Annotations[controllers.AnnotationExportingTarball])
		snapshot.Finalizers = nil
		require.NoError(t, admin.Update(ctx, snapshot))
		replacement := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: snapshot.Name, Namespace: snapshot.Namespace, Annotations: map[string]string{controllers.AnnotationExportingTarball: "true"}}, Spec: *source.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, replacement))
		require.NotEqual(t, snapshot.UID, replacement.UID)
		require.NoError(t, r.cleanupNativeAttempt(ctx, c, a, recipe))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(replacement), replacement))
		require.Equal(t, "true", replacement.Annotations[controllers.AnnotationExportingTarball])
	})
	failedExport := func(t *testing.T) (*appsv1.ChainNode, *snapshotv1.VolumeSnapshot, *Reconciler) {
		t.Helper()
		snapshot, content := nativeReviewSource(t, admin, source, original.Driver)
		c, _, _ := seed(t, template.Namespace, "Invalidated", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			rec, err := exportprotocol.DecodeRecipe(a)
			require.NoError(t, err)
			rec.Snapshot, rec.Content = identity(snapshot), identity(content)
			a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
			require.NoError(t, err)
			a.LocalCleanupState = ""
			a.Job, a.ScratchPVC, a.ScratchPV, a.Pod = nil, nil, nil, nil
			e.SnapshotName, e.SnapshotUID = snapshot.Name, snapshot.UID
			e.Phase = appsv1.SnapshotExportPhaseCleanupRequired
		})
		snapshot.Labels = map[string]string{controllers.LabelChainNode: c.Name}
		snapshot.Annotations[controllers.AnnotationExportingTarball] = "true"
		require.NoError(t, admin.Update(ctx, snapshot))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		c.Status.LatestHeight, c.Status.PvcSize = 1, "1Gi"
		require.NoError(t, admin.Status().Update(ctx, c))
		clients, err := kubernetes.NewForConfig(r.RestConfig)
		require.NoError(t, err)
		return c, snapshot, &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts, recorder: record.NewFakeRecorder(100), ClientSet: clients}
	}
	requireMarker := func(t *testing.T, s *snapshotv1.VolumeSnapshot, marker string, failures int) {
		t.Helper()
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), s))
		require.Equal(t, marker, s.Annotations[controllers.AnnotationExportingTarball])
		require.Equal(t, strconv.Itoa(failures), s.Annotations[controllers.AnnotationTarballExportAttempts])
	}
	t.Run("failed uploads retry with fresh attempts until exhausted", func(t *testing.T) {
		c, s, local := failedExport(t)
		used := map[string]bool{}
		for ordinal := 1; ordinal <= tarballExportMaxAttempts; ordinal++ {
			for range 2 {
				require.NoError(t, local.recoverNativeExports(ctx, c))
				requireMarker(t, s, tarballNativeFailed, ordinal)
			}
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.Empty(t, c.Status.SnapshotExportAuthority.Attempts, "the retry budget outlives compacted authority")
			require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			if ordinal == tarballExportMaxAttempts {
				require.Empty(t, c.Status.SnapshotExportAuthority.Attempts, "no fourth attempt")
				require.EqualValues(t, tarballExportMaxAttempts, c.Status.SnapshotExportAuthority.LastSequence)
				requireMarker(t, s, tarballNativeFailed, tarballExportMaxAttempts)
				events := local.recorder.(*record.FakeRecorder).Events
				close(events)
				warnings := []string{}
				for event := range events {
					if strings.Contains(event, corev1.EventTypeWarning) {
						warnings = append(warnings, event)
					}
				}
				require.Len(t, warnings, 1)
				require.Contains(t, warnings[0], "retry limit reached")
				break
			}
			require.Len(t, c.Status.SnapshotExportAuthority.Attempts, 1)
			next := c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
			require.EqualValues(t, ordinal+1, next.UploadAttempt)
			require.EqualValues(t, ordinal+1, next.Sequence)
			rec, err := exportprotocol.DecodeRecipe(next)
			require.NoError(t, err)
			require.False(t, used[rec.ObjectName], "an upload retry never reuses an object prefix")
			used[rec.ObjectName] = true
			requireMarker(t, s, "true", ordinal)
			require.NoError(t, local.invalidateNativeAttempt(ctx, c, next, "WorkerFailed"))
		}
	})
	t.Run("retry after failure publishes and is never exported again", func(t *testing.T) {
		c, s, local := failedExport(t)
		require.NoError(t, local.recoverNativeExports(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		a := c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
		require.EqualValues(t, 2, a.UploadAttempt)
		recipe, err := exportprotocol.DecodeRecipe(a)
		require.NoError(t, err)
		recoverUntil(t, c, func() bool {
			return c.Status.SnapshotExportAuthority.Attempts[0].ScratchPVC != nil
		})
		job, pvc := &batchv1.Job{}, &corev1.PersistentVolumeClaim{}
		require.NoError(t, admin.Get(ctx, nativeKey(recipe), job))
		require.NoError(t, admin.Get(ctx, nativeKey(recipe), pvc))
		bindScratch(t, recipe, pvc, true)
		pod := workerPod(t, job, corev1.PodStatus{Phase: corev1.PodRunning})
		recoverUntil(t, c, func() bool { return c.Status.SnapshotExportAuthority.Attempts[0].Phase == "Uploading" })
		a = c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
		previous, err := exportprotocol.DecodeEnvelope([]byte(completed.CompletedEnvelope))
		require.NoError(t, err)
		envelope := exportprotocol.EnvelopeFor(a, recipe)
		envelope.Result = previous.Result
		envelope.Result.ObjectKey = recipe.ObjectName + recipe.Compression.Extension()
		body, err := exportprotocol.EncodeEnvelope(envelope)
		require.NoError(t, err)
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "exporter", Image: recipe.Image, ImageID: "fixture", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(body)}}}}
		require.NoError(t, admin.Status().Update(ctx, pod))
		job.Status.StartTime, job.Status.CompletionTime = ptr.To(metav1.Now()), ptr.To(metav1.Now())
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}, {Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		require.NoError(t, admin.Status().Update(ctx, job))
		recoverUntil(t, c, func() bool { return c.Status.SnapshotExportAuthority.Attempts[0].Phase == "Succeeded" })
		for range 2 {
			require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		}
		requireMarker(t, s, tarballNativeFinished, 1)
		require.EqualValues(t, 2, c.Status.SnapshotExportAuthority.LastSequence)
		require.Equal(t, a.ExportID, snapshotExportFor(c, s).ID)
	})
	t.Run("lost first marker counts once and removing both markers restarts the cycle", func(t *testing.T) {
		c, s, local := failedExport(t)
		delete(s.Annotations, controllers.AnnotationExportingTarball)
		require.NoError(t, admin.Update(ctx, s))
		for range 2 {
			require.NoError(t, local.recoverNativeExports(ctx, c))
			requireMarker(t, s, tarballNativeFailed, 1)
		}
		delete(s.Annotations, controllers.AnnotationExportingTarball)
		delete(s.Annotations, controllers.AnnotationTarballExportAttempts)
		require.NoError(t, admin.Update(ctx, s))
		require.NoError(t, local.recoverNativeExports(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), s))
		require.Empty(t, s.Annotations[controllers.AnnotationExportingTarball])
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Len(t, c.Status.SnapshotExportAuthority.Attempts, 1)
		require.EqualValues(t, 1, c.Status.SnapshotExportAuthority.Attempts[0].UploadAttempt)
	})
	t.Run("retry waits for the failed attempt's local resources", func(t *testing.T) {
		c, s, local := failedExport(t)
		a := c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
		recipe, err := exportprotocol.DecodeRecipe(a)
		require.NoError(t, err)
		job := datasnapshot.NativeJob(a, recipe)
		require.NoError(t, admin.Create(ctx, job))
		pvc := datasnapshot.NativePVC(a, recipe, job.UID)
		pvc.Finalizers = append(pvc.Finalizers, "test.example/hold")
		require.NoError(t, admin.Create(ctx, pvc))
		require.NoError(t, local.recoverNativeExports(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), s))
		require.Equal(t, "true", s.Annotations[controllers.AnnotationExportingTarball], "marker repair follows local teardown")
		s.Annotations[controllers.AnnotationExportingTarball] = tarballNativeFailed
		s.Annotations[controllers.AnnotationTarballExportAttempts] = "1"
		require.NoError(t, admin.Update(ctx, s))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.EqualValues(t, 1, c.Status.SnapshotExportAuthority.LastSequence)
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(pvc), pvc))
		controllerutil.RemoveFinalizer(pvc, "test.example/hold")
		controllerutil.RemoveFinalizer(pvc, "kubernetes.io/pvc-protection")
		require.NoError(t, admin.Update(ctx, pvc))
		require.NoError(t, local.recoverNativeExports(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.EqualValues(t, 2, c.Status.SnapshotExportAuthority.LastSequence)
	})
	for _, reason := range []string{"policy disabled", "source expired"} {
		t.Run("retry does not start when "+reason, func(t *testing.T) {
			c, s, local := failedExport(t)
			require.NoError(t, local.recoverNativeExports(ctx, c))
			requireMarker(t, s, tarballNativeFailed, 1)
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			c.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
			if reason == "policy disabled" {
				c.Spec.Persistence.Snapshots.ExportTarball.Protocol = ""
			} else {
				s.Annotations[controllers.AnnotationSnapshotRetention] = "1ns"
				require.NoError(t, admin.Update(ctx, s))
			}
			require.NoError(t, admin.Update(ctx, c))
			require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.EqualValues(t, 1, c.Status.SnapshotExportAuthority.LastSequence)
			if reason == "source expired" {
				require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(s), s)))
			}
		})
	}
	for _, resource := range []string{"secrets", "serviceaccounts"} {
		for _, retrying := range []bool{false, true} {
			t.Run(fmt.Sprintf("cleanup read defers %s retrying %v", resource, retrying), func(t *testing.T) {
				c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
					a.RemoteCleanupState, e.RemoteCleanupState = "Required", "Required"
					e.Phase = appsv1.SnapshotExportPhaseDeleting
					if retrying {
						a.CleanupAttempt = 1
						a.CleanupIssuer = string(uuid.NewUUID())
						a.CleanupIssuedAt = ptr.To(metav1.NewTime(time.Now().Add(-time.Hour)))
						a.CleanupState, a.CleanupReason = "Failed", "CleanupJobFailed"
					}
				})
				name := recipe.Credentials.Name
				if resource == "serviceaccounts" {
					name = recipe.ServiceAccount.Name
				}
				reader, fired := nativeFaultReader(t, r.RestConfig, "/"+resource+"/"+name, 403, 0)
				local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
				before := posts.Load()
				require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
				require.Positive(t, fired.Load())
				require.Equal(t, a, refresh(t, c), "a transient read never consumes an ordinal")
				require.Equal(t, before, posts.Load())
				require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
				current := refresh(t, c)
				require.Empty(t, current.CleanupState)
				require.Equal(t, a.CleanupAttempt+1, current.CleanupAttempt)
				require.NotEqual(t, a.CleanupIssuer, current.CleanupIssuer)
				require.Equal(t, before+1, posts.Load())
			})
		}
	}
	for _, loss := range []string{"snapshot absent", "snapshot replaced", "content absent", "content replaced", "reverse binding despite unready", "size despite unready", "hold absent", "secret absent", "secret replaced", "service account absent", "service account replaced"} {
		t.Run("definitive identity loss "+loss, func(t *testing.T) {
			snapshot, content := nativeReviewSource(t, admin, source, original.Driver)
			c, a, recipe := seed(t, template.Namespace, "Uploading", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				rec, err := exportprotocol.DecodeRecipe(a)
				require.NoError(t, err)
				rec.Snapshot, rec.Content = identity(snapshot), identity(content)
				a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
				require.NoError(t, err)
				e.SnapshotName, e.SnapshotUID = snapshot.Name, snapshot.UID
			})
			holders, err := json.Marshal(map[string]string{a.ID: nativeHolder(recipe, a)})
			require.NoError(t, err)
			snapshot.Annotations[exportprotocol.HoldAnnotation] = string(holders)
			controllerutil.AddFinalizer(snapshot, exportprotocol.SourceFinalizer)
			require.NoError(t, admin.Update(ctx, snapshot))
			switch loss {
			case "snapshot absent", "snapshot replaced":
				snapshot.Finalizers = nil
				require.NoError(t, admin.Update(ctx, snapshot))
				require.NoError(t, admin.Delete(ctx, snapshot))
				if loss == "snapshot replaced" {
					replacement := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: snapshot.Name, Namespace: snapshot.Namespace}, Spec: *snapshot.Spec.DeepCopy()}
					require.NoError(t, admin.Create(ctx, replacement))
					require.NotEqual(t, snapshot.UID, replacement.UID)
				}
			case "content absent", "content replaced":
				require.NoError(t, admin.Delete(ctx, content))
				if loss == "content replaced" {
					replacement := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: content.Name}, Spec: *content.Spec.DeepCopy()}
					require.NoError(t, admin.Create(ctx, replacement))
					require.NotEqual(t, content.UID, replacement.UID)
				}
			case "reverse binding despite unready":
				content.Status.ReadyToUse = ptr.To(false)
				require.NoError(t, admin.Status().Update(ctx, content))
				content.Spec.VolumeSnapshotRef.UID = "different-snapshot"
				require.NoError(t, admin.Update(ctx, content))
			case "size despite unready":
				snapshot.Status.ReadyToUse = ptr.To(false)
				snapshot.Status.RestoreSize = ptr.To(resource.MustParse("2Gi"))
				require.NoError(t, admin.Status().Update(ctx, snapshot))
			case "hold absent":
				delete(snapshot.Annotations, exportprotocol.HoldAnnotation)
				require.NoError(t, admin.Update(ctx, snapshot))
			case "secret absent", "secret replaced":
				require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}))
				if loss == "secret replaced" {
					replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}
					require.NoError(t, admin.Create(ctx, replacement))
					require.NotEqual(t, recipe.Credentials.UID, replacement.UID)
				}
			case "service account absent", "service account replaced":
				require.NoError(t, admin.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.ServiceAccount.Name}}))
				if loss == "service account replaced" {
					replacement := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.ServiceAccount.Name}}
					require.NoError(t, admin.Create(ctx, replacement))
					require.NotEqual(t, recipe.ServiceAccount.UID, replacement.UID)
				}
			}
			before := posts.Load()
			require.NoError(t, r.recoverNativeExports(ctx, c))
			current := refresh(t, c)
			require.Equal(t, "Invalidated", current.Phase)
			reason := "SourceBindingChanged"
			if strings.HasPrefix(loss, "secret") || strings.HasPrefix(loss, "service account") {
				reason = "ExecutionIdentityChanged"
			}
			require.Equal(t, reason, current.Reason)
			require.Equal(t, "Required", current.RemoteCleanupState)
			require.Equal(t, a.Recipe, current.Recipe)
			require.Equal(t, a.JobCreateIssuer, current.JobCreateIssuer)
			require.Equal(t, before, posts.Load())
			require.Nil(t, c.Status.SnapshotExports[0].Archive)
		})
	}
	t.Run("owner and reconcile context failures remain fatal", func(t *testing.T) {
		c, a, _ := seed(t, template.Namespace, "Uploading", nil)
		reader, fired := nativeFaultReader(t, r.RestConfig, "/chainnodes/"+c.Name, 403, 0)
		local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
		err := local.recoverNativeExports(ctx, c)
		require.Error(t, err)
		require.Positive(t, fired.Load())
		handled, fatal := local.handleNativeOutcome(ctx, c, err)
		require.False(t, handled)
		require.Error(t, fatal)
		require.Equal(t, a, refresh(t, c))
		for _, deadline := range []bool{false, true} {
			cancelled, cancel := context.WithCancel(ctx)
			if deadline {
				cancel()
				cancelled, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			} else {
				cancel()
			}
			err := r.recoverNativeExports(cancelled, c)
			cancel()
			expected := context.Canceled
			if deadline {
				expected = context.DeadlineExceeded
			}
			require.ErrorIs(t, err, expected)
			handled, fatal := r.handleNativeOutcome(ctx, c, err)
			require.False(t, handled)
			require.ErrorIs(t, fatal, expected)
			require.Equal(t, a, refresh(t, c))
		}
		stale := c.DeepCopy()
		c.Finalizers = nil
		require.NoError(t, admin.Update(ctx, c))
		require.NoError(t, admin.Delete(ctx, c))
		replacement := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace}, Spec: *c.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, replacement))
		require.NotEqual(t, stale.UID, replacement.UID)
		err = r.recoverNativeExports(ctx, stale)
		handled, fatal = r.handleNativeOutcome(ctx, stale, err)
		require.False(t, handled)
		require.ErrorContains(t, fatal, "authority UID changed")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(replacement), replacement))
		require.Nil(t, replacement.Status.SnapshotExportAuthority)
	})
	for _, phase := range []string{"Reserved", "Held"} {
		t.Run("later source proof blocks "+phase, func(t *testing.T) {
			snapshot, content := nativeReviewSource(t, admin, source, original.Driver)
			c, a, recipe := seed(t, template.Namespace, "Reserved", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				rec, err := exportprotocol.DecodeRecipe(a)
				require.NoError(t, err)
				rec.Snapshot, rec.Content = identity(snapshot), identity(content)
				a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
				require.NoError(t, err)
				a.Job, a.ScratchPVC, a.ScratchPV, a.Pod = nil, nil, nil, nil
				a.JobCreateIssued, a.PVCCreateIssued = false, false
				a.JobCreateIssuer, a.PVCCreateIssuer, a.SourceHoldResourceVersion = "", "", ""
				e.SnapshotName, e.SnapshotUID = snapshot.Name, snapshot.UID
			})
			if phase == "Held" {
				require.NoError(t, r.holdNativeSource(ctx, c, a, recipe))
				a = refresh(t, c)
			}
			reader, fired := nativeFaultReader(t, r.RestConfig, "/volumesnapshots/"+snapshot.Name, 503, 1)
			local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
			before := posts.Load()
			require.Error(t, local.recoverNativeExports(ctx, c))
			require.Positive(t, fired.Load())
			require.Equal(t, a, refresh(t, c))
			require.Equal(t, before, posts.Load())
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
			require.Equal(t, phase == "Held", controllerutil.ContainsFinalizer(snapshot, exportprotocol.SourceFinalizer))
		})
	}
	for _, loss := range []string{"job", "pvc", "pv", "pod", "pv claim", "frozen volume name"} {
		t.Run("publication rejects established binding loss "+loss, func(t *testing.T) {
			snapshot, content := nativeReviewSource(t, admin, source, original.Driver)
			var job *batchv1.Job
			var pvc *corev1.PersistentVolumeClaim
			var pv *corev1.PersistentVolume
			var pod *corev1.Pod
			c, a, recipe := seed(t, template.Namespace, "Uploading", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				rec, err := exportprotocol.DecodeRecipe(a)
				require.NoError(t, err)
				rec.Snapshot, rec.Content = identity(snapshot), identity(content)
				a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
				require.NoError(t, err)
				e.SnapshotName, e.SnapshotUID = snapshot.Name, snapshot.UID
				job = datasnapshot.NativeJob(a, rec)
				require.NoError(t, admin.Create(ctx, job))
				pvc = datasnapshot.NativePVC(a, rec, job.UID)
				require.NoError(t, admin.Create(ctx, pvc))
				pv = &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: rec.ResourceName}, Spec: corev1.PersistentVolumeSpec{StorageClassName: rec.StorageClass, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: rec.Driver, VolumeHandle: rec.ResourceName}}, ClaimRef: &corev1.ObjectReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID}}}
				require.NoError(t, admin.Create(ctx, pv))
				pv.Status.Phase = corev1.VolumeBound
				require.NoError(t, admin.Status().Update(ctx, pv))
				pvc.Spec.VolumeName = pv.Name
				require.NoError(t, admin.Update(ctx, pvc))
				pvc.Status.Phase = corev1.ClaimBound
				require.NoError(t, admin.Status().Update(ctx, pvc))
				pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: rec.ResourceName + "-pod", Namespace: rec.Namespace, Labels: job.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
				require.NoError(t, admin.Create(ctx, pod))
				jobRef, pvcRef, pvRef, podRef := identity(job), identity(pvc), identity(pv), identity(pod)
				a.Job, a.ScratchPVC, a.ScratchPV, a.Pod = &jobRef, &pvcRef, &pvRef, &podRef
				if loss == "frozen volume name" {
					a.ScratchPV.Name = "different-frozen-volume"
				}
			})
			holders, err := json.Marshal(map[string]string{a.ID: nativeHolder(recipe, a)})
			require.NoError(t, err)
			snapshot.Annotations[exportprotocol.HoldAnnotation] = string(holders)
			controllerutil.AddFinalizer(snapshot, exportprotocol.SourceFinalizer)
			require.NoError(t, admin.Update(ctx, snapshot))
			require.NoError(t, datasnapshot.ValidateNativeJob(job, a, recipe))
			require.NoError(t, datasnapshot.ValidateNativePVC(pvc, a, recipe, job.UID))
			require.NoError(t, validateNativePV(pv, pvc, recipe))
			require.NoError(t, datasnapshot.ValidateNativePodSpec(&pod.Spec, a, recipe))
			reason := "PVChanged"
			switch loss {
			case "job":
				job.Finalizers = nil
				require.NoError(t, admin.Update(ctx, job))
				require.NoError(t, admin.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
				require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})))
				reason = "JobMissing"
			case "pvc":
				pvc.Finalizers = nil
				require.NoError(t, admin.Update(ctx, pvc))
				require.NoError(t, admin.Delete(ctx, pvc))
				reason = "PVCMissing"
			case "pv":
				pv.Finalizers = nil
				require.NoError(t, admin.Update(ctx, pv))
				require.NoError(t, admin.Delete(ctx, pv))
				reason = "PVMissing"
			case "pod":
				require.NoError(t, admin.Delete(ctx, pod, client.GracePeriodSeconds(0)))
				reason = "PodMissing"
			case "pv claim":
				pv.Spec.ClaimRef.UID = "replaced-claim"
				require.NoError(t, admin.Update(ctx, pv))
			}
			before := posts.Load()
			require.NoError(t, r.publishNativeArchive(ctx, c, a, recipe))
			current := refresh(t, c)
			require.Equal(t, "Invalidated", current.Phase)
			require.Equal(t, reason, current.Reason)
			require.Equal(t, "Required", current.RemoteCleanupState)
			require.Equal(t, before, posts.Load())
			require.Nil(t, c.Status.SnapshotExports[0].Archive)
		})
	}
	t.Run("rejected cleanup POST fails its ordinal and retries after backoff", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			e.Phase = appsv1.SnapshotExportPhaseDeleting
		})
		var rejected atomic.Int32
		deniedConfig := rest.CopyConfig(r.RestConfig)
		deniedConfig.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
			return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/jobs") {
					rejected.Add(1)
					if req.Body != nil {
						_ = req.Body.Close()
					}
					return &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"test quota denied cleanup"}`)), Request: req}, nil
				}
				return base.RoundTrip(req)
			})
		}
		denied, err := client.New(deniedConfig, client.Options{Scheme: r.Scheme})
		require.NoError(t, err)
		clock := time.Now()
		local := &Reconciler{Client: denied, APIReader: r.APIReader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder, snapshotDeleteNow: func() time.Time { return clock }}
		require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.EqualValues(t, 1, rejected.Load())
		require.EqualValues(t, 1, a.CleanupAttempt)
		require.Equal(t, "Failed", a.CleanupState)
		require.Equal(t, "CleanupCreateRejected", a.CleanupReason)
		require.Equal(t, "Required", a.RemoteCleanupState)
		require.NotEmpty(t, a.CleanupIssuer)
		require.NotNil(t, a.CleanupIssuedAt)
		clock = clock.Add(snapshotDeleteRetryDelay(1) / 2)
		require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		require.Equal(t, a, refresh(t, c), "backoff holds the failed ordinal")
		require.EqualValues(t, 1, rejected.Load())
		clock = clock.Add(snapshotDeleteRetryDelay(1) / 2)
		require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.EqualValues(t, 2, rejected.Load())
		require.EqualValues(t, 2, a.CleanupAttempt)
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, &batchv1.Job{})))
	})
	t.Run("failed cleanup stops and exact acknowledgement retains uncertainty", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			e.Phase = appsv1.SnapshotExportPhaseDeleting
		})
		before := posts.Load()
		dropCleanupACK.Store(true)
		require.ErrorContains(t, r.reconcileNativeDeletion(ctx, c, a, recipe), "lost cleanup POST acknowledgement")
		a = refresh(t, c)
		require.NotNil(t, a.CleanupIssuedAt)
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.Equal(t, before+1, posts.Load())
		require.NotNil(t, a.CleanupIssuedAt)
		job := &batchv1.Job{}
		require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, job))
		job.Status.StartTime = ptr.To(metav1.Now())
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue}, {Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		require.NoError(t, admin.Status().Update(ctx, job))
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.Equal(t, "Failed", a.CleanupState)
		require.True(t, apiMeta.IsStatusConditionTrue(c.Status.Conditions, appsv1.ConditionSnapshotExportDegraded))
		gated := c.DeepCopy()
		gated.Status.SnapshotExportAuthority.Attempts[0].CleanupPod = &appsv1.ExportResourceIdentity{Name: "late-pod", UID: "late-pod"}
		_, _, _, gateErr := exportprotocol.AuthorizeCleanup(gated, gated.UID, a.Sequence, a.ID, 1, "late-pod")
		require.Error(t, gateErr)
		gated.Status.SnapshotExportAuthority.Attempts[0].CleanupState = ""
		_, _, _, gateErr = exportprotocol.AuthorizeCleanup(gated, gated.UID, a.Sequence, a.ID, 1, "late-pod")
		require.NoError(t, gateErr, "otherwise-valid late cleanup is denied specifically by stopped state")
		require.Equal(t, "Required", a.RemoteCleanupState)
		require.False(t, a.CleanupComplete)
		err := r.recoverNativeExports(ctx, c)
		require.Error(t, err)
		handled, fatal := r.handleNativeOutcome(ctx, c, err)
		require.True(t, handled)
		require.NoError(t, fatal)
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})))
		late := datasnapshot.NativeCleanupJob(a, recipe)
		require.NoError(t, admin.Create(ctx, late))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		c.Annotations = map[string]string{exportprotocol.CleanupAckAnnotation: "unknown," + a.ExportID}
		require.NoError(t, admin.Update(ctx, c))
		require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
		a = refresh(t, c)
		require.Equal(t, "Acknowledged", a.CleanupState)
		require.Equal(t, "Required", a.RemoteCleanupState)
		require.False(t, a.CleanupComplete)
		require.Equal(t, "unknown", c.Annotations[exportprotocol.CleanupAckAnnotation])
		require.Equal(t, appsv1.SnapshotExportPhaseAcknowledged, c.Status.SnapshotExports[0].Phase)
		require.Contains(t, c.Status.SnapshotExports[0].Message, "without deletion proof")
		require.NotNil(t, c.Status.SnapshotExports[0].Archive)
		require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
		_, _, _, err = exportprotocol.AuthorizeCleanup(c, c.UID, a.Sequence, a.ID, 1, "late-pod")
		require.Error(t, err)
		for _, mutate := range []func(*appsv1.SnapshotExportAttempt){
			func(a *appsv1.SnapshotExportAttempt) { a.CleanupState = "" },
			func(a *appsv1.SnapshotExportAttempt) { a.CleanupIssuedAt = nil },
			func(a *appsv1.SnapshotExportAttempt) { a.CleanupComplete = true },
			func(a *appsv1.SnapshotExportAttempt) { a.Sequence = 2 },
			func(a *appsv1.SnapshotExportAttempt) { a.AuthorizedAt = nil },
		} {
			invalid := c.DeepCopy()
			mutate(&invalid.Status.SnapshotExportAuthority.Attempts[0])
			require.True(t, apierrors.IsInvalid(admin.Status().Update(ctx, invalid)))
		}
		err = r.recoverNativeExports(ctx, c)
		require.ErrorContains(t, err, "substituted Job")
		handled, fatal = r.handleNativeOutcome(ctx, c, err)
		require.True(t, handled)
		require.NoError(t, fatal)
		preserved := &batchv1.Job{}
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(late), preserved))
		require.Equal(t, late.UID, preserved.UID)
	})
	t.Run("missing issued cleanup fails its ordinal and never repeats the POST", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			a.CleanupAttempt = 1
			a.CleanupIssuer = string(uuid.NewUUID())
			a.CleanupIssuedAt = ptr.To(metav1.NewTime(time.Now().Add(-3 * time.Minute)))
			a.RemoteCleanupState = "Required"
			e.RemoteCleanupState = "Required"
			e.Phase = appsv1.SnapshotExportPhaseDeleting
		})
		before := posts.Load()
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.Equal(t, "Failed", a.CleanupState)
		require.Equal(t, "IssuedCleanupMissing", a.CleanupReason)
		require.Equal(t, before, posts.Load())
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, &batchv1.Job{})))
		late := datasnapshot.NativeCleanupJob(a, recipe)
		require.NoError(t, admin.Create(ctx, late))
		require.NoError(t, r.recoverNativeExports(ctx, c))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(late), &batchv1.Job{})))
		current := refresh(t, c)
		require.Empty(t, current.CleanupState)
		require.EqualValues(t, 2, current.CleanupAttempt)
		require.NotEqual(t, a.CleanupIssuer, current.CleanupIssuer)
		require.Equal(t, before+1, posts.Load())
		require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 2)}, &batchv1.Job{}))
	})
	cleanupWorker := func(t *testing.T, c *appsv1.ChainNode, a *appsv1.SnapshotExportAttempt) (*batchv1.Job, *corev1.Pod) {
		t.Helper()
		job := &batchv1.Job{}
		require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, a.CleanupAttempt)}, job))
		return job, workerPod(t, job, corev1.PodStatus{Phase: corev1.PodRunning})
	}
	failJob := func(t *testing.T, job *batchv1.Job) {
		t.Helper()
		job.Status.StartTime = ptr.To(metav1.Now())
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue}, {Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		require.NoError(t, admin.Status().Update(ctx, job))
	}
	completeJob := func(t *testing.T, job *batchv1.Job, pod *corev1.Pod, image string) {
		t.Helper()
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "exporter", Image: image, ImageID: "fixture", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
		require.NoError(t, admin.Status().Update(ctx, pod))
		job.Status.StartTime, job.Status.CompletionTime = ptr.To(metav1.Now()), ptr.To(metav1.Now())
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}, {Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		require.NoError(t, admin.Status().Update(ctx, job))
	}
	deleting := func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
		e.Phase = appsv1.SnapshotExportPhaseDeleting
	}
	t.Run("numbered cleanup survives restarts and replaced identities then stops after three", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", deleting)
		clock := time.Now()
		restart := func() *Reconciler {
			return &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder, snapshotDeleteNow: func() time.Time { return clock }}
		}
		issuers := map[string]bool{}
		before := posts.Load()
		for ordinal := int32(1); ordinal <= exportprotocol.MaxCleanupAttempts; ordinal++ {
			require.NoError(t, restart().reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			require.Equal(t, ordinal, a.CleanupAttempt)
			require.Empty(t, a.CleanupState)
			require.Nil(t, a.CleanupJob)
			require.Nil(t, a.CleanupPod)
			require.False(t, issuers[a.CleanupIssuer])
			issuers[a.CleanupIssuer] = true
			require.Equal(t, before+int32(ordinal), posts.Load())
			require.NoError(t, restart().reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			job, _ := cleanupWorker(t, c, a)
			failJob(t, job)
			require.NoError(t, restart().reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			require.Equal(t, "Failed", a.CleanupState)
			require.Equal(t, "Required", a.RemoteCleanupState)
			require.Error(t, restart().reconcileNativeDeletion(ctx, c, a, recipe), "the failed ordinal holds through its backoff")
			require.Equal(t, a, refresh(t, c))
			require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})), "authority is revoked before local teardown")
			switch ordinal {
			case 1:
				require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}))
				require.NoError(t, admin.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}, Data: map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("key"), "AWS_SECRET_ACCESS_KEY": []byte("secret")}}))
			case 2:
				require.NoError(t, admin.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.ServiceAccount.Name}}))
			}
			clock = clock.Add(snapshotDeleteRetryDelay(ordinal))
		}
		recreated := &corev1.ServiceAccount{}
		require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: recipe.Namespace, Name: recipe.ServiceAccount.Name}, recreated))
		require.NotEqual(t, recipe.ServiceAccount.UID, recreated.UID)
		for range 2 {
			err := restart().reconcileNativeDeletion(ctx, c, a, recipe)
			var operation *nativeOperationError
			require.ErrorAs(t, err, &operation)
			require.Equal(t, "CleanupFailed", operation.reason)
			require.ErrorContains(t, err, a.ExportID)
			require.ErrorContains(t, err, "exhausted")
			require.Equal(t, a, refresh(t, c), "recreated credentials do not reset the bound")
			clock = clock.Add(time.Hour)
		}
		require.Equal(t, before+exportprotocol.MaxCleanupAttempts, posts.Load())
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, exportprotocol.MaxCleanupAttempts+1)}, &batchv1.Job{})))
		require.NoError(t, r.retainNativeSnapshot(ctx, c, source, &c.Status.SnapshotExports[0]))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(source), &snapshotv1.VolumeSnapshot{}), "exhausted deletion holds CSI retention until acknowledged")
		c.Annotations = map[string]string{exportprotocol.CleanupAckAnnotation: a.ExportID}
		require.NoError(t, admin.Update(ctx, c))
		require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
		a = refresh(t, c)
		require.Equal(t, "Acknowledged", a.CleanupState)
		require.Equal(t, "Required", a.RemoteCleanupState)
		require.False(t, a.CleanupComplete)
	})
	t.Run("late retired cleanup cannot publish or disturb the current ordinal", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", deleting)
		clock := time.Now()
		local := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder, snapshotDeleteNow: func() time.Time { return clock }}
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		retiredAttempt := a.DeepCopy()
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		job, _ := cleanupWorker(t, c, a)
		failJob(t, job)
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		clock = clock.Add(snapshotDeleteRetryDelay(1))
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.EqualValues(t, 2, a.CleanupAttempt)
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		currentJob, currentPod := cleanupWorker(t, c, a)
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.NotNil(t, a.CleanupPod)

		late := datasnapshot.NativeCleanupJob(retiredAttempt, recipe)
		require.NoError(t, admin.Create(ctx, late))
		latePod := workerPod(t, late, corev1.PodStatus{Phase: corev1.PodRunning})
		completeJob(t, late, latePod, recipe.Image)
		_, _, _, err := exportprotocol.AuthorizeCleanup(c, c.UID, a.Sequence, a.ID, 1, currentPod.UID)
		require.Error(t, err, "a retired ordinal is denied even with the current Pod UID")
		_, _, _, err = exportprotocol.AuthorizeCleanup(c, c.UID, a.Sequence, a.ID, 2, latePod.UID)
		require.Error(t, err, "a retired worker is denied by its Pod UID")
		_, _, _, err = exportprotocol.AuthorizeCleanup(c, c.UID, a.Sequence, a.ID, 2, currentPod.UID)
		require.NoError(t, err)
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		require.Equal(t, a, refresh(t, c), "a retired success never completes a later ordinal")
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(late), &batchv1.Job{})))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(latePod), &corev1.Pod{})))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(currentJob), currentJob))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(currentPod), currentPod))

		orphaned := datasnapshot.NativeCleanupJob(retiredAttempt, recipe)
		require.NoError(t, admin.Create(ctx, orphaned))
		require.NoError(t, local.cleanupNativeOrphans(ctx, client.ObjectKeyFromObject(c)))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(orphaned), &batchv1.Job{})), "the live upload attempt does not protect a retired Job")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(currentJob), currentJob))

		completeJob(t, currentJob, currentPod, recipe.Image)
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.True(t, a.CleanupComplete)
		require.Equal(t, "Complete", a.RemoteCleanupState)
		require.Equal(t, appsv1.SnapshotExportPhaseDeleted, c.Status.SnapshotExports[0].Phase)
		require.NotNil(t, c.Status.SnapshotExports[0].Archive)
		require.NoError(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(currentJob), &batchv1.Job{})))
	})
	t.Run("concurrent reconcilers issue one POST per numbered name", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", deleting)
		_, err := r.ensureNativeIdentity(ctx, c)
		require.NoError(t, err)
		before := posts.Load()
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- r.reconcileNativeDeletion(ctx, c.DeepCopy(), a.DeepCopy(), recipe)
			}()
		}
		wg.Wait()
		close(errs)
		failures := 0
		for err := range errs {
			if err != nil {
				failures++
			}
		}
		require.Equal(t, 1, failures)
		require.Equal(t, before+1, posts.Load())
		require.EqualValues(t, 1, refresh(t, c).CleanupAttempt)
	})
	for name, status := range map[string]corev1.PodStatus{"failed": {Phase: corev1.PodFailed}, "succeeded": {Phase: corev1.PodSucceeded}} {
		t.Run("cleanup worker terminal before binding is never authorized "+name, func(t *testing.T) {
			c, a, recipe := seed(t, template.Namespace, "Succeeded", deleting)
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			job := &batchv1.Job{}
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, job))
			workerPod(t, job, status)
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			require.Equal(t, "Failed", a.CleanupState)
			require.Equal(t, "CleanupWorkerTerminated", a.CleanupReason)
			require.Nil(t, a.CleanupPod)
		})
	}
	t.Run("acknowledgement accepts unresolved deletion before any issuance", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", deleting)
		require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}))
		require.Error(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
		refresh(t, c)
		c.Annotations = map[string]string{exportprotocol.CleanupAckAnnotation: a.ExportID}
		require.NoError(t, admin.Update(ctx, c))
		require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
		a = refresh(t, c)
		require.Equal(t, "Acknowledged", a.CleanupState)
		require.Zero(t, a.CleanupAttempt)
		require.Equal(t, "Required", a.RemoteCleanupState)
		before := posts.Load()
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
		require.Equal(t, before, posts.Load())
	})
	t.Run("acknowledgement accepts retained output with leftover temporary objects", func(t *testing.T) {
		c, a, _ := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			a.RemoteCleanupState, e.RemoteCleanupState = "Required", "Required"
		})
		c.Annotations = map[string]string{exportprotocol.CleanupAckAnnotation: a.ExportID}
		require.NoError(t, admin.Update(ctx, c))
		require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
		a = refresh(t, c)
		require.Equal(t, "Acknowledged", a.CleanupState)
		require.Equal(t, "Required", a.RemoteCleanupState)
		require.Equal(t, appsv1.SnapshotExportPhaseAcknowledged, c.Status.SnapshotExports[0].Phase)
	})
	t.Run("cleanup ordinal schema", func(t *testing.T) {
		failedAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
		failed := func(ordinal int32) (*appsv1.ChainNode, string) {
			c, a, _ := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				a.CleanupAttempt = ordinal
				a.CleanupIssuer = string(uuid.NewUUID())
				a.CleanupIssuedAt = &failedAt
				a.CleanupJob = &appsv1.ExportResourceIdentity{Name: "cleanup-job", UID: "cleanup-job"}
				a.CleanupPod = &appsv1.ExportResourceIdentity{Name: "cleanup-pod", UID: "cleanup-pod"}
				a.CleanupState, a.CleanupReason = "Failed", "CleanupJobFailed"
				a.RemoteCleanupState, e.RemoteCleanupState = "Required", "Required"
				e.Phase = appsv1.SnapshotExportPhaseDeleting
			})
			return c, a.CleanupIssuer
		}
		retry := func(a *appsv1.SnapshotExportAttempt) {
			a.CleanupAttempt++
			a.CleanupIssuer = string(uuid.NewUUID())
			a.CleanupIssuedAt = ptr.To(metav1.Now())
			a.CleanupJob, a.CleanupPod = nil, nil
			a.CleanupState, a.CleanupReason = "", ""
		}
		c, issuer := failed(1)
		for _, tc := range []struct {
			name, message string
			mutate        func(*appsv1.SnapshotExportAttempt)
		}{
			{"issuer", "cleanup issuance is immutable", func(a *appsv1.SnapshotExportAttempt) { a.CleanupIssuer = string(uuid.NewUUID()) }},
			{"Job", "cleanup Job identity is immutable", func(a *appsv1.SnapshotExportAttempt) { a.CleanupJob.UID = "other" }},
			{"Pod", "cleanup Pod identity is immutable", func(a *appsv1.SnapshotExportAttempt) { a.CleanupPod = nil }},
			{"timestamp", "cleanup timestamp is immutable", func(a *appsv1.SnapshotExportAttempt) { a.CleanupIssuedAt = ptr.To(metav1.Now()) }},
			{"reopen within ordinal", "stopped cleanup cannot reopen", func(a *appsv1.SnapshotExportAttempt) { a.CleanupState = "" }},
			{"skipped ordinal", "cleanup attempt advances by one", func(a *appsv1.SnapshotExportAttempt) { retry(a); a.CleanupAttempt = 3 }},
			{"decreased ordinal", "cleanup attempt advances by one", func(a *appsv1.SnapshotExportAttempt) { a.CleanupAttempt, a.CleanupIssuer = 0, "" }},
			{"reused issuer", "fresh issuance", func(a *appsv1.SnapshotExportAttempt) { retry(a); a.CleanupIssuer = issuer }},
			{"reused timestamp", "fresh issuance", func(a *appsv1.SnapshotExportAttempt) { retry(a); a.CleanupIssuedAt = &failedAt }},
			{"retained Job", "fresh issuance", func(a *appsv1.SnapshotExportAttempt) {
				retry(a)
				a.CleanupJob = &appsv1.ExportResourceIdentity{Name: "cleanup-job", UID: "cleanup-job"}
			}},
			{"retained stop", "fresh issuance", func(a *appsv1.SnapshotExportAttempt) { retry(a); a.CleanupState = "Failed" }},
			{"recipe on increment", "attempt identity and recipe are immutable", func(a *appsv1.SnapshotExportAttempt) { retry(a); a.Recipe += " " }},
			{"upload witness on increment", "job issuance cannot revert", func(a *appsv1.SnapshotExportAttempt) { retry(a); a.JobCreateIssuer = string(uuid.NewUUID()) }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				invalid := c.DeepCopy()
				tc.mutate(&invalid.Status.SnapshotExportAuthority.Attempts[0])
				err := admin.Status().Update(ctx, invalid)
				require.True(t, apierrors.IsInvalid(err), "%v", err)
				require.ErrorContains(t, err, tc.message)
			})
		}
		valid := c.DeepCopy()
		retry(&valid.Status.SnapshotExportAuthority.Attempts[0])
		require.NoError(t, admin.Status().Update(ctx, valid))

		exhausted, _ := failed(exportprotocol.MaxCleanupAttempts)
		fourth := exhausted.DeepCopy()
		retry(&fourth.Status.SnapshotExportAuthority.Attempts[0])
		require.True(t, apierrors.IsInvalid(admin.Status().Update(ctx, fourth)))
		for _, state := range []string{"Acknowledged", "Abandoned"} {
			stopped, _ := failed(1)
			stopped.Status.SnapshotExportAuthority.Attempts[0].CleanupState = state
			require.NoError(t, admin.Status().Update(ctx, stopped))
			reopened := stopped.DeepCopy()
			retry(&reopened.Status.SnapshotExportAuthority.Attempts[0])
			err := admin.Status().Update(ctx, reopened)
			require.True(t, apierrors.IsInvalid(err), "%s: %v", state, err)
			require.ErrorContains(t, err, "fresh issuance")
		}
	})
	t.Run("postauthorization uncertainty cannot be erased", func(t *testing.T) {
		c, a, _ := seed(t, template.Namespace, "Uploading", nil)
		require.NoError(t, r.invalidateNativeAttempt(ctx, c, a, "WorkerFailed"))
		a = refresh(t, c)
		require.Equal(t, "Required", a.RemoteCleanupState)
		invalid := c.DeepCopy()
		invalid.Status.SnapshotExportAuthority.Attempts[0].RemoteCleanupState = "Complete"
		require.True(t, apierrors.IsInvalid(admin.Status().Update(ctx, invalid)))
	})
	for _, priorFailure := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("owner deletion preserves archive missing credentials %v prior cleanup failure %v", missing, priorFailure), func(t *testing.T) {
				c, _, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
					if priorFailure {
						a.CleanupAttempt = 1
						a.CleanupState = "Failed"
						a.CleanupReason = "IssuedCleanupMissing"
						a.CleanupIssuer = string(uuid.NewUUID())
						a.CleanupIssuedAt = ptr.To(metav1.Now())
						a.RemoteCleanupState = "Required"
						e.Phase = appsv1.SnapshotExportPhaseDeleting
						e.RemoteCleanupState = "Required"
					}
				})
				if missing {
					require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}))
				}
				require.NoError(t, admin.Delete(ctx, c))
				refresh(t, c)
				require.NoError(t, r.recoverNativeExports(ctx, c))
				a := refresh(t, c)
				require.Equal(t, "Abandoned", a.CleanupState)
				require.Equal(t, "OwnerDeleting", a.CleanupReason)
				if priorFailure {
					require.Equal(t, "Required", a.RemoteCleanupState)
				} else {
					require.Equal(t, "Complete", a.RemoteCleanupState)
				}
				require.NotNil(t, c.Status.SnapshotExports[0].Archive)
				require.NotContains(t, c.Finalizers, exportprotocol.AuthorityFinalizer)
				require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, &batchv1.Job{})))
			})
		}
	}
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminating namespace closes cleanup missing credentials %v", missing), func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "native-terminating-"}}
			require.NoError(t, admin.Create(ctx, ns))
			c, _, recipe := seed(t, ns.Name, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				a.CleanupAttempt = 1
				a.CleanupIssuer = string(uuid.NewUUID())
				a.CleanupIssuedAt = ptr.To(metav1.Now())
				a.RemoteCleanupState = "Required"
				e.RemoteCleanupState = "Required"
				e.Phase = appsv1.SnapshotExportPhaseDeleting
			})
			if missing {
				require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}))
			}
			require.NoError(t, admin.Delete(ctx, ns))
			require.NoError(t, admin.Delete(ctx, c))
			refresh(t, c)
			require.NoError(t, r.recoverNativeExports(ctx, c))
			a := refresh(t, c)
			require.Equal(t, "Abandoned", a.CleanupState)
			require.Equal(t, "NamespaceTerminating", a.CleanupReason)
			require.Equal(t, "Required", a.RemoteCleanupState)
			require.NotContains(t, c.Finalizers, exportprotocol.AuthorityFinalizer)
			require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, &batchv1.Job{})))
		})
	}

	for _, terminating := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleting owner quiesces before native cleanup terminating %v", terminating), func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "native-quiesce-"}}
			require.NoError(t, admin.Create(ctx, ns))
			c, a, recipe := seed(t, ns.Name, "Invalidated", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				a.Job, a.ScratchPVC, a.ScratchPV, a.Pod = nil, nil, nil, nil
				a.RemoteCleanupState, e.RemoteCleanupState = "Required", "Required"
				e.Phase = appsv1.SnapshotExportPhaseCleanupRequired
			})
			c.Finalizers = append(c.Finalizers, resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer)
			c.Spec.DeletionPolicy = &appsv1.DeletionPolicy{DataVolumes: ptr.To(appsv1.DeletionPolicyDelete), CosmosignerState: ptr.To(appsv1.DeletionPolicyRetain)}
			require.NoError(t, admin.Update(ctx, c))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:test"}}, TerminationGracePeriodSeconds: ptr.To[int64](0)}}
			require.NoError(t, controllerutil.SetControllerReference(c, pod, r.Scheme))
			require.NoError(t, admin.Create(ctx, pod))
			sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: c.Name + "-signer", Namespace: c.Namespace, Finalizers: []string{"test.example/observe-attribution"}}, Spec: k8sappsv1.StatefulSetSpec{Replicas: ptr.To[int32](0), ServiceName: "signer", Selector: &metav1.LabelSelector{MatchLabels: cosmosigner.InstanceLabels(c.Name + "-signer")}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: cosmosigner.InstanceLabels(c.Name + "-signer")}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "signer", Image: "signer:test"}}}}, PersistentVolumeClaimRetentionPolicy: &k8sappsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType, WhenScaled: k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType}}}
			require.NoError(t, controllerutil.SetControllerReference(c, sts, r.Scheme))
			require.NoError(t, admin.Create(ctx, sts))
			state := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-" + sts.Name + "-0", Namespace: c.Namespace, Labels: map[string]string{"cosmopilot.voluzi.com/cosmosigner-owner": string(c.UID)}, Finalizers: []string{cosmosigner.RetainedStateFinalizer}}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
			require.NoError(t, admin.Create(ctx, state))
			data := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
			_, _, err := resourcecleanup.PrepareGeneratedResource(data, c, r.Scheme, resourcecleanup.ClassDataVolumes, false)
			require.NoError(t, err)
			require.NoError(t, admin.Create(ctx, data))
			job := datasnapshot.NativeJob(a, recipe)
			require.NoError(t, admin.Create(ctx, job))
			pvc := datasnapshot.NativePVC(a, recipe, job.UID)
			pvc.Finalizers = append(pvc.Finalizers, "test.example/hold")
			require.NoError(t, admin.Create(ctx, pvc))
			if terminating {
				require.NoError(t, admin.Delete(ctx, ns))
			}
			require.NoError(t, admin.Delete(ctx, c))
			local := &Reconciler{Client: admin, APIReader: admin, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
			for range 5 {
				_, err := local.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
				require.NoError(t, err)
				if err := admin.Get(ctx, client.ObjectKeyFromObject(sts), sts); err == nil {
					sts.Status.ObservedGeneration = sts.Generation
					require.NoError(t, admin.Status().Update(ctx, sts))
				}
			}
			require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})), "native cleanup must not keep the node alive")
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(sts), sts))
			require.False(t, sts.DeletionTimestamp.IsZero(), "native cleanup must not keep the signer alive")
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(state), state))
			if !terminating {
				require.True(t, resourcecleanup.IsAttributed(state, resourcecleanup.RootOwnerFor(c), resourcecleanup.ClassCosmosignerState), "legacy claim attribution precedes StatefulSet deletion")
			}
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.Contains(t, c.Finalizers, exportprotocol.AuthorityFinalizer)
			require.Contains(t, c.Finalizers, resourcecleanup.Finalizer)
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(data), data))
			require.True(t, data.DeletionTimestamp.IsZero())
			controllerutil.RemoveFinalizer(sts, "test.example/observe-attribution")
			require.NoError(t, admin.Update(ctx, sts))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(pvc), pvc))
			controllerutil.RemoveFinalizer(pvc, "test.example/hold")
			controllerutil.RemoveFinalizer(pvc, "kubernetes.io/pvc-protection")
			require.NoError(t, admin.Update(ctx, pvc))
			for range 5 {
				_, err := local.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
				require.NoError(t, err)
			}
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.NotContains(t, c.Finalizers, exportprotocol.AuthorityFinalizer)
			require.NotContains(t, c.Finalizers, resourcecleanup.Finalizer)
		})
	}
	for _, compacted := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner finalizer waits for late protected upload PVC compacted %v", compacted), func(t *testing.T) {
			c, a, recipe := seed(t, template.Namespace, "Invalidated", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				a.Job = nil
				a.ScratchPVC = nil
				a.ScratchPV = nil
				a.Pod = nil
				a.RemoteCleanupState = "Complete"
				e.Phase = appsv1.SnapshotExportPhaseCleanupRequired
				e.RemoteCleanupState = "Complete"
			})
			if compacted {
				require.NoError(t, r.compactNativeAttempts(ctx, c))
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
				require.Empty(t, c.Status.SnapshotExportAuthority.Attempts)
			}
			job := datasnapshot.NativeJob(a, recipe)
			require.NoError(t, admin.Create(ctx, job))
			pvc := datasnapshot.NativePVC(a, recipe, job.UID)
			pvc.Finalizers = append(pvc.Finalizers, "test.example/hold")
			require.NoError(t, admin.Create(ctx, pvc))
			require.NoError(t, admin.Delete(ctx, c))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			if err := r.recoverNativeExports(ctx, c); err != nil {
				_, fatal := r.handleNativeOutcome(ctx, c, err)
				require.NoError(t, fatal)
			}
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.Contains(t, c.Finalizers, exportprotocol.AuthorityFinalizer)
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(pvc), pvc))
			require.False(t, pvc.DeletionTimestamp.IsZero())
			// Envtest has no protection controller; release only this unused fixture claim's barriers.
			controllerutil.RemoveFinalizer(pvc, "test.example/hold")
			controllerutil.RemoveFinalizer(pvc, "kubernetes.io/pvc-protection")
			require.NoError(t, admin.Update(ctx, pvc))
			require.NoError(t, r.recoverNativeExports(ctx, c))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.NotContains(t, c.Finalizers, exportprotocol.AuthorityFinalizer)
		})
	}
	t.Run("LimitRange defaults authorize actual upload and cleanup pods", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", nil)
		limit := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "native-defaults", Namespace: c.Namespace}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, Default: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")}, DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}}}
		require.NoError(t, admin.Create(ctx, limit))
		t.Cleanup(func() { require.NoError(t, admin.Delete(ctx, limit)) })
		for _, cleanup := range []bool{false, true} {
			job := datasnapshot.NativeJob(a, recipe)
			validate := datasnapshot.ValidateNativePodSpec
			validateJob := datasnapshot.ValidateNativeJob
			if cleanup {
				job = datasnapshot.NativeCleanupJob(a, recipe)
				validate = datasnapshot.ValidateNativeCleanupPodSpec
				validateJob = datasnapshot.ValidateNativeCleanupJob
			}
			job.Labels["admission.example/extra"] = "allowed"
			job.Spec.Template.Labels["admission.example/extra"] = "allowed"
			require.NoError(t, admin.Create(ctx, job))
			require.NotEmpty(t, job.Spec.Template.Labels["batch.kubernetes.io/controller-uid"])
			require.NoError(t, validateJob(job, a, recipe))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "limit-pod-", Namespace: c.Namespace}, Spec: *job.Spec.Template.Spec.DeepCopy()}
			require.NoError(t, admin.Create(ctx, pod))
			require.Contains(t, pod.Spec.Containers[0].Resources.Limits, corev1.ResourceCPU)
			require.NoError(t, validate(&pod.Spec, a, recipe))
			require.NoError(t, admin.Delete(ctx, pod))
		}
	})
	t.Run("twenty nine retained archives admit a thirtieth reservation", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", nil)
		completedAt := c.Status.SnapshotExports[0].Archive.CompletedAt
		c.Status.SnapshotExportAuthority.Attempts = nil
		c.Status.SnapshotExports = nil
		for i := int64(1); i <= 29; i++ {
			copy := a.DeepCopy()
			copy.Sequence = i
			if i > 1 {
				copy.ID = string(uuid.NewUUID())
				copy.ExportID = "export-" + copy.ID
			}
			rec := recipe
			rec.ExportID = copy.ExportID
			rec.ResourceName = copy.ExportID
			rec.ObjectName = copy.ExportID
			text, digest, err := exportprotocol.EncodeRecipe(rec)
			require.NoError(t, err)
			copy.Recipe = text
			copy.RecipeSHA256 = digest
			previous, _ := exportprotocol.DecodeEnvelope([]byte(a.CompletedEnvelope))
			envelope := exportprotocol.EnvelopeFor(copy, rec)
			envelope.Result = previous.Result
			envelope.Result.ObjectKey = rec.ObjectName + rec.Compression.Extension()
			body, err := exportprotocol.EncodeEnvelope(envelope)
			require.NoError(t, err)
			copy.CompletedEnvelope = string(body)
			archive, err := exportprotocol.NewArchive(copy, rec, envelope, completedAt)
			require.NoError(t, err)
			c.Status.SnapshotExportAuthority.Attempts = append(c.Status.SnapshotExportAuthority.Attempts, *copy)
			c.Status.SnapshotExports = append(c.Status.SnapshotExports, appsv1.SnapshotExportStatus{ID: copy.ExportID, SnapshotName: source.Name, SnapshotUID: source.UID, ObjectName: copy.ExportID, Destination: rec.Destination, ProvenanceVersion: 1, Phase: appsv1.SnapshotExportPhaseUploaded, Archive: archive, DeleteOnExpire: true, RemoteCleanupState: "Complete"})
		}
		c.Status.SnapshotExportAuthority.LastSequence = 29
		require.NoError(t, admin.Status().Update(ctx, c))
		next := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "capacity-source", Namespace: c.Namespace}, Spec: source.Spec}
		require.NoError(t, admin.Create(ctx, next))
		content := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: c.Name + "-capacity-content"}, Spec: snapshotv1.VolumeSnapshotContentSpec{DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete, Driver: recipe.Driver, Source: snapshotv1.VolumeSnapshotContentSource{SnapshotHandle: ptr.To("capacity")}, VolumeSnapshotRef: corev1.ObjectReference{Name: next.Name, Namespace: next.Namespace, UID: next.UID}}}
		require.NoError(t, admin.Create(ctx, content))
		content.Status = &snapshotv1.VolumeSnapshotContentStatus{ReadyToUse: ptr.To(true), SnapshotHandle: ptr.To("capacity")}
		require.NoError(t, admin.Status().Update(ctx, content))
		next.Status = source.Status.DeepCopy()
		next.Status.BoundVolumeSnapshotContentName = ptr.To(content.Name)
		require.NoError(t, admin.Status().Update(ctx, next))
		currentBytes, err := json.Marshal(c.Status)
		require.NoError(t, err)
		beforeProjected, err := nativeProjectedStatusSize(c)
		require.NoError(t, err)
		t.Logf("29 retained: actual=%d projected=%d", len(currentBytes), beforeProjected)
		require.NoError(t, r.reserveNativeAttempt(ctx, c, next))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Len(t, c.Status.SnapshotExports, 30)
		require.EqualValues(t, 30, c.Status.SnapshotExportAuthority.LastSequence)
		currentBytes, err = json.Marshal(c.Status)
		require.NoError(t, err)
		projected, err := nativeProjectedStatusSize(c)
		require.NoError(t, err)
		t.Logf("29 retained plus 30th reservation: actual=%d projected=%d limit=%d", len(currentBytes), projected, exportprotocol.MaxNativeStatus)
		blocked := next.DeepCopy()
		blocked.ObjectMeta = metav1.ObjectMeta{Name: "capacity-blocked", Namespace: c.Namespace}
		require.NoError(t, admin.Create(ctx, blocked))
		blockedContent := content.DeepCopy()
		blockedContent.ObjectMeta = metav1.ObjectMeta{Name: c.Name + "-blocked-content"}
		blockedContent.Spec.VolumeSnapshotRef.Name = blocked.Name
		blockedContent.Spec.VolumeSnapshotRef.UID = blocked.UID
		require.NoError(t, admin.Create(ctx, blockedContent))
		blockedContent.Status = content.Status.DeepCopy()
		require.NoError(t, admin.Status().Update(ctx, blockedContent))
		blocked.Status = next.Status.DeepCopy()
		blocked.Status.BoundVolumeSnapshotContentName = ptr.To(blockedContent.Name)
		require.NoError(t, admin.Status().Update(ctx, blocked))
		require.ErrorContains(t, r.reserveNativeAttempt(ctx, c, blocked), "StatusBudget")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Len(t, c.Status.SnapshotExports, 30)
		require.EqualValues(t, 30, c.Status.SnapshotExportAuthority.LastSequence)
		exhausted := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "sequence-exhausted-", Namespace: c.Namespace, Finalizers: []string{exportprotocol.AuthorityFinalizer}}, Spec: *c.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, exhausted))
		exhausted.Status.SnapshotExportAuthority = &appsv1.SnapshotExportAuthority{Version: 1, LastSequence: math.MaxInt64, Attempts: []appsv1.SnapshotExportAttempt{}}
		require.NoError(t, admin.Status().Update(ctx, exhausted))
		require.ErrorContains(t, r.reserveNativeAttempt(ctx, exhausted, blocked), "SequenceExhausted")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(exhausted), exhausted))
		require.Empty(t, exhausted.Status.SnapshotExportAuthority.Attempts)
		require.Equal(t, int64(math.MaxInt64), exhausted.Status.SnapshotExportAuthority.LastSequence)
	})

	// history fills an owner with count terminal records, oldest first: the oldest has its own live source, the rest share one.
	history := func(t *testing.T, count, padding int, record func(int, *appsv1.SnapshotExportAttempt, *appsv1.SnapshotExportStatus) bool) (*appsv1.ChainNode, *snapshotv1.VolumeSnapshot, *snapshotv1.VolumeSnapshot) {
		t.Helper()
		oldest, oldestContent := nativeReviewSource(t, admin, source, original.Driver)
		shared, sharedContent := nativeReviewSource(t, admin, source, original.Driver)
		c, prototype, recipe := seed(t, template.Namespace, "Succeeded", nil)
		prior, err := exportprotocol.DecodeEnvelope([]byte(prototype.CompletedEnvelope))
		require.NoError(t, err)
		if padding > 0 {
			recipe.Destination.Endpoint = "https://store.example/" + strings.Repeat("p", padding)
		}
		recipe.DeleteOnExpire = false
		c.Status.SnapshotExportAuthority.Attempts = []appsv1.SnapshotExportAttempt{}
		c.Status.SnapshotExports = nil
		require.NoError(t, admin.Status().Update(ctx, c))
		first := c.Status.SnapshotExportAuthority.LastSequence + 1
		for i := range count {
			a := prototype.DeepCopy()
			a.Sequence = first + int64(i)
			a.ID = string(uuid.NewUUID())
			a.ExportID = "export-" + a.ID
			rec := recipe
			rec.ExportID, rec.ResourceName, rec.ObjectName = a.ExportID, a.ExportID, a.ExportID
			rec.Snapshot, rec.Content = identity(shared), identity(sharedContent)
			if i == 0 {
				rec.Snapshot, rec.Content = identity(oldest), identity(oldestContent)
			}
			a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
			require.NoError(t, err)
			envelope := exportprotocol.EnvelopeFor(a, rec)
			envelope.Result = prior.Result
			envelope.Result.ObjectKey = rec.ObjectName + rec.Compression.Extension()
			body, err := exportprotocol.EncodeEnvelope(envelope)
			require.NoError(t, err)
			a.CompletedEnvelope = string(body)
			archive, err := exportprotocol.NewArchive(a, rec, envelope, metav1.Now())
			require.NoError(t, err)
			e := appsv1.SnapshotExportStatus{ID: a.ExportID, SnapshotName: rec.Snapshot.Name, SnapshotUID: rec.Snapshot.UID, ObjectName: rec.ObjectName, Destination: rec.Destination, Phase: appsv1.SnapshotExportPhaseUploaded, ProvenanceVersion: 1, Archive: archive, RemoteCleanupState: "Complete"}
			if record != nil && record(i, a, &e) {
				c.Status.SnapshotExportAuthority.Attempts = append(c.Status.SnapshotExportAuthority.Attempts, *a)
			}
			c.Status.SnapshotExports = append(c.Status.SnapshotExports, e)
		}
		c.Status.SnapshotExportAuthority.LastSequence = first + int64(count) - 1
		require.NoError(t, admin.Status().Update(ctx, c))
		return c, oldest, shared
	}
	withEvents := func(r *Reconciler) (*Reconciler, *record.FakeRecorder) {
		events := record.NewFakeRecorder(64)
		return &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts, recorder: events}, events
	}
	requireFits := func(t *testing.T, c *appsv1.ChainNode) {
		t.Helper()
		size, err := nativeProjectedStatusSize(c)
		require.NoError(t, err)
		require.LessOrEqual(t, size, exportprotocol.MaxNativeStatus)
	}
	smallRecord := func(_ int, _ *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) bool {
		e.Archive = nil
		e.Phase = appsv1.SnapshotExportPhaseCleanupRequired
		return false
	}
	t.Run("retained exports keep reserving past the history count with live sources", func(t *testing.T) {
		c, oldest, _ := history(t, nativeHistoryLimit, 0, smallRecord)
		evicted := c.Status.SnapshotExports[0]
		retainedNext := c.Status.SnapshotExports[1].ID
		local, events := withEvents(r)
		before := posts.Load()
		watermark := c.Status.SnapshotExportAuthority.LastSequence
		for i := range 4 {
			next, _ := nativeReviewSource(t, admin, source, original.Driver)
			require.NoError(t, local.reserveNativeAttempt(ctx, c, next))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.EqualValues(t, watermark+int64(i)+1, c.Status.SnapshotExportAuthority.LastSequence, "the watermark stays monotone")
			require.Len(t, c.Status.SnapshotExports, nativeHistoryLimit)
			requireFits(t, c)
			if i == 0 {
				require.Nil(t, snapshotExportByID(c, evicted.ID))
				require.NotNil(t, snapshotExportByID(c, retainedNext), "eviction is oldest first")
				event := <-events.Events
				require.Contains(t, event, evicted.ID)
				require.Contains(t, event, evicted.ObjectName)
				require.Contains(t, event, evicted.Destination.Bucket)
			}
			latest := c.Status.SnapshotExportAuthority.Attempts[len(c.Status.SnapshotExportAuthority.Attempts)-1].DeepCopy()
			require.NoError(t, local.invalidateNativeAttempt(ctx, c, latest, "WorkerFailed"))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			latest = exportprotocol.FindAttempt(c, latest.Sequence, latest.ID).DeepCopy()
			rec, err := exportprotocol.DecodeRecipe(latest)
			require.NoError(t, err)
			require.NoError(t, local.cleanupNativeAttempt(ctx, c, latest, rec))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		}
		require.Equal(t, before, posts.Load(), "status release never issues remote work")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(oldest), oldest))
		require.Equal(t, tarballNativeFailed, oldest.Annotations[controllers.AnnotationExportingTarball], "an evicted source keeps its terminal bookkeeping")
		require.True(t, nativeRetryDue(c, oldest), "eviction keeps the retry budget")
	})
	t.Run("bytes release history below the count limit without touching retained objects", func(t *testing.T) {
		const records, padding = 24, 3000
		c, oldest, _ := history(t, records, padding, nil)
		before, err := nativeProjectedStatusSize(c)
		require.NoError(t, err)
		require.Greater(t, before, exportprotocol.MaxNativeStatus)
		local, events := withEvents(r)
		next, _ := nativeReviewSource(t, admin, source, original.Driver)
		require.NoError(t, local.reserveNativeAttempt(ctx, c, next))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Less(t, len(c.Status.SnapshotExports), records)
		require.Len(t, c.Status.SnapshotExportAuthority.Attempts, 1)
		requireFits(t, c)
		require.False(t, apiMeta.IsStatusConditionTrue(c.Status.Conditions, appsv1.ConditionSnapshotExportCapacityExceeded))
		event := <-events.Events
		require.Contains(t, event, corev1.EventTypeNormal)
		require.Contains(t, event, "retained in place")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(oldest), oldest))
		require.Equal(t, tarballNativeFinished, oldest.Annotations[controllers.AnnotationExportingTarball])
	})
	t.Run("acknowledged uncertainty releases its slot without claiming deletion", func(t *testing.T) {
		acknowledged := func(_ int, a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) bool {
			a.Phase, a.CompletedEnvelope, a.AuthorizedAt = "Invalidated", "", ptr.To(metav1.Now())
			a.RemoteCleanupState, a.CleanupState, a.CleanupReason = "Required", "Acknowledged", "OperatorAcknowledgedWithoutDeletionProof"
			e.Archive, e.Phase, e.RemoteCleanupState = nil, appsv1.SnapshotExportPhaseAcknowledged, "Required"
			return true
		}
		c, oldest, _ := history(t, nativeHistoryLimit, 0, acknowledged)
		evicted := c.Status.SnapshotExports[0].ID
		local, events := withEvents(r)
		next, _ := nativeReviewSource(t, admin, source, original.Driver)
		require.NoError(t, local.reserveNativeAttempt(ctx, c, next))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Len(t, c.Status.SnapshotExportAuthority.Attempts, 1, "acknowledgement frees every attempt slot")
		require.Nil(t, snapshotExportByID(c, evicted))
		for _, e := range c.Status.SnapshotExports[:len(c.Status.SnapshotExports)-1] {
			require.Equal(t, "Required", e.RemoteCleanupState, "release never rewrites uncertainty to Complete")
		}
		event := <-events.Events
		require.Contains(t, event, corev1.EventTypeWarning)
		require.Contains(t, event, "never proven deleted")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(oldest), oldest))
		require.Equal(t, tarballNativeFailed, oldest.Annotations[controllers.AnnotationExportingTarball])
	})
	t.Run("exhausted cleanup holds capacity until acknowledged", func(t *testing.T) {
		exhausted := func(_ int, a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) bool {
			a.CleanupAttempt = exportprotocol.MaxCleanupAttempts
			a.CleanupIssuer, a.CleanupIssuedAt = string(uuid.NewUUID()), ptr.To(metav1.Now())
			a.CleanupState, a.CleanupReason, a.RemoteCleanupState = "Failed", "CleanupJobFailed", "Required"
			e.Archive, e.Phase, e.RemoteCleanupState = nil, appsv1.SnapshotExportPhaseDeleting, "Required"
			return true
		}
		c, _, _ := history(t, nativeHistoryLimit, 0, exhausted)
		watermark := c.Status.SnapshotExportAuthority.LastSequence
		next, _ := nativeReviewSource(t, admin, source, original.Driver)
		var deferred *nativeDeferred
		require.ErrorAs(t, r.reserveNativeAttempt(ctx, c, next), &deferred)
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Len(t, c.Status.SnapshotExportAuthority.Attempts, nativeHistoryLimit)
		condition := apiMeta.FindStatusCondition(c.Status.Conditions, appsv1.ConditionSnapshotExportCapacityExceeded)
		require.NotNil(t, condition)
		require.Equal(t, "AttemptLimit", condition.Reason)
		c.Annotations = map[string]string{exportprotocol.CleanupAckAnnotation: c.Status.SnapshotExports[0].ID}
		require.NoError(t, admin.Update(ctx, c))
		require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
		require.NoError(t, r.reserveNativeAttempt(ctx, c, next))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Equal(t, watermark+1, c.Status.SnapshotExportAuthority.LastSequence)
		require.Nil(t, apiMeta.FindStatusCondition(c.Status.Conditions, appsv1.ConditionSnapshotExportCapacityExceeded))
	})
	for name, duty := range map[string]func(*appsv1.SnapshotExportStatus){
		"delete on expire": func(e *appsv1.SnapshotExportStatus) { e.DeleteOnExpire = true },
		"pending deletion": func(e *appsv1.SnapshotExportStatus) {
			e.DeleteOnExpire, e.Phase, e.RemoteCleanupState = true, appsv1.SnapshotExportPhaseDeleting, "Required"
		},
		"unacknowledged uncertainty": func(e *appsv1.SnapshotExportStatus) {
			e.Archive, e.Phase, e.RemoteCleanupState = nil, appsv1.SnapshotExportPhaseCleanupRequired, "Required"
		},
		"active upload": func(e *appsv1.SnapshotExportStatus) {
			e.Archive, e.Phase, e.RemoteCleanupState = nil, appsv1.SnapshotExportPhaseUploading, ""
		},
	} {
		t.Run("history with a remaining duty is never evicted "+name, func(t *testing.T) {
			c, _, _ := history(t, nativeHistoryLimit, 0, func(_ int, _ *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) bool {
				e.Archive = nil
				duty(e)
				return false
			})
			watermark := c.Status.SnapshotExportAuthority.LastSequence
			next, _ := nativeReviewSource(t, admin, source, original.Driver)
			var deferred *nativeDeferred
			require.ErrorAs(t, r.reserveNativeAttempt(ctx, c, next), &deferred)
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.Len(t, c.Status.SnapshotExports, nativeHistoryLimit)
			require.Equal(t, watermark, c.Status.SnapshotExportAuthority.LastSequence)
		})
	}
	t.Run("local resource lag protects an acknowledged record from eviction", func(t *testing.T) {
		c, _, _ := history(t, nativeHistoryLimit, 0, func(_ int, a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) bool {
			a.RemoteCleanupState, a.CleanupState = "Required", "Acknowledged"
			e.Phase, e.RemoteCleanupState = appsv1.SnapshotExportPhaseAcknowledged, "Required"
			return true
		})
		lagging := c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
		recipe, err := exportprotocol.DecodeRecipe(lagging)
		require.NoError(t, err)
		lagging.CleanupAttempt = 1
		late := datasnapshot.NativeCleanupJob(lagging, recipe)
		require.NoError(t, admin.Create(ctx, late))
		released := c.Status.SnapshotExports[1].ID
		next, _ := nativeReviewSource(t, admin, source, original.Driver)
		require.NoError(t, r.reserveNativeAttempt(ctx, c, next))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.NotNil(t, snapshotExportByID(c, lagging.ExportID))
		require.NotNil(t, exportprotocol.FindAttempt(c, lagging.Sequence, lagging.ID))
		require.Nil(t, snapshotExportByID(c, released))
	})
	t.Run("retained success keeps its authority until the source marker is durable", func(t *testing.T) {
		c, oldest, _ := history(t, 1, 0, func(_ int, _ *appsv1.SnapshotExportAttempt, _ *appsv1.SnapshotExportStatus) bool { return true })
		a := c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
		oldest.Annotations[controllers.AnnotationExportingTarball] = "true"
		require.NoError(t, admin.Update(ctx, oldest))
		require.NoError(t, r.compactNativeAttempts(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.NotNil(t, exportprotocol.FindAttempt(c, a.Sequence, a.ID))
		oldest.Annotations[controllers.AnnotationExportingTarball] = tarballNativeFinished
		require.NoError(t, admin.Update(ctx, oldest))
		require.NoError(t, r.compactNativeAttempts(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Nil(t, exportprotocol.FindAttempt(c, a.Sequence, a.ID))
		require.NotNil(t, snapshotExportByID(c, a.ExportID).Archive, "compaction keeps the retained descriptor")
	})
	t.Run("failed marker write defers release", func(t *testing.T) {
		c, oldest, _ := history(t, nativeHistoryLimit, 0, smallRecord)
		var failed atomic.Bool
		failed.Store(true)
		faulty := rest.CopyConfig(r.RestConfig)
		faulty.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
			return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
				if failed.Load() && req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/volumesnapshots/"+oldest.Name) {
					return nil, fmt.Errorf("test marker write unavailable")
				}
				return base.RoundTrip(req)
			})
		}
		writer, err := client.New(faulty, client.Options{Scheme: r.Scheme})
		require.NoError(t, err)
		local := &Reconciler{Client: writer, APIReader: r.APIReader, Scheme: r.Scheme, opts: r.opts, recorder: r.recorder}
		next, _ := nativeReviewSource(t, admin, source, original.Driver)
		require.ErrorContains(t, local.reserveNativeAttempt(ctx, c, next), "marker write unavailable")
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Len(t, c.Status.SnapshotExports, nativeHistoryLimit, "history is never dropped before its marker is durable")
		failed.Store(false)
		require.NoError(t, local.reserveNativeAttempt(ctx, c, next))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(oldest), oldest))
		require.Equal(t, tarballNativeFailed, oldest.Annotations[controllers.AnnotationExportingTarball])
	})
	t.Run("retained record of a replaced source releases without touching the replacement", func(t *testing.T) {
		c, oldest, _ := history(t, 2, 0, nil)
		require.NoError(t, admin.Delete(ctx, oldest))
		replacement := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: oldest.Name, Namespace: oldest.Namespace}, Spec: *oldest.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, replacement))
		released := c.Status.SnapshotExports[0].ID
		local, events := withEvents(r)
		require.NoError(t, local.compactNativeAttempts(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Nil(t, snapshotExportByID(c, released))
		require.Len(t, c.Status.SnapshotExports, 1)
		require.Contains(t, <-events.Events, released)
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(replacement), replacement))
		require.Empty(t, replacement.Annotations[controllers.AnnotationExportingTarball])
	})
	t.Run("split retained output releases once its source is gone", func(t *testing.T) {
		split := func(_ int, a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) bool {
			recipe, err := exportprotocol.DecodeRecipe(a)
			require.NoError(t, err)
			envelope := exportprotocol.EnvelopeFor(a, recipe)
			envelope.Result = dataexporter.UploadResult{Version: 1, Layout: "split", ObjectCount: 2, Compression: recipe.Compression}
			body, err := exportprotocol.EncodeEnvelope(envelope)
			require.NoError(t, err)
			a.CompletedEnvelope, e.Archive = string(body), nil
			return true
		}
		c, oldest, _ := history(t, 1, 0, split)
		a := c.Status.SnapshotExportAuthority.Attempts[0].DeepCopy()
		oldest.Annotations[controllers.AnnotationExportingTarball] = tarballNativeFinished
		require.NoError(t, admin.Update(ctx, oldest))
		require.NoError(t, r.compactNativeAttempts(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.NotNil(t, exportprotocol.FindAttempt(c, a.Sequence, a.ID), "a split result keeps its only layout evidence while the source lives")
		require.NoError(t, admin.Delete(ctx, oldest))
		require.NoError(t, r.compactNativeAttempts(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Empty(t, c.Status.SnapshotExportAuthority.Attempts)
		require.Empty(t, c.Status.SnapshotExports)
		require.Equal(t, a.Sequence, c.Status.SnapshotExportAuthority.LastSequence)
	})
	t.Run("acknowledged catalog record without authority allows CSI retention", func(t *testing.T) {
		c, oldest, _ := history(t, 1, 0, func(_ int, _ *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) bool {
			e.DeleteOnExpire, e.Phase, e.RemoteCleanupState = true, appsv1.SnapshotExportPhaseAcknowledged, "Required"
			return false
		})
		require.NoError(t, r.retainNativeSnapshot(ctx, c, oldest, &c.Status.SnapshotExports[0]))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(oldest), &snapshotv1.VolumeSnapshot{})))
	})
	for _, marker := range []string{tarballNativeFinished, tarballNativeFailed} {
		t.Run("protocol switch after eviction stays away from legacy cloud paths "+marker, func(t *testing.T) {
			s, _ := nativeReviewSource(t, admin, source, original.Driver)
			c := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: "evicted-", Namespace: template.Namespace}, Spec: *template.Spec.DeepCopy()}
			c.Spec.Persistence.Snapshots.ExportTarball.Protocol = ""
			c.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
			require.NoError(t, admin.Create(ctx, c))
			c.Status.LatestHeight, c.Status.PvcSize = 1, "1Gi"
			require.NoError(t, admin.Status().Update(ctx, c))
			s.Labels = map[string]string{controllers.LabelChainNode: c.Name}
			s.Annotations[controllers.AnnotationExportingTarball] = marker
			s.Annotations[controllers.AnnotationTarballExportAttempts] = "1"
			require.NoError(t, admin.Update(ctx, s))
			clients, err := kubernetes.NewForConfig(r.RestConfig)
			require.NoError(t, err)
			local := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts, recorder: record.NewFakeRecorder(100), ClientSet: clients}
			requireNoJobs := func() {
				t.Helper()
				jobs := &batchv1.JobList{}
				require.NoError(t, admin.List(ctx, jobs, client.InNamespace(c.Namespace)))
				for i := range jobs.Items {
					require.False(t, metav1.IsControlledBy(&jobs.Items[i], c), "legacy cloud work started: %s", jobs.Items[i].Name)
				}
				require.Empty(t, c.Status.SnapshotExports)
			}
			require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
			requireNoJobs()
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(s), s))
			require.Equal(t, marker, s.Annotations[controllers.AnnotationExportingTarball])
			s.Annotations[controllers.AnnotationSnapshotRetention] = "1ns"
			require.NoError(t, admin.Update(ctx, s))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
			requireNoJobs()
			require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(s), &snapshotv1.VolumeSnapshot{})), "retention deletes only the local CSI snapshot")
		})
	}
	for _, authorized := range []bool{false, true} {
		for _, countRetention := range []bool{false, true} {
			remote := "Complete"
			if authorized {
				remote = "Required"
			}
			t.Run(fmt.Sprintf("invalidated snapshot CSI retention authorized %v count %v", authorized, countRetention), func(t *testing.T) {
				ownedSource := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{GenerateName: "a-retention-", Namespace: template.Namespace, Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "true", controllers.AnnotationExportingTarball: "true"}}, Spec: source.Spec}
				if !countRetention {
					ownedSource.Annotations[controllers.AnnotationSnapshotRetention] = "1ns"
				} else {
					ownedSource.Annotations[controllers.AnnotationTarballExportAttempts] = strconv.Itoa(tarballExportMaxAttempts)
				}
				require.NoError(t, admin.Create(ctx, ownedSource))
				ownedSource.Status = source.Status.DeepCopy()
				require.NoError(t, admin.Status().Update(ctx, ownedSource))
				c, a, _ := seed(t, template.Namespace, "Invalidated", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
					rec, err := exportprotocol.DecodeRecipe(a)
					require.NoError(t, err)
					rec.Snapshot = identity(ownedSource)
					text, digest, err := exportprotocol.EncodeRecipe(rec)
					require.NoError(t, err)
					a.Recipe = text
					a.RecipeSHA256 = digest
					a.RemoteCleanupState = remote
					if authorized {
						a.AuthorizedAt = ptr.To(metav1.Now())
					}
					e.SnapshotName = ownedSource.Name
					e.SnapshotUID = ownedSource.UID
					e.Phase = appsv1.SnapshotExportPhaseCleanupRequired
					e.RemoteCleanupState = remote
				})
				ownedSource.Labels = map[string]string{controllers.LabelChainNode: c.Name}
				require.NoError(t, admin.Update(ctx, ownedSource))
				c.Status.LatestHeight = 1
				c.Status.PvcSize = "1Gi"
				require.NoError(t, admin.Status().Update(ctx, c))
				c.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
				if countRetention {
					c.Spec.Persistence.Snapshots.Retain = ptr.To[int32](1)
				}
				require.NoError(t, admin.Update(ctx, c))
				if countRetention {
					newer := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{GenerateName: "z-retention-", Namespace: c.Namespace, Labels: ownedSource.Labels, Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "true", controllers.AnnotationExportingTarball: tarballFailed}}, Spec: source.Spec}
					require.NoError(t, admin.Create(ctx, newer))
					newer.Status = source.Status.DeepCopy()
					require.NoError(t, admin.Status().Update(ctx, newer))
				}
				local := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts, recorder: record.NewFakeRecorder(100)}
				clients, err := kubernetes.NewForConfig(r.RestConfig)
				require.NoError(t, err)
				local.ClientSet = clients
				require.NoError(t, local.recoverNativeExports(ctx, c))
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(ownedSource), ownedSource))
				require.Equal(t, tarballNativeFailed, ownedSource.Annotations[controllers.AnnotationExportingTarball])
				require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
				require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(ownedSource), &snapshotv1.VolumeSnapshot{})))
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
				e := snapshotExportByID(c, a.ExportID)
				require.NotNil(t, e)
				require.Equal(t, remote, e.RemoteCleanupState)
				require.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, e.Phase)
				require.NotEqual(t, appsv1.SnapshotExportPhaseDeleted, e.Phase)
			})
		}
	}

	t.Run("acknowledged cleanup waits for late local resources before CSI retention", func(t *testing.T) {
		snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{GenerateName: "ack-retention-", Namespace: template.Namespace, Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "true", controllers.AnnotationExportingTarball: tarballFinished, controllers.AnnotationSnapshotRetention: "1ns"}}, Spec: source.Spec}
		require.NoError(t, admin.Create(ctx, snapshot))
		snapshot.Status = source.Status.DeepCopy()
		require.NoError(t, admin.Status().Update(ctx, snapshot))
		c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			rec, err := exportprotocol.DecodeRecipe(a)
			require.NoError(t, err)
			rec.Snapshot = identity(snapshot)
			text, digest, err := exportprotocol.EncodeRecipe(rec)
			require.NoError(t, err)
			a.Recipe = text
			a.RecipeSHA256 = digest
			a.CleanupAttempt = 1
			a.CleanupState = "Acknowledged"
			a.CleanupIssuer = string(uuid.NewUUID())
			a.CleanupIssuedAt = ptr.To(metav1.Now())
			a.RemoteCleanupState = "Required"
			e.SnapshotName = snapshot.Name
			e.SnapshotUID = snapshot.UID
			e.Phase = appsv1.SnapshotExportPhaseAcknowledged
			e.RemoteCleanupState = "Required"
		})
		snapshot.Labels = map[string]string{controllers.LabelChainNode: c.Name}
		require.NoError(t, admin.Update(ctx, snapshot))
		c.Status.LatestHeight = 1
		c.Status.PvcSize = "1Gi"
		require.NoError(t, admin.Status().Update(ctx, c))
		c.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
		require.NoError(t, admin.Update(ctx, c))
		// An already-issued POST can arrive between early recovery and retention.
		late := datasnapshot.NativeCleanupJob(a, recipe)
		require.NoError(t, admin.Create(ctx, late))
		clients, err := kubernetes.NewForConfig(r.RestConfig)
		require.NoError(t, err)
		local := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts, recorder: record.NewFakeRecorder(100), ClientSet: clients}
		require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot), "CSI retention must wait for exact stopped cleanup resources")
		require.NoError(t, local.recoverNativeExports(ctx, c))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(late), &batchv1.Job{})))
		require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(snapshot), &snapshotv1.VolumeSnapshot{})))
		require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
		require.Equal(t, "Required", c.Status.SnapshotExports[0].RemoteCleanupState)
		require.Equal(t, appsv1.SnapshotExportPhaseAcknowledged, c.Status.SnapshotExports[0].Phase)
	})
	t.Run("fifty one confirmed deletion cycles compact catalog and advance watermark", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			rec, err := exportprotocol.DecodeRecipe(a)
			require.NoError(t, err)
			rec.Snapshot = appsv1.ExportResourceIdentity{Name: "resolved-absent", UID: "resolved-absent"}
			text, digest, err := exportprotocol.EncodeRecipe(rec)
			require.NoError(t, err)
			a.Recipe = text
			a.RecipeSHA256 = digest
			e.SnapshotName = rec.Snapshot.Name
			e.SnapshotUID = rec.Snapshot.UID
		})
		prototype := a.DeepCopy()
		catalog := c.Status.SnapshotExports[0]
		for sequence := int64(1); sequence <= 51; sequence++ {
			if sequence > 1 {
				a = prototype.DeepCopy()
				a.Sequence = sequence
				a.ID = string(uuid.NewUUID())
				a.ExportID = "export-" + a.ID
				recipe.ExportID = a.ExportID
				recipe.ResourceName = a.ExportID
				recipe.ObjectName = a.ExportID
				text, digest, err := exportprotocol.EncodeRecipe(recipe)
				require.NoError(t, err)
				a.Recipe = text
				a.RecipeSHA256 = digest
				previous, err := exportprotocol.DecodeEnvelope([]byte(prototype.CompletedEnvelope))
				require.NoError(t, err)
				envelope := exportprotocol.EnvelopeFor(a, recipe)
				envelope.Result = previous.Result
				envelope.Result.ObjectKey = recipe.ObjectName + recipe.Compression.Extension()
				body, err := exportprotocol.EncodeEnvelope(envelope)
				require.NoError(t, err)
				a.CompletedEnvelope = string(body)
				e := catalog
				e.ID = a.ExportID
				e.ObjectName = a.ExportID
				e.Archive, err = exportprotocol.NewArchive(a, recipe, envelope, metav1.Now())
				require.NoError(t, err)
				c.Status.SnapshotExportAuthority.LastSequence = sequence
				c.Status.SnapshotExportAuthority.Attempts = []appsv1.SnapshotExportAttempt{*a}
				c.Status.SnapshotExports = []appsv1.SnapshotExportStatus{e}
				require.NoError(t, admin.Status().Update(ctx, c))
			}
			_, err := r.setSnapshotExportPhase(ctx, c, a.ExportID, appsv1.SnapshotExportPhaseDeleting, "")
			require.NoError(t, err)
			a = refresh(t, c)
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			job := &batchv1.Job{}
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: exportprotocol.CleanupJobName(a.ID, 1)}, job))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: c.Namespace, Labels: job.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
			require.NoError(t, admin.Create(ctx, pod))
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "exporter", Image: recipe.Image, ImageID: "fixture", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
			require.NoError(t, admin.Status().Update(ctx, pod))
			job.Status.StartTime = ptr.To(metav1.Now())
			job.Status.CompletionTime = ptr.To(metav1.Now())
			job.Status.Succeeded = 1
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue}, {Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			require.NoError(t, admin.Status().Update(ctx, job))
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			a = refresh(t, c)
			require.True(t, a.CleanupComplete)
			require.Equal(t, appsv1.SnapshotExportPhaseDeleted, c.Status.SnapshotExports[0].Phase)
			require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
			require.NoError(t, r.cleanupNativeAttempt(ctx, c, a, recipe))
			require.NoError(t, r.compactNativeAttempts(ctx, c))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.Empty(t, c.Status.SnapshotExports)
			require.Empty(t, c.Status.SnapshotExportAuthority.Attempts)
			require.Equal(t, sequence, c.Status.SnapshotExportAuthority.LastSequence)
		}
	})
	t.Run("real manager advances pods and image despite persistent native failures", func(t *testing.T) {
		originalTransport := http.DefaultTransport
		nodeUtilsTransport := nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Hostname(), ".svc.cluster.local") {
				return nil, fmt.Errorf("envtest has no executing node-utils service")
			}
			return originalTransport.RoundTrip(req)
		})
		http.DefaultTransport = &nodeUtilsTransport
		t.Cleanup(func() { http.DefaultTransport = originalTransport })
		require.NoError(t, gwapiv1.Install(r.Scheme))
		require.NoError(t, gwapiv1a2.Install(r.Scheme))
		var retentionFaults atomic.Int32
		retentionPath := ""
		managerConfig := rest.CopyConfig(r.RestConfig)
		managerConfig.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
			return nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodDelete && req.URL.Path == retentionPath {
					retentionFaults.Add(1)
					return nil, fmt.Errorf("test native retention transport unavailable")
				}
				return base.RoundTrip(req)
			})
		}
		mgr, err := ctrl.NewManager(managerConfig, ctrl.Options{Scheme: r.Scheme, Logger: testr.NewWithOptions(t, testr.Options{Verbosity: -1}), Metrics: metricsserver.Options{BindAddress: "0"}, Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{template.Namespace: {}}}, Controller: controllerconfig.Controller{SkipNameValidation: ptr.To(true)}})
		require.NoError(t, err)
		clients, err := kubernetes.NewForConfig(r.RestConfig)
		require.NoError(t, err)
		managed, err := New(mgr, clients, &controllers.ControllerRunOptions{WorkerName: "native-availability", WorkerCount: 1, DisableWebhooks: true})
		require.NoError(t, err)
		t.Cleanup(func() { managed.configCache.Stop(); managed.nodeClients.Stop() })
		nodes := []*appsv1.ChainNode{}
		var deferredAttempt *appsv1.SnapshotExportAttempt
		var readFaults *atomic.Int32
		var retentionSource *snapshotv1.VolumeSnapshot
		for _, mode := range []string{"cleanup", "capacity", "read", "retention"} {
			capacity := mode == "capacity"
			readDeferred := mode == "read"
			retention := mode == "retention"
			var deferredSource *snapshotv1.VolumeSnapshot
			var deferredContent *snapshotv1.VolumeSnapshotContent
			if readDeferred || retention {
				deferredSource, deferredContent = nativeReviewSource(t, admin, source, original.Driver)
			}
			phase := "Succeeded"
			if capacity || retention {
				phase = "Invalidated"
			}
			if readDeferred {
				phase = "Reserved"
			}
			node, a, recipe := seed(t, template.Namespace, phase, func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
				if retention {
					rec, err := exportprotocol.DecodeRecipe(a)
					require.NoError(t, err)
					rec.Snapshot, rec.Content = identity(deferredSource), identity(deferredContent)
					a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
					require.NoError(t, err)
					e.SnapshotName, e.SnapshotUID = deferredSource.Name, deferredSource.UID
					e.Phase = appsv1.SnapshotExportPhaseCleanupRequired
					a.RemoteCleanupState, e.RemoteCleanupState = "Required", "Required"
					return
				}
				if readDeferred {
					rec, err := exportprotocol.DecodeRecipe(a)
					require.NoError(t, err)
					rec.Snapshot, rec.Content = identity(deferredSource), identity(deferredContent)
					a.Recipe, a.RecipeSHA256, err = exportprotocol.EncodeRecipe(rec)
					require.NoError(t, err)
					a.Job, a.ScratchPVC, a.ScratchPV, a.Pod = nil, nil, nil, nil
					a.JobCreateIssued, a.PVCCreateIssued = false, false
					a.JobCreateIssuer, a.PVCCreateIssuer, a.SourceHoldResourceVersion = "", "", ""
					e.SnapshotName, e.SnapshotUID = deferredSource.Name, deferredSource.UID
					return
				}
				a.RemoteCleanupState = "Required"
				e.RemoteCleanupState = "Required"
				if capacity {
					e.Phase = appsv1.SnapshotExportPhaseCleanupRequired
				} else {
					a.CleanupAttempt = exportprotocol.MaxCleanupAttempts
					a.CleanupIssuer = string(uuid.NewUUID())
					a.CleanupIssuedAt = ptr.To(metav1.Now())
					a.CleanupState = "Failed"
					a.CleanupReason = "CleanupJobFailed"
					e.Phase = appsv1.SnapshotExportPhaseDeleting
				}
			})
			if readDeferred {
				deferredAttempt = a.DeepCopy()
				managed.APIReader, readFaults = nativeFaultReader(t, r.RestConfig, "/secrets/"+recipe.Credentials.Name, 503, 0)
				deferredSource.Labels = map[string]string{controllers.LabelChainNode: node.Name}
				require.NoError(t, admin.Update(ctx, deferredSource))
			}
			if retention {
				retentionSource = deferredSource
				retentionPath = "/apis/" + snapshotv1.SchemeGroupVersion.String() + "/namespaces/" + node.Namespace + "/volumesnapshots/" + deferredSource.Name
				deferredSource.Labels = map[string]string{controllers.LabelChainNode: node.Name}
				deferredSource.Annotations[controllers.AnnotationSnapshotRetention] = "1ns"
				require.NoError(t, admin.Update(ctx, deferredSource))
				require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: node.Namespace, Name: recipe.Credentials.Name}}))
			}
			if capacity {
				for i := int64(2); i <= 32; i++ {
					copy := a.DeepCopy()
					copy.Sequence = i
					copy.ID = string(uuid.NewUUID())
					copy.ExportID = "export-" + copy.ID
					rec := recipe
					rec.ExportID = copy.ExportID
					rec.ResourceName = copy.ExportID
					rec.ObjectName = copy.ExportID
					text, digest, err := exportprotocol.EncodeRecipe(rec)
					require.NoError(t, err)
					copy.Recipe = text
					copy.RecipeSHA256 = digest
					node.Status.SnapshotExportAuthority.Attempts = append(node.Status.SnapshotExportAuthority.Attempts, *copy)
					e := node.Status.SnapshotExports[0]
					e.ID = copy.ExportID
					e.ObjectName = copy.ExportID
					node.Status.SnapshotExports = append(node.Status.SnapshotExports, e)
				}
				node.Status.SnapshotExportAuthority.LastSequence = 32
			}
			node.Status.PvcSize = "1Gi"
			node.Status.LatestHeight = 1
			node.SetEstablishedChainID("availability-chain")
			require.NoError(t, admin.Status().Update(ctx, node))
			node.Labels = map[string]string{controllers.LabelWorkerName: "native-availability"}
			node.Spec.Genesis = &appsv1.GenesisConfig{ConfigMap: ptr.To("availability-genesis")}
			node.Spec.OverrideImage = ptr.To("application:availability-v1")
			node.Spec.Config = &appsv1.Config{ReconcilePeriod: ptr.To("1s"), TerminationGracePeriodSeconds: ptr.To[int64](0)}
			if retention {
				node.Spec.Persistence.Snapshots.PreserveLastSnapshot = ptr.To(false)
			}
			if capacity {
				node.Spec.Persistence.Snapshots.StopNode = ptr.To(true)
				node.Annotations = map[string]string{controllers.AnnotationStopNodeSnapshotHeight: "1", controllers.AnnotationPvcSnapshotInProgress: "true"}
			}
			require.NoError(t, admin.Update(ctx, node))
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Annotations: map[string]string{controllers.AnnotationDataInitialized: "true", controllers.AnnotationDataHeight: "1"}}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(recipe.StorageClass), AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
			require.NoError(t, admin.Create(ctx, pvc))
			if capacity {
				snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-ready", Namespace: node.Namespace, Labels: map[string]string{controllers.LabelChainNode: node.Name}, Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "false", controllers.AnnotationDataHeight: "1"}}, Spec: snapshotv1.VolumeSnapshotSpec{Source: snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: ptr.To(pvc.Name)}}}
				require.NoError(t, admin.Create(ctx, snapshot))
				content := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-content"}, Spec: snapshotv1.VolumeSnapshotContentSpec{DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete, Driver: recipe.Driver, Source: snapshotv1.VolumeSnapshotContentSource{SnapshotHandle: ptr.To("manager")}, VolumeSnapshotRef: corev1.ObjectReference{Name: snapshot.Name, Namespace: snapshot.Namespace, UID: snapshot.UID}}}
				require.NoError(t, admin.Create(ctx, content))
				content.Status = &snapshotv1.VolumeSnapshotContentStatus{ReadyToUse: ptr.To(true), SnapshotHandle: ptr.To("manager")}
				require.NoError(t, admin.Status().Update(ctx, content))
				snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true), BoundVolumeSnapshotContentName: ptr.To(content.Name), RestoreSize: ptr.To(resource.MustParse("1Gi"))}
				require.NoError(t, admin.Status().Update(ctx, snapshot))
			}
			for _, image := range []string{"application:availability-v1", "application:availability-v2"} {
				copy := node.DeepCopy()
				copy.Spec.OverrideImage = ptr.To(image)
				key, err := configGenerationCacheKey(copy)
				require.NoError(t, err)
				// Envtest cannot execute the config generator; fixture TOML inputs feed the existing cache.
				appConfig, err := utils.TomlDecode("minimum-gas-prices = \"0stake\"\n")
				require.NoError(t, err)
				nodeConfig, err := utils.TomlDecode("moniker = \"fixture\"\n[p2p]\nladdr = \"tcp://0.0.0.0:26656\"\n")
				require.NoError(t, err)
				managed.storeConfigsInCache(key, map[string]interface{}{"app.toml": appConfig, "config.toml": nodeConfig})
			}
			nodes = append(nodes, node)
		}
		managerCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		kubeletDone := make(chan struct{})
		t.Cleanup(func() {
			cancel()
			select {
			case <-kubeletDone:
			case <-time.After(5 * time.Second):
				t.Error("kubelet fixture did not join")
			}
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(15 * time.Second):
				t.Error("manager did not join")
			}
		})
		go func() {
			defer close(kubeletDone)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-managerCtx.Done():
					return
				case <-ticker.C:
				}
				pods := &corev1.PodList{}
				if admin.List(managerCtx, pods, client.InNamespace(template.Namespace)) != nil {
					continue
				}
				for i := range pods.Items {
					pod := &pods.Items[i]
					owned := false
					for _, node := range nodes {
						if pod.Name == node.Name {
							owned = true
							break
						}
					}
					if !owned || len(pod.Status.ContainerStatuses) > 0 || !pod.DeletionTimestamp.IsZero() {
						continue
					}
					pod.Status.Phase = corev1.PodRunning
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Image: pod.Spec.Containers[0].Image, ImageID: "fixture", Started: ptr.To(true), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}
					_ = admin.Status().Update(managerCtx, pod)
				}
			}
		}()
		go func() { done <- mgr.Start(managerCtx) }()
		require.Eventually(t, func() bool { return retentionFaults.Load() > 0 }, 15*time.Second, 100*time.Millisecond, "retention DELETE fault must be reached")

		for i, node := range nodes {
			kind := appsv1.ConditionSnapshotExportDegraded
			wantReason := "RecoveryDeferred"
			if i == 0 {
				wantReason = "CleanupFailed"
			}
			if i == 1 {
				kind = appsv1.ConditionSnapshotExportCapacityExceeded
				wantReason = "AttemptLimit"
			}
			key := client.ObjectKeyFromObject(node)
			observeCondition := func() {
				require.EventuallyWithT(t, func(check *assert.CollectT) {
					if !assert.NoError(check, admin.Get(ctx, key, node)) {
						return
					}
					assert.True(check, apiMeta.IsStatusConditionTrue(node.Status.Conditions, kind), "mode=%d conditions=%+v", i, node.Status.Conditions)
					if condition := apiMeta.FindStatusCondition(node.Status.Conditions, kind); condition != nil {
						assert.Equal(check, wantReason, condition.Reason)
					}
				}, 5*time.Second, 50*time.Millisecond)
			}
			pod := &corev1.Pod{}
			require.EventuallyWithT(t, func(check *assert.CollectT) {
				err := admin.Get(ctx, key, pod)
				if err != nil {
					current := &appsv1.ChainNode{}
					_ = admin.Get(ctx, key, current)
					events := &corev1.EventList{}
					_ = admin.List(ctx, events, client.InNamespace(node.Namespace))
					messages := []string{}
					for _, e := range events.Items {
						if e.InvolvedObject.Name == node.Name {
							messages = append(messages, e.Reason+": "+e.Message)
						}
					}
					assert.NoError(check, err, "phase=%s conditions=%+v events=%v", current.Status.Phase, current.Status.Conditions, messages)
				}
			}, 20*time.Second, 100*time.Millisecond)
			firstUID := pod.UID
			cm := &corev1.ConfigMap{}
			require.NoError(t, admin.Get(ctx, key, cm))
			require.NotEmpty(t, cm.Data["config.toml"], "config=%+v", cm)
			observeCondition()
			if i == 2 {
				require.Positive(t, readFaults.Load())
				require.Equal(t, deferredAttempt, exportprotocol.FindAttempt(node, deferredAttempt.Sequence, deferredAttempt.ID))
				require.Nil(t, node.Status.SnapshotExports[0].Archive)
			}
			if i == 3 {
				require.Positive(t, retentionFaults.Load())
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(retentionSource), retentionSource))
				require.Equal(t, "Invalidated", node.Status.SnapshotExportAuthority.Attempts[0].Phase)
				require.Equal(t, "Required", node.Status.SnapshotExports[0].RemoteCleanupState)
			}
			if i == 1 {
				require.Empty(t, node.Annotations[controllers.AnnotationStopNodeSnapshotHeight])
				require.Equal(t, "false", node.Annotations[controllers.AnnotationPvcSnapshotInProgress])
			}
			require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := admin.Get(ctx, key, node); err != nil {
					return err
				}
				node.Spec.OverrideImage = ptr.To("application:availability-v2")
				return admin.Update(ctx, node)
			}))
			require.Eventually(t, func() bool {
				if admin.Get(ctx, key, pod) != nil || pod.UID == firstUID {
					return false
				}
				for _, container := range pod.Spec.Containers {
					if container.Name == node.Spec.App.App {
						return container.Image == "application:availability-v2"
					}
				}
				return false
			}, 25*time.Second, 100*time.Millisecond)
			observeCondition()
			if i == 2 {
				require.Positive(t, readFaults.Load())
				require.Equal(t, deferredAttempt, exportprotocol.FindAttempt(node, deferredAttempt.Sequence, deferredAttempt.ID))
				require.Nil(t, node.Status.SnapshotExports[0].Archive)
			}
			if i == 1 {
				require.Len(t, node.Status.SnapshotExports, 32)
				for _, e := range node.Status.SnapshotExports {
					require.Equal(t, "Required", e.RemoteCleanupState)
					require.Equal(t, appsv1.SnapshotExportPhaseCleanupRequired, e.Phase)
				}
			}
		}
		held := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{GenerateName: "watch-held-", Namespace: template.Namespace, Finalizers: []string{exportprotocol.SourceFinalizer}, Annotations: map[string]string{exportprotocol.HoldAnnotation: fmt.Sprintf(`{"orphan":"%s/missing-holder/old-uid/1/orphan"}`, template.Namespace)}}, Spec: *source.Spec.DeepCopy()}
		require.NoError(t, admin.Create(ctx, held))
		require.Eventually(t, func() bool {
			return admin.Get(ctx, client.ObjectKeyFromObject(held), held) == nil && !controllerutil.ContainsFinalizer(held, exportprotocol.SourceFinalizer) && held.Annotations[exportprotocol.HoldAnnotation] == ""
		}, 10*time.Second, 100*time.Millisecond, "holder-only snapshot events must route orphan recovery")
	})

}

func nativeReviewSource(t *testing.T, admin client.Client, prototype *snapshotv1.VolumeSnapshot, driver string) (*snapshotv1.VolumeSnapshot, *snapshotv1.VolumeSnapshotContent) {
	t.Helper()
	ctx := t.Context()
	s := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{GenerateName: "read-recovery-", Namespace: prototype.Namespace, Annotations: map[string]string{controllers.AnnotationPvcSnapshotReady: "true"}}, Spec: *prototype.Spec.DeepCopy()}
	require.NoError(t, admin.Create(ctx, s))
	content := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{GenerateName: "read-recovery-"}, Spec: snapshotv1.VolumeSnapshotContentSpec{DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete, Driver: driver, Source: snapshotv1.VolumeSnapshotContentSource{SnapshotHandle: ptr.To("recovery")}, VolumeSnapshotRef: corev1.ObjectReference{Name: s.Name, Namespace: s.Namespace, UID: s.UID}}}
	require.NoError(t, admin.Create(ctx, content))
	content.Status = &snapshotv1.VolumeSnapshotContentStatus{ReadyToUse: ptr.To(true), SnapshotHandle: ptr.To("recovery")}
	require.NoError(t, admin.Status().Update(ctx, content))
	s.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true), BoundVolumeSnapshotContentName: ptr.To(content.Name), RestoreSize: ptr.To(resource.MustParse("1Gi"))}
	require.NoError(t, admin.Status().Update(ctx, s))
	return s, content
}
