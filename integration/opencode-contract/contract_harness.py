#!/usr/bin/env python3
"""Black-box contracts for pinned OpenCode 0.0.0-next-17444."""

from __future__ import annotations

import base64
import http.client
import json
import os
import pathlib
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any, Callable


PINNED_VERSION = "0.0.0-next-17444"
PINNED_DIGEST = "sha256:73688cd6f96ce3b236bb1c2d25607b03566a4ee92f0fedabeb06fd1a3e643c6c"
IMAGE = os.environ.get("FERN_OPENCODE_IMAGE", "fern/opencode:dev")
AUTH = "Basic " + base64.b64encode(b"opencode:contract-password").decode()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_for(check: Callable[[], Any], message: str, timeout: float = 15) -> Any:
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
    detail = f": {last}" if last else ""
    raise AssertionError(message + detail)


class Harness:
    def __init__(self) -> None:
        self.run_id = uuid.uuid4().hex[:12]
        self.container = f"fern-opencode-contract-{self.run_id}"
        self.provider_container = f"fern-opencode-contract-provider-{self.run_id}"
        self.network = f"fern-opencode-contract-{self.run_id}"
        self.provider_port = free_port()
        self.server_port = free_port()
        self.temp = pathlib.Path(tempfile.mkdtemp(prefix="fern-opencode-contract-"))
        self.repo = self.temp / "repository"
        self.data = self.temp / "data"
        self.base = f"http://127.0.0.1:{self.server_port}"
        self.proven: list[str] = []
        self.blocked: list[str] = []
        self.image_id = ""

    def start(self) -> None:
        self.repo.mkdir()
        self.data.mkdir()
        self.repo.chmod(0o777)
        self.data.chmod(0o777)
        config = {
            "$schema": "https://opencode.ai/config.json",
            "model": "test/test-model",
            "small_model": "test/test-model",
            "formatter": False,
            "lsp": False,
            "permission": {"question": "allow", "bash": "ask"},
            "agent": {
                "contract": {
                    "description": "Contract test agent",
                    "permission": {"question": "allow", "bash": "ask"},
                }
            },
            "provider": {
                "test": {
                    "name": "Contract Test",
                    "id": "test",
                    "env": [],
                    "npm": "@ai-sdk/openai-compatible",
                    "options": {
                        "apiKey": "test-key",
                        "baseURL": "http://provider:4100/v1",
                    },
                    "models": {
                        "test-model": {
                            "id": "test-model",
                            "name": "Contract Test Model",
                            "release_date": "2025-01-01",
                            "attachment": False,
                            "reasoning": False,
                            "temperature": False,
                            "tool_call": True,
                            "cost": {"input": 0, "output": 0},
                            "limit": {"context": 100000, "output": 10000},
                            "options": {},
                        }
                    },
                }
            },
        }
        (self.repo / "opencode.json").write_text(json.dumps(config), encoding="utf-8")
        self.image_id = subprocess.check_output(
            ["docker", "image", "inspect", IMAGE, "--format", "{{.Id}}"], text=True
        ).strip()
        require(self.image_id == PINNED_DIGEST, f"expected image {PINNED_DIGEST}, got {self.image_id}")
        subprocess.run(["docker", "network", "create", self.network], check=True, stdout=subprocess.DEVNULL)
        subprocess.run(
            [
                "docker",
                "run",
                "--detach",
                "--name",
                self.provider_container,
                "--network",
                self.network,
                "--network-alias",
                "provider",
                "--publish",
                f"127.0.0.1:{self.provider_port}:4100",
                "--entrypoint",
                "node",
                "--volume",
                f"{pathlib.Path(__file__).with_name('fake_provider.mjs')}:/contract/fake_provider.mjs:ro",
                IMAGE,
                "/contract/fake_provider.mjs",
                "4100",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
        )
        wait_for(lambda: self.provider_stats(), "fake provider did not start")
        self.start_opencode()
        self.proven.append(f"image reports exact version {PINNED_VERSION} at {PINNED_DIGEST}")

    def start_opencode(self) -> None:
        subprocess.run(
            [
                "docker",
                "run",
                "--detach",
                "--name",
                self.container,
                "--network",
                self.network,
                "--publish",
                f"127.0.0.1:{self.server_port}:4096",
                "--env",
                "OPENCODE_PASSWORD=contract-password",
                "--volume",
                f"{self.repo}:/home/user/workspace",
                "--volume",
                f"{self.data}:/home/user/.local/share/opencode",
                IMAGE,
            ],
            check=True,
            stdout=subprocess.DEVNULL,
        )
        self.wait_opencode()

    def wait_opencode(self) -> None:
        health = wait_for(lambda: self.try_json("GET", "/api/health"), "OpenCode did not become healthy", 40)
        require(health.get("version") == PINNED_VERSION, f"expected {PINNED_VERSION}, got {health}")

        ready_since: float | None = None

        def config_ready() -> Any:
            nonlocal ready_since
            config = self.try_json("GET", "/api/config")
            serialized = json.dumps(config)
            expected = (
                config
                and "test-model" in serialized
                and '"action": "shell"' in serialized
                and '"effect": "ask"' in serialized
            )
            if not expected:
                ready_since = None
                return None
            ready_since = ready_since or time.monotonic()
            return config if time.monotonic() - ready_since >= 2 else None

        wait_for(config_ready, "workspace fake-provider config did not become effective", 20)

    def restart_same_container(self) -> None:
        subprocess.run(
            ["docker", "restart", "--time", "1", self.container],
            check=True,
            stdout=subprocess.DEVNULL,
            timeout=20,
        )
        self.wait_opencode()

    def replace_opencode_container(self) -> None:
        # Recreate compute while retaining the mounted repository and OpenCode data.
        subprocess.run(["docker", "rm", "--force", self.container], check=True, stdout=subprocess.DEVNULL)
        self.start_opencode()

    def stop(self) -> None:
        subprocess.run(["docker", "rm", "--force", self.container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(
            ["docker", "rm", "--force", self.provider_container],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        subprocess.run(
            ["docker", "network", "rm", self.network],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        shutil.rmtree(self.temp, ignore_errors=True)

    def request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
        timeout: float = 10,
    ) -> tuple[int, Any, dict[str, str]]:
        data = json.dumps(body).encode() if body is not None else None
        request_headers = {"Authorization": AUTH, **(headers or {})}
        if body is not None:
            request_headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=request_headers)
        try:
            response = urllib.request.urlopen(req, timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        raw = response.read()
        parsed = json.loads(raw) if raw else None
        return response.status, parsed, {key.lower(): value for key, value in response.headers.items()}

    def try_json(self, method: str, path: str) -> Any:
        try:
            status, body, _ = self.request(method, path, timeout=1)
            return body if status == 200 else None
        except (OSError, TimeoutError):
            return None

    def provider_stats(self) -> dict[str, Any] | None:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{self.provider_port}/stats", timeout=1) as response:
                return json.load(response)
        except (OSError, TimeoutError):
            return None

    def json_ok(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        status, result, _ = self.request(method, path, body)
        require(status == 200, f"{method} {path}: expected 200, got {status}: {result}")
        return result

    def create_session(self, name: str, caller_selected: bool = True) -> str:
        session_id = f"ses_contract_{name}_{self.run_id}"
        body: dict[str, Any] = {
            "title": f"Contract {name}",
            "agent": "build",
            "model": {"providerID": "test", "id": "test-model"},
            "location": {"directory": "/home/user/workspace"},
        }
        if caller_selected:
            body["id"] = session_id
        result = self.json_ok("POST", "/api/session", body)
        actual = result["data"]["id"]
        require(not caller_selected or actual == session_id, f"server replaced caller session ID: {actual}")
        projected = result["data"]
        require(projected.get("title") == body["title"], "session title projection changed")
        require(projected.get("agent") == body["agent"], "session agent projection changed")
        require(projected.get("model", {}).get("providerID") == "test", "session provider projection changed")
        require(projected.get("model", {}).get("id") == "test-model", "session model projection changed")
        require(projected.get("location", {}).get("directory") == "/home/user/workspace", "session location projection changed")
        exact = self.json_ok("GET", f"/api/session/{actual}")["data"]
        for key in ("id", "title", "agent", "model", "location"):
            require(exact.get(key) == projected.get(key), f"exact session {key} projection changed")
        return actual

    def send_and_drop(self, method: str, path: str, body: dict[str, Any] | None = None) -> None:
        payload = json.dumps(body).encode() if body is not None else None
        connection = http.client.HTTPConnection("127.0.0.1", self.server_port, timeout=3)
        connection.putrequest(method, path)
        connection.putheader("Authorization", AUTH)
        if payload is not None:
            connection.putheader("Content-Type", "application/json")
            connection.putheader("Content-Length", str(len(payload)))
        connection.endheaders(payload)
        connection.close()

    def sse_events(
        self,
        path: str,
        count: int,
        headers: dict[str, str] | None = None,
        timeout: float = 3,
    ) -> list[dict[str, Any]]:
        connection = http.client.HTTPConnection("127.0.0.1", self.server_port, timeout=timeout)
        connection.request("GET", path, headers={"Authorization": AUTH, **(headers or {})})
        response = connection.getresponse()
        require(response.status == 200, f"SSE {path}: expected 200, got {response.status}")
        events: list[dict[str, Any]] = []
        current: dict[str, Any] = {}
        try:
            while len(events) < count:
                line = response.readline().decode().rstrip("\r\n")
                if not line:
                    if current:
                        events.append(current)
                        current = {}
                    continue
                if line.startswith("id:"):
                    current["id"] = line[3:].strip()
                elif line.startswith("event:"):
                    current["event"] = line[6:].strip()
                elif line.startswith("data:"):
                    value = line[5:].strip()
                    current["data"] = json.loads(value)
        finally:
            connection.close()
        return events

    def finite_sse(self, path: str) -> list[dict[str, Any]]:
        connection = http.client.HTTPConnection("127.0.0.1", self.server_port, timeout=5)
        connection.request("GET", path, headers={"Authorization": AUTH})
        response = connection.getresponse()
        require(response.status == 200, f"SSE {path}: expected 200, got {response.status}")
        raw = response.read().decode()
        connection.close()
        return [
            {"data": json.loads(line[5:].strip())}
            for line in raw.splitlines()
            if line.startswith("data:")
        ]

    def paginated_messages(self, session_id: str, limit: int = 1) -> list[dict[str, Any]]:
        messages: list[dict[str, Any]] = []
        cursor: str | None = None
        for _ in range(100):
            query = f"?limit={limit}&order=asc" if cursor is None else f"?limit={limit}&cursor={urllib.parse.quote(cursor)}"
            page = self.json_ok("GET", f"/api/session/{session_id}/message{query}")
            require(len(page["data"]) <= limit, "message page exceeded requested finite limit")
            if not page["data"]:
                return messages
            messages.extend(page["data"])
            next_cursor = page["cursor"].get("next")
            if next_cursor is None:
                return messages
            require(next_cursor != cursor, "message cursor did not advance")
            cursor = next_cursor
        raise AssertionError("message pagination did not terminate")

    def provider_turns(self, marker: str) -> int:
        stats = self.provider_stats() or {}
        return sum(
            marker in json.dumps(request) and bool(request.get("tools"))
            for request in stats.get("requests", [])
        )

    def test_caller_ids_and_retry(self) -> None:
        session_id = self.create_session("identity")
        adopted = self.json_ok(
            "POST",
            "/api/session",
            {
                "id": session_id,
                "agent": "build",
                "model": {"providerID": "test", "id": "test-model"},
                "location": {"directory": "/home/user/workspace"},
            },
        )
        require(adopted["data"]["id"] == session_id, "reused session ID was not adopted")
        message_id = f"msg_contract_retry_{self.run_id}"
        prompt = {"id": message_id, "text": "lost response contract", "resume": False}
        self.send_and_drop("POST", f"/api/session/{session_id}/prompt", prompt)

        def admitted() -> bool:
            return any(
                item["id"] == message_id
                for item in self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]
            )

        wait_for(admitted, "dropped request was not durably admitted")
        first = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)
        second = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)
        require(first == second, "exact retry did not return the same durable admission")
        require(first["data"]["id"] == message_id, "server replaced caller prompt ID")
        inbox = self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]
        require(
            [item["id"] for item in inbox].count(message_id) == 1,
            "exact retry duplicated the durable inbox item",
        )
        status, conflict, _ = self.request(
            "POST",
            f"/api/session/{session_id}/prompt",
            {"id": message_id, "text": "conflicting body", "resume": False},
        )
        require(status == 409 and conflict.get("_tag") == "ConflictError", f"expected conflict, got {status}: {conflict}")
        self.proven.extend(
            [
                "caller-selected Session ID is accepted and reused as the existing Session",
                "session creation projects the exact title, agent, model provider/model ID, and working directory",
                "caller-selected prompt/message ID is preserved",
                "a response-lost prompt is durable before retry; exact retries are stable and do not duplicate",
                "same prompt ID with a different body returns HTTP 409 ConflictError",
            ]
        )

    def test_restart_retry_and_history(self) -> None:
        session_id = self.create_session("restart")
        message_id = f"msg_contract_restart_{self.run_id}"
        marker = f"CONTRACT_RESTART_ONCE_{self.run_id}"
        prompt = {"id": message_id, "text": marker}
        self.send_and_drop("POST", f"/api/session/{session_id}/prompt", prompt)
        wait_for(lambda: self.provider_turns(marker) == 1, "response-lost prompt did not start one provider turn", 20)

        def completed() -> list[dict[str, Any]] | None:
            context = self.json_ok("GET", f"/api/session/{session_id}/context")["data"]
            serialized = json.dumps(context)
            return context if message_id in serialized and "contract fake response" in serialized else None

        wait_for(completed, "response-lost prompt did not complete", 20)
        wait_for(
            lambda: session_id not in self.json_ok("GET", "/api/session/active")["data"],
            "completed response-lost prompt remained active",
        )
        require(self.provider_turns(marker) == 1, "response-lost prompt made multiple provider turns before restart")
        require(
            all(item["id"] != message_id for item in self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]),
            "completed response-lost prompt remained in the inbox",
        )
        before = self.paginated_messages(session_id)
        before_ids = [message["id"] for message in before]
        require(before_ids.count(message_id) == 1, f"caller message was not projected exactly once: {before_ids}")

        self.replace_opencode_container()
        session = self.json_ok("GET", f"/api/session/{session_id}")["data"]
        require(session["id"] == session_id, "caller-selected session ID did not survive restart")
        after = self.paginated_messages(session_id)
        require([message["id"] for message in after] == before_ids, "finite message IDs changed across restart")
        retry = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)
        require(retry["data"]["id"] == message_id, "exact retry did not reconcile the projected message")
        time.sleep(0.5)
        require(self.provider_turns(marker) == 1, "post-restart exact retry started a second provider turn")
        require(
            all(item["id"] != message_id for item in self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]),
            "post-restart exact retry recreated an inbox item",
        )
        require(
            [message["id"] for message in self.paginated_messages(session_id)] == before_ids,
            "post-restart exact retry duplicated projected messages",
        )
        status, conflict, _ = self.request(
            "POST",
            f"/api/session/{session_id}/prompt",
            {"id": message_id, "text": marker + "_CONFLICT"},
        )
        require(status == 409 and conflict.get("_tag") == "ConflictError", f"restart conflict changed: {status}: {conflict}")
        self.proven.extend(
            [
                "caller-selected session/message IDs and exact finite message pages survive OpenCode container replacement",
                "a response-lost executing prompt completes one provider turn; post-restart exact retry creates no inbox, message, or provider duplicate",
                "same-ID conflicting prompt remains HTTP 409 after restart",
            ]
        )

    def test_history_and_event_cursors(self) -> None:
        session_id = self.create_session("cursor")
        for agent in ("plan", "build", "plan", "build"):
            status, body, _ = self.request("POST", f"/api/session/{session_id}/agent", {"agent": agent})
            require(status == 204, f"switch agent failed: {status}: {body}")
        messages: list[dict[str, Any]] = []
        cursor: str | None = None
        for _ in range(10):
            query = "?limit=2&order=asc" if cursor is None else f"?limit=2&cursor={urllib.parse.quote(cursor)}"
            page = self.json_ok("GET", f"/api/session/{session_id}/message{query}")
            require(len(page["data"]) <= 2, "history exceeded requested finite limit")
            if not page["data"]:
                break
            messages.extend(page["data"])
            next_cursor = page["cursor"].get("next")
            if next_cursor is None:
                break
            require(next_cursor != cursor, "message cursor did not advance")
            cursor = next_cursor
        require(len(messages) == 4, f"finite history omitted messages: {messages}")
        ids = [message["id"] for message in messages]
        require(len(ids) == len(set(ids)), f"finite history duplicated messages: {ids}")
        require(
            [message["time"]["created"] for message in messages]
            == sorted(message["time"]["created"] for message in messages),
            "finite history changed ascending order across pages",
        )

        first_log = self.finite_sse(f"/api/experimental/session/{session_id}/log?follow=false&after=0")
        future_log = self.finite_sse(f"/api/experimental/session/{session_id}/log?follow=false&after=999999")
        require(len(first_log) == 1 and first_log[0]["data"]["type"] == "log.synced", f"unexpected log: {first_log}")
        require(first_log == future_log, f"experimental log unexpectedly replayed after cursor: {future_log}")
        status, _, _ = self.request("GET", f"/api/session/{session_id}/history")
        require(status == 404, "undocumented session history route unexpectedly exists")
        status, _, _ = self.request("GET", f"/api/session/{session_id}/event")
        require(status == 404, "undocumented durable session event route unexpectedly exists")
        global_event = self.sse_events("/api/event", 1, {"Last-Event-ID": "evt_contract_missing"})[0]
        require(global_event["data"]["type"] == "server.connected", f"unexpected global event start: {global_event}")
        self.proven.extend(
            [
                "projected message history is finite, limit-bounded, cursor-paginated, duplicate-free, and order-preserving",
                "global /api/event ignores Last-Event-ID and starts a new volatile stream with server.connected",
            ]
        )
        self.blocked.extend(
            [
                "durable event history replay: /api/session/{id}/history and /event are absent (HTTP 404)",
                "experimental log replay: follow=false returns only the same log.synced watermark for after=0 and a future cursor",
            ]
        )

    def test_permission_surface(self) -> None:
        session_id = self.create_session("permission")
        permission_id = f"per_contract_{self.run_id}"
        created = self.json_ok(
            "POST",
            f"/api/session/{session_id}/permission",
            {
                "id": permission_id,
                "action": "shell",
                "resources": ["echo contract"],
                "agent": "contract",
            },
        )
        require(created["data"] == {"id": permission_id, "effect": "ask"}, f"permission was not pending: {created}")
        listed = self.json_ok("GET", f"/api/session/{session_id}/permission")["data"]
        require([item["id"] for item in listed] == [permission_id], f"permission list mismatch: {listed}")
        got = self.json_ok("GET", f"/api/session/{session_id}/permission/{permission_id}")["data"]
        require(got["sessionID"] == session_id and got["action"] == "shell", "permission owner mismatch")
        status, body, _ = self.request(
            "POST", f"/api/session/{session_id}/permission/{permission_id}/reply", {"reply": "once"}
        )
        require(status == 204, f"permission reply failed: {status}: {body}")
        require(self.json_ok("GET", f"/api/session/{session_id}/permission")["data"] == [], "permission stayed pending")
        status, missing, _ = self.request(
            "POST", f"/api/session/{session_id}/permission/{permission_id}/reply", {"reply": "once"}
        )
        require(status == 404 and missing.get("_tag") == "PermissionNotFoundError", "duplicate permission reply was accepted")
        self.proven.append(
            "permission IDs can be caller-selected; pending requests are session-scoped, reply-once, then return 404"
        )

    def test_permission_process_epochs(self) -> None:
        session_id = self.create_session("permission_epoch")
        provider_calls = int((self.provider_stats() or {}).get("calls", 0))

        def create_and_prove(permission_id: str, resource: str) -> None:
            created = self.json_ok(
                "POST",
                f"/api/session/{session_id}/permission",
                {
                    "id": permission_id,
                    "action": "shell",
                    "resources": [resource],
                    "metadata": {"contract": "process-epoch"},
                    "agent": "contract",
                },
            )
            require(created["data"] == {"id": permission_id, "effect": "ask"}, f"permission was not pending: {created}")
            listed = self.json_ok("GET", f"/api/session/{session_id}/permission")["data"]
            require([item["id"] for item in listed] == [permission_id], f"permission list mismatch: {listed}")
            got = self.json_ok("GET", f"/api/session/{session_id}/permission/{permission_id}")["data"]
            require(
                got["id"] == permission_id
                and got["sessionID"] == session_id
                and got["resources"] == [resource]
                and got["metadata"] == {"contract": "process-epoch"},
                f"permission bytes changed: {got}",
            )

        def prove_lost(permission_id: str) -> None:
            require(self.json_ok("GET", f"/api/session/{session_id}")["data"]["id"] == session_id, "session was lost")
            require(
                self.json_ok("GET", f"/api/session/{session_id}/permission")["data"] == [],
                "pending permission survived its owning process",
            )
            status, missing, _ = self.request("GET", f"/api/session/{session_id}/permission/{permission_id}")
            require(
                status == 404 and missing.get("_tag") == "PermissionNotFoundError",
                f"lost permission get changed: {status}: {missing}",
            )
            status, missing, _ = self.request(
                "POST", f"/api/session/{session_id}/permission/{permission_id}/reply", {"reply": "once"}
            )
            require(
                status == 404 and missing.get("_tag") == "PermissionNotFoundError",
                f"lost permission reply changed: {status}: {missing}",
            )

        same_id = f"per_contract_same_epoch_{self.run_id}"
        create_and_prove(same_id, "echo contract same-container")
        self.restart_same_container()
        prove_lost(same_id)

        replacement_id = f"per_contract_replace_epoch_{self.run_id}"
        create_and_prove(replacement_id, "echo contract replacement")
        self.replace_opencode_container()
        prove_lost(replacement_id)
        require(int((self.provider_stats() or {}).get("calls", 0)) == provider_calls, "permission epochs called provider")
        self.proven.append(
            "pending synthetic permissions are process-epoch state: list/get/reply vanish after same-container restart and replacement while the session persists"
        )

    def test_undelivered_inbox_deletion(self) -> None:
        session_id = self.create_session("inbox_delete")
        message_id = f"msg_contract_inbox_delete_{self.run_id}"
        path = f"/api/session/{session_id}/inbox/{message_id}"
        provider_calls = int((self.provider_stats() or {}).get("calls", 0))
        original = {"id": message_id, "text": "undelivered deletion contract", "resume": False}

        admitted = self.json_ok("POST", f"/api/session/{session_id}/prompt", original)["data"]
        require(admitted["id"] == message_id and admitted["payload"] == {"text": original["text"]}, "admission changed")
        inbox = self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]
        require(
            len(inbox) == 1
            and inbox[0]["id"] == message_id
            and inbox[0]["payload"] == {"text": original["text"]},
            f"undelivered inbox mismatch: {inbox}",
        )
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "resume=false became active")
        require(int((self.provider_stats() or {}).get("calls", 0)) == provider_calls, "resume=false called provider")
        status, missing, _ = self.request("GET", f"/api/session/{session_id}/message/{message_id}")
        require(
            status == 404 and missing.get("_tag") == "MessageNotFoundError",
            f"undelivered exact message changed: {status}: {missing}",
        )

        self.restart_same_container()
        inbox = self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]
        require([item["id"] for item in inbox].count(message_id) == 1, "inbox admission did not survive process restart")
        status, body, _ = self.request("DELETE", path)
        require(status == 204 and body is None, f"inbox delete failed: {status}: {body}")

        def deleted() -> bool:
            return all(item["id"] != message_id for item in self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"])

        wait_for(deleted, "deleted inbox row remained visible")
        status, missing, _ = self.request("GET", f"/api/session/{session_id}/message/{message_id}")
        require(status == 404 and missing.get("_tag") == "MessageNotFoundError", "deleted input became a message")
        require(all(item["id"] != message_id for item in self.paginated_messages(session_id)), "deleted input entered history")
        self.replace_opencode_container()
        require(deleted(), "deleted inbox row returned after replacement")
        status, missing, _ = self.request("GET", f"/api/session/{session_id}/message/{message_id}")
        require(status == 404 and missing.get("_tag") == "MessageNotFoundError", "deleted exact message returned")
        require(all(item["id"] != message_id for item in self.paginated_messages(session_id)), "deleted history returned")
        status, conflict, _ = self.request("DELETE", path)
        require(
            status == 409
            and conflict.get("_tag") == "ConflictError"
            and conflict.get("resource") == message_id
            and conflict.get("message") == f"Pending input can no longer be cancelled: {message_id}",
            f"duplicate inbox delete changed: {status}: {conflict}",
        )

        readmitted = self.json_ok("POST", f"/api/session/{session_id}/prompt", original)["data"]
        require(readmitted["id"] == message_id and readmitted["payload"] == {"text": original["text"]}, "ID reuse failed")
        self.send_and_drop("DELETE", path)
        wait_for(deleted, "response-lost inbox delete did not take effect")
        status, missing, _ = self.request("GET", f"/api/session/{session_id}/message/{message_id}")
        require(status == 404 and missing.get("_tag") == "MessageNotFoundError", "lost delete projected exact message")
        require(all(item["id"] != message_id for item in self.paginated_messages(session_id)), "lost delete projected history")
        status, conflict, _ = self.request("DELETE", path)
        require(
            status == 409 and conflict.get("_tag") == "ConflictError" and conflict.get("resource") == message_id,
            f"post-reconciliation duplicate delete changed: {status}: {conflict}",
        )

        changed = {"id": message_id, "text": "changed bytes after deletion", "resume": False}
        readmitted = self.json_ok("POST", f"/api/session/{session_id}/prompt", changed)["data"]
        require(readmitted["id"] == message_id and readmitted["payload"] == {"text": changed["text"]}, "changed reuse failed")
        status, body, _ = self.request("DELETE", path)
        require(status == 204 and body is None, f"final inbox cleanup failed: {status}: {body}")
        require(deleted(), "final inbox row remained")
        status, missing, _ = self.request("GET", f"/api/session/{session_id}/message/{message_id}")
        require(status == 404 and missing.get("_tag") == "MessageNotFoundError", "final input projected exact message")
        require(all(item["id"] != message_id for item in self.paginated_messages(session_id)), "final input entered history")
        require(int((self.provider_stats() or {}).get("calls", 0)) == provider_calls, "inbox deletion called provider")
        self.proven.append(
            "undelivered resume=false inbox rows survive process restart, delete without projection, remain absent across replacement, and permit same-ID re-admission with new bytes"
        )

    def test_interrupt_before_admission(self) -> None:
        session_id = self.create_session("interrupt_before")
        provider_calls = int((self.provider_stats() or {}).get("calls", 0))
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "empty session was active")
        status, body, _ = self.request("POST", f"/api/session/{session_id}/interrupt")
        require(status == 204 and body is None, f"idle interrupt failed: {status}: {body}")
        message_id = f"msg_contract_interrupt_before_{self.run_id}"
        prompt = {"id": message_id, "text": "admit after idle interrupt", "resume": False}
        admitted = self.json_ok("POST", f"/api/session/{session_id}/prompt", prompt)["data"]
        inbox = self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]
        require(admitted["id"] == message_id and [item["id"] for item in inbox] == [message_id], "interrupt latched")
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "resume=false became active")
        require(int((self.provider_stats() or {}).get("calls", 0)) == provider_calls, "idle interrupt scenario called provider")
        status, body, _ = self.request("DELETE", f"/api/session/{session_id}/inbox/{message_id}")
        require(status == 204 and body is None, f"interrupt-before cleanup failed: {status}: {body}")
        require(self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [], "cleanup inbox remained")
        self.proven.append("interrupting an empty idle session is a 204 no-op and creates no durable cancel latch")

    def test_maximum_prompt_size(self) -> None:
        session_id = self.create_session("maximum_prompt")
        message_id = f"msg_contract_maximum_prompt_{self.run_id}"
        prompt = "p" * 65536
        admitted = self.json_ok(
            "POST",
            f"/api/session/{session_id}/prompt",
            {"id": message_id, "text": prompt, "resume": False},
        )["data"]
        require(admitted.get("id") == message_id, "maximum prompt identity changed")
        inbox = self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]
        matches = [item for item in inbox if item.get("id") == message_id]
        require(len(matches) == 1, "maximum prompt did not appear exactly once")
        require(matches[0].get("sessionID") == session_id, "maximum prompt session changed")
        require(matches[0].get("type") == "user", "maximum prompt type changed")
        require(matches[0].get("delivery") == "steer", "maximum prompt delivery changed")
        require(matches[0].get("payload", {}).get("text") == prompt, "maximum prompt bytes changed")
        require(int(matches[0].get("timeCreated", 0)) > 0, "maximum prompt time is invalid")
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "maximum prompt executed")
        status, body, _ = self.request("DELETE", f"/api/session/{session_id}/inbox/{message_id}")
        require(status == 204 and body is None, f"maximum prompt cleanup failed: {status}: {body}")
        self.proven.append("an exact 65,536-byte resume=false prompt is admitted once without provider execution")

    def test_resume_not_idempotency_bound(self) -> None:
        session_id = self.create_session("resume_binding")
        message_id = f"msg_contract_resume_binding_{self.run_id}"
        marker = f"CONTRACT_RESUME_BINDING_{self.run_id}"
        first = self.json_ok(
            "POST",
            f"/api/session/{session_id}/prompt",
            {"id": message_id, "text": marker, "resume": False},
        )
        require(
            [item.get("id") for item in self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"]].count(message_id) == 1,
            "resume binding setup did not remain pending",
        )
        second = self.json_ok(
            "POST",
            f"/api/session/{session_id}/prompt",
            {"id": message_id, "text": marker, "resume": True},
        )
        require(second == first, "resume-only replay changed the admission projection")
        wait_for(lambda: self.provider_turns(marker) == 1, "resume-only replay did not start exactly one provider turn", 20)
        wait_for(
            lambda: session_id not in self.json_ok("GET", "/api/session/active")["data"],
            "resume-only replay remained active",
            20,
        )
        require(self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [], "resume-only replay stayed pending")
        exact = self.json_ok("GET", f"/api/session/{session_id}/message/{message_id}")["data"]
        require(exact.get("type") == "user" and exact.get("text") == marker, "resume-only replay changed the message")
        self.proven.append(
            "resume is not persisted or idempotency-bound: replaying one pending ID/text from false to true returns the same admission and starts one provider turn"
        )

    def test_interrupt_after_completion(self) -> None:
        session_id = self.create_session("interrupt_complete")
        message_id = f"msg_contract_interrupt_complete_{self.run_id}"
        marker = f"CONTRACT_INTERRUPT_COMPLETE_{self.run_id}"
        self.json_ok("POST", f"/api/session/{session_id}/prompt", {"id": message_id, "text": marker})
        wait_for(lambda: self.provider_turns(marker) == 1, "completed interrupt turn did not reach provider once", 20)

        def completed() -> list[dict[str, Any]] | None:
            context = self.json_ok("GET", f"/api/session/{session_id}/context")["data"]
            serialized = json.dumps(context)
            return context if marker in serialized and "contract fake response" in serialized else None

        context_before = wait_for(completed, "provider turn did not durably complete", 20)
        wait_for(
            lambda: session_id not in self.json_ok("GET", "/api/session/active")["data"],
            "completed session remained active",
            20,
        )
        require(self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [], "completed input stayed in inbox")
        require(len(context_before) <= 20, f"unexpectedly large bounded test context: {len(context_before)}")
        messages_before = self.paginated_messages(session_id)
        require([item["id"] for item in messages_before].count(message_id) == 1, "caller message projection changed")
        exact_message = self.json_ok("GET", f"/api/session/{session_id}/message/{message_id}")["data"]
        require(exact_message.get("id") == message_id, "exact projected caller identity changed")
        require(exact_message.get("type") == "user", "exact projected caller type changed")
        require(exact_message.get("text") == marker, "exact projected caller text changed")
        require(int(exact_message.get("time", {}).get("created", 0)) > 0, "exact projected caller time is invalid")
        require("resume" not in exact_message, "promoted caller unexpectedly retained inbox-only resume")
        stats_before = self.provider_stats() or {}
        requests_before = len(stats_before.get("requests", []))
        disconnects_before = int(stats_before.get("disconnects", 0))
        status, body, _ = self.request("POST", f"/api/session/{session_id}/interrupt")
        require(status == 204 and body is None, f"completed interrupt failed: {status}: {body}")
        time.sleep(0.5)
        require(self.json_ok("GET", f"/api/session/{session_id}/context")["data"] == context_before, "idle interrupt changed context")
        require(self.paginated_messages(session_id) == messages_before, "idle interrupt changed finite messages")
        require("Step interrupted" not in json.dumps(context_before), "completed turn gained interruption evidence")
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "idle interrupt activated session")
        require(self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [], "idle interrupt created inbox work")
        stats_after = self.provider_stats() or {}
        require(len(stats_after.get("requests", [])) == requests_before, "idle interrupt made a provider request")
        require(int(stats_after.get("disconnects", 0)) == disconnects_before, "idle interrupt disconnected provider")
        require(self.provider_turns(marker) == 1, "idle interrupt duplicated provider turn")
        self.replace_opencode_container()
        require(self.json_ok("GET", f"/api/session/{session_id}/context")["data"] == context_before, "completed context changed")
        require(self.paginated_messages(session_id) == messages_before, "completed messages changed after replacement")
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "completed turn restarted")
        require(self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [], "completed inbox returned")
        require(self.provider_turns(marker) == 1, "replacement duplicated completed turn")
        self.proven.append(
            "the exact promoted caller object preserves ID, user type, text, and positive creation time; interrupt after durable completion is a 204 no-op across replacement"
        )

    def test_direct_form_process_epochs(self) -> None:
        session_id = self.create_session("direct_form_epoch")
        form_id = f"frm_contract_direct_epoch_{self.run_id}"
        provider_calls = int((self.provider_stats() or {}).get("calls", 0))
        first = {
            "id": form_id,
            "title": "Direct contract form epoch one",
            "metadata": {"kind": "contract-direct", "epoch": 1},
            "fields": [
                {
                    "key": "choice",
                    "type": "string",
                    "title": "Epoch one choice",
                    "required": True,
                    "options": [
                        {"value": "alpha", "label": "Alpha", "description": "First epoch alpha"},
                        {"value": "beta", "label": "Beta"},
                    ],
                }
            ],
        }
        created = self.json_ok("POST", f"/api/session/{session_id}/form", first)["data"]
        require(
            created == {**first, "sessionID": session_id},
            f"direct form create changed caller bytes: {created}",
        )
        require(self.json_ok("GET", f"/api/session/{session_id}/form")["data"] == [created], "form list mismatch")
        require(self.json_ok("GET", f"/api/session/{session_id}/form/{form_id}")["data"] == created, "form get mismatch")
        require(
            self.json_ok("GET", f"/api/session/{session_id}/form/{form_id}/state")["data"] == {"status": "pending"},
            "direct form was not pending",
        )

        def prove_lost() -> None:
            require(self.json_ok("GET", f"/api/session/{session_id}")["data"]["id"] == session_id, "form session was lost")
            require(self.json_ok("GET", f"/api/session/{session_id}/form")["data"] == [], "form survived process epoch")
            for suffix in ("", "/state"):
                status, missing, _ = self.request("GET", f"/api/session/{session_id}/form/{form_id}{suffix}")
                require(
                    status == 404 and missing.get("_tag") == "FormNotFoundError" and missing.get("id") == form_id,
                    f"lost form get changed: {status}: {missing}",
                )
            status, missing, _ = self.request(
                "POST", f"/api/session/{session_id}/form/{form_id}/reply", {"answer": {"choice": "alpha"}}
            )
            require(
                status == 404 and missing.get("_tag") == "FormNotFoundError" and missing.get("id") == form_id,
                f"lost form reply changed: {status}: {missing}",
            )

        self.restart_same_container()
        prove_lost()

        second = {
            "id": form_id,
            "title": "Direct contract form epoch two",
            "metadata": {"kind": "contract-direct", "epoch": 2, "changed": True},
            "fields": [
                {
                    "key": "choice",
                    "type": "string",
                    "title": "Epoch two choice",
                    "required": True,
                    "options": [
                        {"value": "green", "label": "Green"},
                        {"value": "blue", "label": "Blue", "description": "Changed epoch blue"},
                    ],
                }
            ],
        }
        recreated = self.json_ok("POST", f"/api/session/{session_id}/form", second)["data"]
        require(recreated == {**second, "sessionID": session_id}, f"same-ID form recreation changed: {recreated}")
        require(recreated != created, "same form ID retained old process semantics")
        status, body, _ = self.request(
            "POST", f"/api/session/{session_id}/form/{form_id}/reply", {"answer": {"choice": "blue"}}
        )
        require(status == 204 and body is None, f"direct form reply failed: {status}: {body}")
        state = self.json_ok("GET", f"/api/session/{session_id}/form/{form_id}/state")["data"]
        require(state == {"status": "answered", "answer": {"choice": "blue"}}, f"answered state mismatch: {state}")
        self.replace_opencode_container()
        prove_lost()
        require(int((self.provider_stats() or {}).get("calls", 0)) == provider_calls, "direct forms called provider")
        self.proven.append(
            "direct forms are process-epoch state: pending and answered forms vanish while sessions persist, and a lost caller ID can be recreated with changed semantics"
        )

    def test_question_surface(self) -> None:
        session_id = self.create_session("question")
        message_id = f"msg_contract_question_{self.run_id}"
        self.json_ok(
            "POST",
            f"/api/session/{session_id}/prompt",
            {"id": message_id, "text": "CONTRACT_QUESTION"},
        )

        def pending() -> list[dict[str, Any]] | None:
            data = self.json_ok("GET", f"/api/session/{session_id}/form")["data"]
            return data or None

        forms = wait_for(pending, "model tool call did not surface a question form", 20)
        require(len(forms) == 1 and forms[0]["sessionID"] == session_id, f"question owner mismatch: {forms}")
        form_id = forms[0]["id"]
        require(forms[0]["metadata"]["kind"] == "question", f"question form metadata mismatch: {forms[0]}")
        require(forms[0]["fields"][0]["options"][0]["label"] == "Choice A", "question content mismatch")
        require(
            self.json_ok("GET", f"/api/session/{session_id}/question")["data"] == [],
            "legacy question surface unexpectedly owned the model question",
        )
        self.replace_opencode_container()
        recovered = self.json_ok("GET", f"/api/session/{session_id}/form")["data"]
        require(recovered == [], f"lost pending form unexpectedly recovered: {recovered}")
        status, missing, _ = self.request("GET", f"/api/session/{session_id}/form/{form_id}/state")
        require(status == 404 and missing.get("_tag") == "FormNotFoundError", f"lost form state changed: {status}: {missing}")
        status, missing, _ = self.request(
            "POST",
            f"/api/session/{session_id}/form/{form_id}/reply",
            {"answer": {"q0": "Choice A"}},
        )
        require(status == 404 and missing.get("_tag") == "FormNotFoundError", f"lost form reply changed: {status}: {missing}")
        context = self.json_ok("GET", f"/api/session/{session_id}/context")["data"]
        require("running" in json.dumps(context), "lost form did not leave explicit recovery-required tool state")
        require(session_id not in self.json_ok("GET", "/api/session/active")["data"], "lost form resurrected execution")

        answered_session = self.create_session("question_answered")
        self.json_ok(
            "POST",
            f"/api/session/{answered_session}/prompt",
            {"id": f"msg_contract_answered_{self.run_id}", "text": "CONTRACT_QUESTION"},
        )
        answered_forms = wait_for(
            lambda: self.json_ok("GET", f"/api/session/{answered_session}/form")["data"] or None,
            "second question form did not surface",
            20,
        )
        answered_id = answered_forms[0]["id"]
        status, body, _ = self.request(
            "POST",
            f"/api/session/{answered_session}/form/{answered_id}/reply",
            {"answer": {"q0": "Choice A"}},
        )
        require(status == 204, f"live question reply failed: {status}: {body}")
        state = self.json_ok("GET", f"/api/session/{answered_session}/form/{answered_id}/state")["data"]
        require(state == {"status": "answered", "answer": {"q0": "Choice A"}}, f"live answered state mismatch: {state}")
        wait_for(
            lambda: answered_session not in self.json_ok("GET", "/api/session/active")["data"],
            "answered question continuation did not finish",
            20,
        )
        self.replace_opencode_container()
        status, missing, _ = self.request("GET", f"/api/session/{answered_session}/form/{answered_id}/state")
        require(status == 404 and missing.get("_tag") == "FormNotFoundError", f"answered restart state changed: {status}: {missing}")
        status, missing, _ = self.request(
            "POST",
            f"/api/session/{answered_session}/form/{answered_id}/reply",
            {"answer": {"q0": "Choice A"}},
        )
        require(status == 404 and missing.get("_tag") == "FormNotFoundError", f"answered duplicate changed: {status}: {missing}")
        self.proven.append(
            "question tool calls use session forms and accept one answer while the owning OpenCode process remains alive"
        )
        self.blocked.extend(
            [
                "pending form replacement: form/state/reply disappear with 404 while durable tool state remains running and inactive",
                "answered form replacement: answered state disappears with 404, so duplicate reply cannot return the live-process 409 conflict",
                "legacy /question endpoints do not surface model questions; the pinned image uses /form",
            ]
        )

    def test_cancellation(self) -> None:
        session_id = self.create_session("cancel")
        marker = f"CONTRACT_CANCEL_{self.run_id}"
        message_id = f"msg_contract_cancel_{self.run_id}"
        stats_before = self.provider_stats() or {}
        disconnects = int(stats_before.get("disconnects", 0))
        self.json_ok(
            "POST",
            f"/api/session/{session_id}/prompt",
            {"id": message_id, "text": marker},
        )
        wait_for(
            lambda: self.provider_turns(marker) == 1 and (self.provider_stats() or {}).get("hanging", 0) == 1,
            "exactly one marker-bearing provider stream did not begin hanging",
            20,
        )
        wait_for(
            lambda: session_id in self.json_ok("GET", "/api/session/active")["data"],
            "session never became active",
        )
        wait_for(
            lambda: self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [],
            "active provider input remained in inbox",
        )
        status, body, _ = self.request("POST", f"/api/session/{session_id}/interrupt")
        require(status == 204 and body is None, f"interrupt failed: {status}: {body}")
        wait_for(
            lambda: session_id not in self.json_ok("GET", "/api/session/active")["data"],
            "interrupted session remained active",
            20,
        )
        wait_for(
            lambda: int((self.provider_stats() or {}).get("disconnects", 0)) > disconnects
            and int((self.provider_stats() or {}).get("hanging", 0)) == 0,
            "interrupt did not close the fake provider stream",
            20,
        )

        def interrupted_history() -> bool:
            context = self.json_ok("GET", f"/api/session/{session_id}/context")["data"]
            return "Step interrupted" in json.dumps(context)

        wait_for(interrupted_history, "interrupt was not durably visible in session history", 20)
        context_before = self.json_ok("GET", f"/api/session/{session_id}/context")["data"]
        messages_before = self.paginated_messages(session_id)
        require([item["id"] for item in messages_before].count(message_id) == 1, "caller message was not projected once")
        require(self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [], "interrupted inbox was not empty")
        require(self.provider_turns(marker) == 1, "interrupt duplicated marker-bearing provider turn")
        settled_stats = self.provider_stats() or {}
        settled_disconnects = int(settled_stats.get("disconnects", 0))
        settled_requests = len(settled_stats.get("requests", []))
        status, body, _ = self.request("POST", f"/api/session/{session_id}/interrupt")
        require(status == 204 and body is None, f"second idle interrupt failed: {status}: {body}")
        time.sleep(0.5)
        require(self.json_ok("GET", f"/api/session/{session_id}/context")["data"] == context_before, "idle interrupt changed evidence")
        require(self.paginated_messages(session_id) == messages_before, "idle interrupt changed messages")
        idle_stats = self.provider_stats() or {}
        require(int(idle_stats.get("disconnects", 0)) == settled_disconnects, "idle interrupt changed disconnect count")
        require(len(idle_stats.get("requests", [])) == settled_requests, "idle interrupt called provider")
        require(self.provider_turns(marker) == 1, "idle interrupt duplicated provider turn")
        self.replace_opencode_container()
        after = self.json_ok("GET", f"/api/session/{session_id}/context")["data"]
        require(after == context_before and "Step interrupted" in json.dumps(after), "interrupt evidence changed across replacement")
        require(self.paginated_messages(session_id) == messages_before, "interrupted messages changed across replacement")
        require(
            session_id not in self.json_ok("GET", "/api/session/active")["data"],
            "interrupted work was resurrected after replacement",
        )
        require(self.json_ok("GET", f"/api/session/{session_id}/inbox")["data"] == [], "replacement restored inbox")
        require(self.provider_turns(marker) == 1, "replacement duplicated interrupted turn")
        require(int((self.provider_stats() or {}).get("disconnects", 0)) == settled_disconnects, "replacement changed disconnects")
        self.proven.append(
            "interrupt closes exactly one marker-bearing provider turn, empties inbox, projects one caller message, and records Step interrupted; a second idle interrupt and replacement are no-ops"
        )

    def run(self) -> None:
        self.start()
        tests = [
            self.test_caller_ids_and_retry,
            self.test_restart_retry_and_history,
            self.test_history_and_event_cursors,
            self.test_permission_surface,
            self.test_permission_process_epochs,
            self.test_undelivered_inbox_deletion,
            self.test_interrupt_before_admission,
            self.test_maximum_prompt_size,
            self.test_resume_not_idempotency_bound,
            self.test_interrupt_after_completion,
            self.test_direct_form_process_epochs,
            self.test_question_surface,
            self.test_cancellation,
        ]
        for test in tests:
            print(f"RUN  {test.__name__}", flush=True)
            test()
            print(f"PASS {test.__name__}", flush=True)
        print("\nPROVEN CONTRACTS")
        for contract in self.proven:
            print(f"- {contract}")
        print("\nBLOCKED CONTRACTS")
        for contract in self.blocked:
            print(f"- {contract}")
        print(f"\nIMAGE {IMAGE} {self.image_id}")
        print(f"SCENARIOS {len(tests)}/{len(tests)} passed")


def main() -> None:
    for command in ("docker",):
        if shutil.which(command) is None:
            raise SystemExit(f"required command not found: {command}")
    harness = Harness()
    try:
        harness.run()
    finally:
        harness.stop()


if __name__ == "__main__":
    main()
