# Kueue namespace admission policy

The operator applies administrator-configured labels to each backing host
namespace and validates that it matches Kueue's configured Pod namespace
selector before reporting the LWE Ready or registering it with Argo CD.
These settings belong on the operator Deployment, rather than tenant LWE CRs.

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `KUEUE_ADMISSION_ENABLED` | `true` | Apply admission labels and require selector validation. Set `false` for an explicit administrator opt-out. |
| `KUEUE_NAMESPACE_LABELS` | `{"kueue.openshift.io/managed":"true"}` | JSON object of namespace label keys and string values. Replaces the default map; `{}` adds no labels. |
| `KUEUE_CONFIG_REF` | `kueue-system:kueue-manager-config:controller_manager_config.yaml` | Kueue configuration reference, formatted as `namespace:name:key`. |

Malformed booleans, label maps, Kubernetes names, and attempts to override LWE
identity labels fail operator startup. The default namespace label preserves
compatibility with existing OpenShift installations; it is not a universal
Kueue requirement. Configure the map to match the host installation's policy.

`KUEUE_CONFIG_REF` requires exactly three nonempty components. Each component
is validated as a Kubernetes namespace, ConfigMap name, or data key respectively.
Leaving the variable unset or empty uses the complete default reference.

For Kermit's current configuration:

```yaml
env:
  - name: KUEUE_ADMISSION_ENABLED
    value: "true"
  - name: KUEUE_NAMESPACE_LABELS
    value: '{"kueue.x-k8s.io/managed":"true"}'
  - name: KUEUE_CONFIG_REF
    value: kueue-system:kueue-manager-config:controller_manager_config.yaml
```

## Validation and readiness

Validation reads the ConfigMap and the backing namespace directly from the API
server. It requires the `pod` integration and an explicit namespace selector in
a `config.kueue.x-k8s.io/v1beta1` or `v1beta2` Configuration. It evaluates both
`matchLabels` and `matchExpressions` against the actual namespace labels.

The current `managedJobsNamespaceSelector` field takes precedence. For legacy
v1beta1 configurations, `integrations.podOptions.namespaceSelector` is supported
when the current field is absent. Implicit version-dependent defaults are not
guessed: configure an explicit selector when validation is enabled. Kueue's
[Pod troubleshooting documentation](https://kueue.sigs.k8s.io/docs/tasks/troubleshooting/troubleshooting_pods/)
describes the selector migration.

Failures set `Ready=False` with one of these reasons:

* `KueueNamespaceNotSelected`: the backing namespace does not match the selector.
* `KueueAdmissionConfigurationInvalid`: configuration cannot be interpreted,
  the Pod integration is disabled, or no explicit selector is available.
* `KueueAdmissionValidationFailed`: the ConfigMap or namespace cannot be read,
  including RBAC denials and a missing ConfigMap.

Failed validation retries after 30 seconds. Ready environments are checked
again every five minutes, including environments with no Argo CD profile.
The existing operator ClusterRole already grants ConfigMap and namespace reads;
custom RBAC must allow reading the configured ConfigMap namespace too.

The operator records its applied labels in
`llm.redhat.com/kueue-namespace-labels`. Changing the map removes retired labels
whose values still match the operator's record and preserves unrelated labels.
Existing operator-owned namespaces migrate away from the historical
`kueue.openshift.io/managed=true` label when a different map is configured.

## Administrator opt-out

Set `KUEUE_ADMISSION_ENABLED=false` to skip validation and stop applying these
namespace labels. Reconciliation removes previously applied labels when their
values still match the recorded values. Administrator edits to retired labels
are preserved. ClusterQueue authorization and the default LocalQueue lifecycle
remain in place.

This switch controls the LWE integration. It cannot exempt a namespace from a
host Kueue selector that already matches it, or override other host admission
policies. Administrators must configure host policy consistently with the opt-out.

## Limits of the check

This validates configured namespace eligibility, not end-to-end admission.
It does not inspect webhook selectors, queue quota, Pod selectors, or Kueue
controller health. Pods must still carry the appropriate queue label. The
configured ConfigMap must be the one the running Kueue controller uses;
[Kueue requires a restart](https://kueue.sigs.k8s.io/docs/installation/)
to activate configuration changes. A ConfigMap can therefore differ from running
policy until that restart completes.

Changing namespace labels does not retroactively admit existing running Pods.
After enabling admission, recreate a workload Pod through its normal controller
and confirm that Kueue creates and admits a Workload. Existing applications keep
running when validation subsequently fails; readiness is not a workload
suspension mechanism.
