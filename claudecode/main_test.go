package claudecode

import (
	"os"
	"testing"

	"github.com/mhersson/contextmatrix-vendorkit/internal/fakeclaude"
)

// TestMain lets the test binary stand in for the claude CLI: with
// fakeclaude.EnvVar set it runs the fake and exits.
func TestMain(m *testing.M) {
	if _, ok := os.LookupEnv(fakeclaude.EnvVar); ok {
		fakeclaude.Main()
	}

	os.Exit(m.Run())
}
