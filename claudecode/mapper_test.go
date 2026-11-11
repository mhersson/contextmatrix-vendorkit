package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cmlocal is the spike's bridge prefix recorded in the t5 family of fixtures.
const cmlocal = "mcp__cmlocal__"

const finishInput = `{"commit_message":"chore: answer 2+2 question","summary":"Stated that 2+2 equals 4. No files were changed."}`

func readFixture(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", path))
	require.NoError(t, err)

	return data
}

// replay feeds a fixture through a mapper line by line and finishes it as a
// cleanly exited process would.
func replay(t *testing.T, path string, o mapperOpts) ([]events.Event, harness.Result, error) {
	t.Helper()

	return replayData(t, readFixture(t, path), o, nil)
}

func replayData(t *testing.T, data []byte, o mapperOpts, procErr error) ([]events.Event, harness.Result, error) {
	t.Helper()

	return replayHook(t, data, o, procErr, nil)
}

// replayHook calls before ahead of each line fed.
func replayHook(t *testing.T, data []byte, o mapperOpts, procErr error, before func(line []byte)) ([]events.Event, harness.Result, error) {
	t.Helper()

	var transcript bytes.Buffer

	o.Emitter = events.NewEmitter(nil, &transcript)
	m := newMapper(o)

	for line := range bytes.Lines(data) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if before != nil {
				before(line)
			}

			m.feed(line)
		}
	}

	res, err := m.finish(procErr)

	return decodeEvents(t, transcript.Bytes()), res, err
}

func decodeEvents(t *testing.T, transcript []byte) []events.Event {
	t.Helper()

	var evs []events.Event

	for line := range bytes.Lines(transcript) {
		var ev events.Event
		require.NoError(t, json.Unmarshal(line, &ev))

		evs = append(evs, ev)
	}

	return evs
}

func kinds(evs []events.Event) []events.Kind {
	out := make([]events.Kind, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}

	return out
}

func ofKind(evs []events.Event, k events.Kind) []events.Event {
	var out []events.Event

	for _, ev := range evs {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}

	return out
}

func stops(evs []events.Event) []events.Event {
	var out []events.Event

	for _, ev := range ofKind(evs, events.StateChange) {
		if _, ok := ev.Data["stop"]; ok {
			out = append(out, ev)
		}
	}

	return out
}

type buckets struct{ prompt, completion, cacheRead, cacheCreation int64 }

func (b buckets) add(o buckets) buckets {
	return buckets{b.prompt + o.prompt, b.completion + o.completion, b.cacheRead + o.cacheRead, b.cacheCreation + o.cacheCreation}
}

func usageOf(t *testing.T, ev events.Event) buckets {
	t.Helper()

	n := func(key string) int64 {
		v, ok := ev.Data[key].(float64)
		require.Truef(t, ok, "usage event lacks %s: %v", key, ev.Data)

		return int64(v)
	}

	return buckets{n("prompt_tokens"), n("completion_tokens"), n("cache_read_tokens"), n("cache_creation_tokens")}
}

func sumUsage(t *testing.T, evs []events.Event) buckets {
	t.Helper()

	var sum buckets
	for _, ev := range ofKind(evs, events.UsageKind) {
		sum = sum.add(usageOf(t, ev))
	}

	return sum
}

func resultBuckets(r harness.Result) buckets {
	return buckets{r.PromptTokens, r.CompletionTokens, r.CacheReadTokens, r.CacheCreationTokens}
}

// stripVolatile drops the envelope fields that differ between two replays.
func stripVolatile(evs []events.Event) []events.Event {
	out := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		out = append(out, events.Event{Kind: ev.Kind, Data: ev.Data})
	}

	return out
}

func rewrite(data []byte, pairs ...string) []byte {
	return []byte(strings.NewReplacer(pairs...).Replace(string(data)))
}

func TestMapperBasicRun(t *testing.T) {
	evs, res, err := replay(t, "real/t1.jsonl", mapperOpts{Prefix: "mcp__cm__"})
	require.NoError(t, err)

	require.Equal(t, []events.Kind{events.ModelRequest, events.ModelResponse, events.UsageKind, events.StateChange}, kinds(evs))

	assert.Equal(t, map[string]any{"turn": 1.0, "model": "claude-haiku-5-5"}, evs[0].Data)

	resp := evs[1].Data
	assert.Equal(t, "ok", resp["content"])
	assert.InDelta(t, 2, resp["content_len"], 0)
	assert.InDelta(t, 0, resp["tool_calls"], 0)
	assert.InDelta(t, 1, resp["turn"], 0)
	assert.Equal(t, "claude-haiku-5-5", resp["model"])
	assert.Contains(t, resp, "finish_reason")

	assert.Equal(t, buckets{2, 4, 11382, 6379}, usageOf(t, evs[2]))
	assert.Equal(t, "claude-haiku-5-5", evs[2].Data["model"])
	assert.Equal(t, "subscription", evs[2].Data["cost_basis"])

	assert.Equal(t, map[string]any{"stop": "done", "turns": 1.0}, evs[3].Data)

	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, 1, res.Turns)
	assert.Equal(t, "ok", res.Output)
	assert.Equal(t, "claude-haiku-5-5", res.ModelUsed)
	assert.Equal(t, buckets{2, 4, 11382, 6379}, resultBuckets(res))
	assert.InDelta(t, 0.0013918200000000002, res.TotalCostUSD, 1e-12)
	assert.Nil(t, res.CompletionArgs)
}

