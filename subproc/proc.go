//go:build unix

package subproc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// MaxLine is the default cap on one stdout line.
const MaxLine = 64 << 20

const stderrTailSize = 16 << 10

// Cmd describes the process to run.
type Cmd struct {
	Path string
	Args []string
	Dir  string
	Env  []string
	// MaxLine caps one stdout line; 0 means the package's MaxLine.
	MaxLine int
}

// Proc is a running child in its own process group.
type Proc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	maxLine int
	tail    *tailBuffer

	abandon     chan struct{}
	abandonOnce sync.Once

	readErrMu sync.Mutex
	readErr   error

	done    chan struct{}
	waitErr error // set before done is closed
}

// Start runs cmd in its own process group with piped stdin, stdout split
// into lines, and a bounded stderr tail. Cancellation is the caller's Stop,
// because a context kill would reach only the group leader.
func Start(c Cmd) (*Proc, error) {
	maxLine := c.MaxLine
	if maxLine <= 0 {
		maxLine = MaxLine
	}

	cmd := exec.Command(c.Path, c.Args...) //nolint:gosec,noctx // caller-chosen binary; Stop signals the whole group
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Raw pipes keep cmd.Wait from closing the read ends under the readers.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)

		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	cmd.Stdout = outW
	cmd.Stderr = errW

	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeAll(outR, outW, errR, errW)

		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW)

		return nil, fmt.Errorf("start %s: %w", c.Path, err)
	}

	closeAll(outW, errW)

	p := &Proc{
		cmd:     cmd,
		stdin:   stdin,
		lines:   make(chan []byte),
		maxLine: maxLine,
		tail:    &tailBuffer{},
		abandon: make(chan struct{}),
		done:    make(chan struct{}),
	}

	var readers sync.WaitGroup

	readers.Add(2)

	go func() {
		defer readers.Done()

		p.readStdout(outR)
	}()

	go func() {
		defer readers.Done()

		_, _ = io.Copy(p.tail, errR)
	}()

	go func() {
		exitErr := cmd.Wait()

		// Kill what the leader left behind so a grandchild holding a pipe
		// can neither outlive the run nor hang the readers.
		_ = p.signalGroup(syscall.SIGKILL)

		readers.Wait()
		closeAll(outR, errR)

		p.waitErr = errors.Join(exitErr, p.getReadErr())

		close(p.done)
	}()

	return p, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// Stdin is the child's standard input.
func (p *Proc) Stdin() io.WriteCloser { return p.stdin }

// Lines yields stdout lines without the trailing newline. It is closed at
// stdout EOF or on a read error.
func (p *Proc) Lines() <-chan []byte { return p.lines }

// StderrTail returns the last 16 KiB of stderr.
func (p *Proc) StderrTail() string { return p.tail.String() }

// Stop sends SIGTERM to the process group and SIGKILL after grace, to the
// group and, on Linux, to every descendant that left it. It is idempotent and
// returns nil once the process has exited.
func (p *Proc) Stop(grace time.Duration) error {
	p.abandonLines()

	select {
	case <-p.done:
		return nil
	default:
	}

	if err := p.signalGroup(syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate process group: %w", err)
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-p.done:
		return nil
	case <-timer.C:
	}

	killDescendants(p.cmd.Process.Pid)

	err := p.signalGroup(syscall.SIGKILL)

	<-p.done

	if err != nil {
		return fmt.Errorf("kill process group: %w", err)
	}

	return nil
}

// Wait returns the exit status joined with any stdout read error. Lines not
// yet delivered are dropped, so it returns even if nobody reads Lines.
func (p *Proc) Wait() error {
	p.abandonLines()

	<-p.done

	return p.waitErr
}

func (p *Proc) abandonLines() {
	p.abandonOnce.Do(func() { close(p.abandon) })
}

// signalGroup treats an already-empty group (ESRCH) as success.
func (p *Proc) signalGroup(sig syscall.Signal) error {
	err := syscall.Kill(-p.cmd.Process.Pid, sig)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

func (p *Proc) setReadErr(err error) {
	p.readErrMu.Lock()
	defer p.readErrMu.Unlock()

	p.readErr = err
}

func (p *Proc) getReadErr() error {
	p.readErrMu.Lock()
	defer p.readErrMu.Unlock()

	return p.readErr
}

func (p *Proc) readStdout(r io.Reader) {
	defer close(p.lines)

	br := bufio.NewReaderSize(r, 64<<10)

	var acc []byte

	for {
		chunk, err := br.ReadSlice('\n')
		acc = append(acc, chunk...)

		content := len(acc)
		if bytes.HasSuffix(acc, []byte("\n")) {
			content--
		}

		if content > p.maxLine {
			p.setReadErr(fmt.Errorf("stdout line exceeds the %d byte limit", p.maxLine))
			// Killing is the only way to stop a child that keeps writing a
			// line that will never fit.
			_ = p.signalGroup(syscall.SIGKILL)

			return
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}

		if err == nil || len(acc) > 0 && errors.Is(err, io.EOF) {
			if !p.deliver(bytes.TrimSuffix(acc, []byte("\n"))) {
				// Keep draining so a child blocked on a full pipe can exit.
				_, _ = io.Copy(io.Discard, br)

				return
			}

			acc = nil
		}

		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				p.setReadErr(fmt.Errorf("read stdout: %w", err))
			}

			return
		}
	}
}

// deliver sends a copy of line, reporting false once Stop or Wait has begun.
func (p *Proc) deliver(line []byte) bool {
	select {
	case p.lines <- bytes.Clone(line):
		return true
	case <-p.abandon:
		return false
	}
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, data...)
	if over := len(b.buf) - stderrTailSize; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}

	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buf)
}
