#!/usr/bin/env python3
"""Import and disable internal Sub2API users via admin APIs.

Usage:
    export SUB2API_BASE_URL="https://sub2api.example.com"
    export SUB2API_ADMIN_TOKEN="<admin-jwt>"
    python3 scripts/internal/user_admin.py import employees.csv
    python3 scripts/internal/user_admin.py import employees.csv --dry-run
    python3 scripts/internal/user_admin.py disable leavers.csv

Import CSV columns:
    Required: email, department, quota_tier
    Optional: username, notes
    Extra columns are preserved in retry files but otherwise ignored.

Disable CSV columns:
    Required: email. Other columns are preserved in retry files.

Environment:
    SUB2API_BASE_URL is the server origin, without /api/v1.
    SUB2API_ADMIN_TOKEN is an administrator JWT. Never store it in CSV or JSON.

Outputs:
    Each run writes a detailed result CSV and an input-compatible retry CSV.
    Imports also write conflicts separately. Passwords for newly created users
    are written to a mode-0600 CSV. Existing users never have passwords reset.

Known limitations and security facts:
    Requests are serial and are not automatically retried. bulk-assign is not
    transactional, so inspect result and conflict files before rerunning.
    Disabling a user calls InvalidateAuthCacheByUserID(); keys normally stop
    immediately. On invalidation failure, fallback TTLs are L1=15s/L2=300s.
    Precreated accounts can be automatically bound by Google, GitHub, or OIDC
    when the provider supplies the same verified email. Guards are
    EmailVerified and the registration email-domain policy; the disabled
    registration setting does not block binding an existing account. Internal
    deployments should enable only enterprise OIDC. GitHub is especially risky
    because a personal account can verify a company email address.
"""

from __future__ import annotations

import argparse
import csv
import io
import json
import os
import secrets
import sys
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib import error, parse, request


API_PREFIX = "/api/v1"
RESULT_FIELDS = [
    "row_number",
    "email",
    "department",
    "quota_tier",
    "user_id",
    "user_status",
    "attribute_status",
    "subscription_status",
    "result",
    "details",
]


class ToolError(Exception):
    """An actionable configuration, input, or API error."""


class ApiError(ToolError):
    """An unsuccessful HTTP or API-envelope response."""

    def __init__(self, method: str, path: str, message: str) -> None:
        super().__init__(f"{method} {path}: {message}")


class ApiClient:
    """Small Sub2API JSON client using only the Python standard library."""

    def __init__(self, base_url: str, token: str, timeout: float) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def request(
        self,
        method: str,
        path: str,
        payload: dict[str, Any] | None = None,
        query: dict[str, Any] | None = None,
    ) -> Any:
        url = f"{self.base_url}{API_PREFIX}{path}"
        if query:
            url = f"{url}?{parse.urlencode(query)}"
        body = None
        headers = {
            "Accept": "application/json",
            "Authorization": f"Bearer {self.token}",
        }
        if payload is not None:
            body = json.dumps(payload).encode("utf-8")
            headers["Content-Type"] = "application/json"
        http_request = request.Request(
            url,
            data=body,
            headers=headers,
            method=method,
        )
        try:
            with request.urlopen(
                http_request,
                timeout=self.timeout,
            ) as response:
                raw = response.read()
        except error.HTTPError as exc:
            raw = exc.read()
            message = _error_message(raw, f"HTTP {exc.code}")
            raise ApiError(method, path, message) from exc
        except error.URLError as exc:
            raise ApiError(method, path, str(exc.reason)) from exc
        try:
            envelope = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ApiError(method, path, "response is not valid JSON") from exc
        if not isinstance(envelope, dict):
            raise ApiError(method, path, "response envelope is not an object")
        if envelope.get("code") != 0:
            message = str(envelope.get("message") or "API request failed")
            raise ApiError(method, path, message)
        return envelope.get("data")


def _error_message(raw: bytes, fallback: str) -> str:
    try:
        envelope = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return fallback
    if isinstance(envelope, dict):
        return str(envelope.get("message") or fallback)
    return fallback


@dataclass
class InputRow:
    number: int
    raw: dict[str, str]
    email: str
    department: str = ""
    quota_tier: str = ""
    username: str = ""
    notes: str = ""
    error: str = ""


