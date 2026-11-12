package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	name   string
	desc   string
	params string
	exec   func(ctx context.Context, args map[string]any) (tools.Result, error)
}

func (f fixture) Name() string { return f.name }
func (f fixture) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: f.name, Description: f.desc, Parameters: json.RawMessage(f.params),
	}}
}

func (f fixture) Execute(ctx context.Context, a map[string]any) (tools.Result, error) {
	return f.exec(ctx, a)
}

type terminalFixture struct{ fixture }

func (terminalFixture) Terminal() bool { return true }

const picURL = "data:image/png;base64,aGVsbG8="

func testRegistry() *tools.Registry {
	return tools.NewRegistry(
		fixture{
			name: "echo", desc: "echoes", params: `{"type":"object","properties":{"text":{"type":"string"}}}`,
			exec: func(_ context.Context, a map[string]any) (tools.Result, error) {
				s, _ := a["text"].(string)

				return tools.Result{Text: s}, nil
			},
		},
		fixture{
			name: "fail", desc: "fails",
			exec: func(context.Context, map[string]any) (tools.Result, error) {
				return tools.Result{}, errors.New("boom")
			},
		},
		fixture{
			name: "pic", desc: "image",
			exec: func(context.Context, map[string]any) (tools.Result, error) {
				return tools.Result{Text: "see", Images: []llm.ImageURL{{URL: picURL}, {URL: "not-a-data-url"}}}, nil
			},
		},
		terminalFixture{fixture{
			name: "finish", desc: "ends", params: `{"type":"object","properties":{"commit_message":{"type":"string"}}}`,
			exec: func(_ context.Context, a map[string]any) (tools.Result, error) {
				if s, _ := a["commit_message"].(string); s == "" {
					return tools.Result{}, errors.New("commit_message is required")
				}

				return tools.Result{Text: `{"accepted":true}`}, nil
			},
		}},
	)
}

func startBridge(t *testing.T, reg *tools.Registry) *Bridge {
	t.Helper()

	b, err := Start(t.Context(), "cm", reg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	return b
}

type bearerRT struct{ token string }

func (r bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", r.token)

	return http.DefaultTransport.RoundTrip(req)
}

func connect(t *testing.T, b *Bridge) *mcp.ClientSession {
	t.Helper()

	cfg := b.Config()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   cfg.URL,
		HTTPClient: &http.Client{Transport: bearerRT{cfg.Headers["Authorization"]}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	return sess
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)

	return res
}

func text(t *testing.T, c mcp.Content) string {
	t.Helper()

	tc, ok := c.(*mcp.TextContent)
	require.True(t, ok)

	return tc.Text
}

func TestBridgeServesTools(t *testing.T) {
	b := startBridge(t, testRegistry())
	sess := connect(t, b)

	list, err := sess.ListTools(t.Context(), nil)
	require.NoError(t, err)

	var names []string

	schemas := map[string]string{}

	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		raw, err := json.Marshal(tl.InputSchema)
		require.NoError(t, err)

		schemas[tl.Name] = string(raw)

		if tl.Name == "echo" {
			assert.Equal(t, "echoes", tl.Description)
		}
	}

	assert.ElementsMatch(t, []string{"echo", "fail", "pic", "finish"}, names)
	assert.JSONEq(t, `{"type":"object","properties":{"text":{"type":"string"}}}`, schemas["echo"])
	assert.JSONEq(t, `{"type":"object"}`, schemas["fail"])
	assert.Equal(t, []string{"echo", "fail", "pic", "finish"}, b.Tools())
	assert.Equal(t, []string{"finish"}, b.Terminals())
	assert.Equal(t, "mcp__cm__echo", b.QualifiedName("echo"))

	res := call(t, sess, "echo", map[string]any{"text": "hi"})
	assert.False(t, res.IsError)
	assert.Equal(t, "hi", text(t, res.Content[0]))
}

// The CLI parses this _meta key to keep a large bridged result whole.
func TestBridgeToolsDeclareResultSize(t *testing.T) {
	sess := connect(t, startBridge(t, testRegistry()))

	list, err := sess.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, list.Tools)

	for _, tl := range list.Tools {
		size, ok := tl.Meta["anthropic/maxResultSizeChars"].(float64)
		require.True(t, ok, "tool %s declares no result size", tl.Name)
		assert.InDelta(t, float64(maxResultSizeChars), size, 0, tl.Name)
	}
}

