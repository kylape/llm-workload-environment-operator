// Command lwe provisions and manages LLMWorkloadEnvironment objects —
// the environments CI and laptops run inference E2E tests against.
//
// The suite's contract is kubeconfig-in, results-out; provisioning is a
// CI concern, and this tool is the single provisioning path for both CI
// and laptop repro. Credentials are read from the Secret referenced by
// status.kubeconfigSecretRef (never the deprecated inline status field,
// except as a migration fallback against pre-Secret operators).
//
// Output conventions: machine-readable artifacts go to stdout (the
// environment name from `up`, the kubeconfig from `kubeconfig`);
// progress and diagnostics go to stderr. Exit status is non-zero on
// failure.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	environmentv1alpha1 "github.com/kylape/llm-workload-environment-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(environmentv1alpha1.AddToScheme(scheme))
}

const (
	// managedByLabel marks environments created by this CLI so `sweep`
	// can find CI-owned environments without touching anyone else's.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "lwe-cli"
	// defaultWaitTimeout matches vCluster bring-up; HyperShift takes
	// longer and callers pass -timeout explicitly.
	defaultWaitTimeout = 30 * time.Minute
	// waitInterval is the readiness poll interval.
	waitInterval = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	command, args := os.Args[1], os.Args[2:]
	var run func(context.Context, []string) error
	switch command {
	case "up":
		run = runUp
	case "kubeconfig":
		run = runKubeconfig
	case "down":
		run = runDown
	case "sweep":
		run = runSweep
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "lwe: unknown command %q\n\n", command)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err := run(ctx, args); err != nil {
		fmt.Fprintf(os.Stderr, "lwe %s: %v\n", command, err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `usage: lwe <command> [flags]

commands:
  up          create (or adopt) an environment and wait for it to be Ready
  kubeconfig  print the environment's kubeconfig (see -output for a file)
  down        delete an environment
  sweep       delete stale CI-owned environments

The namespace holding LLMWorkloadEnvironment objects comes from
-namespace, the LWE_NAMESPACE environment variable, or the kubeconfig's
current context, in that order.
`)
}

// commonFlags carries the flags every command shares; each command
// binds the ones it needs into its own FlagSet and passes the values.
type commonFlags struct {
	namespace  string
	kubeconfig string
}

// bindNamespace registers the shared -namespace flag.
func (c *commonFlags) bindNamespace(fs *flag.FlagSet) {
	fs.StringVar(&c.namespace, "namespace", "", "namespace holding the environment objects (or $LWE_NAMESPACE, or the kubeconfig context's namespace)")
}

// resolveNamespace applies the namespace precedence: flag, environment,
// kubeconfig context, "default".
func (c *commonFlags) resolveNamespace() (string, error) {
	if c.namespace != "" {
		return c.namespace, nil
	}
	if ns := os.Getenv("LWE_NAMESPACE"); ns != "" {
		return ns, nil
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if c.kubeconfig != "" {
		loadingRules.ExplicitPath = c.kubeconfig
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	ns, _, err := kubeConfig.Namespace()
	if err != nil {
		return "", fmt.Errorf("resolving default namespace: %w", err)
	}
	return ns, nil
}

// newClientFor builds the controller-runtime client; a variable so
// tests can substitute a fake.
var newClientFor = newClient

// newClient builds a controller-runtime client from the standard
// kubeconfig chain (KUBECONFIG, ~/.kube/config, in-cluster).
func newClient(kubeconfig string) (client.Client, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}
	return c, nil
}

// randomSuffix returns five lowercase alphanumeric characters — enough
// collision resistance for generated environment names within a
// namespace.
func randomSuffix() (string, error) {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.EncodeToString(buf))[:5], nil
}

// labelsFlag collects repeatable -label k=v flags.
type labelsFlag map[string]string

func (l labelsFlag) String() string {
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (l labelsFlag) Set(value string) error {
	k, v, found := strings.Cut(value, "=")
	if !found || k == "" || v == "" {
		return fmt.Errorf("label must be k=v, got %q", value)
	}
	l[k] = v
	return nil
}

// expireAfter applies the context deadline for wait loops, returning
// the context and its cancel function.
func expireAfter(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func runUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var common commonFlags
	common.bindNamespace(fs)
	mode := fs.String("mode", "Namespace", "environment mode: Namespace, VCluster, HyperShift, or SNO")
	duration := fs.String("duration", "", "requested lifetime (Go duration syntax, e.g. 2h); empty uses the operator's default")
	clusterQueue := fs.String("cluster-queue", "", "Kueue ClusterQueue backing the environment; empty uses the CRD default")
	extraLabels := labelsFlag{}
	fs.Var(extraLabels, "label", "extra label k=v to stamp on the environment (repeatable)")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "maximum time to wait for readiness; 0 waits forever")
	name := fs.String("name", "", "environment name; generated when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("up takes no positional arguments; use -name")
	}

	environmentMode := environmentv1alpha1.EnvironmentMode(*mode)
	switch environmentMode {
	case environmentv1alpha1.EnvironmentModeNamespace,
		environmentv1alpha1.EnvironmentModeVCluster,
		environmentv1alpha1.EnvironmentModeHyperShift,
		environmentv1alpha1.EnvironmentModeSNO:
	default:
		return fmt.Errorf("unsupported mode %q", *mode)
	}

	environmentName := *name
	if environmentName == "" {
		suffix, err := randomSuffix()
		if err != nil {
			return fmt.Errorf("generating name: %w", err)
		}
		environmentName = "lwe-" + suffix
	}

	namespace, err := common.resolveNamespace()
	if err != nil {
		return err
	}
	c, err := newClientFor(common.kubeconfig)
	if err != nil {
		return err
	}

	environment := &environmentv1alpha1.LLMWorkloadEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: environmentName, Namespace: namespace},
	}
	err = c.Get(ctx, client.ObjectKeyFromObject(environment), environment)
	switch {
	case err == nil:
		// Adopt: mode is immutable, so a mode mismatch is a caller error.
		if environment.Spec.Mode != environmentMode {
			return fmt.Errorf("environment %s/%s exists with mode %s", namespace, environmentName, environment.Spec.Mode)
		}
		fmt.Fprintf(os.Stderr, "adopting existing environment %s/%s\n", namespace, environmentName)
	case apierrors.IsNotFound(err):
		environment.Spec = environmentv1alpha1.LLMWorkloadEnvironmentSpec{
			Mode:         environmentMode,
			Duration:     *duration,
			ClusterQueue: *clusterQueue,
		}
		if environment.Labels == nil {
			environment.Labels = map[string]string{}
		}
		environment.Labels[managedByLabel] = managedByValue
		for k, v := range extraLabels {
			environment.Labels[k] = v
		}
		if err := c.Create(ctx, environment); err != nil {
			return fmt.Errorf("creating environment: %w", err)
		}
		fmt.Fprintf(os.Stderr, "created environment %s/%s (mode %s)\n", namespace, environmentName, environmentMode)
	default:
		return fmt.Errorf("reading environment: %w", err)
	}

	waitCtx, cancel := expireAfter(ctx, *timeout)
	defer cancel()
	if err := waitReady(waitCtx, c, environment); err != nil {
		return err
	}

	// waitReady polls its own copies; refresh before reading
	// credentials, which were published after our first Get.
	if err := c.Get(ctx, client.ObjectKeyFromObject(environment), environment); err != nil {
		return fmt.Errorf("re-reading ready environment: %w", err)
	}

	// Verify the credentials are readable before handing the name back:
	// an environment whose kubeconfig Secret cannot be read is not a
	// usable environment, and failing here beats failing in the test job.
	kubeconfig, err := readKubeconfig(ctx, c, environment)
	if err != nil {
		return fmt.Errorf("environment is ready but its credentials are unreadable: %w", err)
	}
	fmt.Fprintf(os.Stderr, "kubeconfig verified: %d bytes\n", len(kubeconfig))

	// The name is the machine-readable artifact.
	fmt.Println(environmentName)
	return nil
}

