# LLM Workload Environment operator Helm chart

This chart installs the operator Deployment, ServiceAccount, four ClusterRoles,
ClusterRoleBinding, and LLMWorkloadEnvironment CRD. Install one operator per
cluster: the CRD and ClusterRole names are cluster-wide and are referenced by
the operator. The release namespace is configurable; the chart does not create
or manage that Namespace itself.

The default runtime image is pinned by digest. Chart version `0.3.0` packages
HyperShift mode, optional Kueue integration, and the existing Namespace and
VCluster modes. It does not install HyperShift, OpenShift Virtualization,
Kueue, Argo CD, or workload environments. HyperShift is disabled by default.
The image retains the pinned vCluster chart and scheduling-gate preservation fix.

Tekton PipelineRun `lwe-hypershift-build-ldb2v` on Pirate tested and built
operator revision `1bf2ffa116c8c32f880ef5651850ab0c118b7488` and published
`quay.io/klape/llm-workload-environment-operator:git-1bf2ffa`. Its recorded image
digest is `sha256:34c28dde797a8c2232ed8486ee2ec473322ca038f5cfecf4106ccd016aebb472`.
The image was verified anonymously readable from Quay.

## Installation

Choose host-specific values, then install from the LWE repository root:

```bash
helm upgrade --install lwe charts/llm-workload-environment-operator \
  --namespace llm-workload-environment-operator --create-namespace \
  --values /path/to/cks.yaml
```

The following examples belong in the deploying GitOps repository. Cluster
Applications should select their host flavor and optionally layer a
cluster-specific values file. Creating another values file does not add support
for a new vCluster exposure backend; the operator currently supports the
`coreweave` and `openshift` platform values.

### CKS values

```yaml
platform: coreweave
kueue:
  namespaceLabels:
    kueue.x-k8s.io/managed: "true"
```

### OpenShift values

```yaml
platform: openshift
kueue:
  namespaceLabels:
    kueue.openshift.io/managed: "true"
```

Set the labels to match the actual host Kueue Pod namespace selector. The chart
default is an empty map, which applies no additional namespace labels. With
both Kueue switches enabled, admission validation refuses provisioning if Kueue does not select
the backing namespace. The standalone Kustomize Deployment retains its existing
OpenShift label default; Helm values deliberately make this choice explicit.

`platform: ""` permits OpenShift auto-detection. Configure `coreweave` explicitly
for CKS public vCluster endpoints. No OpenShift cluster registration or Argo CD
Application is created by adding these examples.

## Configuration

| Value | Meaning |
| --- | --- |
| `platform` | Empty, `coreweave`, or `openshift`; maps to `PLATFORM` |
| `hypershift.enabled`, `hypershift.profiles` | Opt-in hosted OpenShift provisioning with administrator-managed profiles; see [setup](../../docs/hypershift.md) |
| `argocd.namespace` | Host Argo CD namespace for cluster Secrets; empty disables registration |
| `kueue.admissionEnabled` | Main integration switch mapped to `KUEUE_ADMISSION_ENABLED`; defaults to `true` |
| `kueue.admissionValidationEnabled` | Namespace admission check mapped to `KUEUE_ADMISSION_VALIDATION_ENABLED`; defaults to `true` |
| `kueue.namespaceLabels` | String map serialized to JSON for `KUEUE_NAMESPACE_LABELS` |
| `kueue.configRef` | `namespace:name:data-key` mapped unchanged to `KUEUE_CONFIG_REF` |
| `image.repository`, `image.digest`, `image.tag` | Image digest takes precedence over tag; clear digest to use a tag |
| `image.pullPolicy`, `imagePullSecrets` | Image pull settings |
| `replicaCount` | Defaults to one; leader election remains enabled |
| `resources` | CPU/memory requests and limits, matching the standalone defaults |
| `nodeSelector`, `tolerations`, `affinity` | Optional scheduling settings |
| `crds.create` | Render the CRD as an ordinary template; defaults to `true` |

