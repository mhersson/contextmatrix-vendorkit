package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-vendorkit/internal/fakeclaude"
)

func TestCheckVersion(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		fake       string
		wantFound  string
		wantTested bool
	}{
		{"tested", "2.1.322", "2.1.322", true},
		{"untested", "2.1.400", "2.1.400", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(fakeclaude.EnvVar, writeScenario(t, fakeclaude.Scenario{Version: tt.fake}))

			found, tested, err := CheckVersion(ctx, os.Args[0])
			require.NoError(t, err)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantTested, tested)
		})
	}

	t.Run("no output", func(t *testing.T) {
		silent := filepath.Join(t.TempDir(), "claude")
		require.NoError(t, os.WriteFile(silent, []byte("#!/bin/sh\nexit 0\n"), 0o700))

		_, _, err := CheckVersion(ctx, silent)
		require.Error(t, err)
	})

	t.Run("missing binary", func(t *testing.T) {
		_, _, err := CheckVersion(ctx, filepath.Join(t.TempDir(), "no-such-claude"))
		require.Error(t, err)
	})
}