// waitReady polls the environment until its Ready condition is true,
// the environment is deleted, the context expires, or it is marked
// Expired (which the operator will act on; waiting is pointless).
func waitReady(ctx context.Context, c client.Client, environment *environmentv1alpha1.LLMWorkloadEnvironment) error {
	key := client.ObjectKeyFromObject(environment)
	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()
	var lastMessage string
poll:
	for {
		current := &environmentv1alpha1.LLMWorkloadEnvironment{}
		if err := c.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) && ctx.Err() == nil {
				return fmt.Errorf("environment %s was deleted while waiting", key)
			}
			if ctx.Err() == nil {
				return fmt.Errorf("reading environment: %w", err)
			}
			break poll
		}
		if apimeta.IsStatusConditionTrue(current.Status.Conditions, environmentv1alpha1.ExpiredCondition) {
			return fmt.Errorf("environment %s expired while waiting", key)
		}
		ready := apimeta.FindStatusCondition(current.Status.Conditions, environmentv1alpha1.ReadyCondition)
		if ready != nil && ready.Status == metav1.ConditionTrue {
			return nil
		}
		if ready != nil && ready.Message != lastMessage {
			lastMessage = ready.Message
			if remaining := current.Status.ExpirationRemaining; remaining != "" {
				fmt.Fprintf(os.Stderr, "waiting: %s (expires in %s)\n", ready.Message, remaining)
			} else {
				fmt.Fprintf(os.Stderr, "waiting: %s\n", ready.Message)
			}
		}
		select {
		case <-ctx.Done():
			break poll
		case <-ticker.C:
		}
	}
	current := &environmentv1alpha1.LLMWorkloadEnvironment{}
	if err := c.Get(context.Background(), key, current); err == nil {
		for _, condition := range current.Status.Conditions {
			fmt.Fprintf(os.Stderr, "  %s=%s %s: %s\n", condition.Type, condition.Status, condition.Reason, condition.Message)
		}
	}
	return fmt.Errorf("timed out waiting for %s to become ready", key)
}

