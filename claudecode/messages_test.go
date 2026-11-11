package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func roles(msgs []llm.Message) []string {
	var r []string
	for _, m := range msgs {
		r = append(r, m.Role)
	}

	return r
}

// requireValidTranscript checks each assistant tool call has exactly one
// tool message after it and each tool message answers an earlier call.
func requireValidTranscript(t *testing.T, msgs []llm.Message) {
	t.Helper()

	open := map[string]int{}

	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				open[tc.ID]++
			}
		case "tool":
			require.Equal(t, 1, open[m.ToolCallID], "tool message %q answers no open call", m.ToolCallID)
			open[m.ToolCallID]--
		}
	}

	for id, n := range open {
		require.Zero(t, n, "call %q unanswered", id)
	}
}

func TestMessagesFromRun(t *testing.T) {
	_, res, err := replay(t, "real/t5.jsonl", mapperOpts{Prefix: cmlocal, SystemPrompt: "sys", Task: "q"})
	require.NoError(t, err)

	require.Equal(t, []string{"system", "user", "assistant", "tool", "assistant"}, roles(res.Messages))
	requireValidTranscript(t, res.Messages)
	assert.Equal(t, "sys", res.Messages[0].Content)
	assert.Equal(t, "q", res.Messages[1].Content)

	a := res.Messages[2]
	assert.Equal(t, "2+2 equals 4.", a.Content)
	require.Len(t, a.ToolCalls, 1)
	tc := a.ToolCalls[0]
	assert.Equal(t, "toolu_014h96jCjuHV3wJj8rGfnEn5", tc.ID)
	assert.Equal(t, "function", tc.Type)
	assert.Equal(t, "finish", tc.Function.Name)
	assert.JSONEq(t, finishInput, tc.Function.Arguments)

	tool := res.Messages[3]
	assert.Equal(t, tc.ID, tool.ToolCallID)
	assert.Equal(t, `{"accepted":true}`, tool.Content)
}

func TestMessagesAppendOnly(t *testing.T) {
	_, res, err := replay(t, "real/t5.jsonl", mapperOpts{Prefix: cmlocal, Task: "q"})
	require.NoError(t, err)

	assert.NotContains(t, roles(res.Messages), "system")
	assert.Equal(t, "user", res.Messages[0].Role)
}

func TestMessagesSeedImages(t *testing.T) {
	img := llm.ImageURL{URL: "data:image/png;base64,AAAA"}
	_, res, err := replay(t, "real/t5.jsonl", mapperOpts{Prefix: cmlocal, Task: "q", TaskImages: []llm.ImageURL{img}})
	require.NoError(t, err)

	u := res.Messages[0]
	assert.Empty(t, u.Content)
	require.Len(t, u.ContentParts, 2)
	assert.Equal(t, "text", u.ContentParts[0].Type)
	assert.Equal(t, "q", u.ContentParts[0].Text)
	assert.Equal(t, "image_url", u.ContentParts[1].Type)
	assert.Equal(t, img, *u.ContentParts[1].ImageURL)
}

func TestMessagesToolResultBlocks(t *testing.T) {
	data := rewrite(readFixture(t, "real/t5.jsonl"),
		`"content":"{\"accepted\":true}"}]`,
		`"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}]`)
	require.NotEqual(t, readFixture(t, "real/t5.jsonl"), data)
	_, res, err := replayData(t, data, mapperOpts{Prefix: cmlocal}, nil)
	require.NoError(t, err)

	var tool *llm.Message

	for i := range res.Messages {
		if res.Messages[i].Role == "tool" {
			tool = &res.Messages[i]
		}
	}

	require.NotNil(t, tool)
	assert.Equal(t, "one\ntwo", tool.Content)
}

func TestMessagesMaskSecret(t *testing.T) {
	const secret = "tok-SECRET-0123456789"

	data := rewrite(readFixture(t, "real/t5.jsonl"),
		"2+2 equals 4.", "leak "+secret, `{\"accepted\":true}`, secret, "Stated that", secret)
	_, res, err := replayData(t, data, mapperOpts{Prefix: cmlocal, Secret: secret, Task: "t " + secret}, nil)
	require.NoError(t, err)

	b, err := json.Marshal(res.Messages)
	require.NoError(t, err)
	assert.NotContains(t, string(b), secret)
}

func TestMessagesSkipBackstoppedCall(t *testing.T) {
	accepted := false
	args := json.RawMessage(finishInput)

	stopped, _, res, err := feedUntilStop(t, readFixture(t, "real/t5.jsonl"), mapperOpts{
		Prefix: cmlocal, Task: "q",
		Terminal: func() (string, json.RawMessage, bool) { return "finish", args, accepted },
	}, nil, func(line []byte) {
		accepted = accepted || strings.Contains(string(line), `"tool_result"`)
	})
	require.True(t, stopped)
	require.NoError(t, err)
	assert.Equal(t, []string{"user", "assistant", "tool"}, roles(res.Messages))
}

func TestRenderTranscript(t *testing.T) {
	_, res, err := replay(t, "real/t5.jsonl", mapperOpts{Prefix: cmlocal, SystemPrompt: "SYSPROMPT", Task: "TASKTEXT"})
	require.NoError(t, err)

	out := RenderTranscript(res.Messages)

	assert.NotContains(t, out, "SYSPROMPT")
	assert.NotContains(t, out, "mcp__cmlocal__")

	last := -1

	for _, want := range []string{"TASKTEXT", "2+2 equals 4.", "finish", "chore: answer 2+2 question", `{"accepted":true}`} {
		i := strings.Index(out, want)
		require.GreaterOrEqual(t, i, 0, want)
		assert.Greater(t, i, last, want)
		last = i
	}
}

func TestMessagesUnansweredCall(t *testing.T) {
	var cut []byte

	for line := range bytes.Lines(readFixture(t, "real/t5.jsonl")) {
		if bytes.Contains(line, []byte(`"tool_result"`)) {
			break
		}

		cut = append(cut, line...)
	}

	_, res, err := replayData(t, cut, mapperOpts{Prefix: cmlocal, Task: "q"}, errors.New("signal: killed"))
	require.Error(t, err)

	require.Equal(t, []string{"user", "assistant", "tool"}, roles(res.Messages))
	assert.Equal(t, "toolu_014h96jCjuHV3wJj8rGfnEn5", res.Messages[2].ToolCallID)
	assert.Equal(t, res.Messages[1].ToolCalls[0].ID, res.Messages[2].ToolCallID)
	requireValidTranscript(t, res.Messages)
}

func TestMessagesIgnoreUnknownResult(t *testing.T) {
	data := rewrite(readFixture(t, "real/t5.jsonl"), `"tool_use_id":"toolu_014h96jCjuHV3wJj8rGfnEn5"`, `"tool_use_id":"toolu_unknown"`)
	_, res, err := replayData(t, data, mapperOpts{Prefix: cmlocal}, nil)
	require.NoError(t, err)

	for _, m := range res.Messages {
		assert.NotEqual(t, "toolu_unknown", m.ToolCallID)
	}

	requireValidTranscript(t, res.Messages)
}