func TestMapperOneUsagePerCall(t *testing.T) {
	evs, res, err := replay(t, "real/t5.jsonl", mapperOpts{Prefix: cmlocal})
	require.NoError(t, err)

	usages := ofKind(evs, events.UsageKind)
	responses := 0

	for i, ev := range evs {
		if ev.Kind != events.ModelResponse {
			continue
		}

		responses++

		require.Less(t, i+1, len(evs))
		assert.Equal(t, events.UsageKind, evs[i+1].Kind, "each call's usage follows its model_response")
	}

	require.Equal(t, 2, responses, "two message ids, two calls")
	assert.LessOrEqual(t, len(usages), responses+1, "one usage per call plus at most one remainder")

	assert.Equal(t, buckets{4, 125, 3844, 3977}, sumUsage(t, evs))
	assert.Equal(t, resultBuckets(res), sumUsage(t, evs))

	for _, ev := range usages {
		assert.Equal(t, "subscription", ev.Data["cost_basis"])
		assert.NotContains(t, ev.Data, "cost_usd")
		assert.NotContains(t, ev.Data, "notional_cost_usd")
	}
}

func TestMapperLivePartial(t *testing.T) {
	evs, res, err := replay(t, "real/p4-partial-messages.jsonl", mapperOpts{Prefix: "mcp__cm__"})
	require.NoError(t, err)

	require.Equal(t, []events.Kind{events.ModelRequest, events.ModelResponse, events.UsageKind, events.StateChange}, kinds(evs))
	assert.Equal(t, buckets{2, 4, 11382, 6379}, usageOf(t, evs[2]))
	assert.Equal(t, resultBuckets(res), usageOf(t, evs[2]))
	assert.Equal(t, "end_turn", evs[1].Data["finish_reason"])
}

func TestMapperPartialMessages(t *testing.T) {
	var deltas []llm.Delta

	evs, res, err := replay(t, "synthetic/partial.jsonl", mapperOpts{
		Prefix:  cmlocal,
		OnDelta: func(d llm.Delta) { deltas = append(deltas, d) },
	})
	require.NoError(t, err)

	require.Equal(t, []events.Kind{
		events.ModelRequest, events.ModelResponse, events.UsageKind, events.ToolCallKind,
		events.ToolResult,
		events.ModelRequest, events.ModelResponse, events.UsageKind,
		events.StateChange,
	}, kinds(evs))

	usages := ofKind(evs, events.UsageKind)
	assert.Equal(t, buckets{2, 120, 0, 3844}, usageOf(t, usages[0]), "call 1 from message_delta")
	assert.Equal(t, buckets{2, 5, 3844, 133}, usageOf(t, usages[1]), "call 2 from message_delta")
	assert.Equal(t, resultBuckets(res), sumUsage(t, evs))

	responses := ofKind(evs, events.ModelResponse)
	assert.Equal(t, "tool_use", responses[0].Data["finish_reason"])
	assert.InDelta(t, 1, responses[0].Data["tool_calls"], 0)
	assert.Equal(t, "end_turn", responses[1].Data["finish_reason"])

	var text, args strings.Builder

	var toolDeltas int

	for _, d := range deltas {
		text.WriteString(d.Content)

		if d.ToolCall == nil {
			continue
		}

		toolDeltas++

		assert.Equal(t, "finish", d.ToolCall.Name)
		assert.Equal(t, "toolu_014h96jCjuHV3wJj8rGfnEn5", d.ToolCall.ID)
		assert.Equal(t, 0, d.ToolCall.Index)
		args.WriteString(d.ToolCall.ArgsFragment)
	}

	assert.Equal(t, "2+2 equals 4.Done.", text.String())
	assert.Greater(t, toolDeltas, 1, "tool input arrives in fragments")
	assert.JSONEq(t, finishInput, args.String())
}

func dropLines(data []byte, markers ...string) []byte {
	var out []byte

	for line := range bytes.Lines(data) {
		keep := true

		for _, mk := range markers {
			if bytes.Contains(line, []byte(mk)) {
				keep = false
			}
		}

		if keep {
			out = append(out, line...)
		}
	}

	return out
}

func TestMapperCallClosesOnNextCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want []events.Kind
		call buckets
	}{
		{
			"assistant lines", dropLines(readFixture(t, "real/t5.jsonl"), `"type":"user"`),
			[]events.Kind{
				events.ModelRequest, events.ModelResponse, events.UsageKind, events.ToolCallKind,
				events.ModelRequest, events.ModelResponse, events.UsageKind, events.UsageKind, events.StateChange,
			},
			buckets{2, 2, 0, 3844},
		},
		{
			"stream events", dropLines(readFixture(t, "synthetic/partial.jsonl"), `"type":"user"`, `"type":"message_stop"`),
			[]events.Kind{
				events.ModelRequest, events.ModelResponse, events.UsageKind, events.ToolCallKind,
				events.ModelRequest, events.ModelResponse, events.UsageKind, events.StateChange,
			},
			buckets{2, 120, 0, 3844},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs, res, err := replayData(t, tc.data, mapperOpts{Prefix: cmlocal}, nil)
			require.NoError(t, err)

			require.Equal(t, tc.want, kinds(evs))
			assert.Equal(t, tc.call, usageOf(t, ofKind(evs, events.UsageKind)[0]))
			assert.Equal(t, resultBuckets(res), sumUsage(t, evs))
		})
	}
}

// dropResultKey removes key from the result line, as an older release would
// omit it.
func dropResultKey(t *testing.T, data []byte, key string) []byte {
	t.Helper()

	var out []byte

	for line := range bytes.Lines(data) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err == nil && string(fields["type"]) == `"result"` {
			require.Contains(t, fields, key)
			delete(fields, key)

			b, err := json.Marshal(fields)
			require.NoError(t, err)

			out = append(out, b...)
			out = append(out, '\n')

			continue
		}

		out = append(out, line...)
	}

	return out
}

