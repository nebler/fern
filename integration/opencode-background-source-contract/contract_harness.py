#!/usr/bin/env python3
"""Black-box real-Docker contract for one exact source-built OpenCode commit."""

from __future__ import annotations

import base64
import json
import os
import pathlib
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any, Callable


COMMIT = "39fb919a054190498f6d5b7985bde231f93ad7a6"
VERSION = f"0.0.0-source-{COMMIT}"
PROFILE = f"source-{COMMIT}"
IMAGE = os.environ.get("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE", "fern/opencode-background-source:dev")
BUILD = os.environ.get("FERN_OPENCODE_BACKGROUND_SOURCE_BUILD", "1") != "0"
EXPECTED_IMAGE_ID = os.environ.get("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID", "")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_for(check: Callable[[], Any], message: str, timeout: float = 30) -> Any:
    deadline = time.monotonic() + timeout
    last: Exception | None = None
    while time.monotonic() < deadline:
        try:
            value = check()
            if value:
                return value
        except Exception as error:
            last = error
        time.sleep(0.1)
    raise AssertionError(message + (f": {last}" if last else ""))


class Harness:
    def __init__(self) -> None:
        self.run_id = uuid.uuid4().hex[:12]
        self.container = f"fern-opencode-background-source-contract-{self.run_id}"
        self.provider = f"fern-opencode-background-source-provider-{self.run_id}"
        self.network = f"fern-opencode-background-source-contract-{self.run_id}"
        self.server_port = free_port()
        self.provider_port = free_port()
        self.password = secrets.token_urlsafe(24)
        self.auth = "Basic " + base64.b64encode(f"opencode:{self.password}".encode()).decode()
        self.base = f"http://127.0.0.1:{self.server_port}"
        self.temp = pathlib.Path(tempfile.mkdtemp(prefix="fern-opencode-background-source-contract-"))
        self.repo = self.temp / "repository"
        self.data = self.temp / "data"
        self.image_id = ""
        self.proven: list[str] = []
        self.blocked: list[str] = []

    def start(self) -> None:
        if BUILD:
            subprocess.run(
                ["docker", "build", "--tag", IMAGE, "images/opencode-background-source"],
                check=True,
            )
        self.image_id = subprocess.check_output(
            ["docker", "image", "inspect", IMAGE, "--format", "{{.Id}}"], text=True
        ).strip()
        require(self.image_id.startswith("sha256:"), f"invalid local image ID: {self.image_id}")
        if EXPECTED_IMAGE_ID:
            require(self.image_id == EXPECTED_IMAGE_ID, f"expected image ID {EXPECTED_IMAGE_ID}, got {self.image_id}")

        inspected = json.loads(subprocess.check_output(["docker", "image", "inspect", self.image_id], text=True))[0]
        labels = inspected["Config"].get("Labels") or {}
        require(labels.get("org.opencontainers.image.source") == "https://github.com/anomalyco/opencode", labels)
        require(labels.get("org.opencontainers.image.revision") == COMMIT, labels)
        require(labels.get("org.opencontainers.image.version") == VERSION, labels)
        require(labels.get("ai.fern.opencode.profile") == PROFILE, labels)
        require(
            inspected["Config"].get("Cmd") == ["opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"],
            f"server command changed: {inspected['Config'].get('Cmd')}",
        )
        image_env = inspected["Config"].get("Env") or []
        require(not any(item.startswith("OPENCODE_SERVER_PASSWORD=") for item in image_env), "image contains a password")

        version = subprocess.check_output(
            ["docker", "run", "--rm", "--entrypoint", "opencode", self.image_id, "--version"], text=True
        ).strip()
        require(version == VERSION, f"expected binary {VERSION}, got {version!r}")
        runtime = subprocess.check_output(
            [
                "docker", "run", "--rm", "--entrypoint", "sh", self.image_id, "-c",
                "printf '%s:%s\\n' \"$(id -u)\" \"$(id -g)\"; "
                "for tool in git gh rg jq node; do command -v \"$tool\" || exit 1; done",
            ],
            text=True,
        ).splitlines()
        require(runtime[0] == "1001:1001" and len(runtime) == 6, f"runtime identity/tools changed: {runtime}")

        self.repo.mkdir()
        self.data.mkdir()
        self.repo.chmod(0o777)
        self.data.chmod(0o777)
        config = {
            "$schema": "https://opencode.ai/config.json",
            "model": "test/test-model",
            "permissions": [
                {"action": "question", "resource": "*", "effect": "allow"},
                {"action": "bash", "resource": "*", "effect": "ask"},
                {"action": "shell", "resource": "*", "effect": "ask"},
            ],
            "agents": {
                "contract": {
                    "description": "Source contract agent",
                    "permissions": [
                        {"action": "question", "resource": "*", "effect": "allow"},
                        {"action": "bash", "resource": "*", "effect": "ask"},
                        {"action": "shell", "resource": "*", "effect": "ask"},
                    ],
                }
            },
            "providers": {
                "test": {
                    "name": "Source Contract",
                    "env": [],
                    "api": {
                        "type": "aisdk",
                        "package": "@ai-sdk/openai-compatible",
                        "url": "http://provider:4100/v1",
                    },
                    "request": {"body": {"apiKey": "test-key"}},
                    "models": {
                        "test-model": {
                            "name": "Source Contract Model",
                            "api": {
                                "id": "test-model",
                                "type": "aisdk",
                                "package": "@ai-sdk/openai-compatible",
                                "url": "http://provider:4100/v1",
                            },
                            "capabilities": {"tools": True, "input": ["text"], "output": ["text"]},
                            "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
                            "limit": {"context": 100000, "output": 10000},
                            "request": {"body": {"apiKey": "test-key"}},
                        }
                    },
                }
            },
        }
        (self.repo / "opencode.json").write_text(json.dumps(config), encoding="utf-8")
        (self.repo / "README.md").write_text("# Source contract repository\n", encoding="utf-8")
        subprocess.run(["git", "init", "--quiet", "--initial-branch=main"], cwd=self.repo, check=True)
        subprocess.run(["git", "add", "."], cwd=self.repo, check=True)
        subprocess.run(
            ["git", "-c", "user.name=Contract", "-c", "user.email=contract@invalid", "commit", "--quiet", "-m", "fixture"],
            cwd=self.repo,
            check=True,
        )

        subprocess.run(["docker", "network", "create", self.network], check=True, stdout=subprocess.DEVNULL)
        subprocess.run(
            [
                "docker", "run", "--detach", "--name", self.provider,
                "--network", self.network, "--network-alias", "provider",
                "--publish", f"127.0.0.1:{self.provider_port}:4100", "--entrypoint", "node",
                "--volume", f"{pathlib.Path(__file__).with_name('fake_provider.mjs')}:/contract/fake_provider.mjs:ro",
                self.image_id, "/contract/fake_provider.mjs", "4100",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
        )
        wait_for(self.provider_stats, "fake provider did not start")
        self.start_opencode()
        self.proven.append(
            f"image labels, binary version, UID:GID, runtime tools, and exact server command identify source commit {COMMIT}; local image ID {self.image_id}"
        )

    def start_opencode(self) -> None:
        subprocess.run(
            [
                "docker", "run", "--detach", "--name", self.container,
                "--network", self.network, "--publish", f"127.0.0.1:{self.server_port}:4096",
                "--env", f"OPENCODE_SERVER_PASSWORD={self.password}",
                "--volume", f"{self.repo}:/home/user/workspace",
                "--volume", f"{self.data}:/home/user/.local/share/opencode",
                self.image_id,
            ],
            check=True,
            stdout=subprocess.DEVNULL,
        )
        wait_for(lambda: self.try_json("GET", "/api/health") == {"healthy": True}, "OpenCode did not become healthy", 60)
        wait_for(lambda: "test-model" in json.dumps(self.try_json("GET", "/api/model") or {}),
                 "V2 fake-provider model did not load", 30)
        wait_for(lambda: '"id": "contract"' in json.dumps(self.try_json("GET", "/api/agent") or {}),
                 "V2 contract agent did not load", 30)

    def replace_opencode(self) -> None:
        subprocess.run(["docker", "rm", "--force", "--volumes", self.container], check=True, stdout=subprocess.DEVNULL)
        self.start_opencode()

    def stop(self) -> None:
        for container in (self.container, self.provider):
            subprocess.run(
                ["docker", "rm", "--force", "--volumes", container],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        subprocess.run(["docker", "network", "rm", self.network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        shutil.rmtree(self.temp, ignore_errors=True)

    def request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        auth: str | None = "correct",
        headers: dict[str, str] | None = None,
        timeout: float = 10,
    ) -> tuple[int, Any, dict[str, str]]:
        data = json.dumps(body).encode() if body is not None else None
        request_headers = dict(headers or {})
        if auth == "correct":
            request_headers["Authorization"] = self.auth
        elif auth == "wrong":
            request_headers["Authorization"] = "Basic " + base64.b64encode(b"opencode:wrong").decode()
        if data is not None:
            request_headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=request_headers)
        try:
            response = urllib.request.urlopen(req, timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        raw = response.read()
        content_type = response.headers.get("content-type", "")
        parsed: Any = json.loads(raw) if raw and "json" in content_type else raw.decode(errors="replace")
        return response.status, parsed, {key.lower(): value for key, value in response.headers.items()}

    def try_json(self, method: str, path: str) -> Any:
        try:
            status, body, _ = self.request(method, path, timeout=1)
            return body if status == 200 else None
        except (OSError, TimeoutError):
            return None

    def json_ok(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        status, result, _ = self.request(method, path, body)
        require(status == 200, f"{method} {path}: expected 200, got {status}: {result}")
        return result

    def provider_stats(self) -> dict[str, Any] | None:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{self.provider_port}/stats", timeout=1) as response:
                return json.load(response)
        except (OSError, TimeoutError):
            return None

    def provider_turns(self, marker: str) -> int:
        stats = self.provider_stats() or {}
        return sum(marker in json.dumps(item) for item in stats.get("requests", []))

    def provider_calls(self) -> int:
        return int((self.provider_stats() or {}).get("calls", 0))

    def create_session(self, name: str) -> tuple[str, dict[str, Any]]:
        session_id = f"ses_source_contract_{name}_{self.run_id}"
        payload = {
            "id": session_id,
            "agent": "contract",
            "model": {"providerID": "test", "id": "test-model"},
            "location": {"directory": "/home/user/workspace"},
        }
        created = self.json_ok("POST", "/api/session", payload)["data"]
        require(created["id"] == session_id, f"server replaced caller session ID: {created}")
        require(created.get("agent") == payload["agent"], f"agent projection changed: {created}")
        require(
            created.get("model") == {**payload["model"], "variant": "default"},
            f"model projection changed: {created}",
        )
        require(created.get("location") == payload["location"], f"location projection changed: {created}")
        exact = self.json_ok("GET", f"/api/session/{session_id}")["data"]
        require(exact == created, "exact session read differs from creation")
        adopted = self.json_ok(
            "POST", "/api/session",
            {"id": session_id, "agent": "build", "model": {"providerID": "wrong", "id": "wrong"},
             "location": {"directory": "/home/user"}},
        )["data"]
        require(adopted == created, "reusing the Session ID mutated its immutable projection")
        return session_id, created

    def history(self, session_id: str, limit: int = 2) -> list[dict[str, Any]]:
        result: list[dict[str, Any]] = []
        after = 0
        for _ in range(200):
            page = self.json_ok("GET", f"/api/session/{session_id}/history?limit={limit}&after={after}")
            require(len(page["data"]) <= limit, "history exceeded its finite requested limit")
            if not page["data"]:
                require(not page["hasMore"], "empty history page claimed more data")
                return result
            seqs = [int(item["durable"]["seq"]) for item in page["data"]]
            require(seqs == sorted(seqs) and min(seqs) > after, f"history sequence did not advance: {seqs}")
            result.extend(page["data"])
            after = max(seqs)
            if not page["hasMore"]:
                return result
        raise AssertionError("history pagination did not terminate")

    def messages(self, session_id: str, limit: int = 2) -> list[dict[str, Any]]:
        result: list[dict[str, Any]] = []
        cursor: str | None = None
        for _ in range(200):
            query = f"?limit={limit}&order=asc" if cursor is None else f"?limit={limit}&cursor={urllib.parse.quote(cursor)}"
            page = self.json_ok("GET", f"/api/session/{session_id}/message{query}")
            require(len(page["data"]) <= limit, "message page exceeded finite limit")
            if not page["data"]:
                return result
            result.extend(page["data"])
            next_cursor = page["cursor"].get("next")
            if next_cursor is None:
                return result
            require(next_cursor != cursor, "message cursor did not advance")
            cursor = next_cursor
        raise AssertionError("message pagination did not terminate")

    def test_surface(self) -> None:
        for auth in (None, "wrong"):
            status, body, headers = self.request("GET", "/api/health", auth=auth)
            require(
                status == 401 and body == {"_tag": "UnauthorizedError", "message": "Authentication required"},
                f"{auth} auth changed: {status}: {body!r}",
            )
            require(headers.get("www-authenticate") == 'Basic realm="Secure Area"', "Basic challenge changed")
        require(self.json_ok("GET", "/api/health") == {"healthy": True}, "V2 health changed")
        global_health = self.json_ok("GET", "/global/health")
        require(global_health == {"healthy": True, "version": VERSION}, f"version health changed: {global_health}")
        self.proven.extend([
            "missing and wrong Basic auth receive the typed UnauthorizedError 401 with the Basic challenge; correct auth succeeds",
            f"V2 /api/health is healthy and /global/health reports the commit-derived version {VERSION}",
        ])

    def test_identity_retry_history_replacement(self) -> None:
        session_id, session = self.create_session("identity")
        attach_session = self.json_ok("GET", f"/session/{session_id}")
        require(attach_session["id"] == session_id, "stable attach client route did not resolve the V2-created session")
        message_id = f"msg_source_contract_retry_{self.run_id}"
        text = f"CONTRACT_ADMISSION_{self.run_id}"
        prompt = {"id": message_id, "prompt": {"text": text}, "delivery": "steer", "resume": False}
        calls = self.provider_calls()
        first = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)
        require(first["data"]["id"] == message_id, "server replaced caller prompt ID")
        require(first["data"]["sessionID"] == session_id, "prompt owner changed")
        require(first["data"]["prompt"] == {"text": text}, "prompt bytes changed")
        require(first["data"]["delivery"] == "steer", "prompt delivery changed")
        require("promotedSeq" not in first["data"], "resume=false prompt was promoted")

        before = self.history(session_id, 1)
        admitted = [item for item in before if item["type"] == "session.next.prompt.admitted"]
        require(len(admitted) == 1, f"durable admission evidence changed: {before}")
        require(admitted[0]["data"]["messageID"] == message_id, "durable prompt ID changed")
        require(admitted[0]["data"]["sessionID"] == session_id, "durable prompt owner changed")
        require(admitted[0]["data"]["prompt"] == {"text": text}, "durable prompt bytes changed")
        require(admitted[0]["data"]["delivery"] == "steer", "durable delivery changed")
        require(first["data"]["admittedSeq"] == admitted[0]["durable"]["seq"], "admission sequence mismatch")

        exact = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)
        require(exact == first, "exact retry changed the admission object")
        require(self.history(session_id, 1) == before, "exact retry added durable history")
        require(self.provider_calls() == calls, "resume=false exact retry called the provider")
        status, conflict, _ = self.request(
            "POST", f"/api/session/{session_id}/prompt",
            {"id": message_id, "prompt": {"text": text + " changed"}, "delivery": "steer", "resume": False},
        )
        require(status == 409 and conflict.get("_tag") == "ConflictError", f"conflicting retry changed: {status}: {conflict}")
        require(self.history(session_id, 1) == before, "conflicting retry changed durable history")
        require(self.provider_calls() == calls, "conflicting retry called the provider")

        self.replace_opencode()
        require(self.json_ok("GET", f"/api/session/{session_id}")["data"] == session, "session changed across replacement")
        require(self.history(session_id, 1) == before, "durable history changed across replacement")
        retry = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)
        require(retry == first, "post-replacement exact retry changed admission")
        require(self.history(session_id, 1) == before, "post-replacement retry duplicated history")
        require(self.provider_calls() == calls, "post-replacement resume=false retry called provider")
        self.proven.extend([
            "the stable OpenCode attach client route resolves the exact caller-selected V2 session",
            "caller-selected Session ID is accepted; exact reads reconcile its agent, model provider/model ID, and location, and same-ID creation adopts without mutation",
            "caller-selected prompt ID and exact text/delivery are represented by one durable prompt-admitted event",
            "finite limit-bounded durable history is ordered and sufficient to reconcile admitted prompt identity",
            "exact retry is byte-stable and side-effect free; conflicting prompt reuse returns HTTP 409 without history or provider effects",
            "the same exact session, prompt admission, IDs, and history survive container replacement; exact retry remains side-effect free",
        ])

    def test_active_loss_and_interrupt(self) -> None:
        session_id, _ = self.create_session("active_loss")
        message_id = f"msg_source_contract_active_{self.run_id}"
        marker = f"CONTRACT_HANG_ACTIVE_{self.run_id}"
        self.json_ok("POST", f"/api/session/{session_id}/prompt", {"id": message_id, "prompt": {"text": marker}})
        wait_for(lambda: self.provider_turns(marker) == 1, "active prompt did not reach provider")
        wait_for(lambda: session_id in self.json_ok("GET", "/api/session/active")["data"], "active session missing")
        wait_for(lambda: any(item.get("id") == message_id for item in self.messages(session_id)), "user message was not projected")
        before = self.history(session_id)
        require(any(item["type"] == "session.next.prompt.admitted" for item in before), "active prompt lacks admission")
        require(any(item["type"] == "session.next.prompted" for item in before), "active prompt lacks promotion")
        require(not any(item["type"] in ("session.next.step.ended", "session.next.step.failed") for item in before), "hanging step settled")
        disconnects = int((self.provider_stats() or {}).get("disconnects", 0))
        self.replace_opencode()
        wait_for(lambda: int((self.provider_stats() or {}).get("disconnects", 0)) > disconnects, "replacement did not disconnect provider")
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "active ownership survived replacement")
        after = self.history(session_id)
        require(after == before, "process replacement fabricated durable settlement")
        exact = self.json_ok("GET", f"/api/session/{session_id}/message/{message_id}")["data"]
        require(exact["id"] == message_id and exact["type"] == "user" and exact["text"] == marker, "admitted user message changed")

        interrupt_session, _ = self.create_session("interrupt")
        interrupt_id = f"msg_source_contract_interrupt_{self.run_id}"
        interrupt_marker = f"CONTRACT_HANG_INTERRUPT_{self.run_id}"
        self.json_ok(
            "POST", f"/api/session/{interrupt_session}/prompt",
            {"id": interrupt_id, "prompt": {"text": interrupt_marker}},
        )
        wait_for(lambda: self.provider_turns(interrupt_marker) == 1, "interrupt prompt did not reach provider")
        wait_for(lambda: interrupt_session in self.json_ok("GET", "/api/session/active")["data"], "interrupt session missing")
        disconnects = int((self.provider_stats() or {}).get("disconnects", 0))
        status, body, _ = self.request("POST", f"/api/session/{interrupt_session}/interrupt")
        require(status == 204 and body == "", f"interrupt changed: {status}: {body!r}")
        wait_for(lambda: interrupt_session not in self.json_ok("GET", "/api/session/active")["data"], "interrupt did not clear active")
        wait_for(lambda: int((self.provider_stats() or {}).get("disconnects", 0)) > disconnects, "interrupt did not disconnect provider")
        require(self.json_ok("GET", f"/api/session/{interrupt_session}/message/{interrupt_id}")["data"]["text"] == interrupt_marker, "interrupt lost user message")
        self.proven.extend([
            "process-local active execution disappears on replacement while durable admission, promotion, and user evidence remain without settlement, making outcome uncertain",
            "interrupt targets the exact active Session, disconnects its provider stream, clears active ownership, and preserves its caller-selected user message",
        ])

    def test_executed_retry(self) -> None:
        session_id, _ = self.create_session("executed_retry")
        message_id = f"msg_source_contract_executed_retry_{self.run_id}"
        marker = f"CONTRACT_EXECUTED_RETRY_{self.run_id}"
        prompt = {"id": message_id, "prompt": {"text": marker}, "delivery": "steer"}
        self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)
        wait_for(lambda: self.provider_turns(marker) == 1, "executed retry setup did not reach provider")
        wait_for(
            lambda: session_id not in self.json_ok("GET", "/api/session/active")["data"],
            "executed retry setup did not become inactive",
        )
        wait_for(
            lambda: "source contract fake response" in json.dumps(self.messages(session_id)),
            "executed retry setup did not project the provider response",
        )
        history = self.history(session_id)
        messages = self.messages(session_id)
        require([item["id"] for item in messages].count(message_id) == 1, "executed caller message duplicated")

        retry = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)["data"]
        require(retry["id"] == message_id and "promotedSeq" in retry, "executed retry did not reconcile promotion")
        time.sleep(0.5)
        require(self.provider_turns(marker) == 1, "executed exact retry caused a second provider turn")
        require(self.history(session_id) == history, "executed exact retry added durable history")
        require(self.messages(session_id) == messages, "executed exact retry changed messages")

        self.replace_opencode()
        retry = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)["data"]
        require(retry["id"] == message_id and "promotedSeq" in retry, "replacement retry lost promotion")
        time.sleep(0.5)
        require(self.provider_turns(marker) == 1, "post-replacement exact retry caused a provider turn")
        require(self.history(session_id) == history, "post-replacement exact retry changed durable history")
        require(self.messages(session_id) == messages, "post-replacement exact retry changed messages")
        self.proven.append(
            "exact retry of a normally resumed, durably completed prompt causes no second provider turn, message, or event before or after replacement"
        )

    def test_question_permission_volatility(self) -> None:
        permission_session, _ = self.create_session("permission")
        permission_id = f"per_source_contract_{self.run_id}"
        provider_calls = self.provider_calls()
        created = self.json_ok(
            "POST", f"/api/session/{permission_session}/permission",
            {"id": permission_id, "action": "shell", "resources": ["contract-no-command"], "agent": "contract"},
        )["data"]
        require(created == {"id": permission_id, "effect": "ask"}, f"permission was not pending: {created}")
        listed = self.json_ok("GET", f"/api/session/{permission_session}/permission")["data"]
        require([item["id"] for item in listed] == [permission_id], f"permission list changed: {listed}")
        self.replace_opencode()
        require(self.json_ok("GET", f"/api/session/{permission_session}/permission")["data"] == [], "permission survived replacement")
        require(self.provider_calls() == provider_calls, "synthetic permission called provider")

        question_session, _ = self.create_session("question")
        question_id = f"msg_source_contract_question_{self.run_id}"
        self.json_ok(
            "POST", f"/api/session/{question_session}/prompt",
            {"id": question_id, "prompt": {"text": "CONTRACT_QUESTION"}},
        )
        questions = wait_for(
            lambda: self.json_ok("GET", f"/api/session/{question_session}/question")["data"] or None,
            "fake-provider question did not become pending",
        )
        require(questions[0]["sessionID"] == question_session, "question owner changed")
        require(questions[0]["questions"][0]["options"][0]["label"] == "Choice A", "question bytes changed")
        self.replace_opencode()
        require(self.json_ok("GET", f"/api/session/{question_session}/question")["data"] == [], "question survived replacement")
        require(self.json_ok("GET", f"/api/session/{question_session}")["data"]["id"] == question_session, "question session was lost")
        self.proven.extend([
            "a directly synthesized ask-only permission requires no provider or shell effect and is volatile across process replacement",
            "a zero-cost fake-provider question is session-scoped and volatile across process replacement; no shell tool is requested or executed",
        ])
        self.blocked.extend([
            "question recovery: pending question state is process-local and cannot be answered after replacement",
            "automatic completion authority: active absence and stream loss are not proof of success",
            "durable provider-turn start: the hanging stream is active and reaches the provider, but history has no step-start event before settlement",
        ])

    def run(self) -> None:
        self.test_surface()
        self.test_identity_retry_history_replacement()
        self.test_executed_retry()
        self.test_active_loss_and_interrupt()
        self.test_question_permission_volatility()


def main() -> int:
    harness = Harness()
    try:
        harness.start()
        harness.run()
        print(f"\nIMAGE TAG: {IMAGE}")
        print(f"CAPTURED LOCAL IMAGE ID: {harness.image_id}")
        print(f"SOURCE COMMIT: {COMMIT}")
        print("\nPROVEN CONTRACT:")
        for item in harness.proven:
            print(f"  PASS {item}")
        print("\nBLOCKED OR UNPROVEN:")
        for item in harness.blocked:
            print(f"  BLOCKED {item}")
        print(f"\nRESULT: PASS ({len(harness.proven)} proven, {len(harness.blocked)} blocked/unproven)")
        return 0
    except Exception as error:
        print(f"\nRESULT: FAIL: {error}")
        traceback.print_exc()
        subprocess.run(["docker", "logs", "--tail", "150", harness.container], check=False)
        return 1
    finally:
        harness.stop()


if __name__ == "__main__":
    raise SystemExit(main())
