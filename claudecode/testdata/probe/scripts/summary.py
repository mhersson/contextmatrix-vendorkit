"""Print what each probe under SCRATCH observed, in the terms of the README decisions table.

Usage: summary.py SCRATCH
"""

import glob
import json
import os
import sys

S = sys.argv[1]


def jl(path):
    try:
        with open(path, encoding="utf-8") as f:
            return [json.loads(l) for l in f if l.strip()]
    except FileNotFoundError:
        return []


def result(lines):
    r = [d for d in lines if d.get("type") == "result"]
    return r[-1] if r else None


def first_line(path):
    try:
        with open(path, encoding="utf-8") as f:
            return f.readline().strip()
    except FileNotFoundError:
        return ""


def section(title):
    print(f"\n== {title}")


def words(path):
    try:
        with open(path, encoding="utf-8") as f:
            return f.read().split()
    except FileNotFoundError:
        return None


section("no-credential captures")
print("version:", first_line(f"{S}/probe/version.txt"))
start = jl(f"{S}/probe/startup.jsonl")
print("startup lines before input:", len(start), [d.get("type") for d in start])
hs = jl(f"{S}/probe/handshake.jsonl")
r1 = [d for d in hs if d.get("type") == "control_response" and d["response"].get("request_id") == "r1"]
print("handshake answered r1:", bool(r1))
if r1:
    resp = r1[0]["response"].get("response", {})
    levels = sorted({tuple(m.get("supportedEffortLevels", [])) for m in resp.get("models", [])})
    print("  account:", resp.get("account"), " effort levels:", levels)

section("permissions and Bash timeouts")
for name in ["perm-allowed", "perm-readonly", "perm-denied", "t-control", "t-max-only", "t-default-and-max", "t-default-only"]:
    lines = jl(f"{S}/mock/bash/{name}/run/stdout.jsonl")
    tr = [c for d in lines if d.get("type") == "user" and isinstance(d["message"].get("content"), list)
          for c in d["message"]["content"] if c.get("type") == "tool_result"]
    r = result(lines)
    t = {}
    for row in open(f"{S}/mock/bash/{name}/run/timing.txt", encoding="utf-8") if lines else []:
        sec, _, rest = row.strip().partition(" ")
        # The first assistant line carries the tool_use, the first user line its result.
        if rest.startswith('{"type":"assistant"') and "tool_use" not in t:
            t["tool_use"] = float(sec)
        if rest.startswith('{"type":"user"') and "tool_result" not in t:
            t["tool_result"] = float(sec)
    took = round(t["tool_result"] - t["tool_use"], 2) if len(t) == 2 else None
    first = tr[0] if tr else {}
    print(f"{name}: tool_result is_error={first.get('is_error')} after={took}s "
          f"content={str(first.get('content'))[:70]!r} denials={len((r or {}).get('permission_denials', []))}")
print("perm-denied file created:", os.path.exists(f"{S}/mock/bash/perm-denied/should-not-exist"))

section("flags")
for d in sorted(glob.glob(f"{S}/mock/flags/*/")):
    name = os.path.basename(d.rstrip("/"))
    if name == "work":
        continue
    reqs = [q for q in jl(f"{d}/requests.jsonl") if (q.get("params") or {}).get("stream")]
    r = result(jl(f"{d}/run/stdout.jsonl"))
    err = first_line(f"{d}/run/stderr.txt")
    effort = [(q["params"].get("output_config") or {}).get("effort") for q in reqs]
    markers = [q.get("system_markers") for q in reqs]
    syslen = [q.get("system_len") for q in reqs]
    transcripts = glob.glob(f"{d}/cfg/projects/*/*.jsonl")
    print(f"{name}: requests={len(reqs)} effort={effort} system_len={syslen} markers={markers} "
          f"transcripts={len(transcripts)} result={(r or {}).get('subtype')} stderr={err[:90]!r}")

section("readiness gate")
counts = {}
for d in sorted(glob.glob(f"{S}/mock/gate/*/")):
    name = os.path.basename(d.rstrip("/")).rstrip("0123456789").rstrip("-")
    if name == "work":
        continue
    r = [x for x in jl(f"{d}/stdout.jsonl") if x.get("type") == "result"]
    want = 2 if name.startswith("two") else 1
    ok, total = counts.get(name, (0, 0))
    counts[name] = (ok + (len(r) >= want), total + 1)
for name, (ok, total) in counts.items():
    print(f"{name}: {ok}/{total} answered every message")
texts = []
for q in jl(f"{S}/mock/gate/requests.jsonl"):
    for m in q.get("messages") or []:
        if m.get("role") == "user" and isinstance(m.get("content"), list):
            texts += [c.get("text") for c in m["content"] if c.get("type") == "text"]
print("distinct user texts the mock saw:", len(set(t for t in texts if t and not t.startswith("<"))))

section("max turns")
lines = jl(f"{S}/mock/maxturns/run/stdout.jsonl")
r = result(lines) or {}
print({k: r.get(k) for k in ["subtype", "is_error", "terminal_reason", "errors", "num_turns"]},
      "has result field:", "result" in r)
for row in open(f"{S}/mock/maxturns/run/timing.txt", encoding="utf-8") if lines else []:
    if "exit rc=" in row:
        print("exit:", row.strip())