func TestMapperResumedRunCountsOnlyThisProcess(t *testing.T) {
	// run-b resumed run-a's session from a fresh config dir: its modelUsage and
	// total_cost_usd are cumulative over both processes.
	for _, path := range []string{"probe/p6/run-a.jsonl", "probe/p6/run-b.jsonl"} {
		t.Run(path, func(t *testing.T) {
			evs, res, err := replay(t, path, mapperOpts{Prefix: "mcp__cm__"})
			require.NoError(t, err)

			assert.Equal(t, buckets{20, 10, 0, 0}, resultBuckets(res), "this process's result.usage")
			assert.Equal(t, resultBuckets(res), sumUsage(t, evs))
			assert.Len(t, ofKind(evs, events.UsageKind), len(ofKind(evs, events.ModelResponse)), "no remainder event")
			assert.InDelta(t, 7e-06, res.TotalCostUSD, 1e-12, "this process's model_usage_since_last_result cost")
		})
	}

	t.Run("fallback without model_usage_since_last_result", func(t *testing.T) {
		data := dropResultKey(t, readFixture(t, "probe/p6/run-b.jsonl"), "model_usage_since_last_result")

		evs, res, err := replayData(t, data, mapperOpts{Prefix: "mcp__cm__"}, nil)
		require.NoError(t, err)

		assert.Equal(t, buckets{40, 20, 0, 0}, sumUsage(t, evs), "remainder from modelUsage")
		assert.Equal(t, sumUsage(t, evs), resultBuckets(res), "tokens from the same source as the cost")
		assert.InDelta(t, 1.4e-05, res.TotalCostUSD, 1e-12, "total_cost_usd")
	})

	t.Run("fallback without per-model usage", func(t *testing.T) {
		data := readFixture(t, "real/t1.jsonl")
		data = dropResultKey(t, dropResultKey(t, data, "model_usage_since_last_result"), "modelUsage")

		evs, res, err := replayData(t, data, mapperOpts{Prefix: "mcp__cm__"}, nil)
		require.NoError(t, err)

		assert.Equal(t, buckets{2, 4, 11382, 6379}, resultBuckets(res), "result.usage")
		assert.Equal(t, resultBuckets(res), sumUsage(t, evs))
	})
}

func TestMapperRemainderPerModel(t *testing.T) {
	evs, res, err := replay(t, "synthetic/two_models.jsonl", mapperOpts{Prefix: cmlocal})
	require.NoError(t, err)

	var haiku []events.Event

	var sonnet buckets

	for _, ev := range ofKind(evs, events.UsageKind) {
		switch ev.Data["model"] {
		case "claude-haiku-5-5":
			haiku = append(haiku, ev)
		case "claude-sonnet-5-5":
			sonnet = sonnet.add(usageOf(t, ev))
		default:
			t.Errorf("usage event for an unexpected model: %v", ev.Data)
		}
	}

	require.Len(t, haiku, 1)
	assert.Equal(t, buckets{350, 12, 0, 0}, usageOf(t, haiku[0]))
	assert.Equal(t, buckets{4, 125, 3844, 3977}, sonnet)
	assert.InDelta(t, 0.0175914, res.TotalCostUSD, 1e-12)

	// result.usage (4/125/3844/3977) leaves out the haiku call; the per-model
	// sums, the cost's source, include it.
	assert.Equal(t, buckets{354, 137, 3844, 3977}, resultBuckets(res))
	assert.Equal(t, resultBuckets(res), sumUsage(t, evs))
	assert.Equal(t, "claude-sonnet-5-5", res.ModelUsed)
}

func TestMapperToolEvents(t *testing.T) {
	evs, res, err := replay(t, "real/t5.jsonl", mapperOpts{Prefix: cmlocal})
	require.NoError(t, err)

	// Without stream events the tool_result line closes the call; the result
	// adds the remainder of the early output_tokens snapshots.
	require.Equal(t, []events.Kind{
		events.ModelRequest, events.ModelResponse, events.UsageKind, events.ToolCallKind,
		events.ToolResult,
		events.ModelRequest, events.ModelResponse, events.UsageKind,
		events.UsageKind, events.StateChange,
	}, kinds(evs))

	calls := ofKind(evs, events.ToolCallKind)
	require.Len(t, calls, 1)
	assert.Equal(t, "toolu_014h96jCjuHV3wJj8rGfnEn5", calls[0].Data["id"])
	assert.Equal(t, "finish", calls[0].Data["name"])

	rawArgs, ok := calls[0].Data["raw_args"].(string)
	require.True(t, ok)
	assert.JSONEq(t, finishInput, rawArgs)

	results := ofKind(evs, events.ToolResult)
	require.Len(t, results, 1)
	assert.Equal(t, map[string]any{"id": "toolu_014h96jCjuHV3wJj8rGfnEn5", "output_len": float64(len(`{"accepted":true}`))}, results[0].Data)

	assert.Equal(t, 1, res.ToolCallCount)
	assert.Equal(t, 0, res.ToolCallFailures)
}

func TestMapperToolErrorEvent(t *testing.T) {
	evs, res, err := replay(t, "real/p3.jsonl", mapperOpts{Prefix: cmlocal})
	require.NoError(t, err)

	results := ofKind(evs, events.ToolResult)
	require.Len(t, results, 2)

	assert.Equal(t, "toolu_0", results[0].Data["id"])
	assert.Equal(t, `validating "arguments": validating root: required: missing properties: ["commit_message" "summary"]`, results[0].Data["error"])
	assert.NotContains(t, results[0].Data, "output_len")

	assert.Equal(t, "toolu_1", results[1].Data["id"])
	assert.Contains(t, results[1].Data, "output_len")
	assert.NotContains(t, results[1].Data, "error")

	assert.Equal(t, 2, res.ToolCallCount)
	assert.Equal(t, 1, res.ToolCallFailures)
	assert.Equal(t, "done", res.Reason)
}

func TestMapperIgnoresUnknownLines(t *testing.T) {
	want, wantRes, wantErr := replay(t, "real/t1.jsonl", mapperOpts{Prefix: "mcp__cm__"})
	got, gotRes, gotErr := replay(t, "synthetic/unknown.jsonl", mapperOpts{Prefix: "mcp__cm__"})

	require.NoError(t, wantErr)
	require.NoError(t, gotErr)
	assert.Equal(t, stripVolatile(want), stripVolatile(got))
	assert.Equal(t, wantRes, gotRes)
}

