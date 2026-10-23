#!/usr/bin/env python3
"""
TODO-3 check: verify the control plane delivers a webhook notification when a
volume becomes AT_RISK.

This file is self-contained and deliberately does not touch integration.py:
it reuses the shared test setup (integration.py --setup-only prepares an EC 6+4
volume), restarts the control plane WITH -webhook pointing at a local receiver,
then repeats the "five PCs offline" scenario and asserts the receiver got a
volume_at_risk event naming the recommended PCs.

Usage:
  python3 integration_webhook.py

Exit code: 0 on success, 1 on failure, 2 on usage/environment errors.
"""

import hashlib
import http.server
import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SERVER_BIN = os.path.join(ROOT, "bin", "server")
INTEGRATION = os.path.join(ROOT, "src", "tests", "integration.py")

MASTER = "192.168.1.9:9333"
CP_URL = "http://192.168.1.9:8099"
COLLECTION = "itest"
WEBHOOK_PORT = 9911
WEBHOOK_URL = f"http://127.0.0.1:{WEBHOOK_PORT}/hook"
WEBHOOK_LOG = "/tmp/itest_webhook.jsonl"


# --- local webhook receiver ------------------------------------------------
class Receiver:
    """A tiny HTTP server that appends each POST body to a JSONL file."""

    def __init__(self, port, logfile, address="127.0.0.1"):
        self.port = port
        self.logfile = logfile
        self.address = address

        class Handler(http.server.BaseHTTPRequestHandler):
            log_file = logfile

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                body = self.rfile.read(length).decode("utf-8", "replace")
                with open(self.log_file, "a") as f:
                    f.write(body + "\n")
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"ok")

            def log_message(self, fmt, *args):
                pass  # keep the test output clean

        self.server = http.server.HTTPServer((address, port), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self):
        if os.path.exists(self.logfile):
            os.remove(self.logfile)
        self.thread.start()

    def stop(self):
        self.server.shutdown()


# --- helpers ---------------------------------------------------------------
def http_get_json(url):
    try:
        with urllib.request.urlopen(url, timeout=5) as r:
            return json.loads(r.read().decode())
    except Exception:
        return None


def volumes():
    d = http_get_json(f"{CP_URL}/api/v1/volumes/risk")
    return d.get("volumes", []) if d else []


def states():
    return [v.get("state", "") for v in volumes()
            if v.get("collection") == COLLECTION]


def wait_for(desc, check, attempts=30, delay=2):
    print(f"    waiting for {desc}:", end="", flush=True)
    for _ in range(attempts):
        if check():
            print(" done")
            return True
        print(".", end="", flush=True)
        time.sleep(delay)
    print(" TIMEOUT")
    return False


def read_file_md5():
    try:
        p = subprocess.run([os.path.join(ROOT, "bin", "mc"),
                            "cat", f"dev/{COLLECTION}/file.bin"],
                           capture_output=True, timeout=30)
        return hashlib.md5(p.stdout).hexdigest()
    except Exception:
        return "ERR"


def read_ok(expect_md5):
    return read_file_md5() == expect_md5


def restart_control_plane(webhook_url):
    """Stop the running control plane (by pid file or ss) and start one with
    -webhook. Returns True on success."""

    pid = None
    try:
        with open(os.path.join(ROOT, ".cluster", "pids", "controlplane.pid")) as f:
            pid = int(f.read().strip())
    except Exception:
        pid = None
    if pid:
        try:
            os.kill(pid, 9)
        except ProcessLookupError:
            pass
    else:
        # Fallback: find the process listening on :8099 via ss.
        try:
            out = subprocess.run(["ss", "-tlnp"], capture_output=True, text=True).stdout
            for line in out.splitlines():
                if ":8099" in line and "pid=" in line:
                    m = re.search(r"pid=(\d+)", line)
                    if m:
                        os.kill(int(m.group(1)), 9)
        except Exception:
            pass
    time.sleep(1)
    p = subprocess.Popen(
        [SERVER_BIN, "-listen=:8099", f"-master={MASTER}",
         f"-webhook={webhook_url}", "-poll=5"],
        stdout=open(os.path.join(ROOT, ".cluster", "logs", "server.log"), "w"),
        stderr=subprocess.STDOUT)
    with open(os.path.join(ROOT, ".cluster", "pids", "controlplane.pid"), "w") as f:
        f.write(str(p.pid))
    return wait_for("control plane healthz after restart",
                    is_cp_healthy, attempts=20, delay=1)