@dataclass
class RowResult:
    source: InputRow
    user_id: int | None = None
    user_status: str = ""
    attribute_status: str = ""
    subscriptions: dict[str, str] = field(default_factory=dict)
    result: str = "pending"
    details: list[str] = field(default_factory=list)

    def fail(self, message: str, conflict: bool = False) -> None:
        self.details.append(message)
        if conflict:
            self.result = "conflict"
        elif self.result != "conflict":
            self.result = "failed"

    def as_dict(self) -> dict[str, str | int]:
        subscriptions = ";".join(
            f"{name}:{status}"
            for name, status in sorted(self.subscriptions.items())
        )
        return {
            "row_number": self.source.number,
            "email": self.source.email,
            "department": self.source.department,
            "quota_tier": self.source.quota_tier,
            "user_id": self.user_id or "",
            "user_status": self.user_status,
            "attribute_status": self.attribute_status,
            "subscription_status": subscriptions,
            "result": self.result,
            "details": " | ".join(self.details),
        }


class SecureCsvWriter:
    """Create a new CSV with mode 0600 and never overwrite an old result."""

    def __init__(self, path: Path, fields: list[str]) -> None:
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        descriptor = os.open(path, flags, 0o600)
        self.path = path
        self.stream = io.TextIOWrapper(
            os.fdopen(descriptor, "wb", closefd=True),
            encoding="utf-8",
            newline="",
        )
        self.writer = csv.DictWriter(self.stream, fieldnames=fields)
        self.writer.writeheader()

    def write(self, row: dict[str, Any]) -> None:
        self.writer.writerow(row)
        self.stream.flush()

    def close(self) -> None:
        self.stream.close()


