package chainnode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/datasnapshot"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
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
	t.Run("credential loss before cleanup issuance retains deletion obligation", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", nil)
		before := a.DeepCopy()
		archive := c.Status.SnapshotExports[0].Archive.DeepCopy()
		postCount := posts.Load()
		require.NoError(t, r.retainNativeSnapshot(ctx, c, source, &c.Status.SnapshotExports[0]))
		a = refresh(t, c)
		require.Equal(t, appsv1.SnapshotExportPhaseDeleting, c.Status.SnapshotExports[0].Phase)
		require.NoError(t, admin.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: recipe.Namespace, Name: recipe.Credentials.Name}}))
		err := r.reconcileNativeDeletion(ctx, c, a, recipe)
		require.Error(t, err)
		a = refresh(t, c)
		require.Equal(t, "Failed", a.CleanupState)
		require.Equal(t, "ExecutionIdentityUnavailable", a.CleanupReason)
		require.Equal(t, "Required", a.RemoteCleanupState)
		require.Equal(t, "Required", c.Status.SnapshotExports[0].RemoteCleanupState)
		require.Contains(t, c.Status.SnapshotExports[0].Message, "deletion remains unproven")
		require.True(t, apiMeta.IsStatusConditionTrue(c.Status.Conditions, appsv1.ConditionSnapshotExportCleanup))
		require.Empty(t, a.CleanupIssuer)
		require.Nil(t, a.CleanupIssuedAt)
		require.False(t, a.CleanupComplete)
		require.Equal(t, postCount, posts.Load())
		require.Equal(t, archive, c.Status.SnapshotExports[0].Archive)
		a.CleanupState, a.CleanupReason, a.RemoteCleanupState = before.CleanupState, before.CleanupReason, before.RemoteCleanupState
		require.Equal(t, before, a, "completed evidence and issuance witnesses must survive cleanup failure")
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
			require.Equal(t, tarballFailed, snapshot.Annotations[controllers.AnnotationExportingTarball])
			if acknowledged {
				c.Annotations = map[string]string{exportprotocol.CleanupAckAnnotation: a.ExportID}
				require.NoError(t, admin.Update(ctx, c))
				require.NoError(t, r.reconcileNativeAcknowledgements(ctx, c))
				a = refresh(t, c)
			}
			old := a.DeepCopy()
			delete(snapshot.Annotations, controllers.AnnotationExportingTarball)
			require.NoError(t, admin.Update(ctx, snapshot))
			require.NoError(t, r.recoverNativeExports(ctx, c))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot))
			require.Empty(t, snapshot.Annotations[controllers.AnnotationExportingTarball], "recovery precedes the normal retry loop")
			refresh(t, c)
			c.Status.LatestHeight, c.Status.PvcSize = 1, "1Gi"
			require.NoError(t, admin.Status().Update(ctx, c))
			clients, err := kubernetes.NewForConfig(r.RestConfig)
			require.NoError(t, err)
			local := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, RestConfig: r.RestConfig, opts: r.opts, recorder: record.NewFakeRecorder(100), ClientSet: clients}
			require.NoError(t, local.ensureVolumeSnapshots(ctx, c, false))
			require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(c), c))
			require.Len(t, c.Status.SnapshotExportAuthority.Attempts, 2)
			next := &c.Status.SnapshotExportAuthority.Attempts[1]
			require.Greater(t, next.Sequence, old.Sequence)
			require.NotEqual(t, old.ID, next.ID)
			nextRecipe, err := exportprotocol.DecodeRecipe(next)
			require.NoError(t, err)
			require.NotEqual(t, recipe.ObjectName, nextRecipe.ObjectName)
			require.Equal(t, next.ExportID, snapshotExportFor(c, snapshot).ID)
			require.Equal(t, old, exportprotocol.FindAttempt(c, old.Sequence, old.ID))
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
		local := &Reconciler{Client: raced, APIReader: r.APIReader, Scheme: r.Scheme, opts: r.opts}
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
	for _, resource := range []string{"secrets", "serviceaccounts"} {
		for _, issued := range []bool{false, true} {
			t.Run(fmt.Sprintf("cleanup read defers %s issued %v", resource, issued), func(t *testing.T) {
				c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
					e.Phase = appsv1.SnapshotExportPhaseDeleting
				})
				if issued {
					require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
					a = refresh(t, c)
				}
				name := recipe.Credentials.Name
				if resource == "serviceaccounts" {
					name = recipe.ServiceAccount.Name
				}
				reader, fired := nativeFaultReader(t, r.RestConfig, "/"+resource+"/"+name, 403, 0)
				local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, opts: r.opts}
				before := posts.Load()
				require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
				require.Positive(t, fired.Load())
				require.Equal(t, a, refresh(t, c))
				require.Equal(t, before, posts.Load())
				require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
				current := refresh(t, c)
				require.Empty(t, current.CleanupState)
				require.NotEmpty(t, current.CleanupIssuer)
				if issued {
					require.Equal(t, a.CleanupIssuer, current.CleanupIssuer)
					require.Equal(t, before, posts.Load())
				} else {
					require.Equal(t, before+1, posts.Load())
				}
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
		local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, opts: r.opts}
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
			local := &Reconciler{Client: r.Client, APIReader: reader, Scheme: r.Scheme, opts: r.opts}
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
	t.Run("rejected cleanup POST remains stopped without reissuance", func(t *testing.T) {
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
		local := &Reconciler{Client: denied, APIReader: r.APIReader, Scheme: r.Scheme, opts: r.opts}
		require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.EqualValues(t, 1, rejected.Load())
		require.Equal(t, "Failed", a.CleanupState)
		require.Equal(t, "CleanupCreateRejected", a.CleanupReason)
		require.Equal(t, "Required", a.RemoteCleanupState)
		require.NotEmpty(t, a.CleanupIssuer)
		require.NotNil(t, a.CleanupIssuedAt)
		require.Error(t, local.reconcileNativeDeletion(ctx, c, a, recipe))
		require.Equal(t, a, refresh(t, c))
		require.EqualValues(t, 1, rejected.Load())
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}, &batchv1.Job{})))
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
		require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}, job))
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
		_, _, _, gateErr := exportprotocol.AuthorizeCleanup(gated, gated.UID, a.Sequence, a.ID, "late-pod")
		require.Error(t, gateErr)
		gated.Status.SnapshotExportAuthority.Attempts[0].CleanupState = ""
		_, _, _, gateErr = exportprotocol.AuthorizeCleanup(gated, gated.UID, a.Sequence, a.ID, "late-pod")
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
		_, _, _, err = exportprotocol.AuthorizeCleanup(c, c.UID, a.Sequence, a.ID, "late-pod")
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
	t.Run("missing issued cleanup stops without reissuance", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
			a.CleanupIssuer = string(uuid.NewUUID())
			a.CleanupIssuedAt = ptr.To(metav1.NewTime(time.Now().Add(-3 * time.Minute)))
			a.RemoteCleanupState = "Required"
			e.RemoteCleanupState = "Required"
			e.Phase = appsv1.SnapshotExportPhaseDeleting
		})
		require.NoError(t, r.reconcileNativeDeletion(ctx, c, a, recipe))
		a = refresh(t, c)
		require.Equal(t, "Failed", a.CleanupState)
		require.Equal(t, "IssuedCleanupMissing", a.CleanupReason)
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}, &batchv1.Job{})))
		late := datasnapshot.NativeCleanupJob(a, recipe)
		require.NoError(t, admin.Create(ctx, late))
		err := r.recoverNativeExports(ctx, c)
		require.Error(t, err)
		handled, fatal := r.handleNativeOutcome(ctx, c, err)
		require.True(t, handled)
		require.NoError(t, fatal)
		require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKeyFromObject(late), &batchv1.Job{})))
		current := refresh(t, c)
		require.Equal(t, "Failed", current.CleanupState)
		require.Equal(t, a.CleanupIssuer, current.CleanupIssuer)
		require.Equal(t, a.CleanupIssuedAt, current.CleanupIssuedAt)
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
				if priorFailure {
					require.Equal(t, "Failed", a.CleanupState)
					require.Equal(t, "IssuedCleanupMissing", a.CleanupReason)
					require.Equal(t, "Required", a.RemoteCleanupState)
				} else {
					require.Equal(t, "Abandoned", a.CleanupState)
					require.Equal(t, "OwnerDeleting", a.CleanupReason)
					require.Equal(t, "Complete", a.RemoteCleanupState)
				}
				require.NotNil(t, c.Status.SnapshotExports[0].Archive)
				require.NotContains(t, c.Finalizers, exportprotocol.AuthorityFinalizer)
				require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}, &batchv1.Job{})))
			})
		}
	}
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminating namespace closes cleanup missing credentials %v", missing), func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "native-terminating-"}}
			require.NoError(t, admin.Create(ctx, ns))
			c, _, recipe := seed(t, ns.Name, "Succeeded", func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
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
			require.True(t, apierrors.IsNotFound(admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}, &batchv1.Job{})))
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
			if cleanup {
				job = datasnapshot.NativeCleanupJob(a, recipe)
				validate = datasnapshot.ValidateNativeCleanupPodSpec
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "limit-pod-", Namespace: c.Namespace}, Spec: *job.Spec.Template.Spec.DeepCopy()}
			require.NoError(t, admin.Create(ctx, pod))
			require.Contains(t, pod.Spec.Containers[0].Resources.Limits, corev1.ResourceCPU)
			require.NoError(t, validate(&pod.Spec, a, recipe))
			require.NoError(t, admin.Delete(ctx, pod))
		}
	})
	t.Run("twenty nine retained archives admit a thirtieth reservation", func(t *testing.T) {
		c, a, recipe := seed(t, template.Namespace, "Succeeded", nil)
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
			archive, err := exportprotocol.NewArchive(copy, rec, envelope, metav1.Now())
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
				require.Equal(t, tarballFailed, ownedSource.Annotations[controllers.AnnotationExportingTarball])
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
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "delete-" + a.ID}, job))
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
		http.DefaultTransport = nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Hostname(), ".svc.cluster.local") {
				return nil, fmt.Errorf("envtest has no executing node-utils service")
			}
			return originalTransport.RoundTrip(req)
		})
		t.Cleanup(func() { http.DefaultTransport = originalTransport })
		require.NoError(t, gwapiv1.Install(r.Scheme))
		require.NoError(t, gwapiv1a2.Install(r.Scheme))
		mgr, err := ctrl.NewManager(r.RestConfig, ctrl.Options{Scheme: r.Scheme, Metrics: metricsserver.Options{BindAddress: "0"}, Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{template.Namespace: {}}}})
		require.NoError(t, err)
		clients, err := kubernetes.NewForConfig(r.RestConfig)
		require.NoError(t, err)
		managed, err := New(mgr, clients, &controllers.ControllerRunOptions{WorkerName: "native-availability", WorkerCount: 1, DisableWebhooks: true})
		require.NoError(t, err)
		t.Cleanup(func() { managed.configCache.Stop(); managed.nodeClients.Stop() })
		nodes := []*appsv1.ChainNode{}
		var deferredAttempt *appsv1.SnapshotExportAttempt
		var readFaults *atomic.Int32
		for _, mode := range []string{"cleanup", "capacity", "read"} {
			capacity := mode == "capacity"
			readDeferred := mode == "read"
			var deferredSource *snapshotv1.VolumeSnapshot
			var deferredContent *snapshotv1.VolumeSnapshotContent
			if readDeferred {
				deferredSource, deferredContent = nativeReviewSource(t, admin, source, original.Driver)
			}
			phase := "Succeeded"
			if capacity {
				phase = "Invalidated"
			}
			if readDeferred {
				phase = "Reserved"
			}
			node, a, recipe := seed(t, template.Namespace, phase, func(a *appsv1.SnapshotExportAttempt, e *appsv1.SnapshotExportStatus) {
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
					a.CleanupState = "Acknowledged"
					e.Phase = appsv1.SnapshotExportPhaseAcknowledged
				} else {
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

		for i, node := range nodes {
			kind := appsv1.ConditionSnapshotExportDegraded
			if i == 1 {
				kind = appsv1.ConditionSnapshotExportCapacityExceeded
			}
			key := client.ObjectKeyFromObject(node)
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
			require.NoError(t, admin.Get(ctx, key, node))
			require.True(t, apiMeta.IsStatusConditionTrue(node.Status.Conditions, kind))
			if i == 2 {
				require.Positive(t, readFaults.Load())
				require.Equal(t, deferredAttempt, exportprotocol.FindAttempt(node, deferredAttempt.Sequence, deferredAttempt.ID))
				require.Nil(t, node.Status.SnapshotExports[0].Archive)
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
			require.NoError(t, admin.Get(ctx, key, node))
			require.True(t, apiMeta.IsStatusConditionTrue(node.Status.Conditions, kind))
			if i == 2 {
				require.Positive(t, readFaults.Load())
				require.Equal(t, deferredAttempt, exportprotocol.FindAttempt(node, deferredAttempt.Sequence, deferredAttempt.ID))
				require.Nil(t, node.Status.SnapshotExports[0].Archive)
			}
			if i == 1 {
				require.Len(t, node.Status.SnapshotExports, 32)
				for _, e := range node.Status.SnapshotExports {
					require.Equal(t, "Required", e.RemoteCleanupState)
					require.Equal(t, appsv1.SnapshotExportPhaseAcknowledged, e.Phase)
				}
			}
		}
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
