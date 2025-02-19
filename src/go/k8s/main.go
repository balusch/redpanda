// Copyright 2021 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	cmapiv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	redpandav1alpha1 "github.com/redpanda-data/redpanda/src/go/k8s/apis/redpanda/v1alpha1"
	redpandacontrollers "github.com/redpanda-data/redpanda/src/go/k8s/controllers/redpanda"
	adminutils "github.com/redpanda-data/redpanda/src/go/k8s/pkg/admin"
	consolepkg "github.com/redpanda-data/redpanda/src/go/k8s/pkg/console"
	"github.com/redpanda-data/redpanda/src/go/k8s/pkg/resources"
	redpandawebhooks "github.com/redpanda-data/redpanda/src/go/k8s/webhooks/redpanda"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	defaultConfiguratorContainerImage = "vectorized/configurator"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

//nolint:wsl // the init was generated by kubebuilder
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(redpandav1alpha1.AddToScheme(scheme))
	utilruntime.Must(cmapiv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

//nolint:funlen // length looks good
func main() {
	var (
		clusterDomain               string
		metricsAddr                 string
		probeAddr                   string
		pprofAddr                   string
		enableLeaderElection        bool
		webhookEnabled              bool
		configuratorBaseImage       string
		configuratorTag             string
		configuratorImagePullPolicy string
		decommissionWaitInterval    time.Duration
		metricsTimeout              time.Duration
		restrictToRedpandaVersion   string

		// allowPVCDeletion controls the PVC deletion feature in the Cluster custom resource.
		// PVCs will be deleted when its Pod has been deleted and the Node that Pod is assigned to
		// does not exist, or has the NoExecute taint. This is intended to support the rancher.io/local-path
		// storage driver.
		allowPVCDeletion bool
		debug            bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&pprofAddr, "pprof-bind-address", ":8082", "The address the metric endpoint binds to.")
	flag.StringVar(&clusterDomain, "cluster-domain", "cluster.local", "Set the Kubernetes local domain (Kubelet's --cluster-domain)")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&webhookEnabled, "webhook-enabled", false, "Enable webhook Manager")
	flag.StringVar(&configuratorBaseImage, "configurator-base-image", defaultConfiguratorContainerImage, "Set the configurator base image")
	flag.StringVar(&configuratorTag, "configurator-tag", "latest", "Set the configurator tag")
	flag.StringVar(&configuratorImagePullPolicy, "configurator-image-pull-policy", "Always", "Set the configurator image pull policy")
	flag.DurationVar(&decommissionWaitInterval, "decommission-wait-interval", 8*time.Second, "Set the time to wait for a node decommission to happen in the cluster")
	flag.DurationVar(&metricsTimeout, "metrics-timeout", 8*time.Second, "Set the timeout for a checking metrics Admin API endpoint. If set to 0, then the 2 seconds default will be used")
	flag.BoolVar(&redpandav1alpha1.AllowDownscalingInWebhook, "allow-downscaling", false, "Allow to reduce the number of replicas in existing clusters (alpha feature)")
	flag.BoolVar(&allowPVCDeletion, "allow-pvc-deletion", false, "Allow the operator to delete PVCs for Pods assigned to failed or missing Nodes (alpha feature)")
	flag.BoolVar(&redpandav1alpha1.AllowConsoleAnyNamespace, "allow-console-any-ns", false, "Allow to create Console in any namespace. Allowing this copies Redpanda SchemaRegistry TLS Secret to namespace (alpha feature)")
	flag.StringVar(&restrictToRedpandaVersion, "restrict-redpanda-version", "", "Restrict management of clusters to those with this version")
	flag.StringVar(&redpandav1alpha1.SuperUsersPrefix, "superusers-prefix", "", "Prefix to add in username of superusers managed by operator. This will only affect new clusters, enabling this will not add prefix to existing clusters (alpha feature)")
	flag.BoolVar(&debug, "debug", false, "Set to enable debugging")

	opts := zap.Options{
		Development: true,
	}

	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if debug {
		go func() {
			pprofMux := http.NewServeMux()
			pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
			pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
			pprofServer := &http.Server{
				Addr:              pprofAddr,
				Handler:           pprofMux,
				ReadHeaderTimeout: 3 * time.Second,
			}
			log.Fatal(pprofServer.ListenAndServe())
		}()
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "aa9fc693.vectorized.io",
	})
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	configurator := resources.ConfiguratorSettings{
		ConfiguratorBaseImage: configuratorBaseImage,
		ConfiguratorTag:       configuratorTag,
		ImagePullPolicy:       corev1.PullPolicy(configuratorImagePullPolicy),
	}

	if err = (&redpandacontrollers.ClusterReconciler{
		Client:                    mgr.GetClient(),
		Log:                       ctrl.Log.WithName("controllers").WithName("redpanda").WithName("Cluster"),
		Scheme:                    mgr.GetScheme(),
		AdminAPIClientFactory:     adminutils.NewInternalAdminAPI,
		DecommissionWaitInterval:  decommissionWaitInterval,
		MetricsTimeout:            metricsTimeout,
		RestrictToRedpandaVersion: restrictToRedpandaVersion,
	}).WithClusterDomain(clusterDomain).WithConfiguratorSettings(configurator).WithAllowPVCDeletion(allowPVCDeletion).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "Cluster")
		os.Exit(1)
	}

	if err = (&redpandacontrollers.ClusterConfigurationDriftReconciler{
		Client:                    mgr.GetClient(),
		Log:                       ctrl.Log.WithName("controllers").WithName("redpanda").WithName("ClusterConfigurationDrift"),
		Scheme:                    mgr.GetScheme(),
		AdminAPIClientFactory:     adminutils.NewInternalAdminAPI,
		RestrictToRedpandaVersion: restrictToRedpandaVersion,
	}).WithClusterDomain(clusterDomain).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClusterConfigurationDrift")
		os.Exit(1)
	}

	if err = redpandacontrollers.NewClusterMetricsController(mgr.GetClient()).
		SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClustersMetrics")
		os.Exit(1)
	}

	// Setup webhooks
	if webhookEnabled {
		setupLog.Info("Setup webhook")
		if err = (&redpandav1alpha1.Cluster{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Unable to create webhook", "webhook", "RedpandaCluster")
			os.Exit(1)
		}
		hookServer := mgr.GetWebhookServer()
		hookServer.Register("/mutate-redpanda-vectorized-io-v1alpha1-console", &webhook.Admission{Handler: &redpandawebhooks.ConsoleDefaulter{Client: mgr.GetClient()}})
		hookServer.Register("/validate-redpanda-vectorized-io-v1alpha1-console", &webhook.Admission{Handler: &redpandawebhooks.ConsoleValidator{Client: mgr.GetClient()}})
	}

	if err = (&redpandacontrollers.ConsoleReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Log:                     ctrl.Log.WithName("controllers").WithName("redpanda").WithName("Console"),
		AdminAPIClientFactory:   adminutils.NewInternalAdminAPI,
		Store:                   consolepkg.NewStore(mgr.GetClient(), mgr.GetScheme()),
		EventRecorder:           mgr.GetEventRecorderFor("Console"),
		KafkaAdminClientFactory: consolepkg.NewKafkaAdmin,
	}).WithClusterDomain(clusterDomain).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Console")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("check", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	if webhookEnabled {
		hookServer := mgr.GetWebhookServer()
		if err := mgr.AddReadyzCheck("webhook", hookServer.StartedChecker()); err != nil {
			setupLog.Error(err, "unable to create ready check")
			os.Exit(1)
		}

		if err := mgr.AddHealthzCheck("webhook", hookServer.StartedChecker()); err != nil {
			setupLog.Error(err, "unable to create health check")
			os.Exit(1)
		}
	}
	setupLog.Info("Starting manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
