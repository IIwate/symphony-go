from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch


LIB_ROOT = Path(__file__).resolve().parents[1]
if str(LIB_ROOT) not in sys.path:
    sys.path.insert(0, str(LIB_ROOT))

from live_smoke import cli


def _base_object(object_type: str, object_id: str) -> dict[str, object]:
    return {
        "id": object_id,
        "object_type": object_type,
        "domain_id": "default",
        "visibility": "restricted",
        "contract_version": "v1",
        "created_at": "2026-03-16T00:00:00Z",
        "updated_at": "2026-03-16T00:00:01Z",
    }


def _linear_reference(identifier: str, external_id: str) -> dict[str, object]:
    return {
        **_base_object("reference", f"ref-{identifier}"),
        "state": "active",
        "type": "linear_issue",
        "system": "linear",
        "locator": identifier,
        "external_id": external_id,
        "display_name": identifier,
    }


def _job(identifier: str, state: str) -> dict[str, object]:
    return {
        **_base_object("job", f"job-{identifier}"),
        "state": state,
        "job_type": "land_change",
        "action_summary": {
            "has_pending_external_actions": False,
            "pending_count": 0,
        },
        "references": [_linear_reference(identifier, external_id="linear-id")],
    }


def _run(identifier: str, state: str, phase: str) -> dict[str, object]:
    return {
        **_base_object("run", f"run-{identifier}-1"),
        "state": state,
        "phase": phase,
        "attempt": 1,
        "candidate_delivery": {
            "kind": "pull_request",
            "reached": True,
            "reached_at": "2026-03-16T00:00:02Z",
            "summary": "候选交付点已达到。",
            "artifact_ids": ["artifact-pr-1"],
        },
        "references": [
            _linear_reference(identifier, external_id="linear-id"),
            {
                **_base_object("reference", f"ref-pr-{identifier}"),
                "state": "active",
                "type": "github_pull_request",
                "system": "github",
                "locator": "https://example.test/pull/12",
                "url": "https://example.test/pull/12",
                "external_id": "12",
            },
        ],
    }


