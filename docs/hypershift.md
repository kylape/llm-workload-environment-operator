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
or upgrade those components. SharedIngress profiles also require cert-manager
with its Certificate and Issuer APIs available.

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
      apiServerPublishingStrategy: SharedIngress
      apiServerAddress: 10.14.217.16
      etcdStorageClass: lvms-a100-tier1-storage
      workerStorageClass: lvms-a100-tier1-storage
      controlPlaneNodeSelector:
        kubernetes.io/hostname: a100-03
      workerNodeSelector:
        node-role.kubernetes.io/a100-gpu: ""
      workerCores: 16
      workerMemory: 128Gi
      workerRootDisk: 258Gi
      maxReplicas: 10
      clusterNetwork: 10.244.0.0/16
      serviceNetwork: 172.29.0.0/16
```

Defaults are one worker per request, 16 CPU / 128Gi memory / 258Gi root disk,
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

`apiServerPublishingStrategy: SharedIngress` exposes client API access through
an LWE-owned TLS-passthrough Route on the host's existing ingress controller.
Its hostname is `api-lwe-<identity>.<baseDomain>` and its port is 443. The base
domain must resolve to the host ingress. Native APIServer publishing stays
NodePort: `apiServerAddress` is still required for worker connectivity.

LWE provisions a namespaced self-signed Issuer, a separate root CA Certificate,
a CA Issuer, and a serving Certificate for each environment. It waits for
current-generation Ready conditions and valid populated TLS Secrets **before
creating the HostedCluster**. The HostedCluster references that issued Secret
through `configuration.apiServer.servingCerts.namedCertificates` and sets
`kubeAPIServerDNSName`. Once HyperShift creates the matching control plane,
LWE creates a Route to its `kube-apiserver` Service and waits for admission.
It then publishes and probes a portable port-443 kubeconfig using the
cert-manager CA and HyperShift's administrator client credentials.

All private keys are generated by cert-manager. CA keys stay in the backing
namespace; LWE never imports HyperShift's signing key. The CA Certificate has
a ten-year duration and renews one year before expiry with the same key. The
API serving Certificate lasts 90 days, renews 30 days before expiry, and rotates
its key. Leaf renewal preserves the CA embedded in client kubeconfigs.
HyperShift propagates serving Secret changes to the guest API server. Root CA
rollover requires clients to retrieve updated trust data; saved kubeconfigs do
not update themselves. Issuers, Certificates and Secrets are removed with the
backing namespace; the Route belongs to the current HostedControlPlane.

The operator refuses foreign resources and target Secrets instead of adopting
them. Certificate or Route drift fails closed. Namespace and VCluster modes,
and HyperShift NodePort profiles, do not require cert-manager.

`apiServerPublishingStrategy: NodePort` publishes the original native API
kubeconfig. It requires `apiServerAddress` and allocates a separate NodePort per
cluster. Omitting the strategy also selects NodePort. Native
`apiServerPublishingStrategy: Route` instead requests HyperShift's dedicated
HCP router and requires a LoadBalancer provider on KubeVirt; it does not select
shared host ingress. Guest application Routes use HyperShift's existing host
ingress passthrough independently of the client API strategy.

The Pirate values preserve the existing `default` NodePort profile and add a
`shared-ingress` profile. Select `spec.hypershift.profile: shared-ingress` in new
requests to use cert-manager and host ingress. Existing environments keep their
original profile and provider identity; changing established profiles remains
unsupported. In particular, this does not automatically adopt or replace the
manual Route/certificate experiment on `ai-grid-hub`.

Pirate's worker selector targets the empty-valued
`node-role.kubernetes.io/a100-gpu` role label, allowing scheduling on all seven
A100 nodes. Its `lvms-a100-tier1-storage` class uses `WaitForFirstConsumer` and
provides node-local volumes across that pool. Each bound worker disk remains
tied to its selected node. Control-plane placement remains on `a100-03`.
The complete administrator values are in
[`config/examples/hypershift-pirate-values.yaml`](../config/examples/hypershift-pirate-values.yaml).

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
NodePorts. LWE does not create a route for every guest application. In the
validated deployment, HyperShift provides the guest default ingress certificate
using the same root CA embedded in its native HyperShift administrator
kubeconfig; use that CA for HTTPS verification, or configure your own guest
ingress certificate. SharedIngress LWE kubeconfigs trust the separate
cert-manager API CA; that CA does not sign guest application certificates. The
guest operator's separately generated `router-ca` is not necessarily its
active ingress trust root.

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
cleanup ordering, certificate-before-cluster sequencing, foreign certificate
resource rejection, stable trust across leaf renewal, shared Route admission,
and HyperShift Argo CD registration. Helm tests validate the
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


## Reference validation: 2026-10-29

Tekton PipelineRun `lwe-hypershift-build-8kwjz` tested and built source revision
`7a4c581bdb19a27b6d5d51016f277185a1e14c87`. The deployed public operator image is
`quay.io/klape/llm-workload-environment-operator@sha256:af6c42f0f79026c38ea264d2803a2255f7064dab9394fa2e75b19a910a4df859`.

A fresh LWE request on Pirate used the original profile: OpenShift 4.22.18,
KubeVirt workers on `a100-03`, local LVM, 4 cores / 16Gi / 64Gi per worker,
and 8Gi for etcd. Observations measured from the LWE request:

| Checkpoint | Observed time |
| --- | --- |
| HostedCluster created | 1 second |
| Guest API available | 47 seconds |
| First worker Ready | 5 minutes 42 seconds |
| LWE Ready with published kubeconfig | 9 minutes 35 seconds |

LWE readiness included full OpenShift installation; monitoring was the final
operator to complete. These are observations from one run on a shared host.
Image caches were not cleared; each worker received a fresh LVM root volume.
They are not a cold-host benchmark or a provisioning SLA.

Using the kubeconfig published by LWE, the test deployed an application in the
guest and received `Hello OpenShift!` over HTTPS through host ingress. Certificate
trust and hostname verification passed using the guest CA from that kubeconfig.

Changing the LWE worker count from one to two completed in approximately
4 minutes 30 seconds. Readiness and published credentials were withdrawn at
the new request generation, then restored once both workers were Ready. The
application remained available during scale-out and after it completed.

Deleting the LWE completed provider cleanup in approximately 4 minutes 30
seconds, including both VMs, the HostedCluster, NodePool, control-plane namespace,
backing namespace and associated PVs. All lifecycle actions in the final run
went through LWE. HyperShift managed the provider resources and ingress routes.

Management API lease-renewal requests timed out during cleanup and caused one
controller restart. The controller reacquired leadership and completed cleanup.
The final Deployment was available. These host API delays remain relevant when
interpreting provisioning and cleanup timings.

## Certificate-managed ingress validation: 2026-10-29

Tekton PipelineRun `lwe-cert-manager-build-r446z` tested and built runtime
source `73e809303f8b4bea479218af31deb66c4715cd36`. Its public image is pinned as
`quay.io/klape/llm-workload-environment-operator@sha256:664374ddb6b2aa080dc0d59b21e61fa6da875ada06de77294de7594a95b55756`.
Go race tests, vet, Helm lint and nine chart tests passed. Additional tests
reject stale certificate readiness, missing TLS Secrets, foreign resources,
changed issuer strategies, and certificates for the wrong hostname.

The fresh `lwe-cert-manager-test/lwe-cert-proof` request used Pirate's
`shared-ingress` profile, with 16 cores / 128Gi / 258Gi and A100 pool placement.
Observed UTC timestamps establish the dependency ordering:

| Resource checkpoint | Time |
| --- | --- |
| CA Certificate Ready | 18:53:29 |
| API serving Certificate Ready, TLS Secret populated | 18:53:34 |
| HostedCluster created | 18:53:39 |
| API Route admitted by host router `default` | 18:53:45 |

The Route uses TLS passthrough to the hosted API Service on port 6443.
Authenticated `/readyz` through host ingress succeeded with CA and hostname
verification. A forced `cmctl renew` advanced the serving Certificate from
revision 1 to 2 and changed its key. HyperShift propagated the Secret and
served the new certificate. The original CA bundle and existing test kubeconfig
continued to verify and authenticate successfully after renewal. No certificate
files or kubeconfigs were regenerated for that check.

The existing `klape-dev/ai-grid-hub` remained Ready on its original profile;
`KUEUE_ADMISSION_ENABLED=false` remained set on the deployed operator.
