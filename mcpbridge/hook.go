package mcpbridge

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
)

const hookStopReason = "terminal tool call accepted; the run is complete"

// Terminal reports the first accepted terminal call and its raw arguments.
func (b *Bridge) Terminal() (tool string, args json.RawMessage, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.termDone {
		return "", nil, false
	}

	return b.termTool, json.RawMessage(append([]byte(nil), b.termArgs...)), true
}

func (b *Bridge) terminalDone() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.termDone
}

// recordTerminal stores the accepted terminal call. Callers hold termExec, so
// at most one call is ever recorded.
func (b *Bridge) recordTerminal(tool string, args json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.termDone = true
	b.termTool = tool

	b.termArgs = append([]byte(nil), args...)
}

func (b *Bridge) handleHook(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.PathValue("token")), []byte(b.hookToken)) != 1 {
		http.NotFound(w, r)

		return
	}

	var in struct {
		HookEventName string `json:"hook_event_name"`
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(body, &in)

	w.Header().Set("Content-Type", "application/json")

	if in.HookEventName == "PostToolUse" && b.terminalDone() {
		_ = json.NewEncoder(w).Encode(map[string]any{"continue": false, "stopReason": hookStopReason})

		return
	}

	_, _ = w.Write([]byte("{}"))
}