class LiveSmokeCliContractTest(unittest.TestCase):
    def test_doctor_missing_codex_command_accepts_legacy_and_formal_paths(self) -> None:
        for path in ("runtime.codex.command", "execution.backend.codex.command"):
            with self.subTest(path=path):
                output = f"missing required secrets:\n- CODEX_COMMAND ({path})\n"
                self.assertTrue(cli._doctor_reports_missing_codex_command(output))

    def test_doctor_missing_codex_command_rejects_unrelated_output(self) -> None:
        output = "other configuration errors:\n- source_adapter.branch_scope is required for linear source\n"
        self.assertFalse(cli._doctor_reports_missing_codex_command(output))

    def test_doctor_missing_codex_command_accepts_structural_validation_output(self) -> None:
        output = "invalid_codex_command: execution.backend.codex.command is required\n"
        self.assertTrue(cli._doctor_reports_missing_codex_command(output))

    def test_run_doctor_and_set_accepts_formal_missing_secret_output(self) -> None:
        with tempfile.TemporaryDirectory() as tmp_dir:
            resources = cli.Resources(temp_dir=Path(tmp_dir), binary_path=Path("symphony.exe"))
            doctor = SimpleNamespace(
                returncode=1,
                stdout="missing required secrets:\n- CODEX_COMMAND (execution.backend.codex.command)\n",
                stderr="",
            )

            with patch.object(cli, "run", return_value=doctor):
                self.assertEqual(cli._run_doctor_and_set(resources), "doctor ok")

    def test_state_surface_rejects_legacy_runtime_records(self) -> None:
        payload = {
            "generated_at": "2026-03-16T00:00:00Z",
            "service_mode": "serving",
            "recovery_in_progress": False,
            "reasons": [],
            "instance": {"id": "automation", "name": "symphony", "version": "dev", "role": "leader"},
            "capabilities": {"capabilities": []},
            "records": [],
        }
        with self.assertRaises(RuntimeError):
            cli._assert_state_surface(payload)

    def test_event_envelope_accepts_formal_objects_and_rejects_record_ids(self) -> None:
        payload = {
            "event_id": "evt-1",
            "event_type": "object_changed",
            "timestamp": "2026-03-16T00:00:00Z",
            "contract_version": "v1",
            "domain_id": "default",
            "service_mode": "serving",
            "objects": [{"object_type": "job", "object_id": "job-ABC-1", "visibility": "restricted"}],
            "reason": None,
        }
        cli._assert_event_envelope(payload, "object_changed")

        with_record_ids = dict(payload)
        with_record_ids["record_ids"] = ["legacy"]
        with self.assertRaises(RuntimeError):
            cli._assert_event_envelope(with_record_ids, "object_changed")

    def test_snapshot_event_envelope_allows_missing_objects(self) -> None:
        payload = {
            "event_id": "evt-1",
            "event_type": "snapshot",
            "timestamp": "2026-03-16T00:00:00Z",
            "contract_version": "v1",
            "domain_id": "default",
            "service_mode": "serving",
            "reason": None,
        }
        cli._assert_event_envelope(payload, "snapshot")

    def test_object_changed_event_requires_objects(self) -> None:
        payload = {
            "event_id": "evt-1",
            "event_type": "object_changed",
            "timestamp": "2026-03-16T00:00:00Z",
            "contract_version": "v1",
            "domain_id": "default",
            "service_mode": "serving",
            "reason": None,
        }
        with self.assertRaises(RuntimeError):
            cli._assert_event_envelope(payload, "object_changed")

    def test_object_query_surface_rejects_reference_type(self) -> None:
        payload = {
            "object_type": "reference",
            "item": _linear_reference("ABC-1", external_id="linear-id"),
        }
        with self.assertRaises(RuntimeError):
            cli._assert_object_query_response(payload, "reference")

    def test_object_list_surface_rejects_reason_type(self) -> None:
        payload = {
            "object_type": "reason",
            "items": [],
        }
        with self.assertRaises(RuntimeError):
            cli._assert_object_list_response(payload, "reason")

    def test_linear_branch_name_matches_runtime_truncation(self) -> None:
        branch = cli._linear_branch_name(
            "live-smoke-local-20260317-030913-38256-feature",
            "integration-scope",
            "IIWATE-297",
        )
        self.assertEqual(branch, "live-smoke-local-20260317-03/linear-integration-scope-iiwate-297")
        self.assertLessEqual(len(branch), 64)

    def test_session_state_surface_uses_formal_objects_snapshot(self) -> None:
        payload = {
            "version": 6,
            "identity": {
                "Compatibility": {
                    "Profile": "default",
                    "ActiveSource": "linear-main",
                    "SourceKind": "linear",
                    "FlowName": "implement",
                    "TrackerKind": "linear",
                    "TrackerRepo": "repo",
                    "TrackerProjectSlug": "proj",
                },
                "Descriptor": {
                    "ConfigRoot": "automation",
                    "WorkspaceRoot": "H:/workspaces",
                    "SessionPersistenceKind": "file",
                    "SessionStatePath": "automation/local/runtime-state.json",
                },
            },
            "saved_at": "2026-03-16T00:00:00Z",
            "service": {"token_total": {"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}},
            "jobs": [{"job_id": "job-ABC-1", "updated_at": "2026-03-16T00:00:01Z"}],
            "formal_objects": {
                "records": [
                    {
                        "object_type": "job",
                        "object_id": "job-ABC-1",
                        "storage_tier": "hot",
                        "lifecycle": "active",
                        "updated_at": "2026-03-16T00:00:01Z",
                        "payload": _job("ABC-1", "intervention_required"),
                    },
                    {
                        "object_type": "run",
                        "object_id": "run-ABC-1-1",
                        "storage_tier": "hot",
                        "lifecycle": "active",
                        "updated_at": "2026-03-16T00:00:01Z",
                        "payload": _run("ABC-1", "completed", "publishing"),
                    },
                ]
            },
        }

        cli._assert_session_state_surface(payload)
        found_job = cli._find_session_state_object(payload, "job", "ABC-1", state="intervention_required")
        self.assertIsNotNone(found_job)
        found_run = cli._find_session_state_object(payload, "run", "ABC-1", state="completed")
        self.assertIsNotNone(found_run)


if __name__ == "__main__":
    unittest.main()
