package claudecode

import (
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
)

// seedMessages mirrors the harness seed: system (only for a replaced system
// prompt), then the user task with one image_url part per image.
func (m *mapper) seedMessages() []llm.Message {
	var msgs []llm.Message

	if m.o.SystemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: m.mask(m.o.SystemPrompt)})
	}

	task := m.mask(m.o.Task)
	if len(m.o.TaskImages) == 0 {
		return append(msgs, llm.Message{Role: "user", Content: task})
	}

	parts := []llm.ContentPart{{Type: "text", Text: task}}

	for i := range m.o.TaskImages {
		img := m.o.TaskImages[i]
		parts = append(parts, llm.ContentPart{Type: "image_url", ImageURL: &img})
	}

	return append(msgs, llm.Message{Role: "user", ContentParts: parts})
}

// assistantMessage is the closed call as the harness would have stored it.
func (m *mapper) assistantMessage(content string, tools []*toolUse) llm.Message {
	msg := llm.Message{Role: "assistant", Content: m.mask(content)}

	for _, t := range tools {
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID:   m.maskSecret(t.id),
			Type: "function",
			Function: llm.FunctionCall{
				Name:      m.maskSecret(m.toolName(t.name)),
				Arguments: m.mask(t.args()),
			},
		})
	}

	return msg
}

// RenderTranscript renders messages as plain text for a primer message: the
// form a resumed blueprint stage or chat session sends as its first user
// message. System messages are skipped; tool calls and results keep bare names.
func RenderTranscript(msgs []llm.Message) string {
	var b strings.Builder

	for _, msg := range msgs {
		if msg.Role == "system" {
			continue
		}

		text := msg.Content

		for _, p := range msg.ContentParts {
			switch p.Type {
			case "text":
				text += p.Text
			case "image_url":
				text += "[image]"
			}
		}

		switch msg.Role {
		case "tool":
			fmt.Fprintf(&b, "[tool result %s]\n%s\n\n", msg.ToolCallID, text)
		case "assistant":
			if text != "" {
				fmt.Fprintf(&b, "[assistant]\n%s\n\n", text)
			}

			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&b, "[assistant tool call %s]\n%s\n\n", tc.Function.Name, tc.Function.Arguments)
			}
		default:
			fmt.Fprintf(&b, "[%s]\n%s\n\n", msg.Role, text)
		}
	}

	return strings.TrimSpace(b.String())
}
