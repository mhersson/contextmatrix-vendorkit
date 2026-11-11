package subproc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildEnv(t *testing.T) {
	t.Parallel()

	got := BuildEnv(
		[]string{"PATH=/bin", "ANTHROPIC_API_KEY=x", "HOME=/old", "KEEP=1"},
		[]string{"ANTHROPIC_API_KEY"},
		map[string]string{"HOME": "/new", "B": "2", "A": "1"},
	)

	assert.Equal(t, []string{"PATH=/bin", "KEEP=1", "A=1", "B=2", "HOME=/new"}, got)
}
