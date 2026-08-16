#!/usr/bin/env python3
"""Deterministic OpenCode HTTP/SSE seam used by the real-Docker harness."""

import base64
import json
import os
import queue
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse


PROTOCOL = os.environ.get("FERN_OPENCODE_PROTOCOL", "v1")
PASSWORD = os.environ.get("OPENCODE_PASSWORD" if PROTOCOL == "v2" else "OPENCODE_SERVER_PASSWORD", "")
USERNAME = "opencode" if PROTOCOL == "v2" else (os.environ.get("OPENCODE_SERVER_USERNAME", "opencode") or "opencode")
STATE_PATH = "/home/user/.local/share/opencode/lifecycle-state.json"
BOOT_ID = f"{socket.gethostname()}-{time.time_ns()}"
subscribers = set()
subscribers_lock = threading.Lock()
statuses = {}
event_connections = 0
last_event_connected_ns = 0


def authorized(header):
    if not PASSWORD:
        return True
    expected = base64.b64encode(f"{USERNAME}:{PASSWORD}".encode()).decode()
    return header == f"Basic {expected}"


def publish(session_id, status):
    statuses[session_id] = {"type": status}
    event = {
        "type": "session.status",
        "data" if PROTOCOL == "v2" else "properties": {"sessionID": session_id, "status": {"type": status}},
    }
    frame = f"data: {json.dumps(event, separators=(',', ':'))}\n\n".encode()
    with subscribers_lock:
        for target in list(subscribers):
            target.put(frame)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        print(f"request ts_ns={time.time_ns()} client={self.client_address[0]} {fmt % args}", flush=True)

    def authenticate(self):
        if authorized(self.headers.get("Authorization", "")):
            return True
        body = b"unauthorized\n"
        self.send_response(401)
        self.send_header("WWW-Authenticate", 'Basic realm="OpenCode"')
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        return False

    def json_response(self, value, status=200):
        body = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        global event_connections, last_event_connected_ns
        if not self.authenticate():
            return
        parsed = urlparse(self.path)
        if parsed.path == ("/api/health" if PROTOCOL == "v2" else "/global/health"):
            self.json_response({"healthy": True, "version": "0.0.0-next-17444" if PROTOCOL == "v2" else "1.18.16", "boot_id": BOOT_ID})
        elif PROTOCOL == "v1" and parsed.path == "/session/status":
            self.json_response(statuses)
        elif PROTOCOL == "v2" and parsed.path == "/api/session/active":
            self.json_response({"data": {key: {"type": "running"} for key, value in statuses.items() if value["type"] != "idle"}})
        elif PROTOCOL == "v2" and parsed.path in ("/api/shell", "/api/pty", "/api/permission/request", "/api/form/request"):
            self.json_response({"data": []})
        elif parsed.path == "/control/identity":
            self.json_response({
                "boot_id": BOOT_ID,
                "event_connections": event_connections,
                "last_event_connected_ns": last_event_connected_ns,
                "now_ns": time.time_ns(),
            })
        elif parsed.path == "/control/persist":
            try:
                with open(STATE_PATH, "r", encoding="utf-8") as source:
                    value = json.load(source)
            except FileNotFoundError:
                value = {}
            self.json_response(value)
        elif parsed.path == "/control/hold":
            seconds = min(float(parse_qs(parsed.query).get("seconds", ["1"])[0]), 30.0)
            time.sleep(max(seconds, 0.0))
            self.json_response({"held_seconds": seconds})
        elif parsed.path == ("/api/event" if PROTOCOL == "v2" else "/event"):
            target = queue.Queue()
            with subscribers_lock:
                subscribers.add(target)
                event_connections += 1
                last_event_connected_ns = time.time_ns()
            print(f"event_connected ts_ns={last_event_connected_ns} boot_id={BOOT_ID}", flush=True)
            try:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Connection", "keep-alive")
                self.end_headers()
                if PROTOCOL == "v2":
                    connected = {"id": f"evt-{time.time_ns()}", "type": "server.connected", "data": {}}
                    self.wfile.write(f"data: {json.dumps(connected, separators=(',', ':'))}\n\n".encode())
                self.wfile.write(b": connected\n\n")
                self.wfile.flush()
                while True:
                    try:
                        frame = target.get(timeout=2)
                    except queue.Empty:
                        frame = b": keepalive\n\n"
                    self.wfile.write(frame)
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                with subscribers_lock:
                    subscribers.discard(target)
        else:
            self.json_response({"path": parsed.path, "boot_id": BOOT_ID})

    def do_POST(self):
        if not self.authenticate():
            return
        parsed = urlparse(self.path)
        if parsed.path == "/control/activity":
            query = parse_qs(parsed.query)
            session_id = query.get("session", ["lifecycle"])[0]
            delay = min(float(query.get("delay", ["0.15"])[0]), 5.0)
            publish(session_id, "busy")

            def become_idle():
                time.sleep(max(delay, 0.01))
                publish(session_id, "idle")

            threading.Thread(target=become_idle, daemon=True).start()
            self.json_response({"session": session_id, "idle_after": delay})
        elif parsed.path == "/control/persist":
            length = min(int(self.headers.get("Content-Length", "0")), 65536)
            value = json.loads(self.rfile.read(length) or b"{}")
            os.makedirs(os.path.dirname(STATE_PATH), exist_ok=True)
            temporary = STATE_PATH + ".tmp"
            with open(temporary, "w", encoding="utf-8") as target:
                json.dump(value, target)
            os.replace(temporary, STATE_PATH)
            with open("/home/user/workspace/container-state.json", "w", encoding="utf-8") as target:
                json.dump(value, target)
            self.json_response(value)
        elif parsed.path == "/control/exit":
            self.json_response({"exiting": 0})
            threading.Thread(target=lambda: (time.sleep(0.1), os._exit(0)), daemon=True).start()
        elif parsed.path == "/control/oom":
            self.json_response({"allocating": True})

            def exhaust_memory():
                chunks = []
                while True:
                    chunks.append(bytearray(8 * 1024 * 1024))
                    time.sleep(0.02)

            threading.Thread(target=exhaust_memory, daemon=True).start()
        else:
            self.json_response({"path": parsed.path}, 404)


server = ThreadingHTTPServer(("0.0.0.0", 4096), Handler)
print(f"fake_opencode_ready ts_ns={time.time_ns()} boot_id={BOOT_ID}", flush=True)
server.serve_forever()