The namespace-label map requires string values, so quote values such as
`"true"`. Helm's values schema rejects booleans in this map, string-valued
admission switches, unsupported platform names, and misspelled configuration
keys. The operator performs its existing Kubernetes label and configuration
validation at runtime.

`kueue.admissionEnabled: false` allows Namespace, VCluster and HyperShift environments
to provision without Kueue installed. It skips queue authorization, Kueue API
discovery and resource reads/writes, namespace labeling, and admission validation.
It takes precedence over `kueue.admissionValidationEnabled`. Existing LocalQueues
and Kueue namespace metadata are left untouched; host webhook policies remain
under the cluster administrator's control.

`kueue.admissionValidationEnabled: false` skips only namespace admission
validation. It keeps ClusterQueue/LocalQueue checks and namespace labeling.
For an installation that used the previous validation-only meaning of
`kueue.admissionEnabled: false`, migrate to:

```yaml
kueue:
  admissionEnabled: true
  admissionValidationEnabled: false
```

Both values default to `true`. See the operator's
[Kueue configuration documentation](../../docs/kueue-admission.md) for details.

The default Argo CD namespace is empty. To enable registration against an
existing instance on the host cluster:

```yaml
argocd:
  namespace: argocd
```

Changing this value to empty cleans up recorded registrations. This chart does
not deploy, uninstall, or otherwise manage that Argo CD instance.

## Argo CD deployment

Argo CD can render the chart directly from this Git repository; a chart registry
is not required. Pin `targetRevision` to a reviewed commit, set the source path
to `charts/llm-workload-environment-operator`, and supply values through Helm
configuration. To keep flavor values in a different GitOps repository, use
Argo CD's multiple-source Applications with a values-only Git source.

Use `CreateNamespace=true` in the Application, since the chart leaves namespace
creation to its caller. `ServerSideApply=true` supports CRD updates without
client-side annotation size limits. The AppProject must permit the operator's
Namespace, CustomResourceDefinition, ClusterRole, ClusterRoleBinding,
ServiceAccount, and Deployment resources at the configured destination.

## CRD updates and future ownership changes

The CRD is an ordinary template, not a Helm hook or a file under `crds/`.
Helm upgrades and Argo CD syncs therefore update its schema. It carries:

```yaml
metadata:
  annotations:
    helm.sh/resource-policy: keep
    argocd.argoproj.io/sync-options: Prune=false,Delete=false
    argocd.argoproj.io/sync-wave: "-1"
```

These retention policies protect the CRD when it disappears from the old
release/Application or that release/Application is removed. They do not prevent
an administrator from explicitly deleting the CRD through the Kubernetes API.
Deleting the CRD deletes its LWE resources.

For a future move to another chart or to `crds/`:

1. Deploy the retention annotations in the old chart first and verify them on
   the live CRD. Do this before removing its template.
2. Set `crds.create: false` in the old deployment or remove the old template.
   The retained CRD and its LWE resources stay in the cluster. Argo CD can report
   skipped pruning until its resource tracking is handed off.
3. Transfer ownership deliberately to the new release/Application. Native Helm
   release ownership metadata and Argo CD tracking are separate; annotations
   alone do not adopt a resource into another release. Avoid two controllers
   reconciling the CRD concurrently.

Moving to Helm's `crds/` directory changes native Helm upgrade behavior: existing
CRDs are skipped rather than updated. Plan future schema updates separately if
making that change. Retention does not validate schema compatibility; review
CRD changes for compatibility with stored LWEs.

## Development and checks

The canonical CRD and RBAC remain under `config/`. Their packaged copies live
in this chart's `files/` directory so the chart can be packaged and rendered
without files outside the chart. After changing the canonical manifests:

```bash
make generate-chart
```

Install the test dependency and run the checks:

```bash
python3 -m pip install -r tools/requirements-chart.txt
make test-chart
```

Checks require Helm 3 and Python 3. They validate packaged-file freshness, lint
the chart, render CKS/OpenShift configurations, compare CRD/RBAC to canonical
manifests, reject invalid values, and render the packaged archive. They contact
no cluster. CI runs the same target for chart and canonical-manifest changes.
