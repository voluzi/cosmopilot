package exportprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

const (
	AuthorityFinalizer   = "cosmopilot.voluzi.com/export-authority"
	SourceFinalizer      = "cosmopilot.voluzi.com/export-source"
	AttemptFinalizer     = "cosmopilot.voluzi.com/export-attempt"
	OwnerNameLabel       = "cosmopilot.voluzi.com/export-owner"
	OwnerUIDLabel        = "cosmopilot.voluzi.com/export-owner-uid"
	AttemptLabel         = "cosmopilot.voluzi.com/export-attempt-id"
	CleanupAckAnnotation = "cosmopilot.voluzi.com/native-export-cleanup-ack"
	HoldAnnotation       = "cosmopilot.voluzi.com/export-holders"
	DataPath             = "/home/app/data"
	ResultPath           = "/dev/termination-log"
	MaxEnvelope          = 3072
	MaxDocument          = 16384
	MaxNativeStatus      = 256 * 1024
)

type Recipe struct {
	Version          int                              `json:"version"`
	Namespace        string                           `json:"namespace"`
	Owner            appsv1.ExportResourceIdentity    `json:"owner"`
	ExportID         string                           `json:"exportID"`
	Snapshot         appsv1.ExportResourceIdentity    `json:"snapshot"`
	Content          appsv1.ExportResourceIdentity    `json:"content"`
	Driver           string                           `json:"driver"`
	RestoreSize      string                           `json:"restoreSize"`
	StorageClass     string                           `json:"storageClass"`
	ResourceName     string                           `json:"resourceName"`
	ObjectName       string                           `json:"objectName"`
	Destination      appsv1.SnapshotExportDestination `json:"destination"`
	Credentials      appsv1.ExportResourceIdentity    `json:"credentials"`
	ServiceAccount   appsv1.ExportResourceIdentity    `json:"serviceAccount"`
	Compression      dataexporter.Compression         `json:"compression"`
	SizeLimit        string                           `json:"sizeLimit"`
	PartSize         string                           `json:"partSize"`
	ChunkSize        string                           `json:"chunkSize"`
	BufferSize       string                           `json:"bufferSize"`
	ConcurrentJobs   int                              `json:"concurrentJobs"`
	Image            string                           `json:"image"`
	PriorityClass    string                           `json:"priorityClass"`
	ImagePullSecrets []corev1.LocalObjectReference    `json:"imagePullSecrets,omitempty"`
	Resources        corev1.ResourceRequirements      `json:"resources"`
	DeleteOnExpire   bool                             `json:"deleteOnExpire"`
	SourceHeight     string                           `json:"sourceHeight,omitempty"`
	SourceHeightKind string                           `json:"sourceHeightKind,omitempty"`
}

func (r Recipe) UploadOptions() []dataexporter.UploadOption {
	return []dataexporter.UploadOption{dataexporter.WithCompression(r.Compression), dataexporter.WithChunkSize(r.ChunkSize), dataexporter.WithPartSize(r.PartSize), dataexporter.WithSizeLimit(r.SizeLimit), dataexporter.WithBufferSize(r.BufferSize), dataexporter.WithConcurrentUploadJobs(r.ConcurrentJobs), dataexporter.WithHeight(r.SourceHeight)}
}

func (r Recipe) Validate() error {
	if r.Version != 1 || r.Namespace == "" || r.ExportID == "" || r.Driver == "" || r.StorageClass == "" || r.RestoreSize == "" || r.Image == "" || r.ResourceName == "" || r.ObjectName == "" {
		return fmt.Errorf("incomplete native recipe")
	}
	for _, ref := range []appsv1.ExportResourceIdentity{r.Owner, r.Snapshot, r.Content, r.Credentials, r.ServiceAccount} {
		if ref.Name == "" || !validUID(ref.UID) {
			return fmt.Errorf("incomplete resource identity")
		}
	}
	size, err := resource.ParseQuantity(r.RestoreSize)
	if err != nil || size.Sign() <= 0 {
		return fmt.Errorf("invalid native restore size")
	}
	if r.ServiceAccount.Name != "export-"+string(r.Owner.UID) {
		return fmt.Errorf("native worker requires dedicated identity")
	}
	if r.SourceHeight != "" {
		height, err := strconv.ParseInt(r.SourceHeight, 10, 64)
		if err != nil || height < 1 || r.SourceHeightKind != "CaptureObserved" {
			return fmt.Errorf("invalid observed source height")
		}
	} else if r.SourceHeightKind != "" {
		return fmt.Errorf("height kind without height")
	}
	d := r.Destination
	if d.Provider != appsv1.SnapshotExportProviderS3 || d.Bucket == "" || d.Region == "" || d.ServiceAccountName != "" || d.CredentialsSecret == nil || d.CredentialsSecret.Name != r.Credentials.Name {
		return fmt.Errorf("unsupported native destination")
	}
	c, err := dataexporter.ParseCompression(string(r.Compression))
	if err != nil || c != r.Compression {
		return fmt.Errorf("unsupported native compression")
	}
	return dataexporter.ValidateS3UploadOptions(r.UploadOptions()...)
}

