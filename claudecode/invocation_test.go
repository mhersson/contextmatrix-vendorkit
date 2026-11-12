package claudecode

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-vendorkit/mcpbridge"
)

const (
	testToken = "sk-ant-oat01-SECRETTOKEN"
	testTask  = "TASKTEXT-do-the-thing"
	testSys   = "SYSTEMPROMPT-be-careful"
	testRun   = "/home/u/.vendorkit/run-abc"
)

func testSpec() Spec {
	return Spec{
		Home:       "/home/u",
		OAuthToken: testToken,
		Model:      "claude-haiku-5-5",
		Task:       testTask,
		Emitter:    events.NewEmitter(nil, &strings.Builder{}),
	}
}

func testBridge() bridgeInfo {
	return bridgeInfo{
		Server: mcpbridge.ServerConfig{
			Name:    "cm",
			URL:     "http://127.0.0.1:4242/mcp",
			Headers: map[string]string{"Authorization": "Bearer " + testToken},
		},
		HookURL:   "http://127.0.0.1:4242/hook/abc",
		Tools:     []string{"mcp__cm__grep", "mcp__cm__finish", "mcp__cm__done.v2"},
		Terminals: []string{"mcp__cm__finish", "mcp__cm__done.v2"},
	}
}

// flagValue returns the element after flag, failing when absent.
func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()

	i := slices.Index(args, flag)
	require.GreaterOrEqual(t, i, 0, "missing %s in %v", flag, args)
	require.Less(t, i+1, len(args), "no value after %s", flag)

	return args[i+1]
}

func TestInvocationInvariants(t *testing.T) {
	for _, session := range []bool{false, true} {
		for _, withTools := range []bool{false, true} {
			s := testSpec()
			s.SystemPrompt = testSys

			bi := bridgeInfo{}
			if withTools {
				bi = testBridge()
				s.BuiltinTools = []string{"Read", "Bash"}
			}

			if session {
				s.Task = ""
			}

			args, files, err := buildInvocation(s, bi, testRun, session)
			require.NoError(t, err)

			assert.Contains(t, args, "-p")
			assert.Equal(t, "stream-json", flagValue(t, args, "--output-format"))
			assert.Equal(t, "stream-json", flagValue(t, args, "--input-format"))
			assert.Contains(t, args, "--verbose")
			assert.Contains(t, args, "--include-partial-messages")
			assert.Equal(t, "user", flagValue(t, args, "--setting-sources"))
			assert.Contains(t, args, "--strict-mcp-config")
			assert.Equal(t, "dontAsk", flagValue(t, args, "--permission-mode"))
			assert.Equal(t, "off", flagValue(t, args, "--system-prompt-snapshot"))
			assert.Equal(t, "claude-haiku-5-5", flagValue(t, args, "--model"))
			assert.Equal(t, filepath.Join(testRun, "mcp.json"), flagValue(t, args, "--mcp-config"))
			assert.Equal(t, filepath.Join(testRun, "settings.json"), flagValue(t, args, "--settings"))
			assert.NotContains(t, args, "--bare")

			for _, a := range args {
				assert.NotContains(t, a, testTask)
				assert.NotContains(t, a, testSys)
				assert.NotContains(t, a, testToken)
			}

			for path := range files {
				assert.True(t, filepath.IsAbs(path), path)
				assert.Equal(t, testRun, filepath.Dir(path))
			}
		}
	}
}

func TestInvocationTools(t *testing.T) {
	s := testSpec()
	args, _, err := buildInvocation(s, bridgeInfo{}, testRun, false)
	require.NoError(t, err)

	i := slices.Index(args, "--tools")
	require.GreaterOrEqual(t, i, 0)
	assert.Empty(t, args[i+1])

	s.BuiltinTools = []string{"Read", "Bash"}
	s.MCPServers = []mcpbridge.ServerConfig{{Name: "contextmatrix", URL: "http://x/mcp"}}

	args, _, err = buildInvocation(s, testBridge(), testRun, false)
	require.NoError(t, err)

	assert.Equal(t, "Read,Bash", flagValue(t, args, "--tools"))

	allowed := strings.Split(flagValue(t, args, "--allowedTools"), ",")
	assert.ElementsMatch(t, []string{
		"Read", "Bash", "mcp__cm__grep", "mcp__cm__finish", "mcp__cm__done.v2", "mcp__contextmatrix",
	}, allowed)
}