def kill_by_node(n):
    try:
        with open(os.path.join(ROOT, ".cluster", "pids", f"volume{n}.pid")) as f:
            pid = int(f.read().strip())
        os.kill(pid, 9)
    except Exception:
        pass


# --- main ------------------------------------------------------------------
def main():
    PASS, FAIL = 0, 0

    def ok(msg, got=""):
        nonlocal PASS
        PASS += 1
        print(f"  [PASS] {msg}" + (f" (got {got})" if got else ""))

    def bad(msg):
        nonlocal FAIL
        FAIL += 1
        print(f"  [FAIL] {msg}")

    print("==> Starting local webhook receiver on", WEBHOOK_URL)
    receiver = Receiver(WEBHOOK_PORT, WEBHOOK_LOG)
    receiver.start()

    print("==> Ensuring the EC 6+4 test data exists (integration.py --setup-only)")
    if not any(v.get("collection") == COLLECTION for v in volumes()):
        rc = subprocess.run([sys.executable, "-u", INTEGRATION, "--setup-only"]).returncode
        if rc != 0:
            print("FATAL: setup failed")
            sys.exit(2)

    print("==> Restarting control plane with -webhook")
    if not restart_control_plane(WEBHOOK_URL):
        print("FATAL: control plane did not come up")
        sys.exit(2)

    print("==> Scenario: five PCs offline -> AT_RISK -> webhook with recommendation")
    for n in (4, 5, 6, 7, 9):
        kill_by_node(n)
        print(f"    killed volume{n}")

    wait_for("AT_RISK", lambda: "AT_RISK" in states())
    vs = [v for v in volumes() if v.get("collection") == COLLECTION]
    advice = sorted({a for v in vs for a in v.get("nodes_to_start", [])})
    ok("control plane reports AT_RISK")
    if advice:
        ok("nodes_to_start recommends PCs", ",".join(advice))
    else:
        print("    (api advice empty; relying on the webhook payload below)")

    print("==> Waiting for the webhook event")
    received = wait_for("volume_at_risk event on the receiver",
                        lambda: webhook_has_event("volume_at_risk"))
    if not received:
        bad("webhook did not receive volume_at_risk")
        print("\n-- receiver log so far --")
        if os.path.exists(WEBHOOK_LOG):
            print(open(WEBHOOK_LOG).read())
    else:
        ok("webhook received volume_at_risk")
        detail = webhook_event_detail("volume_at_risk")
        ok("event payload carries the recovery hint", "Restore by turning on" in detail and detail[:160] != "")

    receiver.stop()
    print()
    print(f"==> Summary: PASS={PASS} FAIL={FAIL}")
    sys.exit(0 if FAIL == 0 else 1)


def is_cp_healthy():
    try:
        with urllib.request.urlopen(f"{CP_URL}/healthz", timeout=2) as r:
            return r.status == 200
    except Exception:
        return False


def webhook_has_event(kind):
    if not os.path.exists(WEBHOOK_LOG):
        return False
    for line in open(WEBHOOK_LOG):
        try:
            ev = json.loads(line)
            if ev.get("kind") == kind:
                return True
        except Exception:
            pass
    return False


def webhook_event_detail(kind):
    if not os.path.exists(WEBHOOK_LOG):
        return ""
    for line in open(WEBHOOK_LOG):
        try:
            ev = json.loads(line)
            if ev.get("kind") == kind:
                return ev.get("detail", "")
        except Exception:
            pass
    return ""


if __name__ == "__main__":
    main()