func TestMapperIgnoresUndecodableLines(t *testing.T) {
	data := readFixture(t, "real/t1.jsonl")
	noise := []byte("not json\n\"a string\"\n[1,2]\n{\"type\":\"assistant\",\"message\":\"text\"}\n" +
		`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"y"}]}}` + "\n")

	want, wantRes, wantErr := replayData(t, data, mapperOpts{}, nil)
	got, gotRes, gotErr := replayData(t, append(noise, data...), mapperOpts{}, nil)

	require.NoError(t, wantErr)
	require.NoError(t, gotErr)
	assert.Equal(t, stripVolatile(want), stripVolatile(got))
	assert.Equal(t, wantRes, gotRes)
}

func TestMapperKeepsForeignServerNames(t *testing.T) {
	const foreign = "mcp__contextmatrix__list_cards"

	for _, path := range []string{"real/t5.jsonl", "synthetic/partial.jsonl"} {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				data []byte
				want string
			}{
				{"bridged", readFixture(t, path), "finish"},
				{"foreign", rewrite(readFixture(t, path), "mcp__cmlocal__finish", foreign), foreign},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var deltaNames []string

					evs, res, err := replayData(t, tc.data, mapperOpts{
						Prefix: cmlocal,
						OnDelta: func(d llm.Delta) {
							if d.ToolCall != nil {
								deltaNames = append(deltaNames, d.ToolCall.Name)
							}
						},
					}, nil)
					require.NoError(t, err)

					calls := ofKind(evs, events.ToolCallKind)
					require.Len(t, calls, 1)
					assert.Equal(t, tc.want, calls[0].Data["name"])

					var stored []string

					for _, msg := range res.Messages {
						for _, c := range msg.ToolCalls {
							stored = append(stored, c.Function.Name)
						}
					}

					assert.Equal(t, []string{tc.want}, stored)

					for _, n := range deltaNames {
						assert.Equal(t, tc.want, n)
					}
				})
			}
		})
	}
}

func TestMapperMasksSecret(t *testing.T) {
	const secret = "tok-SECRET-0123456789"

	redact := func(s string) string { return strings.ReplaceAll(s, "chore", "[r]") }

	t.Run("run", func(t *testing.T) {
		// Text, tool input, result, tool id and model name each carry the secret.
		data := rewrite(readFixture(t, "synthetic/partial.jsonl"),
			"equals", secret, "Done.", secret, "toolu_014h", "toolu_"+secret, "sonnet", secret)

		var deltas []llm.Delta

		// The terminal call is accepted at the end, so no new call trips the backstop.
		accepted := false

		evs, res, err := replayHook(t, data, mapperOpts{
			Prefix:  cmlocal,
			Secret:  secret,
			Redact:  redact,
			OnDelta: func(d llm.Delta) { deltas = append(deltas, d) },
			Terminal: func() (string, json.RawMessage, bool) {
				return "finish", json.RawMessage(`{"summary":"` + secret + `"}`), accepted
			},
		}, nil, func(line []byte) { accepted = accepted || bytes.Contains(line, []byte(`"type":"result"`)) })
		require.NoError(t, err)

		transcript, err := json.Marshal(evs)
		require.NoError(t, err)
		assert.NotContains(t, string(transcript), secret)
		assert.NotContains(t, string(transcript), "chore", "Redact applies to events")

		assert.NotEmpty(t, res.Output)
		assert.NotContains(t, res.Output, secret)
		assert.NotEmpty(t, res.ModelUsed)
		assert.NotContains(t, res.ModelUsed, secret)
		require.NotNil(t, res.CompletionArgs)
		assert.NotContains(t, string(res.CompletionArgs), secret)
		assert.True(t, json.Valid(res.CompletionArgs))

		var fragments strings.Builder

		for _, d := range deltas {
			fragments.WriteString(d.Content)
			fragments.WriteString(d.Reasoning)

			if d.ToolCall != nil {
				fragments.WriteString(d.ToolCall.ID + d.ToolCall.Name + d.ToolCall.ArgsFragment)
			}
		}

		assert.NotContains(t, fragments.String(), secret)
		assert.Contains(t, fragments.String(), "chore", "Redact does not apply to fragments")
	})

	t.Run("error", func(t *testing.T) {
		data := rewrite(readFixture(t, "real/t2.jsonl"), "Not logged in", secret)

		evs, res, err := replayData(t, data, mapperOpts{Secret: secret}, errors.New("exit status 1 "+secret))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secret)
		assert.NotContains(t, res.Output, secret)

		transcript, jerr := json.Marshal(evs)
		require.NoError(t, jerr)
		assert.NotContains(t, string(transcript), secret)
	})

	t.Run("process error", func(t *testing.T) {
		procErr := errors.New("exit status 1: " + secret)

		_, _, err := replayData(t, readFixture(t, "synthetic/crash.jsonl"), mapperOpts{Prefix: cmlocal, Secret: secret}, procErr)
		require.Error(t, err)
		require.ErrorIs(t, err, procErr)
		assert.NotContains(t, err.Error(), secret)
	})
}

func TestMapperCapsReasons(t *testing.T) {
	maxTurns := readFixture(t, "synthetic/max_turns.jsonl")

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"max turns", maxTurns, "max_turns"},
		{"max budget", rewrite(maxTurns, `"subtype":"error_max_turns"`, `"subtype":"error_max_budget_usd"`), "max_cost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 2.1.322 exits 1 after an error_max_turns result.
			evs, res, err := replayData(t, tc.data, mapperOpts{}, errors.New("exit status 1"))
			require.NoError(t, err)

			assert.False(t, res.Completed)
			assert.Equal(t, tc.want, res.Reason)
			assert.Empty(t, ofKind(evs, events.ErrorKind))

			st := stops(evs)
			require.Len(t, st, 1)
			assert.Equal(t, tc.want, st[0].Data["stop"])
		})
	}
}

