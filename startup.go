package main

import (
	"os"
	"sync"
	"syscall"
	"time"
)

// startupAttempt owns termination of a stdio process while startup is pending.
// It also catches a process whose Start returns after the deadline.
type startupAttempt struct {
	mu      sync.Mutex
	phase   string
	process *os.Process
	stopped bool
}

func newStartupAttempt() *startupAttempt {
	return &startupAttempt{phase: "launch"}
}

func (a *startupAttempt) setPhase(phase string) {
	a.mu.Lock()
	a.phase = phase
	a.mu.Unlock()
}

func (a *startupAttempt) started(process *os.Process) {
	a.mu.Lock()
	a.process = process
	stopped := a.stopped
	a.mu.Unlock()
	if stopped {
		terminateProcessGroup(process.Pid)
	}
}

func (a *startupAttempt) snapshot() (string, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pid := 0
	if a.process != nil {
		pid = a.process.Pid
	}
	return a.phase, pid
}

func (a *startupAttempt) stop() {
	a.mu.Lock()
	a.stopped = true
	process := a.process
	a.mu.Unlock()
	if process != nil {
		terminateProcessGroup(process.Pid)
	}
}

// The process was started in its own group. Neither signal can reach the host.
func terminateProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return
	}
	go func() {
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()
}
