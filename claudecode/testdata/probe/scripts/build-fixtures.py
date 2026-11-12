"""Rewrite probe/ and synthetic/ in a testdata directory from fresh probe captures.

Usage: build-fixtures.py SCRATCH TESTDATA
real/ is never touched: those are live recordings. Synthetic fixtures derive from
real/t1.jsonl and real/t5.jsonl; the SCRATCH path in captures becomes /scratch.
"""

import json
import os
import re
import sys

SCR = os.path.realpath(sys.argv[1])
TD = sys.argv[2]
# Claude Code's projects/ key is the cwd with every non-alphanumeric character as "-".
KEY = re.sub(r"[^A-Za-z0-9]", "-", SCR)


def dump(obj):
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False)


def sanitize(text):
    return text.replace(SCR, "/scratch").replace(KEY, "-scratch")


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)


def copy(src, dst):
    with open(src, encoding="utf-8") as f:
        write(dst, sanitize(f.read()))


def lines(path):
    with open(path, encoding="utf-8") as f:
        return [l for l in f.read().split("\n") if l]


for name in ["help.txt", "version.txt", "startup.jsonl", "handshake.jsonl"]:
    copy(f"{SCR}/probe/{name}", f"{TD}/probe/{name}")
for r in "ABC":
    copy(f"{SCR}/p6/run{r}/stdout.jsonl", f"{TD}/probe/p6/run-{r.lower()}.jsonl")
    copy(f"{SCR}/p6/mock{r}/requests.jsonl", f"{TD}/probe/p6/mock-requests-{r.lower()}.jsonl")
for r in "abc":
    copy(f"{SCR}/mock/snapshot/mock-{r}/requests.jsonl", f"{TD}/probe/snapshot/mock-requests-{r}.jsonl")
for r in ["none", "texts", "attribution", "coauthored", "replace-none", "replace-attribution", "announce-off-none",
          "announce-off", "remote-texts", "remote", "no-bash", "read-only", "resume-a", "resume-b"]:
    copy(f"{SCR}/mock/attribution/mock-{r}/requests.jsonl", f"{TD}/probe/attribution/mock-requests-{r}.jsonl")
copy(f"{SCR}/mock/maxturns/run/stdout.jsonl", f"{TD}/synthetic/max_turns.jsonl")
copy(f"{SCR}/mock/maxbudget/end-turn/run/stdout.jsonl", f"{TD}/synthetic/max_budget_end_turn.jsonl")
copy(f"{SCR}/mock/maxbudget/tool-use/run/stdout.jsonl", f"{TD}/synthetic/max_budget_tool_use.jsonl")
copy(f"{SCR}/mock/parallel/run/stdout.jsonl", f"{TD}/synthetic/parallel.jsonl")

t1 = lines(f"{TD}/real/t1.jsonl")
t5 = lines(f"{TD}/real/t5.jsonl")

# rejected: t1 with the rate_limit_event rejected and no result line.
rl = json.loads(t1[2])
assert rl["type"] == "rate_limit_event"
rl["rate_limit_info"]["status"] = "rejected"
rl["rate_limit_info"]["unifiedWindows"]["seven_day"]["utilization"] = 1.0
rl["rate_limit_info"]["unifiedWindows"]["seven_day"]["status"] = "rejected"
write(f"{TD}/synthetic/rejected.jsonl", "\n".join([t1[0], t1[1], dump(rl)]) + "\n")

# crash: t5 without its result line.
assert json.loads(t5[-1])["type"] == "result"
write(f"{TD}/synthetic/crash.jsonl", "\n".join(t5[:-1]) + "\n")

# two_models: t5 whose result also bills a claude-haiku-5-5 call no assistant message served.
res = json.loads(t5[-1])
haiku = {
    "inputTokens": 350, "outputTokens": 12, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0,
    "webSearchRequests": 0, "costUSD": 0.000041, "contextWindow": 1000000, "maxOutputTokens": 128000,
    "thinkingTokens": 0, "cacheCreationEphemeral5mInputTokens": 0, "cacheCreationEphemeral1hInputTokens": 0,
    "canonicalModel": "claude-haiku-5-5", "provider": "firstParty", "costBasis": "list",
}
res["modelUsage"]["claude-haiku-5-5"] = haiku
res["model_usage_since_last_result"]["claude-haiku-5-5"] = dict(haiku)
res["total_cost_usd"] = 0.0175914
write(f"{TD}/synthetic/two_models.jsonl", "\n".join(t5[:-1] + [dump(res)]) + "\n")

# unknown: t1 plus an unknown top-level type, an unknown system subtype, unknown fields everywhere.
sid = json.loads(t1[0])["session_id"]
known = [json.loads(l) for l in t1]
for d in known:
    d["x_future_field"] = {"added_in": "2.9.0", "values": [1, 2]}
