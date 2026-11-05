# SNO mode

`mode: SNO` requests a complete independent OpenShift cluster in a VM. LWE owns
the environment lifecycle and delegates the cluster implementation to
[`sno-vm-operator`](https://github.com/kylape/sno-vm-operator) through its
`sno.llm-d.io/v1alpha1` `SNOVirtualMachine` API. LWE does not build images, run
recert, or allocate guest addresses. The provider remains independently usable.

## Platform prerequisites

* OpenShift Virtualization and the SNO operator API/controller.
* SNO operator runtime permission to create, get, update, and delete Secrets
  across namespaces, so it can provision bootstrap and guest credentials in
  the namespace containing each SNO request.
* A provider-qualified preinstalled seed image and usable local cache capacity.
* Provider-managed DNS, unique guest identity, and external API access.
* The ordinary LWE landing-namespace RBAC and Kueue configuration, if enabled.

The LWE adapter implements the request, readiness, credential handoff, and
deletion contract. The provider must have its instance reconciler enabled and a
qualified seed image configured. Seed image qualification, clone identity
isolation, and the startup target still need end-to-end validation; this mode
makes no five-minute startup claim.

## Request and access

Apply the [sample request](../config/samples/llm_v1alpha1_llmworkloadenvironment_sno.yaml).
LWE creates its backing namespace, labels it `sno.klape.io/enabled=true`, and
creates a request named `sno` using the selected `spec.sno.size` and
`spec.sno.imageProfile`. The size is immutable after creation and defaults to
`small`. `small` requests 16 cores and 128 GiB RAM, `medium` requests 32 cores
and 256 GiB RAM, and `large` requests 64 cores and 512 GiB RAM. Each size uses a
500 GiB root disk. These CPU and memory mappings match the default HyperShift
sizes. The provider controls image eligibility and placement.
The provider reads its configured OpenShift pull Secret and copies its contents
into bootstrap data for the guest. The guest API kubeconfig is published in the
`sno-credentials` Secret in the SNO request namespace.

The mode and SNO options are immutable. To select a different profile, create
a new environment. The provider API is optional at operator startup; its absence
reports `SNOProviderNotAvailable` and never yields host-cluster credentials.

LWE waits for the provider's current-generation `Ready=True`, an access Secret
owned by that exact SNO request UID, and a successful authenticated guest API
readiness check. It accepts only portable credentials with an embedded guest
CA, TLS verification, and the published HTTPS API URL.
It publishes the kubeconfig using the existing base64 `status.kubeconfig`
contract and records `status.snoVirtualMachineRef` for identity and deletion.
Readers of the LWE resource can obtain its administrator kubeconfig; constrain
that access with namespace RBAC.

The provider's phase-one access contract exposes the guest Kubernetes API outside
the host cluster. Application routes are available internally; external guest
application publication is a later phase. ACM installation is outside scope.

## Deletion and failures

LWE requests `rootDiskRetentionPolicy: Delete`. On user deletion and expiration,
it deletes the provider request and waits until it disappears before deleting
the backing namespace, allowing the provider's storage finalizer to complete.
Provider caches are shared and remain under provider administration.

An established SNO request that disappears is not silently recreated. A child
with a different UID, changed profile, or changed retention policy requires
administrator review. Retention changed to `Retain` blocks LWE cleanup so that
namespace deletion cannot discard intentionally retained storage. Provider API
loss during established cleanup also leaves the LWE finalizer in place.

## Development validation

Run `go test -race ./...`, `go vet ./...`, `make test-api`, and `make test-chart`.
The schema check starts an isolated local API server and etcd using pinned test
binaries; it does not connect to the host cluster. Controller tests cover child
ownership, missing providers, current-generation readiness, credential validation,
identity replacement, and storage-aware deletion ordering.