func TestBridgeToolErrorIsMCPError(t *testing.T) {
	sess := connect(t, startBridge(t, testRegistry()))

	res := call(t, sess, "fail", nil)
	assert.True(t, res.IsError)
	assert.Equal(t, "boom", text(t, res.Content[0]))

	res = call(t, sess, "echo", map[string]any{"text": "still up"})
	assert.Equal(t, "still up", text(t, res.Content[0]))
}

func TestBridgeImages(t *testing.T) {
	sess := connect(t, startBridge(t, testRegistry()))

	res := call(t, sess, "pic", nil)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 3)

	img, ok := res.Content[1].(*mcp.ImageContent)
	require.True(t, ok)
	assert.Equal(t, "image/png", img.MIMEType)
	assert.Equal(t, []byte("hello"), img.Data)

	_, isText := res.Content[2].(*mcp.TextContent)
	assert.True(t, isText, "malformed image becomes a text note")
}

func TestBridgeRequiresBearer(t *testing.T) {
	b := startBridge(t, testRegistry())
	url := b.Config().URL

	for _, auth := range []string{"", "Bearer wrong"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader("{}"))
		require.NoError(t, err)

		if auth != "" {
			req.Header.Set("Authorization", auth)
		}

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		_ = resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
}

func TestBridgeTerminalAcceptedOnly(t *testing.T) {
	b := startBridge(t, testRegistry())
	sess := connect(t, b)

	res := call(t, sess, "finish", map[string]any{"commit_message": ""})
	assert.True(t, res.IsError)

	_, _, ok := b.Terminal()
	assert.False(t, ok)

	res = call(t, sess, "finish", map[string]any{"commit_message": "feat: x"})
	require.False(t, res.IsError)

	tool, args, ok := b.Terminal()
	require.True(t, ok)
	assert.Equal(t, "finish", tool)
	assert.JSONEq(t, `{"commit_message":"feat: x"}`, string(args))

	res = call(t, sess, "finish", map[string]any{"commit_message": "feat: y"})
	assert.True(t, res.IsError)

	_, args, _ = b.Terminal()
	assert.JSONEq(t, `{"commit_message":"feat: x"}`, string(args))
}

func postHook(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewBufferString(body))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	var out map[string]any

	_ = json.NewDecoder(resp.Body).Decode(&out)

	return resp.StatusCode, out
}

func TestBridgeHook(t *testing.T) {
	b := startBridge(t, testRegistry())
	sess := connect(t, b)

	const post = `{"hook_event_name":"PostToolUse","tool_name":"mcp__cm__finish"}`

	code, out := postHook(t, b.HookURL(), post)
	assert.Equal(t, http.StatusOK, code)
	assert.Empty(t, out)

	call(t, sess, "finish", map[string]any{"commit_message": "feat: x"})

	code, out = postHook(t, b.HookURL(), post)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, out["continue"])
	assert.NotEmpty(t, out["stopReason"])

	_, out = postHook(t, b.HookURL(), `{"hook_event_name":"PostToolUseFailure","tool_name":"mcp__cm__finish"}`)
	assert.Empty(t, out)

	code, _ = postHook(t, strings.Replace(b.HookURL(), "/hook/", "/hook/x", 1), post)
	assert.Equal(t, http.StatusNotFound, code)
}

func TestBridgeClose(t *testing.T) {
	b, err := Start(t.Context(), "cm", nil)
	require.NoError(t, err)

	addr := strings.TrimSuffix(strings.TrimPrefix(b.Config().URL, "http://"), "/mcp")

	// A connection made before Close is this bridge's, whatever later reuses
	// the port, so the check cannot pass against another listener.
	var d net.Dialer

	conn, err := d.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, b.Close())
	require.NoError(t, b.Close())

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)

	var ne net.Error
	if errors.As(err, &ne) {
		assert.False(t, ne.Timeout(), "the bridge still holds the connection open after Close")
	}
}

func TestBridgeCloseWaitsForToolCalls(t *testing.T) {
	started := make(chan struct{})

	var returned atomic.Bool

	reg := tools.NewRegistry(fixture{
		name: "slow", desc: "cleans up after its cancel",
		exec: func(ctx context.Context, _ map[string]any) (tools.Result, error) {
			close(started)
			<-ctx.Done()

			time.Sleep(200 * time.Millisecond)
			returned.Store(true)

			return tools.Result{}, ctx.Err()
		},
	})

	b := startBridge(t, reg)
	sess := connect(t, b)

	go func() {
		_, _ = sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "slow"})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	require.NoError(t, b.Close())
	assert.True(t, returned.Load(), "Close returned while a tool call was still executing")
}

