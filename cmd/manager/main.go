package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/oprysok/acr-pull-secret-controller/internal/controller"
	"github.com/oprysok/acr-pull-secret-controller/internal/token"
)

type config struct {
	registries        []string
	allowedSuffixes   []string
	secretName        string
	namespaceSelector string
	tenantID          string
	clientID          string
	tokenFile         string
	refreshFraction   float64
	metricsAddr       string
	probeAddr         string
	leaderElect       bool
}

func main() {
	var cfg config
	var registries, suffixes string
	flag.StringVar(&registries, "registry", envOr("REGISTRIES", ""), "Comma-separated ACR login servers, e.g. myregistry.azurecr.io.")
	flag.StringVar(&suffixes, "allowed-registry-suffixes", "azurecr.io", "Comma-separated domain suffixes registries must match. Tokens are never sent anywhere else.")
	flag.StringVar(&cfg.secretName, "secret-name", "acr-pull", "Name of the pull secret written into each namespace.")
	flag.StringVar(&cfg.namespaceSelector, "namespace-selector", "acr-pull-secret-controller/enabled=true", "Label selector for namespaces that receive the pull secret.")
	flag.StringVar(&cfg.tenantID, "tenant-id", os.Getenv("AZURE_TENANT_ID"), "Tenant that owns the registries.")
	flag.StringVar(&cfg.clientID, "client-id", os.Getenv("AZURE_CLIENT_ID"), "Client ID of the multi-tenant app registration.")
	flag.StringVar(&cfg.tokenFile, "token-file", os.Getenv("AZURE_FEDERATED_TOKEN_FILE"), "Projected service account token used as the federated assertion.")
	flag.Float64Var(&cfg.refreshFraction, "refresh-fraction", 0.5, "Renew tokens after this share of their lifetime.")
	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint address.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "Health probe address.")
	flag.BoolVar(&cfg.leaderElect, "leader-elect", false, "Enable leader election (required with more than one replica).")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	cfg.registries = splitList(registries)
	cfg.allowedSuffixes = splitList(suffixes)

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	if err := run(cfg); err != nil {
		log.Error(err, "exiting")
		os.Exit(1)
	}
}

func run(cfg config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	selector, err := labels.Parse(cfg.namespaceSelector)
	if err != nil {
		return fmt.Errorf("invalid --namespace-selector: %w", err)
	}

	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		TenantID:      cfg.tenantID,
		ClientID:      cfg.clientID,
		TokenFilePath: cfg.tokenFile,
	})
	if err != nil {
		return fmt.Errorf("workload identity credential: %w", err)
	}
	provider := token.NewProvider(cred, cfg.tenantID, cfg.registries, cfg.refreshFraction)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	// Only cache secrets this controller manages, not every secret in the cluster.
	managed := labels.SelectorFromSet(labels.Set{controller.ManagedByLabel: controller.ManagedByValue})
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElect,
		LeaderElectionID:       "acr-pull-secret-controller",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Label: managed},
		}},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	r := &controller.NamespaceReconciler{
		Client:      mgr.GetClient(),
		Credentials: provider,
		Selector:    selector,
		SecretName:  cfg.secretName,
		Recorder:    mgr.GetEventRecorder("acr-pull-secret-controller"),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	ctrl.Log.WithName("setup").Info("starting",
		"registries", cfg.registries, "secret", cfg.secretName, "selector", cfg.namespaceSelector,
		"tenant", cfg.tenantID, "client", cfg.clientID, "refreshFraction", strconv.FormatFloat(cfg.refreshFraction, 'f', 2, 64))
	return mgr.Start(ctrl.SetupSignalHandler())
}

func (c config) validate() error {
	var errs []error
	if len(c.registries) == 0 {
		errs = append(errs, errors.New("--registry is required"))
	}
	for _, r := range c.registries {
		if !hasAllowedSuffix(r, c.allowedSuffixes) {
			errs = append(errs, fmt.Errorf("registry %q does not match --allowed-registry-suffixes %v", r, c.allowedSuffixes))
		}
	}
	if c.tenantID == "" || c.clientID == "" || c.tokenFile == "" {
		errs = append(errs, errors.New("tenant ID, client ID and token file are required (set by the Azure workload identity webhook, or pass the flags)"))
	}
	if c.refreshFraction <= 0 || c.refreshFraction >= 1 {
		errs = append(errs, errors.New("--refresh-fraction must be between 0 and 1"))
	}
	return errors.Join(errs...)
}

func hasAllowedSuffix(host string, suffixes []string) bool {
	for _, s := range suffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
