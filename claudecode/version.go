package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Version is the Claude Code release vendorkit is tested against, not a hard
// pin. Images CM builds install it; a host install is the user's, and no CM
// tool installs or changes it. A release that makes --bare the -p default
// ignores the OAuth token, which surfaces as ErrNotLoggedIn; how to move past
// it is the operator's decision when it happens.
const Version = "2.1.322"

const versionTimeout = 10 * time.Second

// CheckVersion runs `<binary> --version` and returns the version it reports
// and whether that equals Version. A mismatch is never an error and never
// fails a run: callers log one warning naming both versions and carry on. err
// is set only when the binary cannot be run or prints no version.
func CheckVersion(ctx context.Context, binary string) (found string, tested bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, binary, "--version").Output() //nolint:gosec // the caller names the claude binary
	if err != nil {
		return "", false, fmt.Errorf("run %s --version: %w", binary, err)
	}

	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false, errors.New("claudecode: --version printed no version")
	}

	return fields[0], fields[0] == Version, nil
}
