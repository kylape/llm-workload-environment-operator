# LLM Workload Environment Operator

LLM Workload Environment Operator provisions isolated Kubernetes environments
for LLM development, serving, and performance testing. It provides a declarative
boundary between clients such as `infra` or a future UI and cluster services
such as Kueue, vCluster, and HyperShift.

The operator provisions a host-cluster namespace, vCluster, or hosted OpenShift
cluster for each
namespaced `LLMWorkloadEnvironment` resource. The resource namespace is the
control boundary: the infra service can manage environments across namespaces, while
direct users can be limited to a namespace by ordinary Kubernetes RBAC.

With Kueue integration enabled (the default), each control (landing) namespace
must allow the ClusterQueues its environments
can use. Set the `llm.redhat.com/allowed-cluster-queues` annotation to a
comma-delimited list on that namespace, for example:

```bash
kubectl annotate namespace alice-dev \
  llm.redhat.com/allowed-cluster-queues='inference-workspaces,gpu-high-memory'
```

See the [implementation plan](docs/implementation-plan.md) for the API and
ownership decisions.

## Environment request

```yaml
apiVersion: llm.redhat.com/v1alpha1
kind: LLMWorkloadEnvironment
metadata:
  name: alice-test
  namespace: alice-dev
spec:
  mode: VCluster
  clusterQueue: inference-workspaces
```

Creating this resource provisions:

* namespace `env-alice-test`;
* a Kueue `LocalQueue` named `default` that references the platform-owned
  `inference-workspaces` ClusterQueue, when Kueue integration is enabled.

In `Namespace` mode, the operator also creates a dedicated ServiceAccount in
the backing namespace. Once Kubernetes has populated its token Secret, the operator
publishes the base64-encoded kubeconfig in `status.kubeconfig`.

For `VCluster` mode, the operator also installs the pinned vCluster chart and
publishes its base64-encoded kubeconfig in status. On OpenShift, it also creates a
TLS-passthrough Route at
`<environment-name>.<cluster-applications-domain>` and publishes an
updated kubeconfig with that public server URL. The public hostname is added to
the vCluster proxy certificate SANs. Set `PLATFORM=coreweave` on CoreWeave
clusters; the operator creates a per-environment public LoadBalancer Service
for the vCluster API and publishes a kubeconfig using its assigned address.
OpenShift is detected automatically when `PLATFORM` is unset, or can be selected
with `PLATFORM=openshift`; it uses Routes. Other Kubernetes clusters remain not
ready with `VClusterPlatformNotConfigured` until a platform is selected.
The vCluster can read host-cluster `StorageClass` and `CSIStorageCapacity`
objects through a dedicated read-only role. The chart's own cluster-wide RBAC
is disabled.

For `HyperShift` mode, an administrator profile selects the OpenShift release,
KubeVirt worker sizing, storage and placement. LWE creates a HostedCluster and
NodePool, publishes a validated guest administrator kubeconfig, and cleans up
provider resources on deletion. Once authenticated API access works, LWE
publishes the kubeconfig with `APIAccessible=True` so clients can apply resources
before workers are ready. `Ready=True` still requires healthy workers and full
OpenShift installation. Worker count can be scaled through the LWE resource. SharedIngress profiles use cert-manager to issue API certificates
before creating the HostedCluster, then publish client access through the host's
existing ingress on port 443. Worker connectivity retains native NodePorts.
Guest Routes use HyperShift host ingress passthrough. See
[HyperShift setup and usage](docs/hypershift.md)
and the [example request](config/samples/hypershift.yaml).

The reusable environment role does not permit its subjects to mutate LocalQueues,
cluster RBAC, or CRDs. Kueue queue selection remains controlled by the operator.
When Kueue integration is enabled, the operator checks that the selected
ClusterQueue both exists and appears in the
landing namespace's allowlist before creating the backing environment namespace.
An omitted `spec.clusterQueue` resolves to `inference-workspaces`, which must also
be listed. Rejected requests report the reason in the `Ready` status condition.

## Optional Kueue integration

Both Kueue switches default to `true`:

* `KUEUE_ADMISSION_ENABLED=false` allows environments to provision without
  Kueue installed. It skips ClusterQueue allowlist/existence checks, LocalQueue
  creation/readiness checks, Kueue namespace labeling, and admission validation.
* `KUEUE_ADMISSION_VALIDATION_ENABLED=false` skips only the check that Kueue's
  Pod integration is configured and selects the backing namespace. Queue checks,
  LocalQueue reconciliation, and namespace labeling remain enabled.

The main switch takes precedence. Existing queues and Kueue namespace metadata
are left untouched when integration is disabled. These switches do not disable
the host's Kueue webhook or its own policies. See
[Kueue configuration](docs/kueue-admission.md) for configuration and migration
from the old validation-only meaning of `KUEUE_ADMISSION_ENABLED`.

## Kueue API compatibility

When integration is enabled, the operator discovers the Kueue APIs served by the cluster and prefers
`kueue.x-k8s.io/v1beta2`, falling back to `v1beta1`. The selected version must
serve both `ClusterQueue` and `LocalQueue`. Compatibility is based on the
installed CRDs. Upstream Kueue v0.15 through v0.19 serve both API versions;
v0.20 and later serve only `v1beta2`. Kueue v0.16 uses `v1beta2` for storage
and provides migration guidance for objects previously stored as `v1beta1`.
See the upstream
[v0.16 upgrade notes](https://github.com/kubernetes-sigs/kueue/releases/tag/v0.16.0)
and [v0.20 upgrade notes](https://github.com/kubernetes-sigs/kueue/releases/tag/v0.20.0).

No fleet-wide Kueue upgrade is required for this operator compatibility. If
neither API version is served with both resources, the environment reports
`Ready=False` with reason `KueueAPINotAvailable` and a message naming the
required APIs. Kueue release upgrades should be planned separately using the
upstream upgrade guidance.

The operator does not grant access to arbitrary identities from the
`LLMWorkloadEnvironment` spec. Existing namespace RBAC determines who may read an
environment; readers can retrieve its kubeconfig from status. The
operator-generated Namespace-mode ServiceAccount is the only identity it binds to
the environment role.

Retrieve a ready environment's kubeconfig with:

```bash
umask 077
kubectl get llmworkloadenvironment alice-test -n alice-dev \
  -o jsonpath='{.status.kubeconfig}' | base64 --decode > kubeconfig
```

## Development

```bash
make test
make build
```

The Tekton resources in `config/tekton` clone a requested Git revision, build the
manager binary, and build the operator container image. Unit tests can be run
separately with `make test`. Image publishing is disabled by default and can be
enabled when a registry credential workspace is supplied.
