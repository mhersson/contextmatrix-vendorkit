//go:build linux

package subproc

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcStopKillsEscapedDescendant(t *testing.T) {
	t.Parallel()

	pidFile := t.TempDir() + "/pid"
	p := startHelper(t, "escape", "SUBPROC_PIDFILE="+pidFile)
	pid := readPID(t, pidFile)

	t.Cleanup(func() {
		if t.Failed() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	select {
	case <-p.Lines():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "helper not ready")
	}

	require.NoError(t, p.Stop(200*time.Millisecond))
	requireGone(t, pid)
}
