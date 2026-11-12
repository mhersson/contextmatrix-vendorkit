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

// closedResult answers a call that arrives while the bridge closes.
const closedResult = "unexecuted: the run is ending"

// maxResultSizeChars is the result size every tool declares: 2.1.322 replaces a bridged
// result over about 50,000 chars with a 2 KB preview unless the tool declares a larger one.
// The bridge caps nothing itself: callers cap their own results, and one over 400,000 chars
// still becomes a preview.
const maxResultSizeChars = 400_000

// omittedImageNote replaces an image result whose URL is not a usable data URL.
const omittedImageNote = "[image omitted: unsupported or malformed image URL]"

var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)

func (b *Bridge) register(runCtx context.Context, server *mcp.Server, t tools.Tool, schema json.RawMessage) {
	fn := t.Schema().Function

	term, isTerm := t.(tools.Terminal)
	terminal := isTerm && term.Terminal()

	meta := mcp.Meta{"anthropic/maxResultSizeChars": maxResultSizeChars}

	server.AddTool(&mcp.Tool{Name: t.Name(), Description: fn.Description, InputSchema: schema, Meta: meta},
		func(reqCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !b.beginCall() {
				return errorResult(closedResult), nil
			}

			defer b.inflight.Done()

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

			if terminal {
				// Held through Execute and record so only one terminal call runs.
				b.termExec.Lock()
				defer b.termExec.Unlock()

				if b.terminalDone() {
					return errorResult(postTerminalResult), nil
				}
			}

			res, err := t.Execute(ctx, args)
			if err != nil {
				return errorResult(err.Error()), nil
			}

			if terminal {
				b.recordTerminal(t.Name(), raw)
			}

			return toResult(res), nil
		})
}

// inputSchema returns the schema go-sdk will accept: empty, null and {} become
// an empty object schema; anything else must already declare type "object".
func inputSchema(t tools.Tool) (json.RawMessage, error) {
	raw := t.Schema().Function.Parameters

	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("mcpbridge tool %q: invalid input schema: %w", t.Name(), err)
		}
	}

	if len(m) == 0 {
		return emptyObjectSchema, nil
	}

	if m["type"] != "object" {
		return nil, fmt.Errorf("mcpbridge tool %q: input schema must have type object", t.Name())
	}

	return raw, nil
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toResult(res tools.Result) *mcp.CallToolResult {
	out := &mcp.CallToolResult{}
	// An empty text block beside images is rejected by the Messages API; with
	// no images it stays, so the result is never empty.
	if res.Text != "" || len(res.Images) == 0 {
		out.Content = append(out.Content, &mcp.TextContent{Text: res.Text})
	}

	for _, img := range res.Images {
		mime, data, ok := parseDataURL(img.URL)
		if !ok {
			out.Content = append(out.Content, &mcp.TextContent{Text: omittedImageNote})

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