known[1]["message"]["x_future_message_field"] = "ignored"
known[3]["usage"]["x_future_bucket_tokens"] = 7
future_type = {"type": "x_future_event", "payload": {"kind": "probe"}, "uuid": "0d3e3f4a-6b1c-4f7e-9a2d-5c8b1e0f7a61", "session_id": sid}
future_sys = {"type": "system", "subtype": "x_future_subtype", "detail": "ignored", "uuid": "7c2b9e1d-3f4a-4d6b-8e5c-1a9f0b2c3d47", "session_id": sid}
write(f"{TD}/synthetic/unknown.jsonl", "\n".join(
    [dump(known[0]), dump(future_type), dump(future_sys), dump(known[1]), dump(known[2]), dump(known[3])]) + "\n")

# partial: t5 with stream_event lines per call, shaped like real/p4-partial-messages.jsonl.
init, a_text, a_tool, rate, user, a_done, result = [json.loads(l) for l in t5]
assert a_text["message"]["id"] == a_tool["message"]["id"] != a_done["message"]["id"]
tool_block = a_tool["message"]["content"][0]
tool_json = dump(tool_block["input"])
uid = iter("%08x-5e1f-4c3a-9b2d-%012x" % (0x5a1e0000 + i, 0x7f3a00000000 + i) for i in range(1, 100))


def ev(msg_id, event, start=None):
    d = {"type": "stream_event", "event": event, "session_id": sid5, "parent_tool_use_id": None, "uuid": next(uid)}
    if start is not None:
        d["ttft_ms"] = start
        d["thinking_display"] = "updates"
    d["api_message_id"] = msg_id
    return dump(d)


def message_start(asst, ttft):
    m = asst["message"]
    msg = {k: m[k] for k in ["model", "id", "type", "role"]}
    msg["content"] = []
    for k in ["container", "stop_reason", "stop_sequence", "stop_details", "usage", "input_transformations",
              "diagnostics", "context_management"]:
        msg[k] = m[k]
    return ev(m["id"], {"type": "message_start", "message": msg}, start=ttft)


def message_delta(msg_id, stop, inp, cc, cr, out):
    it = {"input_tokens": inp, "output_tokens": out, "cache_read_input_tokens": cr,
          "cache_creation_input_tokens": cc,
          "cache_creation": {"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": cc}, "type": "message"}
    usage = {"input_tokens": inp, "cache_creation_input_tokens": cc, "cache_read_input_tokens": cr,
             "output_tokens": out, "output_tokens_details": {"thinking_tokens": 0}, "iterations": [it]}
    return ev(msg_id, {"type": "message_delta",
                       "delta": {"stop_reason": stop, "stop_sequence": None, "stop_details": None, "container": None},
                       "usage": usage, "context_management": {"applied_edits": []}})


sid5 = init["session_id"]
id1 = a_text["message"]["id"]
id2 = a_done["message"]["id"]
u = result["usage"]
# Call 2's final usage is the result's last iteration; call 1 gets the rest of the totals.
last = u["iterations"][-1]
c2 = (last["input_tokens"], last["cache_creation_input_tokens"], last["cache_read_input_tokens"], last["output_tokens"])
c1 = (u["input_tokens"] - c2[0], u["cache_creation_input_tokens"] - c2[1], u["cache_read_input_tokens"] - c2[2],
      u["output_tokens"] - c2[3])
assert c1 == (2, 3844, 0, 120) and c2 == (2, 133, 3844, 5)

out = [t5[0]]
out.append(message_start(a_text, 1310))
out.append(ev(id1, {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}))
out.append(ev(id1, {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "2+2 equals"}}))
out.append(ev(id1, {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": " 4."}}))
out.append(t5[1])
out.append(ev(id1, {"type": "content_block_stop", "index": 0}))
out.append(ev(id1, {"type": "content_block_start", "index": 1, "content_block": {
    "type": "tool_use", "id": tool_block["id"], "name": tool_block["name"], "input": {}}}))
cut = [0, 2, 44, len(tool_json)]
for a, b in zip(cut, cut[1:]):
    out.append(ev(id1, {"type": "content_block_delta", "index": 1,
                        "delta": {"type": "input_json_delta", "partial_json": tool_json[a:b]}}))
out.append(t5[2])
out.append(ev(id1, {"type": "content_block_stop", "index": 1}))
out.append(message_delta(id1, "tool_use", *c1))
out.append(ev(id1, {"type": "message_stop"}))
out.append(t5[3])
out.append(t5[4])
out.append(message_start(a_done, 905))
out.append(ev(id2, {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}))
out.append(ev(id2, {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Done."}}))
out.append(t5[5])
out.append(ev(id2, {"type": "content_block_stop", "index": 0}))
out.append(message_delta(id2, "end_turn", *c2))
out.append(ev(id2, {"type": "message_stop"}))
out.append(t5[6])
assert "".join(json.loads(l)["event"]["delta"]["partial_json"] for l in out
               if '"input_json_delta"' in l) == tool_json
write(f"{TD}/synthetic/partial.jsonl", "\n".join(out) + "\n")
print("ok")
