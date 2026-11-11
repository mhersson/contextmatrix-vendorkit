package mcpbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerConfig is one entry of the CLI's --mcp-config server map.
type ServerConfig struct {
	Name    string
	URL     string
	Headers map[string]string
}

// Bridge serves a tool registry to the CLI over a loopback MCP server and hosts
// the stop-hook endpoint on the same listener.
type Bridge struct {
	name      string
	port      int
	bearer    string
	hookToken string
	tools     []string
	terminals []string

	srv       *http.Server
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error

	callMu   sync.Mutex
	closing  bool
	inflight sync.WaitGroup // tool calls still executing

	termExec sync.Mutex
	mu       sync.Mutex
	termTool string
	termArgs []byte
	termDone bool
}

// Start listens on 127.0.0.1 and serves reg's tools until Close. Tool calls run
// with a context derived from ctx. reg may be nil.
func Start(ctx context.Context, name string, reg *tools.Registry) (*Bridge, error) {
	schemas := map[string]json.RawMessage{}

	if reg != nil {
		for _, t := range reg.All() {
			schema, err := inputSchema(t)
			if err != nil {
				return nil, err
			}

			schemas[t.Name()] = schema
		}
	}

	bearer, err := secret()
	if err != nil {
		return nil, err
	}

	hookToken, err := secret()
	if err != nil {
		return nil, err
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcpbridge listen: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	b := &Bridge{
		name:      name,
		port:      ln.Addr().(*net.TCPAddr).Port,
		bearer:    bearer,
		hookToken: hookToken,
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, nil)

	if reg != nil {
		for _, t := range reg.All() {
			b.tools = append(b.tools, t.Name())

			if term, ok := t.(tools.Terminal); ok && term.Terminal() {
				b.terminals = append(b.terminals, t.Name())
			}

			b.register(runCtx, server, t, schemas[t.Name()])
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", b.requireBearer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil)))
	mux.HandleFunc("/hook/{token}", b.handleHook)

	b.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return runCtx },
	}

	go func() {
		defer close(b.done)

		_ = b.srv.Serve(ln)
	}()

	return b, nil
}

func secret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mcpbridge secret: %w", err)
	}

	return hex.EncodeToString(buf[:]), nil
}

// Config returns the entry the CLI needs to reach the bridge.
func (b *Bridge) Config() ServerConfig {
	return ServerConfig{
		Name:    b.name,
		URL:     fmt.Sprintf("http://127.0.0.1:%d/mcp", b.port),
		Headers: map[string]string{"Authorization": "Bearer " + b.bearer},
	}
}

// QualifiedName is the name the CLI gives a bridged tool.
func (b *Bridge) QualifiedName(tool string) string {
	return "mcp__" + b.name + "__" + tool
}

// Tools returns the bare tool names in registry order.
func (b *Bridge) Tools() []string { return append([]string(nil), b.tools...) }

// Terminals returns the bare names of the tools that end the run.
func (b *Bridge) Terminals() []string { return append([]string(nil), b.terminals...) }

// HookURL is the capability URL for the PostToolUse stop hook.
func (b *Bridge) HookURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/hook/%s", b.port, b.hookToken)
}

// Close stops the server, cancels in-flight tool calls and waits for them to
// return, so no tool runs once it does. It is idempotent.
func (b *Bridge) Close() error {
	b.closeOnce.Do(func() {
		b.callMu.Lock()
		b.closing = true
		b.callMu.Unlock()

		b.cancel()

		if err := b.srv.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.closeErr = fmt.Errorf("mcpbridge close: %w", err)
		}

		<-b.done
		b.inflight.Wait()
	})

	return b.closeErr
}

// beginCall registers a tool call with Close; false once Close has begun.
func (b *Bridge) beginCall() bool {
	b.callMu.Lock()
	defer b.callMu.Unlock()

	if b.closing {
		return false
	}

	b.inflight.Add(1)

	return true
}

func (b *Bridge) requireBearer(next http.Handler) http.Handler {
	want := []byte("Bearer " + b.bearer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}
