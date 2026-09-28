package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// normalizedCommandTransport accepts horizontal whitespace after a JSON-RPC
// message. The Go MCP SDK requires the newline to follow JSON immediately,
// while some stdio upstreams pad responses with spaces before that newline.
type normalizedCommandTransport struct {
	command *exec.Cmd
}

func (t *normalizedCommandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	stdout, err := t.command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := t.command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := t.command.Start(); err != nil {
		return nil, err
	}
	reader := &trimmedLineReader{source: bufio.NewReader(stdout)}
	connection, err := (&mcp.IOTransport{Reader: io.NopCloser(reader), Writer: stdin}).Connect(ctx)
	if err != nil {
		stdin.Close()
		t.command.Process.Kill()
		t.command.Wait()
		return nil, err
	}
	return &commandConnection{Connection: connection, command: t.command}, nil
}

type trimmedLineReader struct {
	source  *bufio.Reader
	pending []byte
	err     error
}

func (r *trimmedLineReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		line, err := r.source.ReadBytes('\n')
		if len(line) == 0 {
			return 0, err
		}
		r.err = err
		terminated := line[len(line)-1] == '\n'
		if terminated {
			line = line[:len(line)-1]
		}
		r.pending = bytes.TrimRight(line, " \t\r")
		if terminated {
			r.pending = append(r.pending, '\n')
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

type commandConnection struct {
	mcp.Connection
	command *exec.Cmd
	once    sync.Once
	err     error
}

func (c *commandConnection) Close() error {
	c.once.Do(func() {
		closeErr := c.Connection.Close()
		finished := make(chan error, 1)
		go func() { finished <- c.command.Wait() }()
		wait := func() (error, bool) {
			select {
			case err := <-finished:
				return err, true
			case <-time.After(5 * time.Second):
				return nil, false
			}
		}
		if waitErr, done := wait(); done {
			c.err = errors.Join(closeErr, waitErr)
			return
		}
		if err := c.command.Process.Signal(syscall.SIGTERM); err == nil {
			if waitErr, done := wait(); done {
				c.err = errors.Join(closeErr, waitErr)
				return
			}
		}
		if err := c.command.Process.Kill(); err != nil {
			c.err = errors.Join(closeErr, err)
			return
		}
		if waitErr, done := wait(); done {
			c.err = errors.Join(closeErr, waitErr)
		} else {
			c.err = errors.Join(closeErr, errors.New("unresponsive upstream process"))
		}
	})
	return c.err
}

var _ mcp.Transport = (*normalizedCommandTransport)(nil)
var _ mcp.Connection = (*commandConnection)(nil)
var _ io.Reader = (*trimmedLineReader)(nil)
