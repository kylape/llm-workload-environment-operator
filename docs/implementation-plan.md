# LLM Workload Environment Operator Implementation Plan

## Purpose

The LLM Workload Environment Operator provides an API for provisioning isolated
environments for LLM development, serving, and performance testing. Client tools
such as `infra` and a future UI create `LLMWorkloadEnvironment` resources; the
operator reconciles the cluster resources needed to make each environment usable.

The operator owns cluster-local environment provisioning. Namespace RBAC and
external lifecycle tools own identity, entitlement, and expiration policy. The
operator does not replace Kueue, Forge, Fournos, or higher-level lifecycle
tools.

## API contract

The API is a namespaced `LLMWorkloadEnvironment` resource in the
`llm.redhat.com/v1alpha1` API group. The resource namespace is the
control namespace for the request. The infra service may manage resources
across namespaces, while direct users can be granted access to only their
control namespace.

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

`spec.mode` is immutable and selects `Namespace` or `VCluster`; it defaults to
`Namespace`. The `infra` service account may receive cluster-wide CRUD access
to the namespaced environment resources. Direct users can instead receive CRUD
access only within a GitOps-provisioned control namespace. Existing namespace
RBAC determines who may read an environment and its kubeconfig; the operator does
not grant access to arbitrary identities named by an environment request.

## Namespace reconciliation

For an `LLMWorkloadEnvironment` named `<name>`, the operator creates a namespace
named `env-<name>`. It labels the namespace with
`kueue.openshift.io/managed: "true"` so the cluster Kueue controller manages
workloads submitted through the environment's LocalQueue.

The operator must not adopt an existing namespace. If the target namespace
already exists and is not controlled by the requesting `LLMWorkloadEnvironment`,
reconciliation fails and the collision is reported through the `Ready`
condition.

For namespace mode, the operator binds only its generated kubeconfig
ServiceAccount to the `llm-workload-environment-user` ClusterRole. The role allows
normal application and model-serving workload management but excludes CRD, RBAC,
and Kueue queue mutation. It grants read-only access to the environment's
LocalQueue and Workloads so users can inspect admission state. Users that
require their own CRDs use vCluster mode.

The operator creates a dedicated ServiceAccount in the backing namespace and
binds it to the same environment role. Once Kubernetes populates the ServiceAccount
token and CA, the operator builds a host-cluster kubeconfig and publishes it as
base64-encoded `status.kubeconfig`. Anyone allowed to read the LWE can use that
environment; no Secret read permission is needed in the control namespace.

The environment role is intentionally defined by this operator instead of using
the built-in `admin` or `edit` roles. Those roles are dynamically extended by
installed operators and cannot guarantee that LocalQueue mutation remains
reserved for the environment operator.

## Kueue integration

Every environment namespace receives a Kueue `LocalQueue` named `default`. It
references the `ClusterQueue` selected through `spec.clusterQueue`, which
defaults to `inference-workspaces` when omitted. Queue selection is immutable
because Kueue does not allow an existing LocalQueue to change its ClusterQueue.

Naming the LocalQueue `default` enables Kueue LocalQueue defaulting for enabled
workload integrations. The cluster's Kueue installation remains responsible
for enabling the required workload integrations and enforcing management of
workloads in environment namespaces.

The operator verifies that the requested ClusterQueue exists and is listed in the
`llm.redhat.com/allowed-cluster-queues` comma-delimited annotation on the
`LLMWorkloadEnvironment`'s landing namespace before creating backing resources.
It does not create or manage ClusterQueues, ResourceFlavors, cohorts, or priority
policy. Environment users must never receive permission to mutate a LocalQueue
because doing so could bypass that authorization boundary.

Because the vCluster control plane is itself scheduled in the managed environment
namespace, the selected ClusterQueue must cover all of its requested resources,
including `ephemeral-storage` in addition to CPU and memory.

## vCluster mode

The operator installs vCluster from the pinned
`quay.io/klape/charts/vcluster:0.0.1-combined.4` chart with the
`quay.io/klape/vcluster:csi-capacity-debug-v26` control-plane image. The chart
is downloaded and checksum-verified while building the operator image, then
installed from that bundled artifact without runtime registry access.

The chart's cluster role and cluster role binding are disabled. A shared
operator-owned ClusterRole permits only `get`, `list`, and `watch` of
`StorageClass` and `CSIStorageCapacity`, and each vCluster environment receives a
binding from its control-plane service account to that role. `CSINode` and
`CSIDriver` resources are not synchronized.

At startup the operator discovers whether `security.openshift.io` is served by
the cluster. On OpenShift it selects the chart's `restricted` security profile;
on Kubernetes it leaves the profile at the chart default. Discovery errors are
fatal so an incompatible profile is never selected silently.

The vCluster kubeconfig grants access to the virtual cluster and must not
contain host cluster credentials. The operator publishes the base64-encoded
kubeconfig in `status.kubeconfig` only after both the control plane and credential
are ready. Anyone allowed to read the LWE can use that environment; the requester
receives no other access to the host backing namespace.

On OpenShift, the operator reads the cluster applications domain from the
cluster-scoped Ingress configuration and creates a Route named after the
environment. The Route uses TLS passthrough to preserve the certificate and CA
provided by vCluster. The operator leaves the Helm-created kubeconfig Secret
unchanged and publishes a kubeconfig in status whose server URL is
`https://<environment-name>.<cluster-applications-domain>`. The public hostname
is explicit rather than relying on the generated
`<route-name>-<namespace>` form. Certificate SAN configuration for that
hostname is deferred; the initial implementation deliberately reuses the
vCluster-provided certificate material.

## Status

Status remains deliberately compact:

```yaml
status:
  namespaceRef:
    name: env-alice-test
  kubeconfig: <base64-encoded kubeconfig>
  conditions:
    - type: Ready
      status: "True"
      observedGeneration: 1
      reason: Ready
      message: Environment is ready
```

`kubeconfig` is populated for a ready environment. Namespace environments wait
for the ServiceAccount token Secret; vCluster environments wait for the control
plane and vCluster credential. `Ready=False` communicates failures
through specific reasons such as `NamespaceCollision`, `ClusterQueueNotFound`,
`QueueNotReady`, `KubeconfigNotReady`, `VClusterNotReady`, or
`ReconciliationFailed`.

Object deletion is represented by `metadata.deletionTimestamp`; no deletion or
expiration condition is added.

## Lifecycle and cleanup

The operator does not implement expiration. External tools may implement
lifespans by deleting the `LLMWorkloadEnvironment` resource.

The operator uses a finalizer to delete the backing namespace. It removes the
finalizer only after the namespace no longer exists. Deleting the namespace
also removes the LocalQueue, vCluster, and all namespaced environment resources.
Kubernetes garbage collection removes the environment-owned vCluster
ClusterRoleBinding.

## Delivery

The repository contains a container image definition, deployment manifests,
unit tests, and Tekton resources. The Tekton pipeline clones a requested Git
revision, builds the manager binary, and builds the operator image. Unit tests
can be run separately with `make test`.
Image pushing remains an explicit pipeline parameter so validation builds do
not require publishing an image.
