package mcpbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// postTerminalResult answers a call made after a terminal call was accepted.
const postTerminalResult = "unexecuted: an earlier call in this batch ended the run"

var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)

func (b *Bridge) register(runCtx context.Context, server *mcp.Server, t tools.Tool) {
	fn := t.Schema().Function

	schema := fn.Parameters
	if len(schema) == 0 {
		schema = emptyObjectSchema
	}

	term, isTerm := t.(tools.Terminal)
	terminal := isTerm && term.Terminal()

	server.AddTool(&mcp.Tool{Name: t.Name(), Description: fn.Description, InputSchema: schema},
		func(reqCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Tool context carries the run's values and cancellation; it also
			// ends with the request, never the bare HTTP context.
			ctx, cancel := context.WithCancel(runCtx)
			defer cancel()

			stop := context.AfterFunc(reqCtx, cancel)
			defer stop()

			raw := req.Params.Arguments

			args := map[string]any{}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &args); err != nil {
					return errorResult(fmt.Sprintf("invalid arguments: %v", err)), nil
				}
			}

			if terminal && b.terminalDone() {
				return errorResult(postTerminalResult), nil
			}

			res, err := t.Execute(ctx, args)
			if err != nil {
				return errorResult(err.Error()), nil
			}

			if terminal && !b.recordTerminal(t.Name(), raw) {
				return errorResult(postTerminalResult), nil
			}

			return toResult(res), nil
		})
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toResult(res tools.Result) *mcp.CallToolResult {
	out := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Text}}}

	for _, img := range res.Images {
		mime, data, ok := parseDataURL(img.URL)
		if !ok {
			out.Content = append(out.Content, &mcp.TextContent{Text: "[image omitted: unsupported or malformed image URL]"})

			continue
		}

		out.Content = append(out.Content, &mcp.ImageContent{Data: data, MIMEType: mime})
	}

	return out
}

// parseDataURL decodes "data:<mime>;base64,<data>".
func parseDataURL(u string) (mime string, data []byte, ok bool) {
	rest, found := strings.CutPrefix(u, "data:")
	if !found {
		return "", nil, false
	}

	meta, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", nil, false
	}

	mime, found = strings.CutSuffix(meta, ";base64")
	if !found || mime == "" {
		return "", nil, false
	}

	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}

	return mime, data, true
}
