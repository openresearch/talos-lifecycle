package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/internal/controller"
	"github.com/openresearch/talos-lifecycle/internal/versions"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(upgradev1alpha1.AddToScheme(scheme))
}

const (
	controllerUpgrade = "upgrade"
	controllerConfig  = "config"
)

func parseControllers(value string) (map[string]bool, error) {
	enabled := map[string]bool{}

	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		switch name {
		case controllerUpgrade, controllerConfig:
			enabled[name] = true
		default:
			return nil, fmt.Errorf("unknown controller %q; valid values are %s and %s",
				name, controllerUpgrade, controllerConfig)
		}
	}

	if len(enabled) == 0 {
		return nil, fmt.Errorf("no controllers enabled; set --controllers to %s, %s, or both",
			controllerUpgrade, controllerConfig)
	}

	return enabled, nil
}

// a changed lease name would let a rolling update run two leaders
func leaderElectionID(enabled map[string]bool) string {
	if enabled[controllerUpgrade] {
		return "15e33b50.openresearch.com"
	}

	return "config.15e33b50.openresearch.com"
}

func main() {
	var (
		metricsAddr, probeAddr, controllers string
		leaderElect, secureMetrics          bool
		pollInterval                        time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"metrics address, e.g. :8443; 0 disables metrics")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leaderElect, "leader-elect", false, "run with leader election")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "serve metrics over HTTPS with authn/authz")
	flag.StringVar(&controllers, "controllers", controllerUpgrade,
		"comma-separated loops to run: "+controllerUpgrade+", "+controllerConfig+", or both; "+
			"normally one per Deployment")
	flag.DurationVar(&pollInterval, "version-poll-interval", 24*time.Hour,
		"how often to check GitHub for new Talos and Kubernetes releases; at least 1h")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := run(metricsAddr, probeAddr, controllers, leaderElect, secureMetrics, pollInterval); err != nil {
		setupLog.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(
	metricsAddr, probeAddr, controllers string, leaderElect, secureMetrics bool, pollInterval time.Duration,
) error {
	enabled, err := parseControllers(controllers)
	if err != nil {
		return err
	}

	// a poll is up to 20 unauthenticated GitHub requests; the limit is 60/h per IP
	if pollInterval < time.Hour {
		return fmt.Errorf("--version-poll-interval %s is below the 1h minimum", pollInterval)
	}

	setupLog.Info("starting", "controllers", controllers)

	metrics := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		// no HTTP/2: GHSA-qppj-fm5r-hxr3, GHSA-4374-p667-p6c8
		TLSOpts: []func(*tls.Config){func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }},
	}
	if secureMetrics {
		metrics.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metrics,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       leaderElectionID(enabled),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if enabled[controllerUpgrade] {
		if err := setupUpgrade(mgr, pollInterval); err != nil {
			return err
		}
	}

	if enabled[controllerConfig] {
		if err := setupConfig(mgr); err != nil {
			return err
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add health check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add ready check: %w", err)
	}

	return mgr.Start(ctrl.SetupSignalHandler())
}

func setupUpgrade(mgr manager.Manager, pollInterval time.Duration) error {
	r := &controller.UpgradePlanReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("talos-lifecycle"), //nolint:staticcheck
		APIReader: mgr.GetAPIReader(),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up UpgradePlan controller: %w", err)
	}

	if err := mgr.Add(&versions.Poller{Client: mgr.GetClient(), Interval: pollInterval}); err != nil {
		return fmt.Errorf("add version poller: %w", err)
	}

	return nil
}

func setupConfig(mgr manager.Manager) error {
	r := &controller.MachineConfigPlanReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("talos-lifecycle-config"), //nolint:staticcheck
		APIReader: mgr.GetAPIReader(),
		NodeName:  os.Getenv("NODE_NAME"),
		HealthCheck: func(ctx context.Context) (bool, string, error) {
			return controller.ClusterHealth(ctx, mgr.GetClient())
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up MachineConfigPlan controller: %w", err)
	}

	return nil
}