class LazyPasswordWriter:
    """Create the protected password CSV only after a user is created."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self.writer: SecureCsvWriter | None = None

    @property
    def created(self) -> bool:
        return self.writer is not None

    def write(self, email: str, password: str) -> None:
        if self.writer is None:
            self.writer = SecureCsvWriter(
                self.path,
                ["email", "password"],
            )
        self.writer.write({"email": email, "password": password})

    def close(self) -> None:
        if self.writer is not None:
            self.writer.close()


def load_csv(
    path: Path,
    required: set[str],
) -> tuple[list[InputRow], list[str]]:
    try:
        stream = path.open("r", encoding="utf-8-sig", newline="")
    except OSError as exc:
        raise ToolError(f"cannot open CSV {path}: {exc}") from exc
    with stream:
        reader = csv.DictReader(stream)
        fields = reader.fieldnames or []
        missing = sorted(required - set(fields))
        if missing:
            raise ToolError(f"CSV is missing columns: {', '.join(missing)}")
        rows = []
        seen: set[str] = set()
        for number, raw_value in enumerate(reader, start=2):
            raw = {key: (value or "") for key, value in raw_value.items()}
            email = raw.get("email", "").strip().lower()
            row = InputRow(
                number=number,
                raw=raw,
                email=email,
                department=raw.get("department", "").strip(),
                quota_tier=raw.get("quota_tier", "").strip(),
                username=raw.get("username", "").strip(),
                notes=raw.get("notes", "").strip(),
            )
            missing_values = [
                name
                for name in required
                if not raw.get(name, "").strip()
            ]
            if missing_values:
                row.error = "empty required values: " + ", ".join(
                    sorted(missing_values)
                )
            elif email in seen:
                row.error = "duplicate email in input CSV"
            seen.add(email)
            rows.append(row)
    return rows, fields


def load_tier_map(path: Path) -> dict[str, list[str]]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except OSError as exc:
        raise ToolError(f"cannot read quota tier map {path}: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise ToolError(f"invalid quota tier JSON {path}: {exc}") from exc
    if not isinstance(value, dict) or not value:
        raise ToolError("quota tier map must be a non-empty JSON object")
    result: dict[str, list[str]] = {}
    for raw_tier, raw_groups in value.items():
        tier = str(raw_tier).strip()
        if not tier or not isinstance(raw_groups, list) or not raw_groups:
            raise ToolError(f"quota tier {raw_tier!r} needs a group-name list")
        groups = [str(item).strip() for item in raw_groups]
        has_empty = any(not name for name in groups)
        if has_empty or len(groups) != len(set(groups)):
            message = f"quota tier {tier!r} has empty or duplicate groups"
            raise ToolError(message)
        result[tier] = groups
    return result


def resolve_groups(
    client: ApiClient,
    tier_map: dict[str, list[str]],
    used_tiers: set[str],
) -> dict[str, list[dict[str, Any]]]:
    data = client.request(
        "GET",
        "/admin/groups/all",
        query={"include_inactive": "true"},
    )
    if not isinstance(data, list):
        raise ToolError("groups/all data is not a list")
    by_name: dict[str, list[dict[str, Any]]] = {}
    for group in data:
        if isinstance(group, dict) and isinstance(group.get("name"), str):
            by_name.setdefault(group["name"], []).append(group)
    resolved: dict[str, list[dict[str, Any]]] = {}
    for tier in sorted(used_tiers):
        resolved[tier] = []
        for name in tier_map[tier]:
            matches = by_name.get(name, [])
            if not matches:
                raise ToolError(f"target Group {name!r} does not exist")
            if len(matches) > 1:
                raise ToolError(f"target Group name {name!r} is not unique")
            group = matches[0]
            _validate_group(name, group)
            resolved[tier].append(group)
    return resolved


def _validate_group(name: str, group: dict[str, Any]) -> None:
    if group.get("status") != "active":
        raise ToolError(f"target Group {name!r} is not active")
    if group.get("subscription_type") != "subscription":
        raise ToolError(f"target Group {name!r} is not subscription type")
    account_count = group.get("account_count")
    if not isinstance(account_count, int) or account_count == 0:
        raise ToolError(f"target Group {name!r} has account_count=0")
    if not isinstance(group.get("id"), int) or group["id"] <= 0:
        raise ToolError(f"target Group {name!r} has an invalid id")


def resolve_department_attribute(client: ApiClient) -> tuple[int, set[str]]:
    query = {"enabled": "true"}
    data = client.request("GET", "/admin/user-attributes", query=query)
    if not isinstance(data, list):
        raise ToolError("user-attributes data is not a list")
    matches = [item for item in data if item.get("key") == "department"]
    if len(matches) != 1 or not isinstance(matches[0].get("id"), int):
        raise ToolError("exactly one enabled department attribute is required")
    definition = matches[0]
    allowed = {
        option["value"].strip()
        for option in definition.get("options", []) if isinstance(option, dict)
        and isinstance(option.get("value"), str)
        and option["value"].strip()
    }
    if not allowed:
        raise ToolError("department attribute has no valid option values")
    return int(definition["id"]), allowed


def find_user(client: ApiClient, email: str) -> dict[str, Any] | None:
    data = client.request(
        "GET",
        "/admin/users",
        query={
            "search": email,
            "page": 1,
            "page_size": 1000,
            "include_subscriptions": "false",
        },
    )
    if not isinstance(data, dict) or not isinstance(data.get("items"), list):
        raise ToolError("admin users search returned an invalid page")
    matches = [
        item
        for item in data["items"]
        if str(item.get("email", "")).strip().lower() == email
    ]
    if len(matches) > 1:
        raise ToolError(f"multiple users matched email {email}")
    return matches[0] if matches else None


def ensure_department(
    client: ApiClient,
    user_id: int,
    attribute_id: int,
    department: str,
    dry_run: bool,
) -> str:
    path = f"/admin/users/{user_id}/attributes"
    data = client.request("GET", path)
    if not isinstance(data, list):
        raise ToolError(f"attributes for user {user_id} are not a list")
    existing = {
        item.get("attribute_id"): item.get("value")
        for item in data
        if isinstance(item, dict)
    }
    if existing.get(attribute_id) == department:
        return "skipped"
    if dry_run:
        return "would_update"
    client.request(
        "PUT",
        path,
        payload={"values": {str(attribute_id): department}},
    )
    return "updated"


def create_user(
    client: ApiClient,
    row: InputRow,
    password: str,
) -> dict[str, Any]:
    payload = {"email": row.email, "password": password}
    if row.username:
        payload["username"] = row.username
    if row.notes:
        payload["notes"] = row.notes
    data = client.request("POST", "/admin/users", payload=payload)
    if not isinstance(data, dict) or not isinstance(data.get("id"), int):
        raise ToolError("create user response does not contain an integer id")
    return data


def prepare_import(
    client: ApiClient,
    rows: list[InputRow],
    tier_map: dict[str, list[str]],
) -> tuple[dict[str, list[dict[str, Any]]], int, list[RowResult]]:
    results = [RowResult(source=row) for row in rows]
    used_tiers = set()
    for result in results:
        row = result.source
        if row.error:
            result.fail(row.error)
        elif row.quota_tier not in tier_map:
            result.fail(f"unknown quota_tier {row.quota_tier!r}")
        else:
            used_tiers.add(row.quota_tier)
    attribute_id, allowed = resolve_department_attribute(client)
    bad = [r for r in rows if not r.error and r.department not in allowed]
    if bad:
        loc = ", ".join(f"line {r.number}={r.department!r}" for r in bad)
        valid = ", ".join(sorted(allowed))
        raise ToolError(f"invalid departments: {loc}; legal values: {valid}")
    groups = resolve_groups(client, tier_map, used_tiers) if used_tiers else {}
    return groups, attribute_id, results


def process_import_users(
    client: ApiClient,
    results: list[RowResult],
    attribute_id: int,
    dry_run: bool,
    passwords: LazyPasswordWriter | None,
) -> dict[str, list[RowResult]]:
    eligible: dict[str, list[RowResult]] = {}
    for result in results:
        if result.result == "failed":
            continue
        row = result.source
        try:
            user = find_user(client, row.email)
            if user is None and dry_run:
                result.user_status = "would_create"
                result.attribute_status = "would_update"
                result.result = "pending"
                print(f"DRY-RUN create user {row.email}")
                print(
                    f"DRY-RUN set department={row.department} "
                    f"for {row.email}"
                )
                eligible.setdefault(row.quota_tier, []).append(result)
                continue
            if user is None:
                password = secrets.token_urlsafe(24)
                user = create_user(client, row, password)
                result.user_status = "created"
                if passwords is None:
                    raise ToolError("password writer is not available")
                passwords.write(row.email, password)
            else:
                result.user_status = "existing"
            result.user_id = int(user["id"])
            result.attribute_status = ensure_department(
                client,
                result.user_id,
                attribute_id,
                row.department,
                dry_run,
            )
            eligible.setdefault(row.quota_tier, []).append(result)
        except ToolError as exc:
            result.fail(str(exc))
    return eligible


def assign_subscriptions(
    client: ApiClient,
    eligible: dict[str, list[RowResult]],
    groups: dict[str, list[dict[str, Any]]],
    dry_run: bool,
) -> None:
    for tier, tier_results in eligible.items():
        for group in groups[tier]:
            name = str(group["name"])
            if dry_run:
                for result in tier_results:
                    result.subscriptions[name] = "would_assign"
                    print(f"DRY-RUN assign {name} to {result.source.email}")
                continue
            identified = [
                item for item in tier_results if item.user_id is not None
            ]
            payload = {
                "user_ids": [item.user_id for item in identified],
                "group_id": group["id"],
                "validity_days": 36500,
                "notes": f"internal quota tier {tier}: {name}",
            }
            try:
                data = client.request(
                    "POST",
                    "/admin/subscriptions/bulk-assign",
                    payload=payload,
                )
                _apply_bulk_result(identified, name, data)
            except ToolError as exc:
                for result in identified:
                    result.subscriptions[name] = "failed"
                    result.fail(f"{name}: {exc}")


def _apply_bulk_result(
    results: list[RowResult],
    group_name: str,
    data: Any,
) -> None:
    if not isinstance(data, dict) or not isinstance(data.get("statuses"), dict):
        raise ToolError("bulk-assign response has no statuses map")
    errors = data.get("errors")
    if not isinstance(errors, list):
        errors = []
    for result in results:
        user_id = result.user_id
        status = str(data["statuses"].get(str(user_id), "failed"))
        if status in {"created", "reused"}:
            result.subscriptions[group_name] = status
            continue
        message = _bulk_error_for_user(errors, user_id)
        is_conflict = "conflict" in message.lower()
        result.subscriptions[group_name] = (
            "conflict" if is_conflict else "failed"
        )
        result.fail(f"{group_name}: {message}", conflict=is_conflict)


def _bulk_error_for_user(errors: list[Any], user_id: int | None) -> str:
    prefix = f"user {user_id}:"
    for value in errors:
        message = str(value)
        if message.lower().startswith(prefix.lower()):
            return message[len(prefix):].strip()
    return "bulk-assign failed without a user-specific error"


def finalize_import(results: list[RowResult], dry_run: bool) -> None:
    for result in results:
        if result.result in {"failed", "conflict"}:
            continue
        statuses = set(result.subscriptions.values())
        if dry_run:
            result.result = "dry_run"
        elif (
            result.user_status == "existing"
            and result.attribute_status == "skipped"
            and statuses == {"reused"}
        ):
            result.result = "skipped"
        else:
            result.result = "success"


def run_import(args: argparse.Namespace, client: ApiClient) -> int:
    rows, input_fields = load_csv(
        Path(args.csv_file),
        {"email", "department", "quota_tier"},
    )
    tier_map = load_tier_map(Path(args.quota_tiers))
    groups, attribute_id, results = prepare_import(client, rows, tier_map)
    output_dir = make_output_dir(Path(args.output_dir))
    run_id = make_run_id()
    password_path = output_dir / f"import-{run_id}-passwords.csv"
    password_writer = None
    if not args.dry_run:
        password_writer = LazyPasswordWriter(password_path)
    try:
        eligible = process_import_users(
            client,
            results,
            attribute_id,
            args.dry_run,
            password_writer,
        )
        assign_subscriptions(client, eligible, groups, args.dry_run)
        finalize_import(results, args.dry_run)
    finally:
        if password_writer is not None:
            password_writer.close()
    paths = write_outputs(
        output_dir,
        f"import-{run_id}",
        results,
        input_fields,
        include_conflicts=True,
    )
    if password_writer is not None and password_writer.created:
        paths.append(password_path)
    print_paths(paths)
    has_failures = any(
        item.result in {"failed", "conflict"} for item in results
    )
    return 1 if has_failures else 0


def run_disable(args: argparse.Namespace, client: ApiClient) -> int:
    rows, input_fields = load_csv(Path(args.csv_file), {"email"})
    results = [RowResult(source=row) for row in rows]
    for result in results:
        if result.source.error:
            result.fail(result.source.error)
            continue
        try:
            user = find_user(client, result.source.email)
            if user is None:
                result.fail("user does not exist")
                continue
            result.user_id = int(user["id"])
            result.user_status = str(user.get("status", ""))
            if result.user_status == "disabled":
                result.result = "skipped"
            elif args.dry_run:
                print(f"DRY-RUN disable user {result.source.email}")
                result.result = "dry_run"
            else:
                path = f"/admin/users/{result.user_id}"
                client.request("PUT", path, payload={"status": "disabled"})
                result.user_status = "disabled"
                result.result = "success"
        except ToolError as exc:
            result.fail(str(exc))
    output_dir = make_output_dir(Path(args.output_dir))
    paths = write_outputs(
        output_dir,
        f"disable-{make_run_id()}",
        results,
        input_fields,
        include_conflicts=False,
    )
    print_paths(paths)
    return 1 if any(item.result == "failed" for item in results) else 0


def make_output_dir(path: Path) -> Path:
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    return path


def make_run_id() -> str:
    now = datetime.now(timezone.utc)
    return now.strftime("%Y%m%dT%H%M%S%fZ")


def write_outputs(
    output_dir: Path,
    prefix: str,
    results: list[RowResult],
    input_fields: list[str],
    include_conflicts: bool,
) -> list[Path]:
    paths = []
    result_path = output_dir / f"{prefix}-results.csv"
    write_secure_csv(
        result_path,
        RESULT_FIELDS,
        [item.as_dict() for item in results],
    )
    paths.append(result_path)
    failures = [
        item.source.raw
        for item in results
        if item.result in {"failed", "conflict"}
    ]
    failure_path = output_dir / f"{prefix}-retry.csv"
    write_secure_csv(failure_path, input_fields, failures)
    paths.append(failure_path)
    if include_conflicts:
        conflict_path = output_dir / f"{prefix}-conflicts.csv"
        conflicts = [
            item.as_dict() for item in results if item.result == "conflict"
        ]
        write_secure_csv(conflict_path, RESULT_FIELDS, conflicts)
        paths.append(conflict_path)
    return paths


def write_secure_csv(
    path: Path,
    fields: list[str],
    rows: list[dict[str, Any]],
) -> None:
    writer = SecureCsvWriter(path, fields)
    try:
        for row in rows:
            writer.write(row)
    finally:
        writer.close()


def print_paths(paths: list[Path]) -> None:
    for path in paths:
        print(f"wrote {path}")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--timeout",
        type=float,
        default=30.0,
        help="HTTP timeout in seconds (default: 30)",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    script_dir = Path(__file__).resolve().parent
    import_parser = subparsers.add_parser("import", help="import employee CSV")
    add_common_arguments(import_parser)
    import_parser.add_argument(
        "--quota-tiers",
        default=str(script_dir / "quota-tiers.json"),
        help="quota tier to Group-name JSON mapping",
    )
    import_parser.set_defaults(handler=run_import)
    disable_parser = subparsers.add_parser(
        "disable",
        help="disable employee CSV",
    )
    add_common_arguments(disable_parser)
    disable_parser.set_defaults(handler=run_disable)
    return parser


def add_common_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("csv_file", help="input CSV path")
    parser.add_argument(
        "--output-dir",
        default="output/user-admin",
        help="result directory (default: output/user-admin)",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="perform reads and print plans without remote writes",
    )


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    base_url = os.environ.get("SUB2API_BASE_URL", "").strip()
    token = os.environ.get("SUB2API_ADMIN_TOKEN", "").strip()
    if not base_url or not token:
        parser.error(
            "SUB2API_BASE_URL and SUB2API_ADMIN_TOKEN must both be set"
        )
    client = ApiClient(base_url, token, args.timeout)
    try:
        return int(args.handler(args, client))
    except ToolError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
