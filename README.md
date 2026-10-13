# LLM Workload Environment Operator

LLM Workload Environment Operator provisions isolated Kubernetes environments
for LLM development, serving, and performance testing. It provides a declarative
boundary between clients such as `infra` or a future UI and cluster services
such as Kueue and vCluster.

The operator provisions either a host-cluster namespace or a vCluster for each
namespaced `LLMWorkloadEnvironment` resource. The resource namespace is the
control boundary: the infra service can manage environments across namespaces, while
direct users can be limited to a namespace by ordinary Kubernetes RBAC.

Each control (landing) namespace must allow the ClusterQueues its environments
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

* namespace `environment-alice-test`;
* a Kueue `LocalQueue` named `default` that references the platform-owned
  `inference-workspaces` ClusterQueue.

In `Namespace` mode, the operator also creates a dedicated ServiceAccount in
the backing namespace and publishes a host-cluster kubeconfig Secret in the
`LLMWorkloadEnvironment`'s namespace. The kubeconfig Secret reference is available
in status after Kubernetes has populated the ServiceAccount token.

For `VCluster` mode, the operator also installs the pinned vCluster chart and
publishes its kubeconfig Secret reference in status. On OpenShift, it also
creates a TLS-passthrough Route at
`<environment-name>.<cluster-applications-domain>` and publishes an
operator-managed copy of the kubeconfig with that public server URL. The copy
retains the CA and certificate data provided by vCluster; certificate SAN
handling for the public hostname is intentionally left for a later iteration.
The vCluster can read host-cluster `StorageClass` and `CSIStorageCapacity`
objects through a dedicated read-only role. The chart's own cluster-wide RBAC
is disabled.

The reusable environment role does not permit its subjects to mutate LocalQueues,
cluster RBAC, or CRDs. Kueue queue selection remains controlled by the operator.
The operator checks that the selected ClusterQueue both exists and appears in the
landing namespace's allowlist before creating the backing environment namespace.
An omitted `spec.clusterQueue` resolves to `inference-workspaces`, which must also
be listed. Rejected requests report the reason in the `Ready` status condition.

The operator does not grant access to arbitrary identities from the
`LLMWorkloadEnvironment` spec. Existing namespace RBAC determines who may create an
environment and read its kubeconfig Secret. The operator-generated Namespace-mode
ServiceAccount is the only identity it binds to the environment role.

## Development

```bash
make test
make build
```

The Tekton resources in `config/tekton` clone a requested Git revision, build the
manager binary, and build the operator container image. Unit tests can be run
separately with `make test`. Image publishing is disabled by default and can be
enabled when a registry credential workspace is supplied.
