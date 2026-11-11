package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToolResultText(t *testing.T) {
	const img = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}`

	for _, tc := range []struct {
		name, raw, want string
	}{
		{"string", `"plain"`, "plain"},
		{"text blocks", `[{"type":"text","text":"one"},{"type":"text","text":"two"}]`, "one\ntwo"},
		{"image between text blocks", `[{"type":"text","text":"one"},` + img + `,{"type":"text","text":"two"}]`, "one\ntwo"},
		{"image only", `[` + img + `]`, ""},
		{"empty array", `[]`, ""},
		{"null", `null`, ""},
		{"unknown shape", `{"type":"text","text":"x"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, toolResultText(json.RawMessage(tc.raw)))
		})
	}
}
