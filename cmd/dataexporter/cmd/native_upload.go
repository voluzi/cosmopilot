package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/exportprotocol"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

func init() { rootCmd.AddCommand(newNativeUploadCmd(), newNativeCleanupCmd()) }

func newNativeUploadCmd() *cobra.Command  { return newNativeCommand(false) }
func newNativeCleanupCmd() *cobra.Command { return newNativeCommand(true) }
func newNativeCommand(cleanup bool) *cobra.Command {
	var namespace, owner, uid, id, pod, kubeconfig string
	var sequence int64
	c := &cobra.Command{Use: "native-upload", Short: "Export an authorized mounted snapshot clone", Args: cobra.NoArgs}
	if cleanup {
		c.Use = "native-cleanup"
		c.Short = "Delete acknowledged native archive output"
	}
	c.Flags().StringVar(&namespace, "namespace", "", "Authority namespace")
	c.Flags().StringVar(&owner, "owner", "", "Authority ChainNode name")
	c.Flags().StringVar(&uid, "owner-uid", "", "Authority ChainNode UID")
	c.Flags().StringVar(&id, "attempt-id", "", "Attempt UUID")
	c.Flags().Int64Var(&sequence, "attempt-sequence", 0, "Attempt sequence")
	c.Flags().StringVar(&pod, "pod-uid", os.Getenv("EXPORT_POD_UID"), "Mounted worker Pod UID")
	c.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Kubernetes client configuration")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		if namespace == "" || owner == "" || uid == "" || id == "" || sequence < 1 || pod == "" {
			return fmt.Errorf("native authority invocation is incomplete")
		}
		var cfg *rest.Config
		var err error
		if kubeconfig != "" {
			cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		} else {
			cfg, err = rest.InClusterConfig()
		}
		if err != nil {
			return fmt.Errorf("native authority client: %w", err)
		}
		cfg.Timeout = 10 * time.Second
		scheme := runtime.NewScheme()
		if err := appsv1.AddToScheme(scheme); err != nil {
			return err
		}
		reader, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
		defer cancel()
		var attempt *appsv1.SnapshotExportAttempt
		var recipe exportprotocol.Recipe
		var completed exportprotocol.Envelope
		for {
			authority := &appsv1.ChainNode{}
			if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: owner}, authority); err != nil {
				return fmt.Errorf("native authority GET denied: %w", err)
			}
			if authority.UID != types.UID(uid) || !authority.DeletionTimestamp.IsZero() {
				return fmt.Errorf("native authority denied")
			}
			a := exportprotocol.FindAttempt(authority, sequence, id)
			if cleanup && a != nil && a.Phase == "Succeeded" && a.CleanupJob != nil && a.CleanupPod != nil {
				attempt, recipe, completed, err = exportprotocol.AuthorizeCleanup(authority, types.UID(uid), sequence, id, types.UID(pod))
				if err != nil {
					return err
				}
				break
			}
			if a == nil || (cleanup && (a.CleanupState != "" || a.CleanupComplete)) || (!cleanup && exportprotocol.Terminal(a)) || (cleanup && a.Phase != "Succeeded") {
				return fmt.Errorf("native authority denied")
			}
			if !cleanup && a.Phase == "Uploading" {
				attempt, recipe, err = exportprotocol.Authorize(authority, types.UID(uid), sequence, id, types.UID(pod))
				if err != nil {
					return err
				}
				break
			}
			if !cleanup && a.Phase != "Reserved" && a.Phase != "Held" && a.Phase != "ResourcesRequested" {
				return fmt.Errorf("unsupported native authority phase")
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("native authority deadline: %w", ctx.Err())
			case <-timer.C:
			}
		}
		if err := exportprotocol.CheckEnvelopeBudget(attempt, recipe); err != nil {
			return err
		}
		credentials := &aws.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN")}
		exporter, err := dataexporter.NewS3Exporter(cmd.Context(), dataexporter.S3Config{Region: recipe.Destination.Region, Endpoint: recipe.Destination.Endpoint, ForcePathStyle: recipe.Destination.ForcePathStyle, ExplicitCredentials: credentials})
		if err != nil {
			return err
		}
		if cleanup {
			if completed.Result.Layout == "single" {
				return exporter.DeleteExact(cmd.Context(), recipe.Destination.Bucket, completed.Result.ObjectKey)
			}
			return exporter.DeleteResult(cmd.Context(), recipe.Destination.Bucket, recipe.ObjectName, completed.Result, recipe.ConcurrentJobs)
		}
		result, err := exporter.UploadWithResult(cmd.Context(), exportprotocol.DataPath, recipe.Destination.Bucket, recipe.ObjectName, recipe.UploadOptions()...)
		if err != nil {
			return err
		}
		envelope := exportprotocol.EnvelopeFor(attempt, recipe)
		envelope.Result = result
		if err := exportprotocol.ValidateEnvelope(envelope, attempt, recipe); err != nil {
			return err
		}
		encoded, err := exportprotocol.EncodeEnvelope(envelope)
		if err != nil {
			return err
		}
		if err := os.WriteFile(exportprotocol.ResultPath, encoded, 0600); err != nil {
			return fmt.Errorf("write native result: %w", err)
		}
		return nil
	}
	return c
}
