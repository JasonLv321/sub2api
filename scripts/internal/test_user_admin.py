#!/usr/bin/env python3
"""Standard-library tests for user_admin.py."""

from __future__ import annotations

import argparse
import contextlib
import csv
import json
import stat
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Iterator
from urllib import parse

import user_admin


def group(
    name: str = "internal-openai-50",
    group_id: int = 11,
    status: str = "active",
    subscription_type: str = "subscription",
    account_count: int = 1,
) -> dict[str, Any]:
    return {
        "id": group_id,
        "name": name,
        "platform": "openai",
        "status": status,
        "subscription_type": subscription_type,
        "monthly_limit_usd": 50,
        "account_count": account_count,
    }


class MockState:
    def __init__(self) -> None:
        self.groups = [group()]
        self.users: dict[str, dict[str, Any]] = {}
        self.attributes: dict[int, dict[int, str]] = {}
        self.subscriptions: set[tuple[int, int]] = set()
        self.conflicts: set[tuple[int, int]] = set()
        self.bulk_failures: set[tuple[int, int]] = set()
        self.writes: list[tuple[str, str, Any]] = []
        self.next_user_id = 1
        self.lock = threading.Lock()

    def add_user(self, email: str, status: str = "active") -> int:
        user_id = self.next_user_id
        self.next_user_id += 1
        self.users[email] = {
            "id": user_id,
            "email": email,
            "status": status,
        }
        return user_id


class MockHandler(BaseHTTPRequestHandler):
    @property
    def state(self) -> MockState:
        return getattr(self.server, "mock_state")

    def log_message(self, *_args: Any) -> None:
        return

    def reply(
        self,
        data: Any = None,
        status: int = 200,
        message: str = "success",
    ) -> None:
        body = json.dumps(
            {
                "code": 0 if status < 400 else status,
                "message": message,
                "data": data,
            }
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(length).decode("utf-8"))

    def do_GET(self) -> None:
        parsed = parse.urlparse(self.path)
        path = parsed.path.removeprefix("/api/v1")
        query = parse.parse_qs(parsed.query)
        if path == "/admin/groups/all":
            self.reply(self.state.groups)
            return
        if path == "/admin/user-attributes":
            self.reply(
                [{
                    "id": 7,
                    "key": "department",
                    "enabled": True,
                    "options": [
                        {"value": "engineering", "label": "Engineering"},
                        {"value": "finance", "label": "Finance"},
                        {"value": "security", "label": "Security"},
                    ],
                }]
            )
            return
        if path == "/admin/users":
            search = query.get("search", [""])[0].lower()
            items = [
                value
                for email, value in self.state.users.items()
                if search in email.lower()
            ]
            self.reply(
                {
                    "items": items,
                    "total": len(items),
                    "page": 1,
                    "page_size": 1000,
                    "pages": 1,
                }
            )
            return
        prefix = "/admin/users/"
        suffix = "/attributes"
        if path.startswith(prefix) and path.endswith(suffix):
            raw_id = path[len(prefix):-len(suffix)]
            user_id = int(raw_id)
            values = [
                {"attribute_id": key, "value": value}
                for key, value in self.state.attributes.get(
                    user_id,
                    {},
                ).items()
            ]
            self.reply(values)
            return
        self.reply(status=404, message="not found")

    def do_POST(self) -> None:
        path = parse.urlparse(self.path).path.removeprefix("/api/v1")
        payload = self.read_json()
        self.state.writes.append(("POST", path, payload))
        if path == "/admin/users":
            email = str(payload["email"]).lower()
            if email in self.state.users:
                self.reply(status=409, message="email already exists")
                return
            self.state.add_user(email)
            self.reply(self.state.users[email])
            return
        if path == "/admin/subscriptions/bulk-assign":
            self.reply(self.bulk_assign(payload))
            return
        self.reply(status=404, message="not found")

    def bulk_assign(self, payload: dict[str, Any]) -> dict[str, Any]:
        group_id = int(payload["group_id"])
        statuses = {}
        errors = []
        created = 0
        reused = 0
        for user_id in payload["user_ids"]:
            key = (int(user_id), group_id)
            if key in self.state.conflicts:
                statuses[str(user_id)] = "failed"
                errors.append(
                    f"user {user_id}: subscription assignment conflict"
                )
            elif key in self.state.bulk_failures:
                statuses[str(user_id)] = "failed"
                errors.append(f"user {user_id}: simulated failure")
            elif key in self.state.subscriptions:
                statuses[str(user_id)] = "reused"
                reused += 1
            else:
                self.state.subscriptions.add(key)
                statuses[str(user_id)] = "created"
                created += 1
        failed = len(payload["user_ids"]) - created - reused
        return {
            "success_count": created + reused,
            "created_count": created,
            "reused_count": reused,
            "failed_count": failed,
            "subscriptions": [],
            "errors": errors,
            "statuses": statuses,
        }

    def do_PUT(self) -> None:
        path = parse.urlparse(self.path).path.removeprefix("/api/v1")
        payload = self.read_json()
        self.state.writes.append(("PUT", path, payload))
        prefix = "/admin/users/"
        suffix = "/attributes"
        if path.startswith(prefix) and path.endswith(suffix):
            raw_id = path[len(prefix):-len(suffix)]
            user_id = int(raw_id)
            self.state.attributes.setdefault(user_id, {}).update(
                {
                    int(key): str(value)
                    for key, value in payload["values"].items()
                }
            )
            self.reply([])
            return
        if path.startswith(prefix):
            user_id = int(path[len(prefix):])
            for user in self.state.users.values():
                if user["id"] == user_id:
                    user["status"] = payload["status"]
                    self.reply(user)
                    return
        self.reply(status=404, message="not found")


