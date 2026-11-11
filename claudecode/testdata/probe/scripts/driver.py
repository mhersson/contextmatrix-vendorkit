"""Spawn a command with this process's env, feed stdin on a schedule, capture stdout.

Usage: driver.py --out F [--timing F] [--err F] [--gate] [--send T:JSONLINE]... [--close T]
                 [--close-on-result N] -- cmd...
--gate writes an initialize control_request first and waits for its control_response;
--send offsets then count from that moment. --close-on-result N closes stdin after the
N-th result line. Writes raw stdout to --out, per-line arrival offsets to --timing.
"""

import argparse
import json
import os
import subprocess
import sys
import threading
import time

ap = argparse.ArgumentParser()
ap.add_argument("--out", required=True)
ap.add_argument("--timing")
ap.add_argument("--err")
ap.add_argument("--gate", action="store_true")
ap.add_argument("--send", action="append", default=[])
ap.add_argument("--close", type=float, default=10.0)
ap.add_argument("--close-on-result", type=int, default=0)
ap.add_argument("cmd", nargs=argparse.REMAINDER)
a = ap.parse_args()
cmd = a.cmd[1:] if a.cmd and a.cmd[0] == "--" else a.cmd

errf = open(a.err, "wb") if a.err else subprocess.DEVNULL
t0 = time.monotonic()
p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errf, env=os.environ.copy())
timing = []
log = []
out = open(a.out, "wb")
stdin_lock = threading.Lock()
gate_ok = threading.Event()
done = threading.Event()
results = [0]


def note(msg):
    log.append((time.monotonic() - t0, msg))


def close_stdin(why):
    with stdin_lock:
        if not p.stdin.closed:
            try:
                p.stdin.close()
            except BrokenPipeError:
                pass
            note("stdin closed (" + why + ")")


def write(js):
    with stdin_lock:
        if p.stdin.closed:
            note("stdin already closed, not sent " + js[:80])
            return
        try:
            p.stdin.write(js.encode() + b"\n")
            p.stdin.flush()
            note("sent " + js[:100])
        except BrokenPipeError:
            note("broken pipe on send")


def reader():
    for line in p.stdout:
        timing.append((time.monotonic() - t0, line[:140]))
        out.write(line)
        out.flush()
        try:
            d = json.loads(line)
        except ValueError:
            d = {}
        if d.get("type") == "control_response" and d.get("response", {}).get("request_id") == "r1":
            gate_ok.set()
        if d.get("type") == "result":
            results[0] += 1
            if a.close_on_result and results[0] >= a.close_on_result:
                close_stdin("result")
    done.set()


th = threading.Thread(target=reader)
th.start()

base = 0.0
if a.gate:
    write('{"type":"control_request","request_id":"r1","request":{"subtype":"initialize"}}')
    if not gate_ok.wait(30):
        note("gate timeout")
    base = time.monotonic() - t0

events = []
for s in a.send:
    t, _, js = s.partition(":")
    events.append((base + float(t), "send", js))
events.append((base + a.close, "close", None))
events.sort(key=lambda e: e[0])
for t, kind, js in events:
    now = time.monotonic() - t0
    if t > now:
        if done.wait(t - now):
            note("process stdout ended before " + kind)
            break
    if kind == "send":
        write(js)
    else:
        close_stdin("timer")

try:
    rc = p.wait(timeout=90)
except subprocess.TimeoutExpired:
    p.kill()
    rc = p.wait()
    note("SIGKILL after 90s")
th.join()
note(f"exit rc={rc}")
if a.timing:
    with open(a.timing, "w") as f:
        rows = timing + [(t, ("# " + m).encode()) for t, m in log]
        for t, line in sorted(rows, key=lambda x: x[0]):
            f.write(f"{t:7.3f} {line.decode(errors='replace').rstrip()}\n")
print(f"rc={rc} results={results[0]}", file=sys.stderr)