section("parallel tool calls")
order = []
for d in jl(f"{S}/mock/parallel/run/stdout.jsonl"):
    if d.get("type") == "stream_event" and d["event"].get("type") in ("content_block_start", "message_stop"):
        order.append(d["event"]["type"].replace("content_block_start", "block") + str(d["event"].get("index", "")))
    elif d.get("type") == "user" and isinstance(d["message"].get("content"), list):
        order += ["result" + c["tool_use_id"].rsplit("_", 1)[-1] for c in d["message"]["content"]
                  if c.get("type") == "tool_result"]
print("blocks, results and message ends, in stream order:", order)

section("P6 config-dir isolation and resume")
P = f"{S}/p6"
auth = {q.get("authorization") for r_ in "ABC" for q in jl(f"{P}/mock{r_}/requests.jsonl")}
keys = {q.get("x_api_key") for r_ in "ABC" for q in jl(f"{P}/mock{r_}/requests.jsonl")}
print("authorization headers:", auth, " x-api-key:", keys)
b1 = next((q for q in jl(f"{P}/mockB/requests.jsonl") if (q.get("params") or {}).get("stream")), {})
roles = []
for m in b1.get("messages") or []:
    if m.get("role") == "system":
        continue
    c = m["content"]
    kinds = [x.get("type") for x in c] if isinstance(c, list) else ["text"]
    text = [x.get("text", "")[:30] for x in c if x.get("type") == "text"] if isinstance(c, list) else [c[:30]]
    roles.append((m["role"], kinds, [t for t in text if not t.startswith("<")]))
print("B first request messages:", roles)
for run in "abc":
    print(f"fired after {run}:", words(f"{P}/state/fired-after-{run}.txt"))
print("HOME contents:", words(f"{P}/state/home-files.txt"))
print("cfgA files:", words(f"{P}/state/cfgA-files.txt"))
sids = {(result(jl(f"{P}/run{r_}/stdout.jsonl")) or {}).get("session_id") for r_ in "ABC"}
print("session ids:", sids)

section("system prompt on resume")
for run in "abc":
    reqs = [q for q in jl(f"{S}/mock/snapshot/mock-{run}/requests.jsonl") if (q.get("params") or {}).get("stream")]
    print(f"{run}: markers={[q.get('system_markers') for q in reqs]}")
print("(a: first launch file APPEND; b: resumed with REPLACE file; c: same plus --system-prompt-snapshot off)")

section("commit and PR attribution")
for name in ["none", "attribution", "coauthored", "replace-none", "replace-attribution", "announce-env", "resume-a",
             "resume-b"]:
    reqs = [q for q in jl(f"{S}/mock/attribution/mock-{name}/requests.jsonl") if (q.get("params") or {}).get("stream")]
    last = [m for m in (reqs[0].get("messages") or []) if m.get("role") == "user"][-1:] if reqs else []
    text = json.dumps(last[0].get("content"), ensure_ascii=False) if last else ""
    print(f"{name}: per part={reqs[0].get('attribution') if reqs else None} "
          f"newest user message={[a for a in ('Co-Authored-By', 'Generated with [Claude Code]') if a in text]}")
print("(resume-b resumes resume-a, recorded without the setting, with attribution set to empty)")

section("account check")
for name in ["oauth", "oauth-key", "oauth-bedrock", "oauth-mantle", "oauth-simple", "oauth-socket", "both"]:
    cr = [d for d in jl(f"{S}/mock/account/{name}/stdout.jsonl") if d.get("type") == "control_response"]
    print(f"{name}: account={cr[0]['response'].get('response', {}).get('account') if cr else None}")
reqs = [q for q in jl(f"{S}/mock/account/both/requests.jsonl") if (q.get("params") or {}).get("stream")]
print("both: authorization", {q.get("authorization") for q in reqs}, "x-api-key", {q.get("x_api_key") for q in reqs})

section("MCP tool idle timeout")
for d in sorted(glob.glob(f"{S}/mock/idle-*/*/")):
    name = os.path.relpath(d, f"{S}/mock").rstrip("/")
    if name.endswith("/work"):
        continue
    t = {}
    for row in open(f"{d}/timing.txt", encoding="utf-8") if os.path.exists(f"{d}/timing.txt") else []:
        sec, _, rest = row.strip().partition(" ")
        # The idle scenario's first assistant line is the sleep call, the first user line its result.
        if rest.startswith('{"type":"assistant"') and "tool_use" not in t:
            t["tool_use"] = float(sec)
        if rest.startswith('{"type":"user"') and "tool_result" not in t:
            t["tool_result"] = float(sec)
    took = round(t["tool_result"] - t["tool_use"], 1) if len(t) == 2 else None
    lines = jl(f"{d}/stdout.jsonl")
    tr = [c for x in lines if x.get("type") == "user" and isinstance(x["message"].get("content"), list)
          for c in x["message"]["content"] if c.get("type") == "tool_result"]
    first = tr[0] if tr else {}
    content = first.get("content")
    if isinstance(content, list):
        content = " ".join(c.get("text", "") for c in content if isinstance(c, dict))
    calls = [c for c in jl(f"{d}/mcp-calls.jsonl") if c.get("method", "").startswith(("tools/call", "notifications/cancelled"))]
    print(f"{name}: tool_result after={took}s is_error={first.get('is_error')} content={str(content)[:110]!r} "
          f"result={(result(lines) or {}).get('subtype')} server={[(c['method'], c.get('delivered')) for c in calls]}")
