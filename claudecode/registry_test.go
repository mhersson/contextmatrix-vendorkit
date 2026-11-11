package claudecode

import (
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
)

type termTool struct{ name string }

func (t termTool) Name() string     { return t.name }
func (t termTool) Schema() llm.Tool { return llm.Tool{} }
func (t termTool) Terminal() bool   { return true }
func (t termTool) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

func TestSplitRegistry(t *testing.T) {
	root := t.TempDir()
	reg := tools.NewRegistry(
		tools.NewReadTool(root), tools.NewWriteTool(root), tools.NewEditTool(root), tools.NewBashTool(root),
		tools.NewGrepTool(root), tools.NewGlobTool(root), tools.NewGitTool(root), termTool{"finish"},
	)

	builtins, bridged := SplitRegistry(reg)
	assert.Equal(t, []string{"Read", "Write", "Edit", "Bash"}, builtins)

	var names []string
	for _, tl := range bridged.All() {
		names = append(names, tl.Name())
	}

	assert.Equal(t, []string{"grep", "glob", "git", "finish"}, names)

	b, br := SplitRegistry(nil)
	assert.Empty(t, b)
	assert.Nil(t, br)
}