func TestInvocationFiles(t *testing.T) {
	t.Run("mcp and settings", func(t *testing.T) {
		s := testSpec()
		s.MCPServers = []mcpbridge.ServerConfig{{Name: "contextmatrix", URL: "http://x/mcp", Headers: map[string]string{"K": "v"}}}
		bi := testBridge()

		_, files, err := buildInvocation(s, bi, testRun, false)
		require.NoError(t, err)

		var mcp struct {
			MCPServers map[string]struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
				Timeout *int64            `json:"timeout"`
			} `json:"mcpServers"`
		}

		require.NoError(t, json.Unmarshal(files[filepath.Join(testRun, "mcp.json")], &mcp))
		require.Len(t, mcp.MCPServers, 2)
		assert.Equal(t, "http", mcp.MCPServers["cm"].Type)
		assert.Equal(t, bi.Server.URL, mcp.MCPServers["cm"].URL)
		assert.Equal(t, bi.Server.Headers, mcp.MCPServers["cm"].Headers)
		assert.Equal(t, "http", mcp.MCPServers["contextmatrix"].Type)
		assert.Equal(t, "v", mcp.MCPServers["contextmatrix"].Headers["K"])

		// A bridged tool may run silent for minutes (agent verify); 2.1.322
		// aborts a silent http call unless the server entry carries a timeout.
		require.NotNil(t, mcp.MCPServers["cm"].Timeout)
		assert.Equal(t, int64(100_000_000), *mcp.MCPServers["cm"].Timeout)
		assert.Nil(t, mcp.MCPServers["contextmatrix"].Timeout, "only the bridge entry is lifted")

		raw := files[filepath.Join(testRun, "settings.json")]

		var settings struct {
			Hooks struct {
				PostToolUse []struct {
					Matcher string `json:"matcher"`
					Hooks   []struct {
						Type string `json:"type"`
						URL  string `json:"url"`
					} `json:"hooks"`
				} `json:"PostToolUse"`
			} `json:"hooks"`
		}

		require.NoError(t, json.Unmarshal(raw, &settings))
		require.Len(t, settings.Hooks.PostToolUse, 1)

		entry := settings.Hooks.PostToolUse[0]
		require.Len(t, entry.Hooks, 1)
		assert.Equal(t, "http", entry.Hooks[0].Type)
		assert.Equal(t, bi.HookURL, entry.Hooks[0].URL)
		assert.NotContains(t, string(raw), `"command"`)

		re := regexp.MustCompile(entry.Matcher)
		for _, term := range bi.Terminals {
			assert.True(t, re.MatchString(term), term)
		}

		assert.False(t, re.MatchString("mcp__cm__grep"))
		assert.False(t, re.MatchString("mcp__cm__doneXv2"), "dot must be quoted")
		assert.False(t, re.MatchString("xmcp__cm__finish"))
		assert.False(t, re.MatchString("mcp__cm__finish2"))
	})

	t.Run("no terminals no hook", func(t *testing.T) {
		bi := testBridge()
		bi.Terminals = nil

		_, files, err := buildInvocation(testSpec(), bi, testRun, false)
		require.NoError(t, err)

		raw := files[filepath.Join(testRun, "settings.json")]
		assert.NotContains(t, string(raw), "PostToolUse")
		assert.True(t, json.Valid(raw))
	})

	t.Run("system prompt", func(t *testing.T) {
		s := testSpec()
		s.SystemPrompt = testSys
		args, files, err := buildInvocation(s, bridgeInfo{}, testRun, false)
		require.NoError(t, err)

		path := flagValue(t, args, "--system-prompt-file")
		assert.Equal(t, testSys, string(files[path]))
		assert.NotContains(t, args, "--append-system-prompt-file")

		s.SystemPrompt, s.SystemPromptAppend = "", "APPEND"
		args, files, err = buildInvocation(s, bridgeInfo{}, testRun, false)
		require.NoError(t, err)

		path = flagValue(t, args, "--append-system-prompt-file")
		assert.Equal(t, "APPEND", string(files[path]))
		assert.NotContains(t, args, "--system-prompt-file")

		args, files, err = buildInvocation(testSpec(), bridgeInfo{}, testRun, false)
		require.NoError(t, err)
		assert.NotContains(t, args, "--system-prompt-file")
		assert.NotContains(t, args, "--append-system-prompt-file")
		assert.Len(t, files, 2)
	})
}

