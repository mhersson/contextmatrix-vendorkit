package claudecode

import (
	"fmt"
	"os"
	"testing"
)

// TestMain lets the test binary stand in for the claude CLI in CheckVersion
// tests: with CLAUDECODE_FAKE_VERSION set it prints that version and exits.
func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv("CLAUDECODE_FAKE_VERSION"); ok {
		fmt.Println(v + " (Claude Code)")
		os.Exit(0)
	}

	os.Exit(m.Run())
}
