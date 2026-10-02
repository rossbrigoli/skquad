"""Fast cross-component integration smoke tests for skquad.

These tests deliberately stay CI-friendly: they use unit-test fakes and Helm
renders instead of requiring a live Kubernetes cluster. Cluster admission and
full rollout checks remain separate operational verification.
"""

from __future__ import annotations

import os
import subprocess
import sys
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]


class IntegrationSmokeTest(unittest.TestCase):
    maxDiff = 2000

    def test_control_plane_runtime_operator_contracts(self) -> None:
        """Exercise the core API -> outbox -> operator -> runtime contracts."""

        self.run_command(
            [
                "go",
                "test",
                "./internal/httpapi",
                "-run",
                (
                    "TestAgentIdentityCreateAndRotate|"
                    "TestAgentRuntimeTaskClaimAndStatusFlow|"
                    "TestAgentRuntimeTaskContextIncludesScopedMemory|"
                    "TestAssignedTaskMirrorsAgentBusyAndCompletionMirrorsIdle|"
                    "TestAgentMessagingInboxFlow"
                ),
            ],
            cwd=REPO_ROOT / "control-plane",
        )
        self.run_command(
            [
                "go",
                "test",
                "./internal/kube",
                "-run",
                (
                    "TestProcessOutboxOnceAppliesQueuedSquadAndAgentEvents|"
                    "TestUpsertAgentMapsGeneratedSecretRefs|"
                    "TestWriteAgentCredentialAppliesOpaqueSecret"
                ),
            ],
            cwd=REPO_ROOT / "control-plane",
        )
        self.run_command(
            [
                "go",
                "test",
                "./internal/controller",
                "-run",
                (
                    "TestSquadReconcilerCreatesNamespace|"
                    "TestAgentReconcilerCreatesDeployment|"
                    "TestAgentReconcilerFinalizerDeletesDeployment"
                ),
            ],
            cwd=REPO_ROOT / "operator",
        )
        self.run_command(
            [sys.executable, "-m", "unittest", "discover", "-s", "tests"],
            cwd=REPO_ROOT / "agent-runtime",
            env={"PYTHONPATH": str(REPO_ROOT / "agent-runtime")},
        )

    def test_helm_render_preserves_runtime_and_gateway_contracts(self) -> None:
        rendered = self.run_command(
            [
                "helm",
                "template",
                "skquad",
                "charts/skquad",
                "--namespace",
                "skquad-system",
                "--include-crds",
            ],
            cwd=REPO_ROOT,
        ).stdout

        expected_fragments = [
            "kind: CustomResourceDefinition",
            "name: agents.skquad.io",
            "credentialSecret:",
            "virtualKeySecret:",
            "desiredActive:",
            "name: skquad-api-server",
            "name: SKQUAD_K8S_ENABLED",
            "name: SKQUAD_AGENT_IMAGE",
            "name: SKQUAD_CONTROL_PLANE_URL",
            "name: SKQUAD_LLM_GATEWAY_URL",
            "name: SKQUAD_LITELLM_ADMIN_URL",
            "name: SKQUAD_LITELLM_MASTER_KEY",
            "name: skquad-llm-gateway",
            "name: DATABASE_URL",
            "name: LITELLM_MASTER_KEY",
            "name: SKQUAD_GATEWAY_CALLBACK_TOKEN",
            "name: skquad-operator",
            "name: SKQUAD_AGENT_TASK_POLL_INTERVAL_SECONDS",
            "name: SKQUAD_AGENT_INBOX_POLL_INTERVAL_SECONDS",
            "name: SKQUAD_AGENT_TASK_TIMEOUT_SECONDS",
            "name: SKQUAD_AGENT_MAX_LLM_STEPS",
            "name: skquad-web",
            "name: SKQUAD_API_BASE_URL",
            "name: NEXT_PUBLIC_SKQUAD_API_BASE_URL",
        ]
        for fragment in expected_fragments:
            with self.subTest(fragment=fragment):
                self.assertIn(fragment, rendered)

    def test_helm_embedder_auto_mode_renders_cpu_only_fallback(self) -> None:
        """Default/auto mode must install cleanly on clusters with no GPU nodes."""

        renders = {
            "default": self.run_command(
                [
                    "helm",
                    "template",
                    "skquad",
                    "charts/skquad",
                    "--namespace",
                    "skquad-system",
                ],
                cwd=REPO_ROOT,
            ).stdout,
            "explicit-auto-no-gpu-resource": self.run_command(
                [
                    "helm",
                    "template",
                    "skquad",
                    "charts/skquad",
                    "--namespace",
                    "skquad-system",
                    "-f",
                    "-",
                ],
                cwd=REPO_ROOT,
                stdin=(
                    "embedder:\n"
                    "  gpu:\n"
                    "    mode: auto\n"
                    "    resourceNames: example.com/gpu\n"
                    "    devicePaths: []\n"
                ),
            ).stdout,
        }

        for name, rendered in renders.items():
            with self.subTest(render=name):
                deployment = self.extract_rendered_doc(
                    rendered,
                    kind="Deployment",
                    object_name="skquad-embedder",
                )
                self.assertIn('cpu: "1"', deployment)
                self.assertNotIn("nvidia.com/gpu:", deployment)
                self.assertNotIn("amd.com/gpu:", deployment)
                self.assertNotIn("gpu.intel.com/i915:", deployment)
                self.assertNotIn("example.com/gpu:", deployment)
                self.assertNotIn("nodeAffinity:", deployment)

    def run_command(
        self,
        args: list[str],
        *,
        cwd: Path,
        env: dict[str, str] | None = None,
        stdin: str | None = None,
    ) -> subprocess.CompletedProcess[str]:
        merged_env = os.environ.copy()
        if env:
            merged_env.update(env)
        result = subprocess.run(
            args,
            cwd=cwd,
            env=merged_env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            input=stdin,
            check=False,
        )
        if result.returncode != 0:
            self.fail(
                f"{' '.join(args)} failed in {cwd} with exit {result.returncode}\n"
                f"{result.stdout}"
            )
        return result

    def extract_rendered_doc(self, rendered: str, *, kind: str, object_name: str) -> str:
        for doc in rendered.split("\n---"):
            if f"\nkind: {kind}\n" in f"\n{doc}\n" and f"\n  name: {object_name}\n" in f"\n{doc}\n":
                return doc
        self.fail(f"rendered {kind}/{object_name} not found")


if __name__ == "__main__":
    unittest.main()