func TestMapperUnknownTerminalReason(t *testing.T) {
	t1 := readFixture(t, "real/t1.jsonl")

	want, wantRes, wantErr := replayData(t, t1, mapperOpts{}, nil)
	require.NoError(t, wantErr)

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"absent", rewrite(t1, `"terminal_reason":"completed",`, "")},
		{"unknown", rewrite(t1, `"terminal_reason":"completed"`, `"terminal_reason":"x_future_reason"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEqual(t, t1, tc.data)

			got, gotRes, err := replayData(t, tc.data, mapperOpts{}, nil)
			require.NoError(t, err)
			assert.Equal(t, stripVolatile(want), stripVolatile(got))
			assert.Equal(t, wantRes, gotRes)
		})
	}
}

func TestMapperResultDecidesOverExitError(t *testing.T) {
	_, res, err := replayData(t, readFixture(t, "real/t1.jsonl"), mapperOpts{}, errors.New("exit status 1"))
	require.NoError(t, err)
	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
}

func TestMapperErrorResult(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		calls int
	}{
		// The auth failure's assistant message is <synthetic>: no API call.
		{"auth failure", readFixture(t, "real/t2.jsonl"), 0},
		{"is_error with completed", rewrite(readFixture(t, "real/t1.jsonl"), `"is_error":false`, `"is_error":true`), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs, res, err := replayData(t, tc.data, mapperOpts{}, nil)
			require.Error(t, err)

			assert.Len(t, ofKind(evs, events.ModelRequest), tc.calls)
			assert.Len(t, ofKind(evs, events.UsageKind), tc.calls)

			assert.False(t, res.Completed, "is_error wins over subtype success")
			assert.Equal(t, "error", res.Reason)

			errs := ofKind(evs, events.ErrorKind)
			require.Len(t, errs, 1)
			assert.NotEmpty(t, errs[0].Data["error"])

			require.GreaterOrEqual(t, len(evs), 2)
			assert.Equal(t, events.ErrorKind, evs[len(evs)-2].Kind, "error comes right before the stop")
			assert.Equal(t, map[string]any{"stop": "error", "turns": 1.0}, evs[len(evs)-1].Data)
		})
	}
}

func TestMapperNoResult(t *testing.T) {
	t.Run("process error", func(t *testing.T) {
		procErr := errors.New("signal: killed")

		evs, res, err := replayData(t, readFixture(t, "synthetic/crash.jsonl"), mapperOpts{Prefix: cmlocal}, procErr)
		require.ErrorIs(t, err, procErr)

		assert.False(t, res.Completed)
		assert.Equal(t, "error", res.Reason)
		assert.Equal(t, 2, res.Turns)
		assert.Equal(t, "claude-sonnet-5-5", res.ModelUsed)
		assert.Equal(t, 1, res.ToolCallCount)
		assert.Equal(t, sumUsage(t, evs), resultBuckets(res), "the result carries the usage streamed so far")
		assert.Equal(t, buckets{4, 7, 3844, 3977}, resultBuckets(res))

		require.Len(t, ofKind(evs, events.ModelResponse), 2, "the open call closes at finish")
		require.Len(t, ofKind(evs, events.ErrorKind), 1)
		assert.Equal(t, events.ErrorKind, evs[len(evs)-2].Kind)
		assert.Equal(t, map[string]any{"stop": "error", "turns": 2.0}, evs[len(evs)-1].Data)
	})

	t.Run("clean exit", func(t *testing.T) {
		_, res, err := replay(t, "synthetic/crash.jsonl", mapperOpts{Prefix: cmlocal})
		require.Error(t, err)
		assert.Equal(t, "error", res.Reason)
	})

	t.Run("canceled", func(t *testing.T) {
		cause := errors.New("cost cap")
		procErr := fmt.Errorf("%w: %w", context.Canceled, cause)

		evs, res, err := replayData(t, readFixture(t, "synthetic/crash.jsonl"), mapperOpts{Prefix: cmlocal}, procErr)
		require.ErrorIs(t, err, cause)

		assert.False(t, res.Completed)
		assert.Equal(t, "canceled", res.Reason)
		assert.Empty(t, ofKind(evs, events.ErrorKind))

		st := stops(evs)
		require.Len(t, st, 1)
		assert.Equal(t, "canceled", st[0].Data["stop"])
	})
}

func TestMapperOneStopPerRun(t *testing.T) {
	fixtures := []string{
		"real/t1.jsonl", "real/t2.jsonl", "real/t5.jsonl", "real/p3.jsonl", "real/p4-partial-messages.jsonl",
		"synthetic/partial.jsonl", "synthetic/two_models.jsonl", "synthetic/max_turns.jsonl",
		"synthetic/unknown.jsonl", "synthetic/crash.jsonl",
	}

	for _, path := range fixtures {
		t.Run(path, func(t *testing.T) {
			var transcript bytes.Buffer

			m := newMapper(mapperOpts{Prefix: cmlocal, Emitter: events.NewEmitter(nil, &transcript)})
			for line := range bytes.Lines(readFixture(t, path)) {
				m.feed(bytes.TrimSpace(line))
			}

			res1, err1 := m.finish(errors.New("exit status 1"))
			res2, err2 := m.finish(errors.New("exit status 2"))

			evs := decodeEvents(t, transcript.Bytes())
			st := stops(evs)
			require.Len(t, st, 1)
			assert.Equal(t, st[0], evs[len(evs)-1], "the stop is the last event")
			assert.Equal(t, res1.Reason, st[0].Data["stop"])
			assert.LessOrEqual(t, len(ofKind(evs, events.ErrorKind)), 1)

			assert.Equal(t, res1, res2, "finish is idempotent")
			assert.Equal(t, err1, err2)
		})
	}
}

func TestMapperStopsFeedingAfterRunResult(t *testing.T) {
	var transcript bytes.Buffer

	m := newMapper(mapperOpts{Emitter: events.NewEmitter(nil, &transcript)})

	lines := bytes.Split(bytes.TrimSpace(readFixture(t, "real/t1.jsonl")), []byte("\n"))
	require.Len(t, lines, 4)

	var verdicts []verdict
	for _, line := range lines {
		verdicts = append(verdicts, m.feed(line))
	}

	assert.Equal(t, []verdict{keepGoing, keepGoing, keepGoing, stopNow}, verdicts, "a Run is over at its result")

	before := transcript.Len()

	assert.Equal(t, stopNow, m.feed(lines[1]))
	assert.Equal(t, before, transcript.Len(), "nothing is emitted after the result")
}

func TestMapperSessionTurnEndsAwaiting(t *testing.T) {
	evs, res, err := replay(t, "real/t1.jsonl", mapperOpts{Session: true})
	require.NoError(t, err)

	states := ofKind(evs, events.StateChange)
	require.Len(t, states, 1)
	assert.Equal(t, map[string]any{"state": "awaiting_human", "turns": 1.0}, states[0].Data)
	assert.Empty(t, stops(evs))
	assert.Equal(t, "done", res.Reason)
}

func TestMapperSessionTurnAccounting(t *testing.T) {
	data := readFixture(t, "real/t7b.jsonl")
	lines := bytes.SplitAfter(data, []byte("\n"))

	// The second turn starts after the first result line.
	var second []byte

	for i, line := range lines {
		if bytes.Contains(line, []byte(`"type":"result"`)) {
			second = bytes.Join(lines[i+1:], nil)

			break
		}
	}

	require.NotEmpty(t, second)

	evs, res, err := replayData(t, second, mapperOpts{Session: true}, nil)
	require.NoError(t, err)

	assert.Equal(t, "7", res.Output)
	assert.Equal(t, buckets{2, 3, 14247, 3572}, resultBuckets(res), "per-turn result.usage")
	assert.InDelta(t, 0.00085857, res.TotalCostUSD, 1e-12, "the turn's cost, not the cumulative total")
	assert.Equal(t, resultBuckets(res), sumUsage(t, evs), "remainder from model_usage_since_last_result")
	assert.Empty(t, stops(evs))
}

func TestMapperCompactBoundary(t *testing.T) {
	data := readFixture(t, "real/t5.jsonl")
	compact := []byte(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":1234},"session_id":"s"}` + "\n")
	lines := bytes.SplitAfter(data, []byte("\n"))
	withCompact := bytes.Join([][]byte{bytes.Join(lines[:5], nil), compact, bytes.Join(lines[5:], nil)}, nil)

	evs, res, err := replayData(t, withCompact, mapperOpts{Prefix: cmlocal}, nil)
	require.NoError(t, err)
	assert.Equal(t, "done", res.Reason)

	var compactions []events.Event

	for _, ev := range ofKind(evs, events.StateChange) {
		if ev.Data["event"] == "compaction" {
			compactions = append(compactions, ev)
		}
	}

	require.Len(t, compactions, 1)
	assert.Equal(t, map[string]any{"event": "compaction"}, compactions[0].Data)
	assert.Len(t, stops(evs), 1)
}

