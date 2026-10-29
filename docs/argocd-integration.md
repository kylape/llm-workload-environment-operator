# Argo CD cluster registration

The operator can register VCluster and HyperShift environments with an existing Argo CD
instance. Argo CD ApplicationSets then select an application-profile label to
deploy applications into those environments.

## Administrator configuration

Set `ARGOCD_NAMESPACE` on the operator's manager container to the namespace
where Argo CD reads cluster Secrets. For the CKS demo, this will be
`llm-workload-environment-operator`, alongside the operator. Retain
`PLATFORM=coreweave` for CKS.

The default empty setting disables registration. Invalid namespace names stop
manager startup. Changing the namespace removes recorded registrations from the
old namespace before creating replacements. Clearing the setting cleans up
recorded registrations; it does not leave their credentials behind.

Install the updated LWE CRD before rolling out this controller. The optional
`status.argoCDClusterSecretRef` field is needed to record registration ownership
before creating a Secret. The controller verifies the persisted reference with
an uncached API read and refuses registration if an old schema prunes it.

The existing operator ClusterRole already grants the required Secret
permissions. Tenant requester permissions do not change. The operator does not
install Argo CD, create Applications, or manage ApplicationSets.

## Request registration

Add this label to a VCluster- or HyperShift-mode LWE:

```yaml
metadata:
  labels:
    llm.redhat.com/application-profile: demo
```

The presence of the label requests registration; its value selects an
administrator-defined profile. It can be added after the environment is ready.
See the [complete example](../config/samples/llm_v1alpha1_llmworkloadenvironment_argocd.yaml).
Namespace-mode requests continue provisioning normally but report
`ArgoCDRegistered=False` with reason `UnsupportedMode` if labeled.

Registration waits for the environment's finalized kubeconfig. On CKS it uses
the public LoadBalancer endpoint already published by the operator, with the
same embedded authentication and TLS settings. Argo CD does not receive host
administrator credentials. Embedded bearer tokens and client certificate/key
pairs are supported. Credential file paths, exec/auth-provider plugins, basic
authentication, impersonation, and proxy options are rejected.

## Registration Secret

The operator creates an Opaque Secret named `lwe-<environment UID>` in the Argo CD
namespace. Its `name`, `server`, and JSON `config` data follow Argo CD's cluster
registration format. Cluster identity remains stable across profile changes and
opt-out / opt-in, and differs when an LWE is deleted and recreated.

The Secret receives Argo CD's required label
`argocd.argoproj.io/secret-type: cluster` and a copy of
`llm.redhat.com/application-profile`. No other LWE labels are forwarded. Ownership
annotations record the originating LWE namespace/name and UID; cross-namespace
owner references are not used.

The operator synchronizes credentials, endpoint, and profile, repairs a deleted
registration, and rejects collisions with unrelated Secrets. Source credential
and registration Secret watches trigger reconciliation. Registered environments
also resynchronize every five minutes to recover from missed events.

## Readiness and lifecycle

`Ready` continues to describe environment provisioning. The independent
`ArgoCDRegistered` condition describes registration:

| Status / reason | Meaning |
| --- | --- |
| True / Registered | The desired cluster Secret is synchronized. |
| False / NotRequested | No profile label; any recorded registration is removed. |
| False / IntegrationDisabled | A profile was requested but the target namespace is unset. |
| False / UnsupportedMode | Registration was requested for Namespace mode. |
| False / WaitingForEnvironment | Waiting for ready environment credentials. |
| Unknown / EnvironmentUnavailable | Environment credentials cannot be refreshed; the recorded registration is retained. |
| False / Unregistering | Waiting to confirm registration deletion. |
| False / RegistrationFailed | Conversion, configuration, ownership, write, or cleanup failed; reconciliation retries. |

Successful Secret publication does not prove connectivity or application health;
those are reported by Argo CD. Registration failure does not clear a provisioned
environment's readiness or credentials.

Removing the profile label deletes the owned registration Secret, including
when the environment's queue is no longer allowed. It leaves the environment
running. Deleting the LWE removes its registration before the operator finishes
environment cleanup. A temporary provisioning failure retains the existing
registration rather than automatically unregistering it.

## ApplicationSet responsibilities

Configure an ApplicationSet cluster generator in the Argo CD namespace to select
both `argocd.argoproj.io/secret-type: cluster` and the desired application-profile
value. Keep repository, revision, application paths, and destination permissions
in administrator-controlled ApplicationSet/AppProject configuration.

To implement the demo's preserve-on-opt-out policy, set
`spec.syncPolicy.preserveResourcesOnDeletion: true` on the ApplicationSet from
its initial creation, allow deletion of generated Applications, and do not add a
resource-deletion finalizer to them. Removing the cluster selection then removes
the generated Application while leaving its resources running unmanaged in the
vCluster. The operator alone cannot enforce this Argo CD policy.

Reapplying the same label restores the stable registration identity so the
ApplicationSet can resume management. Changing profiles can leave resources
from the previous profile behind. Deleting the environment still destroys the
vCluster; preserve-on-opt-out does not preserve a deleted environment.

## References

* [Argo CD cluster Secret format](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#clusters).
* [ApplicationSet cluster generator](https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Generators-Cluster/).
* [Application resource preservation](https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Controlling-Resource-Modification/).