func EncodeRecipe(r Recipe) (string, string, error) {
	if err := r.Validate(); err != nil {
		return "", "", err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", "", err
	}
	if len(b) > MaxDocument {
		return "", "", fmt.Errorf("native recipe exceeds budget")
	}
	return string(b), fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func DecodeRecipe(a *appsv1.SnapshotExportAttempt) (Recipe, error) {
	var r Recipe
	if a == nil || len(a.Recipe) > MaxDocument || fmt.Sprintf("%x", sha256.Sum256([]byte(a.Recipe))) != a.RecipeSHA256 {
		return r, fmt.Errorf("native recipe digest mismatch")
	}
	if err := strictJSON([]byte(a.Recipe), &r); err != nil {
		return r, err
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	encoded, digest, err := EncodeRecipe(r)
	if err != nil || encoded != a.Recipe || digest != a.RecipeSHA256 || r.ExportID != a.ExportID || r.ResourceName != "export-"+a.ID || r.ObjectName != r.ResourceName || r.ExportID != r.ResourceName || !validAttemptID(a.ID) {
		return r, fmt.Errorf("noncanonical native recipe")
	}
	return r, nil
}

type Envelope struct {
	Version         int                       `json:"version"`
	OwnerUID        types.UID                 `json:"ownerUID"`
	ExportID        string                    `json:"exportID"`
	AttemptID       string                    `json:"attemptID"`
	AttemptSequence int64                     `json:"attemptSequence"`
	RecipeSHA256    string                    `json:"recipeSHA256"`
	JobUID          types.UID                 `json:"jobUID"`
	PodUID          types.UID                 `json:"podUID"`
	ScratchPVCUID   types.UID                 `json:"scratchPVCUID"`
	ScratchPVUID    types.UID                 `json:"scratchPVUID"`
	Result          dataexporter.UploadResult `json:"result"`
}

func EnvelopeFor(a *appsv1.SnapshotExportAttempt, r Recipe) Envelope {
	e := Envelope{Version: 1, OwnerUID: r.Owner.UID, ExportID: a.ExportID, AttemptID: a.ID, AttemptSequence: a.Sequence, RecipeSHA256: a.RecipeSHA256}
	if a.Job != nil {
		e.JobUID = a.Job.UID
	}
	if a.Pod != nil {
		e.PodUID = a.Pod.UID
	}
	if a.ScratchPVC != nil {
		e.ScratchPVCUID = a.ScratchPVC.UID
	}
	if a.ScratchPV != nil {
		e.ScratchPVUID = a.ScratchPV.UID
	}
	return e
}
func (e Envelope) Validate() error {
	if e.Version != 1 || e.AttemptSequence < 1 || e.OwnerUID == "" || e.ExportID == "" || e.AttemptID == "" || e.JobUID == "" || e.PodUID == "" || e.ScratchPVCUID == "" || e.ScratchPVUID == "" || len(e.RecipeSHA256) != 64 {
		return fmt.Errorf("incomplete native envelope")
	}
	for _, c := range e.RecipeSHA256 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return fmt.Errorf("invalid recipe digest")
		}
	}
	for _, uid := range []types.UID{e.OwnerUID, e.JobUID, e.PodUID, e.ScratchPVCUID, e.ScratchPVUID} {
		if !validUID(uid) {
			return fmt.Errorf("invalid native resource UID")
		}
	}
	return e.Result.Validate()
}
func EncodeEnvelope(e Envelope) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxEnvelope {
		return nil, fmt.Errorf("native envelope exceeds 3072 bytes")
	}
	return b, nil
}
func DecodeEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	if len(b) > MaxEnvelope {
		return e, fmt.Errorf("native envelope exceeds 3072 bytes")
	}
	if err := strictJSON(b, &e); err != nil {
		return e, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	for _, k := range []string{"version", "ownerUID", "exportID", "attemptID", "attemptSequence", "recipeSHA256", "jobUID", "podUID", "scratchPVCUID", "scratchPVUID", "result"} {
		if _, ok := fields[k]; !ok {
			return e, fmt.Errorf("missing envelope field %s", k)
		}
	}
	if err := exactKeys(fields, "version", "ownerUID", "exportID", "attemptID", "attemptSequence", "recipeSHA256", "jobUID", "podUID", "scratchPVCUID", "scratchPVUID", "result"); err != nil {
		return e, err
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(fields["result"], &result)
	for _, k := range []string{"version", "layout", "objectCount", "compression", "cleanupRequired"} {
		if _, ok := result[k]; !ok {
			return e, fmt.Errorf("missing result field %s", k)
		}
	}
	if err := exactKeys(result, "version", "layout", "objectCount", "compression", "objectKey", "sha256", "sizeBytes", "cleanupRequired"); err != nil {
		return e, err
	}
	return e, e.Validate()
}
func ValidateEnvelope(e Envelope, a *appsv1.SnapshotExportAttempt, r Recipe) error {
	expected := EnvelopeFor(a, r)
	expected.Result = e.Result
	if e != expected {
		return fmt.Errorf("native result identity mismatch")
	}
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Result.Compression != r.Compression || (e.Result.Layout == "single" && e.Result.ObjectKey != r.ObjectName+r.Compression.Extension()) {
		return fmt.Errorf("native result recipe mismatch")
	}
	return nil
}
func CheckEnvelopeBudget(a *appsv1.SnapshotExportAttempt, r Recipe) error {
	e := EnvelopeFor(a, r)
	e.JobUID = types.UID(strings.Repeat("j", 128))
	e.PodUID = types.UID(strings.Repeat("p", 128))
	e.ScratchPVCUID = types.UID(strings.Repeat("c", 128))
	e.ScratchPVUID = types.UID(strings.Repeat("v", 128))
	e.Result = dataexporter.UploadResult{Version: 1, Layout: "single", ObjectCount: 1, Compression: r.Compression, ObjectKey: r.ObjectName + r.Compression.Extension(), SHA256: strings.Repeat("f", 64), SizeBytes: math.MaxInt64, CleanupRequired: false}
	if _, err := EncodeEnvelope(e); err != nil {
		return err
	}
	prototype := a.DeepCopy()
	prototype.SourceHoldResourceVersion = strings.Repeat("1", 128)
	ref := func(name string, uid types.UID) *appsv1.ExportResourceIdentity {
		return &appsv1.ExportResourceIdentity{Name: name, UID: uid}
	}
	prototype.Job = ref(r.ResourceName, e.JobUID)
	prototype.ScratchPVC = ref(r.ResourceName, e.ScratchPVCUID)
	prototype.Pod = ref(strings.Repeat("p", 253), e.PodUID)
	prototype.ScratchPV = ref(strings.Repeat("v", 253), e.ScratchPVUID)
	_, err := NewArchive(prototype, r, e, metav1.Now())
	return err
}
func Terminal(a *appsv1.SnapshotExportAttempt) bool {
	return a.Phase == "Succeeded" || a.Phase == "Invalidated"
}
func FindAttempt(c *appsv1.ChainNode, seq int64, id string) *appsv1.SnapshotExportAttempt {
	if c.Status.SnapshotExportAuthority == nil || c.Status.SnapshotExportAuthority.Version != 1 {
		return nil
	}
	for i := range c.Status.SnapshotExportAuthority.Attempts {
		a := &c.Status.SnapshotExportAuthority.Attempts[i]
		if a.Sequence == seq && a.ID == id {
			return a
		}
	}
	return nil
}
func Authorize(c *appsv1.ChainNode, uid types.UID, seq int64, id string, pod types.UID) (*appsv1.SnapshotExportAttempt, Recipe, error) {
	a := FindAttempt(c, seq, id)
	var r Recipe
	if c.UID != uid || !c.DeletionTimestamp.IsZero() || a == nil || a.Phase != "Uploading" || a.Pod == nil || a.Pod.UID != pod || a.Job == nil || a.ScratchPVC == nil || a.ScratchPV == nil || a.SourceHoldResourceVersion == "" {
		return nil, r, fmt.Errorf("native authority denied")
	}
	r, err := DecodeRecipe(a)
	if err != nil {
		return nil, r, err
	}
	if r.Owner.UID != uid || r.Owner.Name != c.Name || r.Namespace != c.Namespace {
		return nil, r, fmt.Errorf("native authority owner mismatch")
	}
	return a, r, nil
}

func strictJSON(b []byte, out any) error {
	if !utf8.Valid(b) {
		return fmt.Errorf("invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := checkValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	dec = json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
func checkValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("null is not permitted")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			k, ok := key.(string)
			if !ok || seen[k] {
				return fmt.Errorf("duplicate or invalid JSON key")
			}
			seen[k] = true
			if err := checkValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkValue(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func AuthorizeCleanup(c *appsv1.ChainNode, uid types.UID, seq int64, id string, pod types.UID) (*appsv1.SnapshotExportAttempt, Recipe, Envelope, error) {
	a := FindAttempt(c, seq, id)
	var r Recipe
	var e Envelope
	if c.UID != uid || !c.DeletionTimestamp.IsZero() || a == nil || a.CleanupState != "" || a.Phase != "Succeeded" || a.CleanupComplete || a.CleanupIssuer == "" || a.CleanupJob == nil || a.CleanupPod == nil || a.CleanupPod.UID != pod {
		return nil, r, e, fmt.Errorf("native cleanup authority denied")
	}
	r, err := DecodeRecipe(a)
	if err != nil {
		return nil, r, e, err
	}
	if r.Owner.UID != uid || r.Owner.Name != c.Name || r.Namespace != c.Namespace {
		return nil, r, e, fmt.Errorf("native cleanup owner mismatch")
	}
	found := false
	for _, record := range c.Status.SnapshotExports {
		if record.ID == a.ExportID && record.ProvenanceVersion == 1 && record.Phase == appsv1.SnapshotExportPhaseDeleting {
			found = true
		}
	}
	if !found {
		return nil, r, e, fmt.Errorf("native cleanup catalog authority denied")
	}
	e, err = DecodeEnvelope([]byte(a.CompletedEnvelope))
	if err != nil {
		return nil, r, e, err
	}
	if err := ValidateEnvelope(e, a, r); err != nil {
		return nil, r, e, err
	}
	return a, r, e, nil
}

func validAttemptID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func validUID(uid types.UID) bool {
	if len(uid) == 0 || len(uid) > 128 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func NewArchive(a *appsv1.SnapshotExportAttempt, r Recipe, e Envelope, completedAt metav1.Time) (*appsv1.SnapshotExportArchive, error) {
	if err := ValidateEnvelope(e, a, r); err != nil {
		return nil, err
	}
	if e.Result.Layout != "single" {
		return nil, nil
	}
	d := &appsv1.SnapshotExportArchive{
		Version: 1, Namespace: r.Namespace, AuthorityOwner: r.Owner, ExportID: a.ExportID, AttemptID: a.ID, AttemptSequence: a.Sequence, Recipe: a.Recipe, RecipeSHA256: a.RecipeSHA256,
		ObjectKey: e.Result.ObjectKey, SHA256: e.Result.SHA256, SizeBytes: e.Result.SizeBytes, Compression: appsv1.TarballCompression(r.Compression), SourceHoldResourceVersion: a.SourceHoldResourceVersion,
		Job: *a.Job, Pod: *a.Pod, ScratchPVC: *a.ScratchPVC, ScratchPV: *a.ScratchPV, CompletedAt: completedAt,
		SourceSnapshot: r.Snapshot, SourceContent: r.Content, CSIDriver: r.Driver, Destination: r.Destination,
		ReadIdentity: appsv1.SnapshotExportReadIdentity{Mode: "Secret", Namespace: r.Namespace, CredentialsSecret: r.Credentials, KeySet: "AWS_ACCESS_KEY_ID+AWS_SECRET_ACCESS_KEY+AWS_SESSION_TOKEN", ExecutionServiceAccount: r.ServiceAccount},
		SourceHeight: r.SourceHeight, SourceHeightKind: r.SourceHeightKind,
	}
	data, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxDocument {
		return nil, fmt.Errorf("native descriptor exceeds 16KiB")
	}
	return d, nil
}

func exactKeys(fields map[string]json.RawMessage, allowed ...string) error {
	for key := range fields {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown JSON key %s", key)
		}
	}
	return nil
}