func TestInvocationSession(t *testing.T) {
	for _, session := range []bool{false, true} {
		t.Run("", func(t *testing.T) {
			s := testSpec()
			if session {
				s.Task = ""
			}

			args, _, err := buildInvocation(s, bridgeInfo{}, testRun, session)
			require.NoError(t, err)
			assert.NotContains(t, args, "--session-id")
			assert.NotContains(t, args, "--resume")
			assert.NotContains(t, args, "--max-turns")
			assert.NotContains(t, args, "--max-budget-usd")
			assert.NotContains(t, args, "--effort")
			assert.Equal(t, !session, slices.Contains(args, "--no-session-persistence"))

			s.SessionID = "sid-1"
			args, _, err = buildInvocation(s, bridgeInfo{}, testRun, session)
			require.NoError(t, err)
			assert.Equal(t, "sid-1", flagValue(t, args, "--session-id"))
			assert.NotContains(t, args, "--no-session-persistence")

			s.SessionID, s.Resume = "", "sid-2"
			args, _, err = buildInvocation(s, bridgeInfo{}, testRun, session)
			require.NoError(t, err)
			assert.Equal(t, "sid-2", flagValue(t, args, "--resume"))
			assert.NotContains(t, args, "--no-session-persistence")

			s.MaxTurns, s.MaxBudgetUSD, s.Effort = 7, 1.5, "high"
			args, _, err = buildInvocation(s, bridgeInfo{}, testRun, session)
			require.NoError(t, err)
			assert.Equal(t, "7", flagValue(t, args, "--max-turns"))
			assert.Equal(t, "high", flagValue(t, args, "--effort"))

			if session {
				assert.NotContains(t, args, "--max-budget-usd")
			} else {
				assert.Equal(t, "1.5", flagValue(t, args, "--max-budget-usd"))
			}
		})
	}
}

func TestSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Spec)
		session bool
		field   string
	}{
		{"token", func(s *Spec) { s.OAuthToken = "" }, false, "OAuthToken"},
		{"home", func(s *Spec) { s.Home = "" }, false, "Home"},
		{"emitter", func(s *Spec) { s.Emitter = nil }, false, "Emitter"},
		{"both prompts", func(s *Spec) { s.SystemPrompt, s.SystemPromptAppend = "a", "b" }, false, "SystemPrompt"},
		{"session and resume", func(s *Spec) { s.SessionID, s.Resume = "a", "b" }, false, "SessionID"},
		{"reserved server", func(s *Spec) { s.MCPServers = []mcpbridge.ServerConfig{{Name: "cm"}} }, false, "MCPServers"},
		{"model", func(s *Spec) { s.Model = "" }, false, "Model"},
		{"unnamed server", func(s *Spec) { s.MCPServers = []mcpbridge.ServerConfig{{URL: "http://x/mcp"}} }, false, "MCPServers"},
		{"duplicate server", func(s *Spec) {
			s.MCPServers = []mcpbridge.ServerConfig{{Name: "contextmatrix"}, {Name: "contextmatrix"}}
		}, false, "MCPServers"},
		{"session id path", func(s *Spec) { s.SessionID = "../x" }, false, "SessionID"},
		{"resume glob", func(s *Spec) { s.Resume = "*" }, false, "Resume"},
		{"effort", func(s *Spec) { s.Effort = "minimal" }, false, "Effort"},
		{"effort case", func(s *Spec) { s.Effort = "High" }, false, "Effort"},
		{"session task", func(s *Spec) {}, true, "Task"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSpec()
			tt.mutate(&s)

			err := s.validate(tt.session)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.field)
		})
	}

	require.NoError(t, testSpec().validate(false))

	s := testSpec()
	s.Task, s.Effort = "", "xhigh"
	require.NoError(t, s.validate(true))

	s.Task = "x"
	_, _, err := buildInvocation(s, bridgeInfo{}, testRun, true)
	require.Error(t, err)
}

func TestNormalizeEffort(t *testing.T) {
	for _, v := range effortValues() {
		assert.Equal(t, v, NormalizeEffort(v))
		assert.Equal(t, v, NormalizeEffort("  "+strings.ToUpper(v)+" "))
	}

	for _, v := range []string{"", "minimal", "bogus"} {
		assert.Empty(t, NormalizeEffort(v))
	}
}

func TestChildEnv(t *testing.T) {
	// Every variable 2.1.322 reads that moves a run off the subscription token.
	dropped := []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_UNIX_SOCKET",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_HOST_AUTH_ENV_VAR",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_SIMPLE",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_RETRY_WATCHDOG",
	}

	s := testSpec()
	s.Env = []string{"HOME=/x", "CLAUDE_CONFIG_DIR=/y", "KEEP=1"}

	for _, name := range dropped {
		s.Env = append(s.Env, name+"=1")
	}

	env := childEnv(s, "/run/config")

	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		assert.NotContains(t, dropped, name)
	}

	assert.Contains(t, env, "HOME=/home/u")
	assert.Contains(t, env, "CLAUDE_CONFIG_DIR=/run/config")
	assert.Contains(t, env, "CLAUDE_CODE_OAUTH_TOKEN="+testToken)
	assert.Contains(t, env, "DISABLE_AUTOUPDATER=1")
	assert.Contains(t, env, "KEEP=1")
	assert.NotContains(t, env, "HOME=/x")
	assert.NotContains(t, env, "CLAUDE_CONFIG_DIR=/y")
}

