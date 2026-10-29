# Hosted OpenShift environments

`spec.mode: HyperShift` provisions a hosted OpenShift control plane and a
KubeVirt worker NodePool through an existing HyperShift installation. It uses
the same LWE request, readiness condition, kubeconfig status field, optional
Argo CD registration, and deletion flow as other environment modes.

This replaces the proposed SNO integration. It does not require a SNO operator,
seed image, recert, or an independently maintained OpenShift cloning flow.

## Administrator setup

Install HyperShift and OpenShift Virtualization on the OpenShift management
cluster first. Verify that the selected release, KubeVirt worker disks, API
NodePorts, and host ingress passthrough work on this host. LWE does not install
or upgrade those components.

Create an administrator-owned pull Secret in the operator namespace. It must
contain the OpenShift pull secret as `.dockerconfigjson`. Reference this Secret
from a profile; never put credentials in Helm values or environment requests.
An optional SSH Secret contains the public key under `id_rsa.pub`.

Example Helm values for the validated lab configuration:

```yaml
platform: openshift
hypershift:
  enabled: true
  profiles:
    default:
      releaseImage: quay.io/openshift-release-dev/ocp-release@sha256:4ebbe67679969175b6be1626a64eeab44191117abba298e43742d72b26bf2ecd
      pullSecret:
        namespace: llm-workload-environment-operator
        name: hypershift-pull-secret
      baseDomain: apps.openshift.nm.rdu4.corp.redhat.com
      apiServerAddress: 10.14.217.16
      etcdStorageClass: lvms-a100-tier1-storage
      workerStorageClass: lvms-a100-tier1-storage
      controlPlaneNodeSelector:
        kubernetes.io/hostname: a100-03
      workerNodeSelector:
        kubernetes.io/hostname: a100-03
      workerCores: 4
      workerMemory: 16Gi
      workerRootDisk: 64Gi
      maxReplicas: 10
      clusterNetwork: 10.244.0.0/16
      serviceNetwork: 172.29.0.0/16
```

Defaults are one worker per request, 4 CPU / 16Gi memory / 64Gi root disk,
8Gi etcd storage, a profile maximum of ten workers, and the guest CIDRs above.
Choose guest networks that do not overlap the management cluster's machine,
pod, service or other routed networks. Guest pod networks need room for /23
node subnets. Release images must be pinned by digest. The initial implementation
uses amd64 workers, a single control-plane replica, local block storage, manual
guest-to-host storage-class mapping (`worker-lvm`), and replacement upgrades.
It does not offer high availability or VM live migration with local LVM.

For a manifest-based installation, set `HYPERSHIFT_CONFIG` on the manager to
the JSON equivalent of `hypershift` in these values. Invalid profiles fail
manager startup. Empty configuration disables HyperShift without requiring its
CRDs for the other modes. Apply the updated LWE CRD and ClusterRole along with
the operator image.

## User flow

Create a request in an authorized landing namespace:

```yaml
apiVersion: llm.redhat.com/v1alpha1
kind: LLMWorkloadEnvironment
metadata:
  name: my-openshift
  namespace: workload-requests
spec:
  mode: HyperShift
  clusterQueue: inference-workspaces
  hypershift:
    profile: default
    replicas: 1
```

The mode and profile are immutable. `replicas` can be changed within the
profile limit. HyperShift environment names are limited to 51 characters so
the generated control-plane namespace fits Kubernetes naming limits.

```bash
kubectl wait llmworkloadenvironment/my-openshift -n workload-requests \
  --for=condition=Ready --timeout=15m
umask 077
kubectl get llmworkloadenvironment my-openshift -n workload-requests \
  -o jsonpath='{.status.kubeconfig}' | base64 --decode > guest.kubeconfig
kubectl get nodes --kubeconfig guest.kubeconfig
kubectl patch llmworkloadenvironment my-openshift -n workload-requests \
  --type=merge -p '{"spec":{"hypershift":{"replicas":2}}}'
kubectl delete llmworkloadenvironment my-openshift -n workload-requests
```