func TestMapperRateLimits(t *testing.T) {
	resetsAt := time.Unix(1794510000, 0).UTC()

	t.Run("fields missing", func(t *testing.T) {
		var got []RateLimit

		evs, _, err := replay(t, "probe/p6/run-a.jsonl", mapperOpts{
			Prefix:      "mcp__cm__",
			OnRateLimit: func(r RateLimit) { got = append(got, r) },
		})
		require.NoError(t, err)

		require.Equal(t, []RateLimit{{Status: "allowed"}}, got)

		usages := ofKind(evs, events.UsageKind)
		require.NotEmpty(t, usages)

		for _, ev := range usages {
			assert.NotContains(t, ev.Data, "subscription")
		}
	})

	t.Run("windowless event keeps the last windows", func(t *testing.T) {
		lines := bytes.SplitAfter(readFixture(t, "real/t1.jsonl"), []byte("\n"))
		windowless := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","isUsingOverage":false}}` + "\n")
		data := bytes.Join([][]byte{bytes.Join(lines[:3], nil), windowless, bytes.Join(lines[3:], nil)}, nil)

		evs, _, err := replayData(t, data, mapperOpts{}, nil)
		require.NoError(t, err)

		usages := ofKind(evs, events.UsageKind)
		require.Len(t, usages, 1)
		assert.Contains(t, usages[0].Data, "subscription")
	})

	t.Run("window without reset dropped", func(t *testing.T) {
		data := rewrite(readFixture(t, "real/t1.jsonl"), `"unifiedWindows":{`,
			`"unifiedWindows":{"five_hour":{"utilization":0.5,"status":"allowed"},"a_day":{"utilization":0.2,"resetsAt":1794510000,"status":"allowed_warning"},`)

		var got []RateLimit

		evs, _, err := replayData(t, data, mapperOpts{OnRateLimit: func(r RateLimit) { got = append(got, r) }}, nil)
		require.NoError(t, err)

		require.Len(t, got, 1)
		assert.Equal(t, "allowed", got[0].Status)
		assert.Equal(t, "seven_day", got[0].Window)
		assert.True(t, resetsAt.Equal(got[0].ResetsAt))

		names := make([]string, 0, len(got[0].Windows))
		for _, w := range got[0].Windows {
			names = append(names, w.Window)
		}

		assert.Equal(t, []string{"a_day", "seven_day"}, names)

		usages := ofKind(evs, events.UsageKind)
		require.Len(t, usages, 1)

		sub, ok := usages[0].Data["subscription"].([]any)
		require.True(t, ok, "usage after a rate_limit_event carries subscription")
		require.Len(t, sub, 2)

		w, ok := sub[1].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "seven_day", w["window"])
		assert.InDelta(t, 0.13, w["utilization"], 1e-9)
		assert.Equal(t, "allowed", w["status"])
		assert.Equal(t, resetsAt.Format(time.RFC3339), w["resets_at"])
	})
}