// envValue returns the value of name in env, and whether env holds it.
func envValue(t *testing.T, env []string, name string) (string, bool) {
	t.Helper()

	var value string

	found := false

	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			require.False(t, found, "%s appears twice", name)

			value, found = v, true
		}
	}

	return value, found
}

func TestChildEnvProxyBypassesLoopback(t *testing.T) {
	const loopback = "127.0.0.1,localhost,::1"

	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Run(name, func(t *testing.T) {
			s := testSpec()
			s.Env = []string{"PATH=/bin", name + "=http://proxy:3128"}

			env := childEnv(s, "/run/config")

			for _, np := range []string{"NO_PROXY", "no_proxy"} {
				v, ok := envValue(t, env, np)
				require.True(t, ok, np)
				assert.Equal(t, loopback, v, np)
			}

			assert.Contains(t, env, name+"=http://proxy:3128")
		})
	}

	for _, tc := range []struct {
		name           string
		env            []string
		upper, lower   string
		wantNoProxySet bool
	}{
		{name: "no proxy", env: []string{"PATH=/bin"}},
		{name: "empty proxy", env: []string{"PATH=/bin", "HTTP_PROXY="}},
		{name: "no proxy keeps NO_PROXY as is", env: []string{"NO_PROXY=corp"}, upper: "corp", wantNoProxySet: true},
		{
			name: "NO_PROXY kept and seeds no_proxy", env: []string{"HTTPS_PROXY=http://p:1", "NO_PROXY=corp.internal"},
			upper: "corp.internal," + loopback, lower: "corp.internal," + loopback, wantNoProxySet: true,
		},
		{
			name: "no_proxy kept and seeds NO_PROXY", env: []string{"https_proxy=http://p:1", "no_proxy=.corp"},
			upper: ".corp," + loopback, lower: ".corp," + loopback, wantNoProxySet: true,
		},
		{
			name: "each form keeps its own entries", env: []string{"ALL_PROXY=socks5://p:1", "NO_PROXY=a", "no_proxy=b, localhost"},
			upper: "a," + loopback, lower: "b, localhost,127.0.0.1,::1", wantNoProxySet: true,
		},
		{
			name: "empty no_proxy starts from NO_PROXY", env: []string{"HTTPS_PROXY=http://p:1", "NO_PROXY=corp", "no_proxy="},
			upper: "corp," + loopback, lower: "corp," + loopback, wantNoProxySet: true,
		},
		{
			name: "empty NO_PROXY starts from no_proxy", env: []string{"HTTPS_PROXY=http://p:1", "NO_PROXY=", "no_proxy=corp"},
			upper: "corp," + loopback, lower: "corp," + loopback, wantNoProxySet: true,
		},
		{
			name: "loopback already listed", env: []string{"HTTP_PROXY=http://p:1", "NO_PROXY=::1,LOCALHOST,127.0.0.1"},
			upper: "::1,LOCALHOST,127.0.0.1", lower: "::1,LOCALHOST,127.0.0.1", wantNoProxySet: true,
		},
		{
			name: "trailing comma", env: []string{"HTTP_PROXY=http://p:1", "NO_PROXY=corp,"},
			upper: "corp," + loopback, lower: "corp," + loopback, wantNoProxySet: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSpec()
			s.Env = tc.env

			env := childEnv(s, "/run/config")

			upper, upperOK := envValue(t, env, "NO_PROXY")
			lower, lowerOK := envValue(t, env, "no_proxy")

			if !tc.wantNoProxySet {
				assert.False(t, upperOK, "NO_PROXY=%s", upper)
				assert.False(t, lowerOK, "no_proxy=%s", lower)

				return
			}

			assert.Equal(t, tc.upper, upper, "NO_PROXY")

			if tc.lower == "" {
				assert.False(t, lowerOK, "no_proxy=%s", lower)

				return
			}

			assert.Equal(t, tc.lower, lower, "no_proxy")
		})
	}
}

func TestUserMessage(t *testing.T) {
	got, err := userMessage("hi", nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`, string(got))
	assert.True(t, strings.HasSuffix(string(got), "\n"))

	got, err = userMessage("hi", []llm.ImageURL{{URL: "data:image/png;base64,QUJD"}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"},`+
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}}`, string(got))

	got, err = userMessage("", []llm.ImageURL{{URL: "data:image/png;base64,QUJD"}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"user","message":{"role":"user","content":[`+
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}}`, string(got),
		"an image-only message carries no text block")

	for _, bad := range []string{"https://x/y.png", "data:image/png,QUJD", "data:;base64,QUJD", "data:image/png;base64,"} {
		_, err = userMessage("hi", []llm.ImageURL{{URL: bad}})
		require.Error(t, err, bad)
	}
}
