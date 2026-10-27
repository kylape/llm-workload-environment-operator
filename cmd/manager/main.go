package main

import (
	"context"
	"flag"
	"os"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	environmentcontroller "github.com/kylape/llm-workload-environment-operator/internal/controller"
	environmentplatform "github.com/kylape/llm-workload-environment-operator/internal/platform"
	environmentvcluster "github.com/kylape/llm-workload-environment-operator/internal/vcluster"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	kueuev1beta1 "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
	utilruntime.Must(environmentv1alpha1.AddToScheme(scheme))
	utilruntime.Must(kueuev1beta1.AddToScheme(scheme))
	utilruntime.Must(kueuev1beta2.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var leaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&leaderElection, "leader-elect", false, "Enable leader election.")
	zapOptions := zap.Options{Development: true}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	argoCDNamespace := os.Getenv("ARGOCD_NAMESPACE")
	if err := environmentcontroller.ValidateArgoCDNamespace(argoCDNamespace); err != nil {
		ctrl.Log.Error(err, "invalid Argo CD registration configuration")
		os.Exit(1)
	}
	kueueAdmission, err := environmentcontroller.KueueAdmissionConfigFromEnv(os.Getenv)
	if err != nil {
		ctrl.Log.Error(err, "invalid Kueue admission configuration")
		os.Exit(1)
	}

	config := ctrl.GetConfigOrDie()
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		ctrl.Log.Error(err, "unable to create discovery client")
		os.Exit(1)
	}
	openShift, err := environmentplatform.IsOpenShift(context.Background(), discoveryClient)
	if err != nil {
		ctrl.Log.Error(err, "unable to determine cluster platform")
		os.Exit(1)
	}
	platform, err := environmentplatform.Resolve(os.Getenv("PLATFORM"), openShift)
	if err != nil {
		ctrl.Log.Error(err, "unable to resolve cluster platform")
		os.Exit(1)
	}
	ctrl.Log.Info("resolved cluster platform", "platform", platform)

	leaseDuration := 60 * time.Second
	renewDeadline := 30 * time.Second
	retryPeriod := 10 * time.Second
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr, SecureServing: true},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "llm-workload-environment-operator.llm.redhat.com",
		LeaseDuration:          &leaseDuration,
		RenewDeadline:          &renewDeadline,
		RetryPeriod:            &retryPeriod,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	var kueueClient environmentcontroller.KueueClient
	if kueueAdmission.Enabled {
		kueueClient = environmentcontroller.NewKueueAdapter(manager.GetClient(), discoveryClient)
	}
	if err := (&environmentcontroller.LLMWorkloadEnvironmentReconciler{
		Client:            manager.GetClient(),
		APIReader:         manager.GetAPIReader(),
		Scheme:            manager.GetScheme(),
		Kueue:             kueueClient,
		VClusterInstaller: environmentvcluster.HelmInstaller{},
		Platform:          platform,
		OpenShift:         openShift,
		APIServerURL:      config.Host,
		ArgoCDNamespace:   argoCDNamespace,
		KueueAdmission:    &kueueAdmission,
	}).SetupWithManager(manager); err != nil {
		ctrl.Log.Error(err, "unable to create controller")
		os.Exit(1)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to configure health check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to configure readiness check")
		os.Exit(1)
	}

	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "manager exited")
		os.Exit(1)
	}
}
