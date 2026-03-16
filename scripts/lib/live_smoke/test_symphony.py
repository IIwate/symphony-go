from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path


LIB_ROOT = Path(__file__).resolve().parents[1]
if str(LIB_ROOT) not in sys.path:
    sys.path.insert(0, str(LIB_ROOT))

from live_smoke import symphony


class SymphonyConfigWriterTest(unittest.TestCase):
    def test_write_smoke_config_uses_formal_schema(self) -> None:
        with tempfile.TemporaryDirectory() as tmp_dir:
            base_dir = Path(tmp_dir)
            config = symphony.SmokeConfig(
                base_dir=base_dir,
                port=8123,
                namespace="live-smoke",
                repo_url="https://github.com/example/repo",
                linear_api_key="linear-key",
                linear_project_slug="proj",
                linear_branch_scope="scope",
                session_state_path=base_dir / "local" / "runtime-state.json",
                notification_port=9100,
            )
            symphony.write_smoke_config(config, prompt_text="formal smoke")

            project_yaml = (base_dir / "project.yaml").read_text(encoding="utf-8")
            source_yaml = (base_dir / "sources" / "linear-main.yaml").read_text(encoding="utf-8")
            flow_yaml = (base_dir / "flows" / "implement.yaml").read_text(encoding="utf-8")
            before_run_hook = (base_dir / "hooks" / "before_run.py").read_text(encoding="utf-8")
            continuation_hook = (base_dir / "hooks" / "before_run_continuation.py").read_text(encoding="utf-8")

            self.assertIn("service:\n", project_yaml)
            self.assertIn("  notifications:\n", project_yaml)
            self.assertIn("url_ref:\n", project_yaml)
            self.assertIn("incoming_webhook_url_ref:\n", project_yaml)
            self.assertIn("domain:\n", project_yaml)
            self.assertIn("execution:\n", project_yaml)
            self.assertIn("job_policy:\n", project_yaml)
            self.assertIn("persistence:\n", project_yaml)
            self.assertNotIn("runtime:\n", project_yaml)
            self.assertNotIn("selection:\n", project_yaml)
            self.assertNotIn("incoming_webhook_url:", project_yaml)
            self.assertIn("before_run: hooks/before_run.py\n", flow_yaml)
            self.assertIn("before_run_continuation: hooks/before_run_continuation.py\n", flow_yaml)
            self.assertIn('os.environ["SYMPHONY_GIT_REPO_URL"]', before_run_hook)
            self.assertIn('subprocess.run(["git", "fetch", "--all", "--prune"], check=False)', continuation_hook)

            self.assertIn("credentials:\n", source_yaml)
            self.assertIn("api_key_ref:\n", source_yaml)
            self.assertNotIn("\napi_key:", source_yaml)

    def test_write_doctor_config_uses_formal_schema(self) -> None:
        with tempfile.TemporaryDirectory() as tmp_dir:
            base_dir = Path(tmp_dir)
            symphony.write_doctor_config(base_dir)

            project_yaml = (base_dir / "project.yaml").read_text(encoding="utf-8")
            self.assertIn("service:\n", project_yaml)
            self.assertIn("execution:\n", project_yaml)
            self.assertIn("job_policy:\n", project_yaml)
            self.assertIn("persistence:\n", project_yaml)
            self.assertNotIn("runtime:\n", project_yaml)
            self.assertNotIn("selection:\n", project_yaml)


if __name__ == "__main__":
    unittest.main()