// feedUntilStop feeds lines the way the supervisor does: it stops at the first
// stopNow. before runs ahead of each line.
func feedUntilStop(t *testing.T, data []byte, o mapperOpts, procErr error, before func(line []byte)) (stopped bool, evs []events.Event, res harness.Result, err error) {
	t.Helper()

	var transcript bytes.Buffer

	o.Emitter = events.NewEmitter(nil, &transcript)
	m := newMapper(o)

	for line := range bytes.Lines(data) {
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}

		if before != nil {
			before(line)
		}

		if m.feed(line) == stopNow {
			stopped = true

			break
		}
	}

	res, err = m.finish(procErr)

	return stopped, decodeEvents(t, transcript.Bytes()), res, err
}

func TestMapperRateLimitEvent(t *testing.T) {
	resetsAt := time.Unix(1794510000, 0).UTC()

	var got []RateLimit

	evs, _, err := replay(t, "real/t1.jsonl", mapperOpts{OnRateLimit: func(r RateLimit) { got = append(got, r) }})
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, "allowed", got[0].Status)
	assert.Equal(t, "seven_day", got[0].Window)
	assert.True(t, resetsAt.Equal(got[0].ResetsAt))
	require.Len(t, got[0].Windows, 1)
	assert.Equal(t, "seven_day", got[0].Windows[0].Window)
	assert.InDelta(t, 0.13, got[0].Windows[0].Utilization, 1e-9)
	assert.Equal(t, "allowed", got[0].Windows[0].Status)
	assert.True(t, resetsAt.Equal(got[0].Windows[0].ResetsAt))

	usages := ofKind(evs, events.UsageKind)
	require.Len(t, usages, 1)

	sub, ok := usages[0].Data["subscription"].([]any)
	require.True(t, ok)
	require.Len(t, sub, 1)

	w, ok := sub[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, resetsAt.Format(time.RFC3339), w["resets_at"])
}

func TestMapperUsageLimit(t *testing.T) {
	resetsAt := time.Unix(1794510000, 0).UTC()

	stopped, evs, res, err := feedUntilStop(t, readFixture(t, "synthetic/rejected.jsonl"), mapperOpts{},
		errors.New("signal: terminated"), nil)
	require.True(t, stopped, "the rejected line stops the process at once")

	var ule *UsageLimitError

	require.ErrorAs(t, err, &ule)
	assert.Equal(t, "seven_day", ule.Window)
	assert.True(t, resetsAt.Equal(ule.ResetsAt))

	assert.False(t, res.Completed)
	assert.Equal(t, "error", res.Reason)
	assert.Equal(t, buckets{2, 4, 11382, 6379}, resultBuckets(res), "usage streamed so far")

	limits := ofKind(evs, events.Kind("usage_limit"))
	require.Len(t, limits, 1)
	assert.Equal(t, map[string]any{"window": "seven_day", "resets_at": resetsAt.Format(time.RFC3339)}, limits[0].Data)

	assert.Empty(t, ofKind(evs, events.ErrorKind), "the usage_limit event is the report")

	last := evs[len(evs)-1]
	assert.Equal(t, events.StateChange, last.Kind)
	assert.Equal(t, "error", last.Data["stop"])
	assert.Equal(t, events.Kind("usage_limit"), evs[len(evs)-2].Kind)
	assert.Len(t, stops(evs), 1)
}

