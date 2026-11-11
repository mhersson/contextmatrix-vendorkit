"""Minimal MCP server (Streamable HTTP, JSON responses) with one silent tool, sleep, standing
in for a bridged tool that runs long without sending anything.

Usage: sleepmcp.py PORT SECONDS LOG
  sleep sleeps SECONDS before answering. Every JSON-RPC message is appended to LOG as one
  JSON line with its arrival time; a tools/call also logs when it answered and whether the
  client was still connected to read the answer.
"""

import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
SECONDS = float(sys.argv[2])
LOG = sys.argv[3]
lock = threading.Lock()


def log(rec):
    rec["t"] = round(time.time(), 3)
    with lock:
        with open(LOG, "a") as f:
            f.write(json.dumps(rec) + "\n")


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        self.empty(405)

    def do_DELETE(self):
        self.empty(200)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length", 0)))
        try:
            msg = json.loads(raw or b"{}")
        except ValueError:
            return self.empty(400)
        method = msg.get("method")
        log({"method": method, "id": msg.get("id"), "params": msg.get("params") if method != "tools/call" else None})
        if "id" not in msg:
            return self.empty(202)
        if method == "initialize":
            result = {"protocolVersion": msg["params"].get("protocolVersion", "2025-06-18"),
                      "capabilities": {"tools": {}}, "serverInfo": {"name": "cm", "version": "1"}}
        elif method == "tools/list":
            result = {"tools": [{"name": "sleep", "description": "Runs a long silent task.",
                                 "inputSchema": {"type": "object"}}]}
        elif method == "tools/call":
            time.sleep(SECONDS)
            result = {"content": [{"type": "text", "text": f"slept {SECONDS}s"}]}
        else:
            result = {}
        body = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}).encode()
        try:
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            self.wfile.flush()
            delivered = True
        except OSError:
            delivered = False
        if method == "tools/call":
            log({"method": "tools/call answered", "id": msg["id"], "delivered": delivered})

    def empty(self, code):
        self.send_response(code)
        self.send_header("content-length", "0")
        self.end_headers()


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