`status.kubeconfig` grants guest cluster administrator access. Restrict read
access to LWE resources accordingly. LWE publishes only the provider's current
context, with embedded CA and credentials, after verifying the API endpoint,
TLS trust, authenticated API access and the requested number of Ready workers.
It rejects credential plugins, external credential files and host credentials.
Status credentials are withdrawn while the environment is unavailable or scales.

The controller creates `HostedCluster/cluster` and `NodePool/workers` in
`env-my-openshift`. HyperShift creates the separate
`env-my-openshift-cluster` control-plane namespace. References in LWE status
record the provider resource UIDs. Names are intentionally scoped to each
environment namespace.

Each hosted API receives an automatically allocated NodePort on the configured
node address. Clients and worker VMs must be able to reach that address and
the management cluster's NodePort range. The address must remain stable and
reachable; it is not a cloud load balancer or a newly allocated public IP.

`baseDomainPassthrough: true` lets HyperShift publish guest application routes
through the management cluster's ingress. Each guest receives an immutable base
domain `<LWE-name>-<UID-hash>.<profile-base-domain>`, preventing collisions
between hosted clusters that use the same local resource name. The default
cluster-name prefix remains `cluster`; application routes therefore live beneath
`apps.cluster.<LWE-name>-<UID-hash>.<profile-base-domain>`. This deliberately
avoids `baseDomainPrefix`: HyperShift 4.22 automatic passthrough routes derive
their hostnames from the cluster name and base domain. Deploy Services and Routes inside
the guest normally. Wildcard DNS and network access for the host's ingress
domain must already exist. HyperShift owns the resulting host routes and router
NodePorts. LWE does not create a route for every guest application.

Add the existing `llm.redhat.com/application-profile` label to request Argo CD
registration when `ARGOCD_NAMESPACE` is configured. Profile ApplicationSets
can select the same registration labels used for VCluster environments.

## Capacity, reconciliation and cleanup

Existing LWE Kueue policy remains in effect: when enabled, the landing namespace
must allow the requested ClusterQueue and the backing namespace must match the
host's queue selectors. Configure `kueue.namespaceLabels` and `kueue.configRef`
to match the host. Disable Kueue only if that is the intended installation policy.

The backing namespace's LocalQueue does **not** reserve capacity for HyperShift
control-plane pods or worker VMs in the separate control-plane namespace, nor
does it install Kueue in the guest. Worker count and sizing are bounded by the
administrator profile; host quota, guest GPU passthrough and guest workload
admission remain separate concerns.

LWE polls provider readiness every five seconds during provisioning and every
30 seconds when ready, avoiding a hard watch dependency on optional CRDs.
It does not replace a missing or externally replaced established cluster.
Provider configuration drift or changed administrator profiles require operator
attention; use a new profile and environment for release or sizing changes.
Scaling the worker count is the supported mutable operation.

Deletion removes any Argo CD registration and requests HostedCluster deletion.
HyperShift destroys the CAPI cluster and its workers together. LWE waits for the
HostedCluster, NodePool and control-plane namespace to disappear before deleting
the backing namespace and releasing its finalizer. Deleting the last NodePool
first would leave drains waiting on guest disruption budgets, so LWE delegates
the cluster destruction sequence to HyperShift. Do not strip
provider finalizers or delete encryption material to accelerate cleanup.
An independent random 32-byte etcd encryption key is persisted before each
cluster is created and retained across reconciliation. Foreign resources and
replaced identities stop reconciliation and cleanup rather than being adopted.

## Validation

Unit tests cover creation, scaling readiness, profile policy, durable identity,
encryption key stability, portable credential validation, TLS API probing,
cleanup ordering and HyperShift Argo CD registration. Helm tests validate the
disabled default, profile serialization and malformed input rejection.

```bash
go test -race ./...
go vet ./...
make test-chart
# Optional: Kubernetes 1.35 envtest binaries, local control plane only.
KUBEBUILDER_ASSETS=/path/to/envtest go test ./api/v1alpha1 -count=1
```

The Tekton pipeline tests and builds an exact Git revision and records its
source SHA and pushed image digest. Use the resulting digest when deploying.