@contextlib.contextmanager
def mock_server(state: MockState) -> Iterator[str]:
    server = ThreadingHTTPServer(("127.0.0.1", 0), MockHandler)
    setattr(server, "mock_state", state)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        host, port = server.server_address
        yield f"http://{host}:{port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def write_csv(path: Path, rows: list[dict[str, str]]) -> None:
    fields = list(rows[0])
    with path.open("w", encoding="utf-8", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        writer.writerows(rows)


def read_csv(path: Path) -> list[dict[str, str]]:
    with path.open("r", encoding="utf-8", newline="") as stream:
        return list(csv.DictReader(stream))


def import_args(root: Path, dry_run: bool = False) -> argparse.Namespace:
    return argparse.Namespace(
        csv_file=str(root / "employees.csv"),
        quota_tiers=str(root / "quota-tiers.json"),
        output_dir=str(root / "output"),
        dry_run=dry_run,
    )


def disable_args(root: Path, dry_run: bool = False) -> argparse.Namespace:
    return argparse.Namespace(
        csv_file=str(root / "leavers.csv"),
        output_dir=str(root / "output"),
        dry_run=dry_run,
    )


def write_import_files(root: Path, rows: list[dict[str, str]]) -> None:
    write_csv(root / "employees.csv", rows)
    (root / "quota-tiers.json").write_text(
        json.dumps({"standard": ["internal-openai-50"]}),
        encoding="utf-8",
    )


class UserAdminTests(unittest.TestCase):
    def api_client(self, base_url: str) -> user_admin.ApiClient:
        return user_admin.ApiClient(base_url, "test-token", 2.0)

    def assert_preflight_rejects(self, state: MockState, message: str) -> None:
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [{
                    "email": "alice@example.com",
                    "department": "engineering",
                    "quota_tier": "standard",
                }],
            )
            with mock_server(state) as base_url:
                with self.assertRaisesRegex(user_admin.ToolError, message):
                    user_admin.run_import(
                        import_args(root),
                        self.api_client(base_url),
                    )
            self.assertEqual(state.writes, [])

    def test_preflight_rejects_missing_group_before_writes(self) -> None:
        state = MockState()
        state.groups = []
        self.assert_preflight_rejects(state, "does not exist")

    def test_preflight_rejects_inactive_group_before_writes(self) -> None:
        state = MockState()
        state.groups = [group(status="inactive")]
        self.assert_preflight_rejects(state, "not active")

    def test_preflight_rejects_standard_group_before_writes(self) -> None:
        state = MockState()
        state.groups = [group(subscription_type="standard")]
        self.assert_preflight_rejects(state, "not subscription type")

    def test_preflight_rejects_empty_group_before_writes(self) -> None:
        state = MockState()
        state.groups = [group(account_count=0)]
        self.assert_preflight_rejects(state, "account_count=0")

    def test_preflight_rejects_invalid_departments_before_writes(self) -> None:
        state = MockState()
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [
                    {
                        "email": "alice@example.com",
                        "department": "engineeering",
                        "quota_tier": "standard",
                    },
                    {
                        "email": "bob@example.com",
                        "department": "unknown-team",
                        "quota_tier": "standard",
                    },
                ],
            )
            with mock_server(state) as base_url:
                with self.assertRaises(user_admin.ToolError) as raised:
                    user_admin.run_import(
                        import_args(root),
                        self.api_client(base_url),
                    )
            message = str(raised.exception)
            self.assertIn("line 2='engineeering'", message)
            self.assertIn("line 3='unknown-team'", message)
            self.assertIn("engineering, finance, security", message)
            self.assertEqual(state.writes, [])

    def test_import_ignores_groups_for_unused_tiers(self) -> None:
        state = MockState()
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [{
                    "email": "alice@example.com",
                    "department": "engineering",
                    "quota_tier": "standard",
                }],
            )
            (root / "quota-tiers.json").write_text(
                json.dumps({
                    "standard": ["internal-openai-50"],
                    "heavy": ["missing-heavy-group"],
                }),
                encoding="utf-8",
            )
            with mock_server(state) as base_url:
                code = user_admin.run_import(
                    import_args(root),
                    self.api_client(base_url),
                )
            self.assertEqual(code, 0)
            self.assertIn("alice@example.com", state.users)
            self.assertEqual(state.subscriptions, {(1, 11)})

    def test_empty_department_fails_only_that_row(self) -> None:
        state = MockState()
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [
                    {
                        "email": "alice@example.com",
                        "department": "",
                        "quota_tier": "standard",
                    },
                    {
                        "email": "bob@example.com",
                        "department": "finance",
                        "quota_tier": "standard",
                    },
                ],
            )
            with mock_server(state) as base_url:
                code = user_admin.run_import(
                    import_args(root),
                    self.api_client(base_url),
                )
            self.assertEqual(code, 1)
            retry_path = next((root / "output").glob("*-retry.csv"))
            self.assertEqual(read_csv(retry_path), [{
                "email": "alice@example.com",
                "department": "",
                "quota_tier": "standard",
            }])
            self.assertNotIn("alice@example.com", state.users)
            self.assertIn("bob@example.com", state.users)
            self.assertEqual(state.subscriptions, {(1, 11)})

    def test_import_rerun_is_idempotent_and_keeps_passwords_once(self) -> None:
        state = MockState()
        state.groups.append(
            group(name="internal-anthropic-50", group_id=12)
        )
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [{
                    "email": "alice@example.com",
                    "department": "engineering",
                    "quota_tier": "standard",
                }],
            )
            (root / "quota-tiers.json").write_text(
                json.dumps({
                    "standard": [
                        "internal-openai-50",
                        "internal-anthropic-50",
                    ]
                }),
                encoding="utf-8",
            )
            with mock_server(state) as base_url:
                client = self.api_client(base_url)
                self.assertEqual(
                    user_admin.run_import(import_args(root), client),
                    0,
                )
                first_passwords = list(
                    (root / "output").glob("*-passwords.csv")
                )
                self.assertEqual(len(first_passwords), 1)
                mode = stat.S_IMODE(first_passwords[0].stat().st_mode)
                self.assertEqual(mode, 0o600)
                self.assertEqual(
                    user_admin.run_import(import_args(root), client),
                    0,
                )
            passwords = list((root / "output").glob("*-passwords.csv"))
            self.assertEqual(passwords, first_passwords)
            create_writes = [
                item for item in state.writes
                if item[:2] == ("POST", "/admin/users")
            ]
            attribute_writes = [
                item for item in state.writes
                if item[0] == "PUT" and item[1].endswith("/attributes")
            ]
            self.assertEqual(len(create_writes), 1)
            self.assertEqual(len(attribute_writes), 1)
            self.assertEqual(state.subscriptions, {(1, 11), (1, 12)})
            result_paths = sorted(
                (root / "output").glob("*-results.csv")
            )
            second = read_csv(result_paths[-1])
            self.assertEqual(second[0]["result"], "skipped")
            self.assertIn("reused", second[0]["subscription_status"])

    def test_partial_conflict_is_separate_and_retryable(self) -> None:
        state = MockState()
        alice_id = state.add_user("alice@example.com")
        bob_id = state.add_user("bob@example.com")
        state.attributes[alice_id] = {7: "engineering"}
        state.attributes[bob_id] = {7: "finance"}
        state.conflicts.add((alice_id, 11))
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [
                    {
                        "email": "alice@example.com",
                        "department": "engineering",
                        "quota_tier": "standard",
                    },
                    {
                        "email": "bob@example.com",
                        "department": "finance",
                        "quota_tier": "standard",
                    },
                ],
            )
            with mock_server(state) as base_url:
                code = user_admin.run_import(
                    import_args(root),
                    self.api_client(base_url),
                )
            self.assertEqual(code, 1)
            conflict_path = next(
                (root / "output").glob("*-conflicts.csv")
            )
            retry_path = next((root / "output").glob("*-retry.csv"))
            conflicts = read_csv(conflict_path)
            retry = read_csv(retry_path)
            self.assertEqual([item["email"] for item in conflicts], [
                "alice@example.com"
            ])
            self.assertEqual(retry, [{
                "email": "alice@example.com",
                "department": "engineering",
                "quota_tier": "standard",
            }])
            self.assertIn((bob_id, 11), state.subscriptions)
            self.assertNotIn((alice_id, 11), state.subscriptions)

    def test_unknown_tier_fails_only_that_row(self) -> None:
        state = MockState()
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [
                    {
                        "email": "alice@example.com",
                        "department": "engineering",
                        "quota_tier": "standard",
                    },
                    {
                        "email": "eve@example.com",
                        "department": "security",
                        "quota_tier": "unapproved",
                    },
                ],
            )
            with mock_server(state) as base_url:
                code = user_admin.run_import(
                    import_args(root),
                    self.api_client(base_url),
                )
            self.assertEqual(code, 1)
            retry_path = next((root / "output").glob("*-retry.csv"))
            self.assertEqual(
                [item["email"] for item in read_csv(retry_path)],
                ["eve@example.com"],
            )
            self.assertIn("alice@example.com", state.users)
            self.assertNotIn("eve@example.com", state.users)

    def test_dry_run_sends_no_write_requests(self) -> None:
        state = MockState()
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_import_files(
                root,
                [{
                    "email": "alice@example.com",
                    "department": "engineering",
                    "quota_tier": "standard",
                }],
            )
            with mock_server(state) as base_url:
                code = user_admin.run_import(
                    import_args(root, dry_run=True),
                    self.api_client(base_url),
                )
            self.assertEqual(code, 0)
            self.assertEqual(state.writes, [])
            self.assertEqual(
                list((root / "output").glob("*-passwords.csv")),
                [],
            )
            result_path = next(
                (root / "output").glob("*-results.csv")
            )
            self.assertEqual(read_csv(result_path)[0]["result"], "dry_run")

    def test_disable_updates_active_and_skips_disabled_user(self) -> None:
        state = MockState()
        state.add_user("active@example.com")
        state.add_user("disabled@example.com", status="disabled")
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            write_csv(
                root / "leavers.csv",
                [
                    {"email": "active@example.com"},
                    {"email": "disabled@example.com"},
                    {"email": "missing@example.com"},
                ],
            )
            with mock_server(state) as base_url:
                code = user_admin.run_disable(
                    disable_args(root),
                    self.api_client(base_url),
                )
            self.assertEqual(code, 1)
            disable_writes = [
                item for item in state.writes
                if item[0] == "PUT" and not item[1].endswith("/attributes")
            ]
            self.assertEqual(len(disable_writes), 1)
            self.assertEqual(
                state.users["active@example.com"]["status"],
                "disabled",
            )
            result_path = next(
                (root / "output").glob("*-results.csv")
            )
            by_email = {
                item["email"]: item for item in read_csv(result_path)
            }
            self.assertEqual(
                by_email["active@example.com"]["result"],
                "success",
            )
            self.assertEqual(
                by_email["disabled@example.com"]["result"],
                "skipped",
            )
            self.assertEqual(
                by_email["missing@example.com"]["result"],
                "failed",
            )


if __name__ == "__main__":
    unittest.main()