func TestMapperUsageLimitUnknownReset(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}` + "\n"

	stopped, evs, _, err := feedUntilStop(t, []byte(line), mapperOpts{}, nil, nil)
	require.True(t, stopped)

	var ule *UsageLimitError

	require.ErrorAs(t, err, &ule)
	assert.True(t, ule.ResetsAt.IsZero())

	limits := ofKind(evs, events.Kind("usage_limit"))
	require.Len(t, limits, 1)
	assert.NotContains(t, limits[0].Data, "resets_at")
}

func TestMapperUsageLimitFromResult(t *testing.T) {
	data := `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"Claude AI usage limit reached","num_turns":1,"usage":{}}` + "\n"

	evs, res, err := replayData(t, []byte(data), mapperOpts{}, nil)

	var ule *UsageLimitError

	require.ErrorAs(t, err, &ule)
	assert.Equal(t, "error", res.Reason)
	require.Len(t, ofKind(evs, events.Kind("usage_limit")), 1)
	assert.Empty(t, ofKind(evs, events.ErrorKind))

	// A Session turn that hit the limit does not await the human; the
	// session's stop follows.
	evs, _, err = replayData(t, []byte(data), mapperOpts{Session: true}, nil)
	require.ErrorAs(t, err, &ule)
	assert.Empty(t, ofKind(evs, events.StateChange))
}

func TestMapperNotLoggedIn(t *testing.T) {
	evs, res, err := replay(t, "real/t2.jsonl", mapperOpts{})
	require.ErrorIs(t, err, ErrNotLoggedIn)

	assert.False(t, res.Completed, "subtype success must not count")
	assert.Equal(t, "error", res.Reason)
	require.Len(t, ofKind(evs, events.ErrorKind), 1)

	t.Run("process dies after the assistant line", func(t *testing.T) {
		lines := bytes.SplitAfter(readFixture(t, "real/t2.jsonl"), []byte("\n"))
		data := bytes.Join(lines[:3], nil)

		_, res, err := replayData(t, data, mapperOpts{}, errors.New("exit status 1"))
		require.ErrorIs(t, err, ErrNotLoggedIn)
		assert.Equal(t, "error", res.Reason)
	})

	t.Run("no version annotation", func(t *testing.T) {
		data := rewrite(readFixture(t, "real/t2.jsonl"), `"claude_code_version":"2.1.322"`, `"claude_code_version":"2.1.400"`)

		_, _, err := replayData(t, data, mapperOpts{}, nil)
		require.ErrorIs(t, err, ErrNotLoggedIn)
		assert.NotContains(t, err.Error(), "2.1.400")
	})
}

func TestMapperUntestedVersionProceeds(t *testing.T) {
	newer := func(data []byte) []byte {
		return rewrite(data, `"claude_code_version":"2.1.322"`, `"claude_code_version":"2.1.400"`)
	}

	baseEvs, baseRes, baseErr := replay(t, "real/t1.jsonl", mapperOpts{})
	require.NoError(t, baseErr)

	evs, res, err := replayData(t, newer(readFixture(t, "real/t1.jsonl")), mapperOpts{}, nil)
	require.NoError(t, err)
	assert.Equal(t, kinds(baseEvs), kinds(evs))

	for i := range evs {
		assert.Equal(t, baseEvs[i].Data, evs[i].Data)
	}

	assert.Equal(t, baseRes, res)

	_, _, err = replayData(t, newer(readFixture(t, "synthetic/crash.jsonl")), mapperOpts{Prefix: cmlocal}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2.1.400")
	assert.Contains(t, err.Error(), Version)

	_, _, err = replay(t, "synthetic/crash.jsonl", mapperOpts{Prefix: cmlocal})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "tested", "a matching version adds nothing")

	t.Run("not on cancellation", func(t *testing.T) {
		procErr := fmt.Errorf("%w: stop", context.Canceled)

		_, res, err := replayData(t, newer(readFixture(t, "synthetic/crash.jsonl")), mapperOpts{Prefix: cmlocal}, procErr)
		require.Error(t, err)
		assert.Equal(t, "canceled", res.Reason)
		assert.NotContains(t, err.Error(), "2.1.400")
	})
}

func TestMapperBridgeFailed(t *testing.T) {
	data := rewrite(readFixture(t, "real/t1.jsonl"), `"mcp_servers":[]`, `"mcp_servers":[{"name":"cm","status":"failed"},{"name":"other","status":"connected"}]`)

	stopped, evs, res, err := feedUntilStop(t, data, mapperOpts{Prefix: "mcp__cm__"}, errors.New("signal: killed"), nil)
	require.True(t, stopped)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"cm"`)
	assert.Equal(t, "error", res.Reason)
	assert.Empty(t, ofKind(evs, events.ModelRequest), "nothing after init is processed")
	require.Len(t, ofKind(evs, events.ErrorKind), 1)

	t.Run("other servers do not stop the run", func(t *testing.T) {
		data := rewrite(readFixture(t, "real/t1.jsonl"), `"mcp_servers":[]`, `"mcp_servers":[{"name":"other","status":"failed"}]`)

		stopped, _, _, err := feedUntilStop(t, data, mapperOpts{Prefix: "mcp__cm__"}, nil, nil)
		require.NoError(t, err)
		assert.True(t, stopped, "only the result stops a Run")
	})
}

func TestMapperInitCaptures(t *testing.T) {
	m := newMapper(mapperOpts{})
	assert.Empty(t, m.sessionID())

	line := bytes.Lines(readFixture(t, "real/t1.jsonl"))
	for l := range line {
		m.feed(bytes.TrimSpace(l))

		break
	}

	assert.Equal(t, "e7d93133-865e-4f91-b7e2-348d66e79b59", m.sessionID())
}

func TestMapperBackstopAfterTerminal(t *testing.T) {
	args := json.RawMessage(finishInput)

	for _, tc := range []struct{ name, fixture string }{
		{"stream events", "synthetic/partial.jsonl"},
		{"assistant lines only", "real/t5.jsonl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepted := false

			stopped, evs, res, err := feedUntilStop(t, readFixture(t, tc.fixture), mapperOpts{
				Prefix: cmlocal,
				Terminal: func() (string, json.RawMessage, bool) {
					return "finish", args, accepted
				},
			}, errors.New("signal: killed"), func(line []byte) {
				accepted = accepted || bytes.Contains(line, []byte(`"tool_result"`))
			})
			require.True(t, stopped, "the call after the terminal one stops the process")
			require.NoError(t, err, "vendorkit stopped the process itself")

			assert.True(t, res.Completed)
			assert.Equal(t, "done", res.Reason)
			assert.JSONEq(t, finishInput, string(res.CompletionArgs))
			assert.Equal(t, 1, res.Turns)
			assert.Empty(t, ofKind(evs, events.ErrorKind))

			st := stops(evs)
			require.Len(t, st, 1)
			assert.Equal(t, "done", st[0].Data["stop"])
		})
	}
}

// TestMapperBackstopOrdersByStream: the bridge accepts the terminal call
// before the stream shows it, so the call itself must not trip the backstop.
func TestMapperBackstopOrdersByStream(t *testing.T) {
	args := json.RawMessage(finishInput)

	for _, tc := range []struct{ name, fixture string }{
		{"stream events", "synthetic/partial.jsonl"},
		{"assistant lines only", "real/t5.jsonl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopped, evs, res, err := feedUntilStop(t, readFixture(t, tc.fixture), mapperOpts{
				Prefix:   cmlocal,
				Task:     "q",
				Terminal: func() (string, json.RawMessage, bool) { return "finish", args, true },
			}, errors.New("signal: killed"), nil)
			require.True(t, stopped, "the call after the terminal one stops the process")
			require.NoError(t, err)

			assert.Equal(t, "done", res.Reason)
			assert.Equal(t, 1, res.Turns)
			assert.Equal(t, []string{"user", "assistant", "tool"}, roles(res.Messages))
			assert.Len(t, ofKind(evs, events.ToolCallKind), 1)
			assert.Len(t, ofKind(evs, events.ToolResult), 1)
		})
	}
}
