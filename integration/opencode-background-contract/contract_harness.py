#!/usr/bin/env python3
"""Black-box real-Docker contract for official opencode-ai 1.18.16."""

from __future__ import annotations

import base64
import http.client
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


VERSION = "1.18.16"
IMAGE = os.environ.get("FERN_OPENCODE_BACKGROUND_IMAGE", "fern/opencode-background:dev")
BUILD = os.environ.get("FERN_OPENCODE_BACKGROUND_BUILD", "1") != "0"
EXPECTED_IMAGE_ID = os.environ.get("FERN_OPENCODE_BACKGROUND_IMAGE_ID", "")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_for(check: Callable[[], Any], message: str, timeout: float = 25) -> Any:
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
        self.container = f"fern-opencode-background-contract-{self.run_id}"
        self.provider = f"fern-opencode-background-provider-{self.run_id}"
        self.network = f"fern-opencode-background-contract-{self.run_id}"
        self.server_port = free_port()
        self.provider_port = free_port()
        self.password = secrets.token_urlsafe(24)
        self.auth = "Basic " + base64.b64encode(f"opencode:{self.password}".encode()).decode()
        self.base = f"http://127.0.0.1:{self.server_port}"
        self.temp = pathlib.Path(tempfile.mkdtemp(prefix="fern-opencode-background-contract-"))
        self.repo = self.temp / "repository"
        self.data = self.temp / "data"
        self.image_id = ""
        self.proven: list[str] = []
        self.blocked: list[str] = []

    def start(self) -> None:
        if BUILD:
            subprocess.run(
                ["docker", "build", "--tag", IMAGE, "images/opencode-background"],
                check=True,
            )
        self.image_id = subprocess.check_output(
            ["docker", "image", "inspect", IMAGE, "--format", "{{.Id}}"], text=True
        ).strip()
        require(self.image_id.startswith("sha256:"), f"invalid local image ID: {self.image_id}")
        if EXPECTED_IMAGE_ID:
            require(
                self.image_id == EXPECTED_IMAGE_ID,
                f"expected image ID {EXPECTED_IMAGE_ID}, got {self.image_id}",
            )

        version = subprocess.check_output(
            ["docker", "run", "--rm", "--entrypoint", "opencode", IMAGE, "--version"], text=True
        ).strip()
        require(version == VERSION, f"expected binary {VERSION}, got {version!r}")
        identity = subprocess.check_output(
            ["docker", "run", "--rm", "--entrypoint", "sh", IMAGE, "-c", "printf '%s:%s' \"$(id -u)\" \"$(id -g)\""],
            text=True,
        ).strip()
        require(identity == "1001:1001", f"expected runtime UID:GID 1001:1001, got {identity}")
        image_env = json.loads(
            subprocess.check_output(
                ["docker", "image", "inspect", IMAGE, "--format", "{{json .Config.Env}}"], text=True
            )
        )
        require(
            not any(item.startswith("OPENCODE_SERVER_PASSWORD=") for item in image_env),
            "image contains a baked OpenCode server password",
        )

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
            "provider": {
                "test": {
                    "name": "Background Contract",
                    "id": "test",
                    "env": [],
                    "npm": "@ai-sdk/openai-compatible",
                    "options": {"apiKey": "test-key", "baseURL": "http://provider:4100/v1"},
                    "models": {
                        "test-model": {
                            "id": "test-model",
                            "name": "Background Contract Model",
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
        (self.repo / "README.md").write_text("# Contract repository\n", encoding="utf-8")
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
                "--publish", f"127.0.0.1:{self.provider_port}:4100",
                "--entrypoint", "node",
                "--volume", f"{pathlib.Path(__file__).with_name('fake_provider.mjs')}:/contract/fake_provider.mjs:ro",
                IMAGE, "/contract/fake_provider.mjs", "4100",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
        )
        wait_for(self.provider_stats, "fake provider did not start")
        self.start_opencode()
        self.proven.append(
            f"official opencode binary reports exactly {VERSION}, runs as UID:GID 1001:1001, and has no baked server password; local image ID {self.image_id}"
        )

    def start_opencode(self) -> None:
        subprocess.run(
            [
                "docker", "run", "--detach", "--name", self.container,
                "--network", self.network,
                "--publish", f"127.0.0.1:{self.server_port}:4096",
                "--env", f"OPENCODE_SERVER_PASSWORD={self.password}",
                "--volume", f"{self.repo}:/home/user/workspace",
                "--volume", f"{self.data}:/home/user/.local/share/opencode",
                IMAGE,
            ],
            check=True,
            stdout=subprocess.DEVNULL,
        )
        wait_for(
            lambda: self.try_json("GET", "/global/health") == {"healthy": True, "version": VERSION},
            "OpenCode did not become healthy",
            45,
        )
        wait_for(
            lambda: "test-model" in json.dumps(self.try_json("GET", "/config") or {}),
            "workspace fake-provider config did not load",
        )

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

    def create_session(self, title: str) -> str:
        result = self.json_ok("POST", "/session", {"title": title})
        require(result["title"] == title and result["version"] == VERSION, f"session projection changed: {result}")
        return result["id"]

    def messages(self, session_id: str) -> list[dict[str, Any]]:
        return self.json_ok("GET", f"/session/{session_id}/message")

    def message(self, session_id: str, message_id: str) -> dict[str, Any] | None:
        status, body, _ = self.request("GET", f"/session/{session_id}/message/{message_id}")
        return body if status == 200 else None

    def prompt(
        self,
        session_id: str,
        message_id: str,
        text: str,
        no_reply: bool = False,
        asynchronous: bool = True,
    ) -> tuple[int, Any, dict[str, str]]:
        return self.request(
            "POST",
            f"/session/{session_id}/{'prompt_async' if asynchronous else 'message'}",
            {
                "messageID": message_id,
                "agent": "build",
                "model": {"providerID": "test", "modelID": "test-model"},
                "noReply": no_reply,
                "parts": [{"type": "text", "text": text}],
            },
        )

    def first_sse(self, last_event_id: str | None = None) -> dict[str, Any]:
        connection = http.client.HTTPConnection("127.0.0.1", self.server_port, timeout=4)
        headers = {"Authorization": self.auth}
        if last_event_id:
            headers["Last-Event-ID"] = last_event_id
        connection.request("GET", "/event", headers=headers)
        response = connection.getresponse()
        require(response.status == 200, f"GET /event returned {response.status}")
        event: dict[str, Any] = {}
        try:
            while True:
                line = response.readline().decode().strip()
                if line.startswith("data:"):
                    event = json.loads(line[5:].strip())
                    break
        finally:
            connection.close()
        return event

    def test_surface(self) -> None:
        for auth in (None, "wrong"):
            status, body, headers = self.request("GET", "/global/health", auth=auth)
            require(status == 401 and body == "", f"{auth} auth changed: {status}: {body!r}")
            require(headers.get("www-authenticate") == 'Basic realm="Secure Area"', "Basic challenge changed")
        status, health, _ = self.request("GET", "/global/health")
        require(status == 200 and health == {"healthy": True, "version": VERSION}, f"health changed: {health}")
        status, html, headers = self.request("GET", "/", headers={"Accept": "text/html"})
        require(status == 200 and "<!doctype html" in html.lower(), "embedded official UI was not served at /")
        require("text/html" in headers.get("content-type", ""), "official UI content type changed")
        self.proven.extend([
            "missing and wrong Basic auth receive an exact bodyless 401 and Basic challenge; correct auth succeeds",
            f"GET /global/health is exactly healthy=true and version={VERSION}",
            "the image serves the embedded official OpenCode HTML UI locally at /",
        ])

    def test_identity_retry_history(self) -> None:
        selected_session = f"ses_background_contract_{self.run_id}"
        status, generated, _ = self.request("POST", "/session", {"id": selected_session, "title": "Selected"})
        require(
            status == 200 and generated.get("id") != selected_session,
            f"caller-selected session ID behavior changed: {status}: {generated}",
        )
        self.blocked.append("caller-selected session ID: POST /session ignores id and generates a different Session ID")

        session_id = self.create_session("Background identity contract")
        message_id = f"msg_background_contract_{self.run_id}"
        text = f"CONTRACT_ADMISSION_{self.run_id}"
        status, body, _ = self.prompt(session_id, message_id, text, no_reply=True, asynchronous=False)
        require(status == 200 and body["info"]["id"] == message_id, f"prompt admission changed: {status}: {body!r}")
        exact = wait_for(
            lambda: (message if (message := self.message(session_id, message_id)) and message.get("parts") else None),
            "caller-selected message and text part were not durable",
        )
        require(exact["info"]["id"] == message_id, "server replaced caller-selected message ID")
        require(exact["info"]["role"] == "user", "admitted message role changed")
        require(len(exact["parts"]) == 1 and exact["parts"][0].get("text") == text, "admitted prompt text changed")
        before = self.messages(session_id)
        require([item["info"]["id"] for item in before].count(message_id) == 1, "history duplicated prompt")

        same_status, same_body, _ = self.prompt(session_id, message_id, text, no_reply=True, asynchronous=False)
        conflict_status, conflict_body, _ = self.prompt(
            session_id, message_id, text + " changed", no_reply=True, asynchronous=False
        )
        require(same_status == 200, f"exact duplicate status changed: {same_status}: {same_body}")
        require(conflict_status == 200, f"conflicting duplicate status changed: {conflict_status}: {conflict_body}")
        require(same_body["info"]["id"] == message_id, "exact retry replaced the message ID")
        require(conflict_body["info"]["id"] == message_id, "conflicting retry replaced the message ID")
        after_retries = self.messages(session_id)
        require(
            [item["info"]["id"] for item in after_retries].count(message_id) == 1,
            "duplicate attempts created another top-level message",
        )
        retried = next(item for item in after_retries if item["info"]["id"] == message_id)
        require(
            [part.get("text") for part in retried["parts"]] == [text, text, text + " changed"],
            f"retry part semantics changed: {retried}",
        )

        replay = self.first_sse("evt_not_present")
        require(replay.get("type") == "server.connected", f"SSE Last-Event-ID behavior changed: {replay}")
        session_before = self.json_ok("GET", f"/session/{session_id}")
        self.replace_opencode()
        require(self.json_ok("GET", f"/session/{session_id}") == session_before, "session changed across replacement")
        require(self.messages(session_id) == after_retries, "history changed across replacement")
        require(self.message(session_id, message_id) == retried, "exact message changed across replacement")
        self.proven.extend([
            "caller-selected prompt messageID is preserved with exact user text in durable history",
            "exact and conflicting duplicate messageID submissions both return HTTP 200 and append text parts to the same message; no conflict is detected",
            "finite session history and exact-message reads are sufficient to reconcile admitted prompt identity",
            "the same generated session, exact message, and complete history survive container replacement with the same data mount",
        ])
        self.blocked.append("durable SSE replay: Last-Event-ID is ignored and a new stream starts with server.connected")

    def test_active_restart_and_interrupt(self) -> None:
        session_id = self.create_session("Background active contract")
        message_id = f"msg_background_hang_{self.run_id}"
        marker = f"CONTRACT_HANG_RESTART_{self.run_id}"
        status, body, _ = self.prompt(session_id, message_id, marker)
        require(status == 204 and body == "", f"hanging prompt admission changed: {status}: {body!r}")
        wait_for(lambda: self.provider_turns(marker) == 1, "hanging prompt did not reach fake provider")
        wait_for(lambda: session_id in (self.try_json("GET", "/session/status") or {}), "busy status did not appear")
        wait_for(lambda: len(self.messages(session_id)) >= 2, "in-progress assistant record did not become durable")
        durable = self.messages(session_id)
        assistant = next(item for item in durable if item["info"].get("role") == "assistant")
        require("completed" not in assistant["info"].get("time", {}), "hanging assistant was marked completed")
        self.replace_opencode()
        require(self.json_ok("GET", "/session/status") == {}, "active projection survived process replacement")
        require(self.messages(session_id) == durable, "partial non-complete history changed across replacement")

        interrupt_session = self.create_session("Background interrupt contract")
        interrupt_id = f"msg_background_interrupt_{self.run_id}"
        interrupt_marker = f"CONTRACT_HANG_INTERRUPT_{self.run_id}"
        status, _, _ = self.prompt(interrupt_session, interrupt_id, interrupt_marker)
        require(status == 204, "interrupt prompt was not admitted")
        wait_for(lambda: self.provider_turns(interrupt_marker) == 1, "interrupt prompt did not reach provider")
        wait_for(lambda: interrupt_session in self.json_ok("GET", "/session/status"), "interrupt session was not busy")
        disconnects = int((self.provider_stats() or {}).get("disconnects", 0))
        aborted = self.json_ok("POST", f"/session/{interrupt_session}/abort")
        require(aborted is True, f"active abort did not report true: {aborted}")
        wait_for(lambda: interrupt_session not in self.json_ok("GET", "/session/status"), "abort did not clear busy status")
        wait_for(
            lambda: int((self.provider_stats() or {}).get("disconnects", 0)) > disconnects,
            "abort did not disconnect the provider stream",
        )
        idle_abort = self.json_ok("POST", f"/session/{interrupt_session}/abort")
        require(idle_abort is True, f"idle abort response changed: {idle_abort}")
        require(self.message(interrupt_session, interrupt_id) is not None, "interrupt lost admitted prompt history")
        self.proven.extend([
            "session/status projects a live busy run only in the owning process and clears after process replacement",
            "durable partial history survives while the active projection disappears, so absence is not completion authority",
            "active abort returns true, disconnects the provider, clears active status, and preserves prompt history; idle abort also returns true",
        ])

    def test_question_permission_volatility(self) -> None:
        session_id = self.create_session("Background question contract")
        message_id = f"msg_background_question_{self.run_id}"
        status, _, _ = self.prompt(session_id, message_id, "CONTRACT_QUESTION")
        require(status == 204, "question prompt was not admitted")
        pending = wait_for(
            lambda: [item for item in self.json_ok("GET", "/question") if item.get("sessionID") == session_id] or None,
            "question tool did not create a pending question",
        )
        require(pending[0]["questions"][0]["options"][0]["label"] == "Choice A", "question bytes changed")
        request_id = pending[0]["id"]
        answered = self.json_ok("POST", f"/question/{request_id}/reply", {"answers": [["Choice A"]]})
        require(answered is True, f"question reply changed: {answered}")
        wait_for(
            lambda: not [item for item in self.json_ok("GET", "/question") if item.get("sessionID") == session_id],
            "answered question remained pending",
        )

        volatile_session = self.create_session("Background volatile question")
        volatile_id = f"msg_background_question_volatile_{self.run_id}"
        status, _, _ = self.prompt(volatile_session, volatile_id, "CONTRACT_QUESTION")
        require(status == 204, "volatile question prompt was not admitted")
        wait_for(
            lambda: [item for item in self.json_ok("GET", "/question") if item.get("sessionID") == volatile_session] or None,
            "volatile pending question did not appear",
        )
        self.replace_opencode()
        require(self.json_ok("GET", "/question") == [], "pending question survived process replacement")
        require(self.json_ok("GET", "/permission") == [], "unexpected permission appeared without a shell tool")
        self.proven.append(
            "model questions can be listed and answered, but an unanswered question is volatile across process replacement"
        )
        self.blocked.append(
            "permission volatility: public 1.18.16 exposes only list/reply, and the harness deliberately does not ask the model to execute a shell tool"
        )

    def run(self) -> None:
        self.test_surface()
        self.test_identity_retry_history()
        self.test_active_restart_and_interrupt()
        self.test_question_permission_volatility()


def main() -> int:
    harness = Harness()
    try:
        harness.start()
        harness.run()
        print(f"\nIMAGE TAG: {IMAGE}")
        print(f"CAPTURED LOCAL IMAGE ID: {harness.image_id}")
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
        if harness.container:
            subprocess.run(["docker", "logs", "--tail", "100", harness.container], check=False)
        return 1
    finally:
        harness.stop()


if __name__ == "__main__":
    raise SystemExit(main())