// readKubeconfig resolves the environment's kubeconfig through
// status.kubeconfigSecretRef. The deprecated inline field is only
// consulted for operators that predate Secret publication, with a
// warning; it goes away with the field.
func readKubeconfig(ctx context.Context, c client.Client, environment *environmentv1alpha1.LLMWorkloadEnvironment) ([]byte, error) {
	if ref := environment.Status.KubeconfigSecretRef; ref != nil {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: environment.Namespace, Name: ref.Name}, secret); err != nil {
			return nil, fmt.Errorf("reading kubeconfig Secret: %w", err)
		}
		kubeconfig := secret.Data["kubeconfig"]
		if len(kubeconfig) == 0 {
			return nil, fmt.Errorf("kubeconfig Secret %s/%s has no kubeconfig data", secret.Namespace, secret.Name)
		}
		return kubeconfig, nil
	}
	if legacy := environment.Status.Kubeconfig; len(legacy) != 0 { //nolint:staticcheck // SA1019: migration fallback for pre-Secret operators
		fmt.Fprintln(os.Stderr, "warning: operator predates kubeconfig Secret publication; reading the deprecated inline field")
		return legacy, nil
	}
	return nil, errors.New("environment has not published a kubeconfig (not ready?)")
}

func runKubeconfig(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kubeconfig", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var common commonFlags
	common.bindNamespace(fs)
	output := fs.String("output", "", "write the kubeconfig to this file instead of stdout (mode 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("kubeconfig takes exactly one environment name")
	}
	environmentName := fs.Arg(0)

	namespace, err := common.resolveNamespace()
	if err != nil {
		return err
	}
	c, err := newClientFor(common.kubeconfig)
	if err != nil {
		return err
	}

	environment := &environmentv1alpha1.LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: environmentName, Namespace: namespace}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(environment), environment); err != nil {
		return fmt.Errorf("reading environment: %w", err)
	}
	kubeconfig, err := readKubeconfig(ctx, c, environment)
	if err != nil {
		return err
	}

	if *output == "" {
		_, err := os.Stdout.Write(kubeconfig)
		return err
	}
	if err := os.WriteFile(*output, kubeconfig, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", *output, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *output)
	return nil
}

func runDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var common commonFlags
	common.bindNamespace(fs)
	wait := fs.Bool("wait", false, "wait for the environment to be fully deleted")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "maximum time to wait for deletion (with -wait)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("down takes exactly one environment name")
	}
	environmentName := fs.Arg(0)

	namespace, err := common.resolveNamespace()
	if err != nil {
		return err
	}
	c, err := newClientFor(common.kubeconfig)
	if err != nil {
		return err
	}

	key := types.NamespacedName{Namespace: namespace, Name: environmentName}
	environment := &environmentv1alpha1.LLMWorkloadEnvironment{ObjectMeta: metav1.ObjectMeta{Name: environmentName, Namespace: namespace}}
	if err := c.Delete(ctx, environment); err != nil {
		if apierrors.IsNotFound(err) {
			// Idempotent: a deleted environment is the goal state.
			fmt.Fprintf(os.Stderr, "environment %s/%s is already gone\n", namespace, environmentName)
			return nil
		}
		return fmt.Errorf("deleting environment: %w", err)
	}
	fmt.Fprintf(os.Stderr, "deleted environment %s/%s\n", namespace, environmentName)
	if !*wait {
		return nil
	}

	waitCtx, cancel := expireAfter(ctx, *timeout)
	defer cancel()
	ticker := time.NewTicker(waitInterval)
	defer ticker.Stop()
	for {
		err := c.Get(waitCtx, key, environment)
		if apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "environment %s/%s is gone\n", namespace, environmentName)
			return nil
		}
		if err != nil && waitCtx.Err() == nil {
			return fmt.Errorf("reading environment: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for %s to be deleted", key)
		case <-ticker.C:
		}
	}
}

func runSweep(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var common commonFlags
	common.bindNamespace(fs)
	selector := fs.String("selector", managedByLabel+"="+managedByValue, "label selector matching sweepable environments")
	olderThan := fs.Duration("older-than", 6*time.Hour, "delete environments created more than this long ago")
	dryRun := fs.Bool("dry-run", false, "print what would be deleted without deleting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("sweep takes no positional arguments")
	}

	parsedSelector, err := labels.Parse(*selector)
	if err != nil {
		return fmt.Errorf("parsing selector: %w", err)
	}
	namespace, err := common.resolveNamespace()
	if err != nil {
		return err
	}
	c, err := newClientFor(common.kubeconfig)
	if err != nil {
		return err
	}

	list := &environmentv1alpha1.LLMWorkloadEnvironmentList{}
	if err := c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: parsedSelector}); err != nil {
		return fmt.Errorf("listing environments: %w", err)
	}

	cutoff := time.Now().Add(-*olderThan)
	swept := 0
	for i := range list.Items {
		environment := &list.Items[i]
		if environment.CreationTimestamp.After(cutoff) {
			continue
		}
		if *dryRun {
			fmt.Fprintf(os.Stderr, "would delete %s/%s (created %s)\n", environment.Namespace, environment.Name, environment.CreationTimestamp.Format(time.RFC3339))
			swept++
			continue
		}
		if err := c.Delete(ctx, environment); err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "failed to delete %s/%s: %v\n", environment.Namespace, environment.Name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "deleted %s/%s (created %s)\n", environment.Namespace, environment.Name, environment.CreationTimestamp.Format(time.RFC3339))
		swept++
	}
	fmt.Fprintf(os.Stderr, "swept %d environment(s)\n", swept)
	return nil
}
