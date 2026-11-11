"""Mock Anthropic Messages API for token-free claude -p probes.

Adapted from the prototype's mock-api/mockapi.py; the smoke scenario's finish calls from its mockp3.py.

Usage: mock.py PORT LOG SCENARIO [TOOL_INPUT_JSON [HOLD_FILE]]
  SCENARIO text: every streamed call answers "ok" and ends the turn.
  SCENARIO bash: when the last user turn is not a tool_result, answer one Bash tool_use with
                 TOOL_INPUT_JSON; after a tool_result, answer "done" and end the turn.
  SCENARIO idle: with mcp__cm__sleep in the request's tools, one sleep call; after its
                 tool_result, answer "done" and end the turn.
  SCENARIO smoke: for claudecode's smoke tests, by the request's tools. With mcp__cm__finish:
                 finish {} (refused), then finish with commit_message and summary, then "all done"
                 (the stop hook should end the run before it). With Bash: one Bash tool_use whose
                 command writes its PID to bash.pid in the cwd and execs a long sleep.
  HOLD_FILE:     the first streamed answer waits until this file exists.
Every request is appended to LOG as one JSON line (auth headers, parameters, messages, system
prompt length, hash and markers); the full system text goes to LOG.system.<n>.txt.
"""

import hashlib
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
LOG = sys.argv[2]
SCENARIO = sys.argv[3]
TOOL_INPUT = json.loads(sys.argv[4]) if len(sys.argv) > 4 else {}
HOLD = sys.argv[5] if len(sys.argv) > 5 else ""
MAX_TOOL_ANSWERS = 6  # guard against a tool loop when a probe goes wrong
FINISH = "mcp__cm__finish"
SLEEP = "mcp__cm__sleep"
FINISH_INPUTS = [{}, {"commit_message": "feat: smoke", "summary": "done"}]
SLEEPER = {"command": "sh -c 'echo $$ > bash.pid; exec sleep 300'", "description": "Sleep"}
MARKERS = ["MARKER-REPLACE-7f3a", "MARKER-APPEND-7f3a"]
# Unique per mock start: a resumed transcript must not meet message or tool IDs it already holds.
RUN = format(int(time.time() * 1000) % 0xFFFFFF, "06x")
lock = threading.Lock()
counter = [0]
held = [False]


def sse(ev, data):
    return f"event: {ev}\ndata: {json.dumps(data)}\n\n".encode()


def system_text(system):
    if isinstance(system, str):
        return system
    if isinstance(system, list):
        return "\n".join(b.get("text", "") for b in system if isinstance(b, dict))
    return ""


def tool_results(msgs):
    return sum(1 for m in msgs if isinstance(m.get("content"), list)
               for b in m["content"] if isinstance(b, dict) and b.get("type") == "tool_result")


def tool_answer(body, n):
    """The (name, input) of the tool_use to answer with, or None for text."""
    msgs = body.get("messages", [])
    if SCENARIO == "bash":
        return ("Bash", TOOL_INPUT) if not last_is_tool_result(msgs) and n <= MAX_TOOL_ANSWERS else None
    if SCENARIO == "idle":
        names = [t.get("name") for t in body.get("tools", []) if isinstance(t, dict)]
        return (SLEEP, {}) if SLEEP in names and tool_results(msgs) == 0 else None
    if SCENARIO == "smoke":
        names = [t.get("name") for t in body.get("tools", []) if isinstance(t, dict)]
        done = tool_results(msgs)
        if FINISH in names and done < len(FINISH_INPUTS):
            return FINISH, FINISH_INPUTS[done]
        if "Bash" in names and done == 0:
            return "Bash", SLEEPER
    return None


def last_is_tool_result(msgs):
    # Claude Code appends role "system" entries to messages, so judge the last user turn.
    users = [m for m in msgs if m.get("role") == "user"]
    if not users:
        return False
    c = users[-1].get("content")
    return isinstance(c, list) and any(isinstance(b, dict) and b.get("type") == "tool_result" for b in c)


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def record(self, body):
        with lock:
            counter[0] += 1
            n = counter[0]
        sys_txt = system_text(body.get("system")) if isinstance(body, dict) else ""
        rec = {
            "n": n,
            "t": round(time.time(), 3),
            "method": self.command,
            "path": self.path,
            "authorization": self.headers.get("authorization"),
            "x_api_key": self.headers.get("x-api-key"),
            "anthropic_beta": self.headers.get("anthropic-beta"),
        }
        if isinstance(body, dict) and body:
            rec["params"] = {k: v for k, v in body.items() if k not in ("messages", "system", "tools")}
            rec["tools"] = [t.get("name") for t in body.get("tools", []) if isinstance(t, dict)]
            rec["system_len"] = len(sys_txt)
            rec["system_sha256"] = hashlib.sha256(sys_txt.encode()).hexdigest()[:16]
            rec["system_markers"] = [m for m in MARKERS if m in sys_txt]
            rec["messages"] = body.get("messages")
            if sys_txt:
                with open(f"{LOG}.system.{n}.txt", "w") as f:
                    f.write(sys_txt)
        with lock:
            with open(LOG, "a") as f:
                f.write(json.dumps(rec) + "\n")
        return n

    def do_GET(self):
        self.record({})
        self.send_response(404)
        self.send_header("content-length", "0")
        self.end_headers()

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length", 0)))
        try:
            body = json.loads(raw or b"{}")
        except ValueError:
            body = {}
        n = self.record(body)
        if not self.path.startswith("/v1/messages"):
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()
            return
        if "count_tokens" in self.path:
            return self.json({"input_tokens": 10})
        model = body.get("model", "claude-haiku-5-5")
        usage0 = {"input_tokens": 10, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 1}
        if not body.get("stream"):
            return self.json({"id": f"msg_mock{RUN}_{n}", "type": "message", "role": "assistant", "model": model,
                              "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                              "stop_sequence": None, "usage": usage0})
        if HOLD and not held[0]:
            held[0] = True
            for _ in range(600):
                if os.path.exists(HOLD):
                    break
                time.sleep(0.05)
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("connection", "close")
        self.end_headers()
        w = self.wfile.write
        w(sse("message_start", {"type": "message_start", "message": {
            "id": f"msg_mock{RUN}_{n}", "type": "message", "role": "assistant", "model": model, "content": [],
            "stop_reason": None, "stop_sequence": None, "usage": usage0}}))
        answer = tool_answer(body, n)
        if answer:
            w(sse("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {
                "type": "tool_use", "id": f"toolu_mock{RUN}_{n}", "name": answer[0], "input": {}}}))
            w(sse("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {
                "type": "input_json_delta", "partial_json": json.dumps(answer[1])}}))
            stop = "tool_use"
        else:
            text = {"bash": "done", "idle": "done", "smoke": "all done"}.get(SCENARIO, "ok")
            w(sse("content_block_start", {"type": "content_block_start", "index": 0,
                                          "content_block": {"type": "text", "text": ""}}))
            w(sse("content_block_delta", {"type": "content_block_delta", "index": 0,
                                          "delta": {"type": "text_delta", "text": text}}))
            stop = "end_turn"
        w(sse("content_block_stop", {"type": "content_block_stop", "index": 0}))
        w(sse("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                                "usage": dict(usage0, output_tokens=5)}))
        w(sse("message_stop", {"type": "message_stop"}))
        self.wfile.flush()
        self.close_connection = True

    def json(self, obj):
        b = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
