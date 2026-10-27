"""Render the chart locally and verify operator configuration contracts."""

import json
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "charts/llm-workload-environment-operator"


def render(values=None, namespace="chart-test", chart=CHART):
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "values.yaml"
        path.write_text(yaml.safe_dump(values or {}))
        return subprocess.run(
            ["helm", "template", "lwe", str(chart), "--namespace", namespace,
             "--kube-version", "1.29.0", "--values", str(path)],
            capture_output=True, text=True,
        )


def resources(values=None, **kwargs):
    result = render(values, **kwargs)
    if result.returncode:
        raise AssertionError(result.stderr)
    return [obj for obj in yaml.safe_load_all(result.stdout) if obj]


def deployment(objects):
    return next(obj for obj in objects if obj["kind"] == "Deployment")


def environment(objects):
    container = deployment(objects)["spec"]["template"]["spec"]["containers"][0]
    return {item["name"]: item["value"] for item in container["env"]}


class HelmChartTests(unittest.TestCase):
    def test_flavor_configuration_and_boolean_serialization(self):
        for platform, label in (("coreweave", "kueue.x-k8s.io/managed"),
                                ("openshift", "kueue.openshift.io/managed")):
            for admission, validation in ((True, True), (True, False), (False, True), (False, False)):
                with self.subTest(platform=platform, admission=admission, validation=validation):
                    labels = {label: "true", "team": "inference"}
                    values = {"platform": platform,
                              "argocd": {"namespace": "guest-registration"},
                              "kueue": {"admissionEnabled": admission, "admissionValidationEnabled": validation,
                                        "namespaceLabels": labels,
                                        "configRef": "queue-system:custom-config:settings.yaml"}}
                    env = environment(resources(values))
                    self.assertTrue(all(isinstance(value, str) for value in env.values()))
                    self.assertEqual(env["PLATFORM"], platform)
                    self.assertEqual(env["ARGOCD_NAMESPACE"], "guest-registration")
                    self.assertEqual(env["KUEUE_ADMISSION_ENABLED"], str(admission).lower())
                    self.assertEqual(env["KUEUE_ADMISSION_VALIDATION_ENABLED"], str(validation).lower())
                    self.assertEqual(json.loads(env["KUEUE_NAMESPACE_LABELS"]), labels)
                    self.assertEqual(env["KUEUE_CONFIG_REF"], "queue-system:custom-config:settings.yaml")

    def test_default_security_and_neutral_labels(self):
        objects = resources()
        env = environment(objects)
        self.assertEqual(env["PLATFORM"], "")
        self.assertEqual(env["ARGOCD_NAMESPACE"], "")
        self.assertEqual(env["KUEUE_ADMISSION_ENABLED"], "true")
        self.assertEqual(env["KUEUE_ADMISSION_VALIDATION_ENABLED"], "true")
        self.assertEqual(json.loads(env["KUEUE_NAMESPACE_LABELS"]), {})
        source = yaml.safe_load((ROOT / "config/manager/deployment.yaml").read_text())
        pod = deployment(objects)["spec"]["template"]["spec"]
        original = source["spec"]["template"]["spec"]
        for key in ("securityContext", "volumes", "terminationGracePeriodSeconds"):
            self.assertEqual(pod[key], original[key])
        for key in ("args", "ports", "securityContext", "resources", "volumeMounts",
                    "livenessProbe", "readinessProbe"):
            self.assertEqual(pod["containers"][0][key], original["containers"][0][key])
        self.assertIn("@sha256:", pod["containers"][0]["image"])

    def test_namespace_and_scheduling_overrides(self):
        values = {"replicaCount": 2, "imagePullSecrets": [{"name": "registry-auth"}],
                  "nodeSelector": {"node-type": "cpu"},
                  "tolerations": [{"key": "dedicated", "operator": "Exists"}],
                  "affinity": {"nodeAffinity": {}},
                  "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}}}
        objects = resources(values, namespace="custom-operator")
        self.assertFalse(any(obj["kind"] == "Namespace" for obj in objects))
        for obj in objects:
            if obj["kind"] in ("ServiceAccount", "Deployment"):
                self.assertEqual(obj["metadata"]["namespace"], "custom-operator")
            if obj["kind"] == "ClusterRoleBinding":
                self.assertEqual(obj["subjects"][0]["namespace"], "custom-operator")
        dep = deployment(objects)
        self.assertEqual(dep["spec"]["replicas"], 2)
        pod = dep["spec"]["template"]["spec"]
        for key in ("imagePullSecrets", "nodeSelector", "tolerations", "affinity"):
            self.assertEqual(pod[key], values[key])
        self.assertEqual(pod["containers"][0]["resources"]["requests"], values["resources"]["requests"])

    def test_image_digest_takes_precedence_and_tag_is_supported(self):
        digest = "sha256:" + "a" * 64
        for image, expected in (({"repository": "registry.example/lwe", "tag": "v2", "digest": digest},
                                 f"registry.example/lwe@{digest}"),
                                ({"repository": "registry.example/lwe", "tag": "v2", "digest": ""},
                                 "registry.example/lwe:v2")):
            with self.subTest(image=image):
                dep = deployment(resources({"image": image}))
                self.assertEqual(dep["spec"]["template"]["spec"]["containers"][0]["image"], expected)

    def test_canonical_crd_and_clusterrole_permissions(self):
        objects = resources()
        crd = next(obj for obj in objects if obj["kind"] == "CustomResourceDefinition")
        canonical = yaml.safe_load((ROOT / "config/crd/bases/llm.redhat.com_llmworkloadenvironments.yaml").read_text())
        self.assertEqual(crd["spec"], canonical["spec"])
        roles = {obj["metadata"]["name"]: obj["rules"] for obj in objects if obj["kind"] == "ClusterRole"}
        expected = {}
        for path in (ROOT / "config/rbac").glob("*.yaml"):
            obj = yaml.safe_load(path.read_text())
            if obj["kind"] == "ClusterRole":
                expected[obj["metadata"]["name"]] = obj["rules"]
        self.assertEqual(roles, expected)

    def test_crd_retention_policy_and_disabling_crd_rendering(self):
        before = resources()
        crd = next(obj for obj in before if obj["kind"] == "CustomResourceDefinition")
        annotations = crd["metadata"]["annotations"]
        self.assertEqual(annotations["helm.sh/resource-policy"], "keep")
        self.assertEqual(annotations["argocd.argoproj.io/sync-options"], "Prune=false,Delete=false")
        self.assertEqual(annotations["argocd.argoproj.io/sync-wave"], "-1")
        after = resources({"crds": {"create": False}})
        self.assertEqual([obj for obj in before if obj["kind"] != "CustomResourceDefinition"], after)

    def test_invalid_values_fail_before_rendering(self):
        invalid = [
            {"platform": "eks"},
            {"argocd": {"namespace": "Bad_Namespace"}},
            {"kueue": {"admissionEnabled": "false"}},
            {"kueue": {"admissionValidationEnabled": "false"}},
            {"kueue": {"namespaceLabels": {"managed": True}}},
            {"kueue": {"namespaceLabels": {"llm.redhat.com/environment": "other"}}},
            {"kueue": {"configRef": "namespace:name"}},
            {"image": {"digest": "sha256:bad"}},
            {"crds": {"create": "false"}},
            {"replicaCount": 0},
            {"argocd": {"namesapce": "typo"}},
        ]
        for values in invalid:
            with self.subTest(values=values):
                result = render(values)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("schema", result.stderr)

    def test_packaged_chart_is_self_contained(self):
        with tempfile.TemporaryDirectory() as directory:
            subprocess.run(["helm", "package", str(CHART), "--destination", directory],
                           capture_output=True, text=True, check=True)
            archive = next(Path(directory).glob("*.tgz"))
            self.assertEqual(resources(chart=archive), resources())


if __name__ == "__main__":
    unittest.main()