func TestBridgeNilRegistry(t *testing.T) {
	b := startBridge(t, nil)
	assert.Empty(t, b.Tools())
	assert.Empty(t, b.Terminals())
}

type ctxKey struct{}

func TestBridgeToolContext(t *testing.T) {
	started := make(chan context.Context, 1)
	finished := make(chan struct{})

	var once sync.Once

	reg := tools.NewRegistry(fixture{
		name: "slow", desc: "blocks",
		exec: func(ctx context.Context, _ map[string]any) (tools.Result, error) {
			started <- ctx

			<-ctx.Done()
			once.Do(func() { close(finished) })

			return tools.Result{}, ctx.Err()
		},
	})

	parent, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "v"))
	defer cancel()

	b, err := Start(parent, "cm", reg)
	require.NoError(t, err)

	t.Cleanup(func() { _ = b.Close() })

	sess := connect(t, b)

	go func() {
		_, _ = sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "slow"})
	}()

	var toolCtx context.Context
	select {
	case toolCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	assert.Equal(t, "v", toolCtx.Value(ctxKey{}))

	cancel()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("tool ctx not cancelled by Start ctx")
	}
}

func TestStartSchemaHandling(t *testing.T) {
	mk := func(name, params string) *tools.Registry {
		return tools.NewRegistry(fixture{
			name: name, params: params,
			exec: func(context.Context, map[string]any) (tools.Result, error) { return tools.Result{}, nil },
		})
	}

	for _, p := range []string{``, `null`, `{}`} {
		b, err := Start(t.Context(), "cm", mk("t", p))
		require.NoError(t, err, p)
		require.NoError(t, b.Close())
	}

	for _, p := range []string{`{"anyOf":[{"type":"object"}]}`, `{"properties":{}}`, `{"type":"string"}`, `[1]`, `{bad`} {
		b, err := Start(t.Context(), "cm", mk("badtool", p))
		require.Error(t, err, p)
		assert.Nil(t, b)
		assert.Contains(t, err.Error(), "badtool")
	}
}

func TestBridgeConcurrentTerminalCalls(t *testing.T) {
	var (
		execs   atomic.Int32
		started = make(chan struct{}, 2)
		release = make(chan struct{})
	)

	reg := tools.NewRegistry(terminalFixture{fixture{
		name: "finish", params: `{"type":"object"}`,
		exec: func(_ context.Context, a map[string]any) (tools.Result, error) {
			execs.Add(1)

			started <- struct{}{}

			<-release

			return tools.Result{Text: "ok"}, nil
		},
	}})
	b := startBridge(t, reg)
	sess := connect(t, b)

	type outcome struct {
		res *mcp.CallToolResult
		err error
	}

	results := make(chan outcome, 2)
	fire := func(msg string) {
		res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "finish", Arguments: map[string]any{"m": msg}})
		results <- outcome{res, err}
	}

	go fire("a")

	<-started

	go fire("b")

	// The second call must be parked behind the first, not executing.
	select {
	case <-started:
		t.Fatal("second terminal call executed concurrently")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	var okCount, errCount int

	for range 2 {
		o := <-results
		require.NoError(t, o.err)

		if o.res.IsError {
			errCount++
		} else {
			okCount++
		}
	}

	assert.Equal(t, 1, okCount)
	assert.Equal(t, 1, errCount)
	assert.Equal(t, int32(1), execs.Load())

	_, args, ok := b.Terminal()
	require.True(t, ok)
	assert.JSONEq(t, `{"m":"a"}`, string(args))
}

func TestToResultContent(t *testing.T) {
	kinds := func(r *mcp.CallToolResult) []string {
		var out []string

		for _, c := range r.Content {
			switch v := c.(type) {
			case *mcp.TextContent:
				out = append(out, "text:"+v.Text)
			case *mcp.ImageContent:
				out = append(out, "image:"+v.MIMEType)
			default:
				out = append(out, "other")
			}
		}

		return out
	}

	for _, tc := range []struct {
		name string
		in   tools.Result
		want []string
	}{
		{"text and image", tools.Result{Text: "see", Images: []llm.ImageURL{{URL: picURL}}}, []string{"text:see", "image:image/png"}},
		{"image only", tools.Result{Images: []llm.ImageURL{{URL: picURL}}}, []string{"image:image/png"}},
		{"malformed image only", tools.Result{Images: []llm.ImageURL{{URL: "not-a-data-url"}}}, []string{"text:[image omitted: unsupported or malformed image URL]"}},
		{"empty", tools.Result{}, []string{"text:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, kinds(toResult(tc.in)))
		})
	}
}
