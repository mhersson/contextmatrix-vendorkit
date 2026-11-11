//go:build unix

package subproc

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperEnv = "SUBPROC_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		runHelper(mode)
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func spawnSleeper() *exec.Cmd {
	gc := exec.Command(os.Args[0]) //nolint:gosec // re-execs the test binary

	gc.Env = append(os.Environ(), helperEnv+"=sleep")
	gc.Stdout = os.Stdout

	return gc
}

func runHelper(mode string) {
	switch mode {
	case "lines":
		fmt.Println("one")
		fmt.Println("two")
		fmt.Println("three")
	case "long":
		fmt.Println(strings.Repeat("x", 5<<20))
		fmt.Println("ok")
	case "long2k":
		fmt.Println(strings.Repeat("y", 2048))
		// Keep writing so a reader that stopped draining would hang the child.
		for {
			fmt.Println(strings.Repeat("y", 2048))
		}
	case "group":
		gc := spawnSleeper()
		if err := gc.Start(); err != nil {
			os.Exit(2)
		}

		_ = os.WriteFile(os.Getenv("SUBPROC_PIDFILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600) //nolint:gosec // test-supplied path

		fmt.Println("ready")

		select {}
	case "leaderexits":
		gc := spawnSleeper()
		if err := gc.Start(); err != nil {
			os.Exit(2)
		}

		_ = os.WriteFile(os.Getenv("SUBPROC_PIDFILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600) //nolint:gosec // test-supplied path

		fmt.Println("hi")
	case "ignoreterm":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")

		select {}
	case "stderr":
		for range 40 {
			fmt.Fprint(os.Stderr, strings.Repeat("a", 1023)+"\n")
		}

		fmt.Fprint(os.Stderr, "END")
	case "flood":
		for {
			fmt.Println("flood")
		}
	case "sleep":
		time.Sleep(300 * time.Second)
	}
}

func startHelper(t *testing.T, mode string, extra ...string) *Proc {
	t.Helper()

	return startHelperCmd(t, Cmd{}, mode, extra...)
}

func startHelperCmd(t *testing.T, c Cmd, mode string, extra ...string) *Proc {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	c.Path = exe

	c.Env = append(append(os.Environ(), helperEnv+"="+mode), extra...)

	p, err := Start(c)
	require.NoError(t, err)

	t.Cleanup(func() { _ = p.Stop(100 * time.Millisecond) })

	return p
}

func readPID(t *testing.T, path string) int {
	t.Helper()

	var pid int

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}

		pid, err = strconv.Atoi(string(b))

		return err == nil
	}, 5*time.Second, 20*time.Millisecond)

	return pid
}

func requireGone(t *testing.T, pid int) {
	t.Helper()

	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}, 2*time.Second, 20*time.Millisecond)
}

func waitWithin(t *testing.T, p *Proc, d time.Duration) error {
	t.Helper()

	errc := make(chan error, 1)

	go func() { errc <- p.Wait() }()

	select {
	case err := <-errc:
		return err
	case <-time.After(d):
		require.FailNow(t, "Wait did not return in time")

		return nil
	}
}

func TestProcLines(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "lines")

	var got []string
	for l := range p.Lines() {
		got = append(got, string(l))
	}

	assert.Equal(t, []string{"one", "two", "three"}, got)
	require.NoError(t, p.Wait())
}

func TestProcLongLines(t *testing.T) {
	t.Parallel()

	t.Run("within cap", func(t *testing.T) {
		t.Parallel()

		p := startHelper(t, "long")

		var got [][]byte
		for l := range p.Lines() {
			got = append(got, l)
		}

		require.Len(t, got, 2)
		assert.Equal(t, bytes.Repeat([]byte("x"), 5<<20), got[0])
		assert.Equal(t, "ok", string(got[1]))
		require.NoError(t, p.Wait())
	})

	t.Run("over cap", func(t *testing.T) {
		t.Parallel()

		p := startHelperCmd(t, Cmd{MaxLine: 1024}, "long2k")

		closed := make(chan struct{})

		go func() {
			for range p.Lines() {
			}

			close(closed)
		}()

		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "Lines did not close")
		}

		err := waitWithin(t, p, 5*time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "1024")
	})
}

func TestProcStopKillsGroup(t *testing.T) {
	t.Parallel()

	pidFile := t.TempDir() + "/pid"
	p := startHelper(t, "group", "SUBPROC_PIDFILE="+pidFile)
	pid := readPID(t, pidFile)

	require.NoError(t, p.Stop(500*time.Millisecond))
	requireGone(t, pid)
}

func TestProcStopEscalates(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "ignoreterm")

	select {
	case <-p.Lines():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "helper not ready")
	}

	require.NoError(t, p.Stop(200*time.Millisecond))
	_ = waitWithin(t, p, 2*time.Second)
}

func TestProcStopIdempotentAfterExit(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "lines")

	for range p.Lines() {
	}

	require.NoError(t, p.Wait())
	require.NoError(t, p.Stop(100*time.Millisecond))
	require.NoError(t, p.Stop(100*time.Millisecond))
}

func TestProcStderrTail(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "stderr")

	for range p.Lines() {
	}

	require.NoError(t, p.Wait())

	tail := p.StderrTail()
	assert.Len(t, tail, 16<<10)
	assert.True(t, strings.HasSuffix(tail, "aaa\nEND"))
}

func TestProcWaitKillsLeftoverGroup(t *testing.T) {
	t.Parallel()

	pidFile := t.TempDir() + "/pid"
	p := startHelper(t, "leaderexits", "SUBPROC_PIDFILE="+pidFile)
	pid := readPID(t, pidFile)

	require.NoError(t, waitWithin(t, p, 5*time.Second))
	requireGone(t, pid)
}

func TestProcWaitAfterStopWithoutReader(t *testing.T) {
	t.Parallel()

	p := startHelper(t, "flood")

	// Let the reader block on an undelivered line.
	time.Sleep(200 * time.Millisecond)

	require.NoError(t, p.Stop(200*time.Millisecond))
	_ = waitWithin(t, p, 5*time.Second)
}
