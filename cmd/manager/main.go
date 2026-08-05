package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	monitoring "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/controllers/chainnode"
	"github.com/voluzi/cosmopilot/v2/internal/controllers/chainnodeset"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

var (
	scheme               = runtime.NewScheme()
	setupLog             = ctrl.Log.WithName("setup")
	metricsAddr          string
	enableLeaderElection bool
	probeAddr            string
	runOpts              controllers.ControllerRunOptions
	debugMode            bool
	zapOpts              zap.Options
	certsDir             string
)

func main() {
	flag.Parse()

	zapOpts.Development = debugMode
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	if err := monitoring.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "unable to add prometheus crds to scheme")
		os.Exit(1)
	}

	if err := snapshotv1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "unable to add volumesnapshot crds to scheme")
		os.Exit(1)
	}

	if err := gwapiv1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "unable to add gateway api v1 types to scheme")
		os.Exit(1)
	}

	if err := gwapiv1a2.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "unable to add gateway api v1alpha2 types to scheme")
		os.Exit(1)
	}

	leaderElectionID := fmt.Sprintf("%s.cosmopilot.voluzi.com", runOpts.ReleaseName)
	if runOpts.WorkerName != "" {
		leaderElectionID = fmt.Sprintf("%s.%s.cosmopilot.voluzi.com", runOpts.WorkerName, runOpts.ReleaseName)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    9443,
			CertDir: certsDir,
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	clientSet, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Fatalf("unable to create clientset: %v", err)
	}

	// Controller-runtime starts leader-elected runnables concurrently. Gate both controllers until the
	// elected worker has protected every pre-upgrade root and migrated its verified durable resources.
	rootProtectionReady := make(chan struct{})
	runOpts.RootProtectionReady = rootProtectionReady

	chainNodeReconciler, err := chainnode.New(mgr, clientSet, &runOpts)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ChainNode")
		os.Exit(1)
	}

	chainNodeSetReconciler, err := chainnodeset.New(mgr, clientSet, &runOpts)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ChainNodeSet")
		os.Exit(1)
	}
	if err := mgr.Add(&resourcecleanup.RootProtector{
		Client: mgr.GetClient(), WorkerName: runOpts.WorkerName, Ready: rootProtectionReady,
		Migrate: func(ctx context.Context) error {
			for {
				pending, err := migrateLegacyDurableResources(ctx, mgr.GetClient(), chainNodeReconciler, chainNodeSetReconciler)
				if err != nil || !pending {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
		},
	}); err != nil {
		setupLog.Error(err, "unable to register existing-root protection")
		os.Exit(1)
	}

	if !runOpts.DisableWebhooks {
		if err := appsv1.SetupChainNodeValidationWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to setup validation webhook", "resource", "ChainNode")
			os.Exit(1)
		}

		if err := appsv1.SetupChainNodeSetValidationWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to setup validation webhook", "resource", "ChainNodeSet")
			os.Exit(1)
		}
	}

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err = mgr.AddReadyzCheck("readyz", rootProtectionReadiness(rootProtectionReady)); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func rootProtectionReadiness(ready <-chan struct{}) healthz.Checker {
	return func(_ *http.Request) error {
		select {
		case <-ready:
			return nil
		default:
			return fmt.Errorf("existing-root protection and durable-resource migration are still running")
		}
	}
}

func migrateLegacyDurableResources(
	ctx context.Context,
	c client.Client,
	chainNodeReconciler *chainnode.Reconciler,
	chainNodeSetReconciler *chainnodeset.Reconciler,
) (bool, error) {
	pending := false
	nodeSets := &appsv1.ChainNodeSetList{}
	if err := c.List(ctx, nodeSets); err != nil {
		return false, err
	}
	for i := range nodeSets.Items {
		nodeSet := &nodeSets.Items[i]
		if !controllers.MatchesWorker(nodeSet.GetLabels(), runOpts.WorkerName) {
			continue
		}
		changed, err := chainNodeSetReconciler.MigrateLegacyDurableResources(ctx, nodeSet)
		if err != nil {
			return false, err
		}
		pending = pending || changed
	}
	nodes := &appsv1.ChainNodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return false, err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !controllers.MatchesWorker(node.GetLabels(), runOpts.WorkerName) || isRecordedStartupNodeSetChild(node, nodeSets.Items) {
			continue
		}
		changed, err := chainNodeReconciler.MigrateLegacyDurableResources(ctx, node)
		if err != nil {
			return false, err
		}
		pending = pending || changed
	}
	return pending, nil
}

func isRecordedStartupNodeSetChild(node *appsv1.ChainNode, nodeSets []appsv1.ChainNodeSet) bool {
	for i := range nodeSets {
		nodeSet := &nodeSets[i]
		if nodeSet.GetNamespace() != node.GetNamespace() || !controllers.MatchesWorker(nodeSet.GetLabels(), runOpts.WorkerName) {
			continue
		}
		for _, status := range nodeSet.Status.Nodes {
			if status.Name == node.GetName() {
				return status.UID == "" || status.UID == node.GetUID()
			}
		}
		for _, status := range nodeSet.Status.Validators {
			if status.Name == node.GetName() {
				return status.UID == "" || status.UID == node.GetUID()
			}
		}
	}
	return false
}
