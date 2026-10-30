package integration

import (
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

var _ = Describe("Native export attempt recovery", func() {
	It("invalidates disabled authority before unfinished data initialization returns", WithNamespace(func(ns *corev1.Namespace) {
		ctx := Framework().Context()
		c := Framework().Client()
		node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSpec{App: DefaultChainNodeTestApp, Genesis: &appsv1.GenesisConfig{ConfigMap: ptr.To("unavailable-genesis")}, Persistence: &appsv1.Persistence{Restore: &appsv1.SnapshotRestoreConfig{Snapshot: appsv1.SnapshotRestoreSource{Provider: "s3", Bucket: "backups", Name: "unavailable.tar.gz"}}}}}
		Expect(c.Create(ctx, node)).To(Succeed())
		Eventually(func() error {
			return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: node.Name + "-init-data"}, &corev1.Pod{})
		}).Should(Succeed())
		source := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "policy-source", Namespace: ns.Name}, Spec: snapshotv1.VolumeSnapshotSpec{Source: snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: ptr.To(node.Name)}}}
		Expect(c.Create(ctx, source)).To(Succeed())
		id := string(uuid.NewUUID())
		ref := func(name string) appsv1.ExportResourceIdentity {
			return appsv1.ExportResourceIdentity{Name: name, UID: types.UID(name + "-uid")}
		}
		recipe := exportprotocol.Recipe{Version: 1, Namespace: ns.Name, Owner: appsv1.ExportResourceIdentity{Name: node.Name, UID: node.UID}, ExportID: "export-" + id, Snapshot: appsv1.ExportResourceIdentity{Name: source.Name, UID: source.UID}, Content: ref("missing-content"), Driver: "hostpath.csi.k8s.io", RestoreSize: "1Gi", StorageClass: "csi-hostpath-sc", ResourceName: "export-" + id, ObjectName: "export-" + id, Destination: appsv1.SnapshotExportDestination{Provider: appsv1.SnapshotExportProviderS3, Bucket: "backups", Region: "us-east-1", CredentialsSecret: &appsv1.SnapshotExportSecretReference{Name: "credentials"}}, Credentials: ref("credentials"), ServiceAccount: ref("export-" + string(node.UID)), Compression: dataexporter.CompressionGzip, SizeLimit: dataexporter.DefaultSizeLimit, PartSize: dataexporter.DefaultPartSize, ChunkSize: dataexporter.DefaultS3ChunkSize, BufferSize: dataexporter.DefaultBufferSize, ConcurrentJobs: 1, Image: "exporter:test"}
		encoded, digest, err := exportprotocol.EncodeRecipe(recipe)
		Expect(err).NotTo(HaveOccurred())
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
				return err
			}
			controllerutil.AddFinalizer(node, exportprotocol.AuthorityFinalizer)
			return c.Update(ctx, node)
		})).To(Succeed())
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
				return err
			}
			node.Status.SnapshotExportAuthority = &appsv1.SnapshotExportAuthority{Version: 1, LastSequence: 1, Attempts: []appsv1.SnapshotExportAttempt{{Sequence: 1, ID: id, ExportID: recipe.ExportID, Recipe: encoded, RecipeSHA256: digest, Phase: "Reserved", CreatedAt: metav1.Now()}}}
			node.Status.SnapshotExports = []appsv1.SnapshotExportStatus{{ID: recipe.ExportID, SnapshotName: recipe.Snapshot.Name, SnapshotUID: recipe.Snapshot.UID, ObjectName: recipe.ObjectName, Destination: recipe.Destination, Phase: appsv1.SnapshotExportPhaseUploading, ProvenanceVersion: 1}}
			return c.Status().Update(ctx, node)
		})).To(Succeed())
		requestRestoreReconcile(node)
		Eventually(func() string {
			if c.Get(ctx, client.ObjectKeyFromObject(node), node) != nil || len(node.Status.SnapshotExports) != 1 {
				return ""
			}
			return node.Status.SnapshotExports[0].Message
		}, 30*time.Second, time.Second).Should(Equal("PolicyOrOwnerChanged"))
		Expect(node.Status.SnapshotExports[0].Archive).To(BeNil())
		Expect(node.Status.SnapshotExportAuthority.LastSequence).To(Equal(int64(1)))
		initPod := &corev1.Pod{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: node.Name + "-init-data"}, initPod)).To(Succeed())
		Expect(initPod.Status.Phase).NotTo(Equal(corev1.PodSucceeded))
	}))
})
