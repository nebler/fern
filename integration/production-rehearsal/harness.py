#!/usr/bin/env python3
"""Resumable, redacted evidence recorder for Fern physical rehearsals."""

from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import json
import os
import pathlib
import re
import sys
import tempfile
import urllib.parse
from typing import Any, Callable


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCHEMA_PATH = ROOT / "deploy/release/production-rehearsal-evidence.schema.json"
STATE_FILE = "evidence.json"
PHASES = (
    "source-preflight",
    "pre-reboot",
    "post-reboot",
    "source-backup",
    "source-fence",
    "target-restore",
    "tls-wss",
    "phone",
    "acl-negative",
    "finalize",
)
ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
BOOT_RE = re.compile(r"^[A-Fa-f0-9-]{8,64}$")
SHA_RE = re.compile(r"^[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40,64}$")
VERSION_RE = re.compile(r"^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$")
SECRET_KEY_RE = re.compile(
    r"(^|_)(password|passwd|secret|token|api_?key|private_?key|authorization|cookie|session)(_|$)",
    re.IGNORECASE,
)
SECRET_VALUE_RES = (
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    re.compile(r"\b(?:Bearer|Basic)\s+[A-Za-z0-9+/_.=-]+", re.IGNORECASE),
    re.compile(r"\b(?:github_pat_|gh[pousr]_)[A-Za-z0-9_]{8,}"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(r"\bsk-[A-Za-z0-9_-]{16,}\b"),
)


class EvidenceError(ValueError):
    pass


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def is_timestamp(value: Any) -> bool:
    if not isinstance(value, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", value):
        return False
    try:
        dt.datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        return False
    return True


def timestamp_value(value: str) -> dt.datetime:
    return dt.datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc)


def is_id(value: Any) -> bool:
    return isinstance(value, str) and ID_RE.fullmatch(value) is not None


def is_boot_id(value: Any) -> bool:
    return isinstance(value, str) and BOOT_RE.fullmatch(value) is not None


def is_sha(value: Any) -> bool:
    return isinstance(value, str) and SHA_RE.fullmatch(value) is not None


def is_commit(value: Any) -> bool:
    return isinstance(value, str) and COMMIT_RE.fullmatch(value) is not None


def is_version(value: Any) -> bool:
    return isinstance(value, str) and VERSION_RE.fullmatch(value) is not None


def is_origin(value: Any) -> bool:
    if not isinstance(value, str):
        return False
    try:
        parsed = urllib.parse.urlsplit(value)
        if parsed.scheme != "https" or parsed.username or parsed.password:
            return False
        if parsed.path not in ("", "/") or parsed.query or parsed.fragment:
            return False
        if parsed.hostname is None or re.fullmatch(r"[a-z0-9.-]+\.ts\.net", parsed.hostname) is None:
            return False
        port = parsed.port
    except ValueError:
        return False
    return port is None or 1 <= port <= 65535


def one_of(*values: str) -> Callable[[Any], bool]:
    return lambda value: isinstance(value, str) and value in values


TRUE = lambda value: value is True
FALSE = lambda value: value is False


PHASE_FIELDS: dict[str, dict[str, Callable[[Any], bool]]] = {
    "source-preflight": {
        "observed_at": is_timestamp,
        "source_host_id": is_id,
        "source_boot_id": is_boot_id,
        "release_version": is_version,
        "release_commit": is_commit,
        "image_digest": lambda value: isinstance(value, str) and value.startswith("sha256:") and is_sha(value[7:]),
        "service_active": TRUE,
        "origin": is_origin,
    },
    "pre-reboot": {
        "observed_at": is_timestamp,
        "source_host_id": is_id,
        "boot_id_before": is_boot_id,
        "service_active": TRUE,
        "reboot_requested": TRUE,
    },
    "post-reboot": {
        "observed_at": is_timestamp,
        "source_host_id": is_id,
        "boot_id_before": is_boot_id,
        "boot_id_after": is_boot_id,
        "boot_changed": TRUE,
        "service_active": TRUE,
    },
    "source-backup": {
        "observed_at": is_timestamp,
        "source_host_id": is_id,
        "source_boot_id": is_boot_id,
        "backup_id": is_id,
        "backup_manifest_sha256": is_sha,
        "backup_payload_sha256": is_sha,
        "credential_policy": one_of("exclude", "external"),
        "verification_passed": TRUE,
    },
    "source-fence": {
        "observed_at": is_timestamp,
        "source_host_id": is_id,
        "source_boot_id": is_boot_id,
        "fence_id": is_id,
        "service_active": FALSE,
        "origin_disabled": TRUE,
    },
    "target-restore": {
        "observed_at": is_timestamp,
        "target_host_id": is_id,
        "target_boot_id": is_boot_id,
        "release_version": is_version,
        "release_commit": is_commit,
        "image_digest": lambda value: isinstance(value, str) and value.startswith("sha256:") and is_sha(value[7:]),
        "backup_id": is_id,
        "backup_manifest_sha256": is_sha,
        "restore_transaction_sha256": is_sha,
        "service_active": TRUE,
    },
    "tls-wss": {
        "observed_at": is_timestamp,
        "origin": is_origin,
        "observer_id": is_id,
        "tls_valid": TRUE,
        "certificate_hostname_valid": TRUE,
        "wss_connected": TRUE,
    },
    "phone": {
        "observed_at": is_timestamp,
        "origin": is_origin,
        "device_id": is_id,
        "device_platform": one_of("android", "ios"),
        "network_path": one_of("cellular", "external-wifi"),
        "page_loaded": TRUE,
        "wss_connected": TRUE,
        "operator_action_passed": TRUE,
    },
    "acl-negative": {
        "revocation_recorded_at": is_timestamp,
        "denial_observed_at": is_timestamp,
        "origin": is_origin,
        "revoked_device_id": is_id,
        "independent_observer_id": is_id,
        "control_device_id": is_id,
        "probe_network": one_of("cellular", "external-wifi"),
        "acl_denied": TRUE,
        "denial_kind": one_of("tailscale-acl-denied", "connection-refused", "timeout", "http-403"),
        "authorized_control_passed": TRUE,
    },
    "finalize": {
        "observed_at": is_timestamp,
        "reviewer_id": is_id,
        "redaction_reviewed": TRUE,
        "evidence_complete": TRUE,
        "source_remains_fenced": TRUE,
        "target_service_active": TRUE,
    },
}


def reject_secret_like(value: Any, location: str = "evidence") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if SECRET_KEY_RE.search(str(key)):
                raise EvidenceError(f"prohibited secret-like field at {location}.{key}")
            reject_secret_like(child, f"{location}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            reject_secret_like(child, f"{location}[{index}]")
    elif isinstance(value, str):
        if "\n" in value or "\r" in value:
            raise EvidenceError(f"multiline value rejected at {location}")
        if any(pattern.search(value) for pattern in SECRET_VALUE_RES):
            raise EvidenceError(f"prohibited secret-like value at {location}")
        try:
            parsed = urllib.parse.urlsplit(value)
            if parsed.scheme and (parsed.username or parsed.password):
                raise EvidenceError(f"URL credentials rejected at {location}")
        except ValueError as error:
            raise EvidenceError(f"malformed URL-like value at {location}") from error


def require_exact_keys(value: dict[str, Any], expected: set[str], location: str) -> None:
    missing = expected - value.keys()
    unknown = value.keys() - expected
    if missing:
        raise EvidenceError(f"{location} missing fields: {', '.join(sorted(missing))}")
    if unknown:
        raise EvidenceError(f"{location} has unknown fields: {', '.join(sorted(unknown))}")


def validate_phase_evidence(phase: str, evidence: Any) -> None:
    if not isinstance(evidence, dict):
        raise EvidenceError(f"{phase} evidence must be an object")
    reject_secret_like(evidence, phase)
    fields = PHASE_FIELDS[phase]
    require_exact_keys(evidence, set(fields), phase)
    for field, validator in fields.items():
        if not validator(evidence[field]):
            raise EvidenceError(f"{phase}.{field} has an invalid or unsuccessful value")
    if phase == "post-reboot" and evidence["boot_id_before"] == evidence["boot_id_after"]:
        raise EvidenceError("post-reboot must demonstrate a changed boot ID")
    if phase == "acl-negative":
        if timestamp_value(evidence["denial_observed_at"]) < timestamp_value(evidence["revocation_recorded_at"]):
            raise EvidenceError("ACL denial must be observed after revocation")
        if evidence["control_device_id"] == evidence["revoked_device_id"]:
            raise EvidenceError("ACL control device must differ from the revoked device")


def phase_map(state: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return {entry["name"]: entry["evidence"] for entry in state["phases"]}


def expect_equal(facts: dict[str, dict[str, Any]], references: list[tuple[str, str]], label: str) -> None:
    present = [(phase, facts[phase][field]) for phase, field in references if phase in facts]
    if present and any(value != present[0][1] for _, value in present[1:]):
        locations = ", ".join(f"{phase}.{field}" for phase, field in references if phase in facts)
        raise EvidenceError(f"inconsistent {label}: {locations}")


def validate_cross_phase(state: dict[str, Any]) -> None:
    facts = phase_map(state)
    expect_equal(facts, [
        ("source-preflight", "source_host_id"), ("pre-reboot", "source_host_id"),
        ("post-reboot", "source_host_id"), ("source-backup", "source_host_id"),
        ("source-fence", "source_host_id"),
    ], "source host identity")
    expect_equal(facts, [
        ("source-preflight", "source_boot_id"), ("pre-reboot", "boot_id_before"),
        ("post-reboot", "boot_id_before"),
    ], "pre-reboot boot identity")
    expect_equal(facts, [
        ("post-reboot", "boot_id_after"), ("source-backup", "source_boot_id"),
        ("source-fence", "source_boot_id"),
    ], "post-reboot boot identity")
    for field in ("release_version", "release_commit", "image_digest"):
        expect_equal(facts, [("source-preflight", field), ("target-restore", field)], field)
    for field in ("backup_id", "backup_manifest_sha256"):
        expect_equal(facts, [("source-backup", field), ("target-restore", field)], field)
    expect_equal(facts, [
        ("source-preflight", "origin"), ("tls-wss", "origin"),
        ("phone", "origin"), ("acl-negative", "origin"),
    ], "remote origin")
    if "target-restore" in facts and facts["target-restore"]["target_host_id"] == facts["source-preflight"]["source_host_id"]:
        raise EvidenceError("target host must differ from the fenced source host")
    if "acl-negative" in facts:
        if facts["acl-negative"]["revoked_device_id"] != facts["phone"]["device_id"]:
            raise EvidenceError("ACL revocation must identify the positively rehearsed phone")
        if facts["acl-negative"]["independent_observer_id"] == facts["acl-negative"]["revoked_device_id"]:
            raise EvidenceError("independent ACL observer must differ from the revoked device")
        if facts["acl-negative"]["independent_observer_id"] == state["operator_id"]:
            raise EvidenceError("independent ACL observer must differ from the recording operator")


def validate_state(state: Any, require_final: bool = False) -> None:
    if not isinstance(state, dict):
        raise EvidenceError("state must be an object")
    reject_secret_like(state, "state")
    require_exact_keys(state, {
        "schema_version", "schema", "rehearsal_id", "operator_id", "created_at",
        "updated_at", "status", "phases",
    }, "state")
    if state["schema_version"] != 1:
        raise EvidenceError("unsupported schema_version")
    if state["schema"] != "deploy/release/production-rehearsal-evidence.schema.json":
        raise EvidenceError("unexpected schema path")
    if not is_id(state["rehearsal_id"]) or not is_id(state["operator_id"]):
        raise EvidenceError("rehearsal_id and operator_id must be pseudonymous safe IDs")
    if not is_timestamp(state["created_at"]) or not is_timestamp(state["updated_at"]):
        raise EvidenceError("state timestamps must be UTC RFC3339 seconds")
    if state["status"] not in ("in-progress", "finalized") or not isinstance(state["phases"], list):
        raise EvidenceError("invalid state status or phases")
    if len(state["phases"]) > len(PHASES):
        raise EvidenceError("too many phases")
    for index, entry in enumerate(state["phases"]):
        if not isinstance(entry, dict):
            raise EvidenceError(f"phase {index} must be an object")
        require_exact_keys(entry, {"name", "completed_at", "operator_confirmation", "evidence"}, f"phase {index}")
        expected_phase = PHASES[index]
        if entry["name"] != expected_phase:
            raise EvidenceError(f"phase {index} must be {expected_phase}")
        if not is_timestamp(entry["completed_at"]):
            raise EvidenceError(f"{expected_phase}.completed_at is invalid")
        confirmation = entry["operator_confirmation"]
        if not isinstance(confirmation, dict):
            raise EvidenceError(f"{expected_phase} confirmation must be an object")
        require_exact_keys(confirmation, {"operator_id", "confirmed_at", "statement"}, f"{expected_phase} confirmation")
        if confirmation["operator_id"] != state["operator_id"] or not is_timestamp(confirmation["confirmed_at"]):
            raise EvidenceError(f"{expected_phase} confirmation identity or timestamp is invalid")
        expected_statement = f"CONFIRM {expected_phase} {state['rehearsal_id']}"
        if confirmation["statement"] != expected_statement:
            raise EvidenceError(f"{expected_phase} confirmation statement is invalid")
        validate_phase_evidence(expected_phase, entry["evidence"])
    finalized = len(state["phases"]) == len(PHASES)
    if state["status"] != ("finalized" if finalized else "in-progress"):
        raise EvidenceError("state status does not match completed phases")
    if require_final and not finalized:
        raise EvidenceError(f"cannot finalize or validate final evidence: {len(PHASES) - len(state['phases'])} phases missing")
    validate_cross_phase(state)


def evidence_dir(value: str) -> pathlib.Path:
    return pathlib.Path(value).expanduser().resolve()


def load_state(directory: pathlib.Path) -> dict[str, Any]:
    path = directory / STATE_FILE
    if path.is_symlink() or not path.is_file():
        raise EvidenceError(f"state file not found or unsafe: {path}")
    try:
        state = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise EvidenceError(f"cannot read state: {error}") from error
    validate_state(state)
    return state


def write_state(directory: pathlib.Path, state: dict[str, Any]) -> None:
    validate_state(state)
    temporary = directory / f".{STATE_FILE}.{os.getpid()}.tmp"
    data = json.dumps(state, indent=2, sort_keys=True) + "\n"
    try:
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, directory / STATE_FILE)
        directory_descriptor = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(directory_descriptor)
        finally:
            os.close(directory_descriptor)
    finally:
        with contextlib.suppress(FileNotFoundError):
            temporary.unlink()


@contextlib.contextmanager
def state_lock(directory: pathlib.Path):
    lock = directory / ".lock"
    try:
        lock.mkdir(mode=0o700)
    except FileExistsError as error:
        raise EvidenceError(f"evidence is locked by another recorder: {lock}") from error
    try:
        yield
    finally:
        with contextlib.suppress(FileNotFoundError):
            lock.rmdir()


def initialize(args: argparse.Namespace) -> None:
    directory = evidence_dir(args.evidence)
    if directory.exists():
        if directory.is_symlink() or not directory.is_dir() or any(directory.iterdir()):
            raise EvidenceError("evidence directory must be absent or empty and must not be a symlink")
    else:
        directory.mkdir(parents=True, mode=0o700)
    os.chmod(directory, 0o700)
    now = utc_now()
    state = {
        "schema_version": 1,
        "schema": "deploy/release/production-rehearsal-evidence.schema.json",
        "rehearsal_id": args.rehearsal_id,
        "operator_id": args.operator_id,
        "created_at": now,
        "updated_at": now,
        "status": "in-progress",
        "phases": [],
    }
    write_state(directory, state)
    print(f"initialized {directory / STATE_FILE}; next phase: {PHASES[0]}")


def read_input(path_value: str) -> Any:
    try:
        if path_value == "-":
            return json.load(sys.stdin)
        path = pathlib.Path(path_value)
        if path.is_symlink() or not path.is_file():
            raise EvidenceError(f"input is not a regular file: {path}")
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise EvidenceError(f"cannot parse evidence input: {error}") from error


def record(args: argparse.Namespace) -> None:
    directory = evidence_dir(args.evidence)
    supplied_evidence = read_input(args.input)
    with state_lock(directory):
        state = load_state(directory)
        index = len(state["phases"])
        if index == len(PHASES):
            raise EvidenceError("rehearsal is already finalized")
        expected = PHASES[index]
        if args.phase != expected:
            raise EvidenceError(f"phase ordering violation: next phase is {expected}, not {args.phase}")
        expected_confirmation = f"CONFIRM {expected} {state['rehearsal_id']}"
        if args.confirm != expected_confirmation:
            raise EvidenceError(f"explicit confirmation required: --confirm '{expected_confirmation}'")
        validate_phase_evidence(expected, supplied_evidence)
        now = utc_now()
        state["phases"].append({
            "name": expected,
            "completed_at": now,
            "operator_confirmation": {
                "operator_id": state["operator_id"],
                "confirmed_at": now,
                "statement": expected_confirmation,
            },
            "evidence": supplied_evidence,
        })
        state["updated_at"] = now
        if len(state["phases"]) == len(PHASES):
            state["status"] = "finalized"
        write_state(directory, state)
    next_phase = PHASES[len(state["phases"])] if state["status"] == "in-progress" else None
    print(f"recorded {expected}; " + (f"next phase: {next_phase}" if next_phase else "evidence finalized"))


def status(args: argparse.Namespace) -> None:
    state = load_state(evidence_dir(args.evidence))
    completed = [entry["name"] for entry in state["phases"]]
    next_phase = PHASES[len(completed)] if len(completed) < len(PHASES) else None
    print(json.dumps({
        "rehearsal_id": state["rehearsal_id"],
        "status": state["status"],
        "completed_phases": completed,
        "next_phase": next_phase,
        "required_confirmation": f"CONFIRM {next_phase} {state['rehearsal_id']}" if next_phase else None,
    }, indent=2, sort_keys=True))


def validate_command(args: argparse.Namespace) -> None:
    state = load_state(evidence_dir(args.evidence))
    validate_state(state, require_final=args.require_final)
    print(f"valid {state['status']} rehearsal evidence ({len(state['phases'])}/{len(PHASES)} phases)")


def fixture_evidence() -> dict[str, dict[str, Any]]:
    before = "11111111-1111-4111-8111-111111111111"
    after = "22222222-2222-4222-8222-222222222222"
    timestamp = "2026-08-27T12:00:00Z"
    origin = "https://fern-test.tail123.ts.net"
    sha_a, sha_b, sha_c = "a" * 64, "b" * 64, "c" * 64
    common_release = {
        "release_version": "v1.2.3",
        "release_commit": "d" * 40,
        "image_digest": "sha256:" + "e" * 64,
    }
    return {
        "source-preflight": {"observed_at": timestamp, "source_host_id": "source-a", "source_boot_id": before,
                             **common_release, "service_active": True, "origin": origin},
        "pre-reboot": {"observed_at": timestamp, "source_host_id": "source-a", "boot_id_before": before,
                       "service_active": True, "reboot_requested": True},
        "post-reboot": {"observed_at": timestamp, "source_host_id": "source-a", "boot_id_before": before,
                        "boot_id_after": after, "boot_changed": True, "service_active": True},
        "source-backup": {"observed_at": timestamp, "source_host_id": "source-a", "source_boot_id": after,
                          "backup_id": "backup-a", "backup_manifest_sha256": sha_a,
                          "backup_payload_sha256": sha_b, "credential_policy": "external",
                          "verification_passed": True},
        "source-fence": {"observed_at": timestamp, "source_host_id": "source-a", "source_boot_id": after,
                         "fence_id": "fence-a", "service_active": False, "origin_disabled": True},
        "target-restore": {"observed_at": timestamp, "target_host_id": "target-b", "target_boot_id": after,
                           **common_release, "backup_id": "backup-a", "backup_manifest_sha256": sha_a,
                           "restore_transaction_sha256": sha_c, "service_active": True},
        "tls-wss": {"observed_at": timestamp, "origin": origin, "observer_id": "observer-a",
                    "tls_valid": True, "certificate_hostname_valid": True, "wss_connected": True},
        "phone": {"observed_at": timestamp, "origin": origin, "device_id": "phone-a", "device_platform": "ios",
                  "network_path": "cellular", "page_loaded": True, "wss_connected": True,
                  "operator_action_passed": True},
        "acl-negative": {"revocation_recorded_at": timestamp, "denial_observed_at": "2026-08-27T12:01:00Z",
                         "origin": origin, "revoked_device_id": "phone-a", "independent_observer_id": "observer-b",
                         "control_device_id": "control-b", "probe_network": "cellular", "acl_denied": True,
                         "denial_kind": "tailscale-acl-denied", "authorized_control_passed": True},
        "finalize": {"observed_at": "2026-08-27T12:02:00Z", "reviewer_id": "reviewer-a",
                     "redaction_reviewed": True, "evidence_complete": True,
                     "source_remains_fenced": True, "target_service_active": True},
    }


def append_test_phase(state: dict[str, Any], phase: str, evidence: dict[str, Any]) -> None:
    statement = f"CONFIRM {phase} {state['rehearsal_id']}"
    state["phases"].append({
        "name": phase,
        "completed_at": "2026-08-27T12:03:00Z",
        "operator_confirmation": {"operator_id": state["operator_id"], "confirmed_at": "2026-08-27T12:03:00Z", "statement": statement},
        "evidence": evidence,
    })
    state["updated_at"] = "2026-08-27T12:03:00Z"
    if len(state["phases"]) == len(PHASES):
        state["status"] = "finalized"


def expect_failure(label: str, action: Callable[[], None], contains: str) -> None:
    try:
        action()
    except EvidenceError as error:
        if contains not in str(error):
            raise AssertionError(f"{label}: unexpected error: {error}") from error
    else:
        raise AssertionError(f"{label}: expected rejection")


def self_test(_: argparse.Namespace) -> None:
    schema = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
    schema_phases = [
        schema["$defs"][item["$ref"].removeprefix("#/$defs/")]["properties"]["name"]["const"]
        for item in schema["properties"]["phases"]["prefixItems"]
    ]
    assert schema_phases == list(PHASES), "schema phase order differs from harness"
    fixtures = fixture_evidence()
    with tempfile.TemporaryDirectory(prefix="fern-production-rehearsal-test.") as temporary:
        directory = pathlib.Path(temporary) / "resume"
        initialize(argparse.Namespace(evidence=str(directory), rehearsal_id="rehearsal-test", operator_id="operator-test"))
        state = load_state(directory)
        append_test_phase(state, PHASES[0], fixtures[PHASES[0]])
        write_state(directory, state)
        resumed = load_state(directory)
        assert len(resumed["phases"]) == 1 and PHASES[len(resumed["phases"])] == "pre-reboot"

        out_of_order = json.loads(json.dumps(resumed))
        append_test_phase(out_of_order, "post-reboot", fixtures["post-reboot"])
        expect_failure("phase ordering", lambda: validate_state(out_of_order), "must be pre-reboot")

        malformed = dict(fixtures["pre-reboot"])
        malformed.pop("boot_id_before")
        expect_failure("malformed evidence", lambda: validate_phase_evidence("pre-reboot", malformed), "missing fields")

        secret_like = dict(fixtures["pre-reboot"])
        secret_like["api_token"] = "not-a-real-token"
        expect_failure("secret-like field", lambda: validate_phase_evidence("pre-reboot", secret_like), "secret-like field")

        expect_failure("missing final phases", lambda: validate_state(resumed, require_final=True), "phases missing")
        premature = json.loads(json.dumps(resumed))
        append_test_phase(premature, "finalize", fixtures["finalize"])
        expect_failure("premature finalize", lambda: validate_state(premature), "must be pre-reboot")

        complete = load_state(directory)
        for phase in PHASES[1:]:
            append_test_phase(complete, phase, fixtures[phase])
        validate_state(complete, require_final=True)
        write_state(directory, complete)
        assert load_state(directory)["status"] == "finalized"
    print("production rehearsal self-tests passed: ordering, resume, malformed/secret rejection, and finalization gate")


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    init_parser = commands.add_parser("init", help="create an empty resumable evidence bundle")
    init_parser.add_argument("--evidence", required=True)
    init_parser.add_argument("--rehearsal-id", required=True)
    init_parser.add_argument("--operator-id", required=True)
    init_parser.set_defaults(function=initialize)

    record_parser = commands.add_parser("record", help="validate and record the next physical phase")
    record_parser.add_argument("--evidence", required=True)
    record_parser.add_argument("--phase", required=True, choices=PHASES)
    record_parser.add_argument("--input", required=True, help="redacted phase JSON file, or - for stdin")
    record_parser.add_argument("--confirm", required=True, help="exact confirmation phrase shown by status/README")
    record_parser.set_defaults(function=record)

    status_parser = commands.add_parser("status", help="show completed and next phases")
    status_parser.add_argument("--evidence", required=True)
    status_parser.set_defaults(function=status)

    validate_parser = commands.add_parser("validate", help="validate an in-progress or finalized bundle")
    validate_parser.add_argument("--evidence", required=True)
    validate_parser.add_argument("--require-final", action="store_true")
    validate_parser.set_defaults(function=validate_command)

    test_parser = commands.add_parser("self-test", help="run local recorder and schema tests")
    test_parser.set_defaults(function=self_test)
    return result


def main() -> int:
    args = parser().parse_args()
    try:
        args.function(args)
    except (EvidenceError, OSError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
