package sim

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Supervisor runs a program (the worker or the naive receiver), restarts it
// whenever it exits, and can hard-kill it at random moments. Each start gets a
// different FAULT_SEED so a crash is never replayed at the same point forever.
type Supervisor struct {
	Path         string
	LogPath      string
	CrashRate    float64       // passed to the program as FAULT_CRASH_RATE
	KillInterval time.Duration // average time between hard kills; 0 = none
	Seed         int64

	mu     sync.Mutex
	cmd    *exec.Cmd
	starts int
	kills  int
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Supervisor) Start() error {
	logFile, err := os.OpenFile(s.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(ctx, logFile)
	if s.KillInterval > 0 {
		go s.killRandomly(ctx)
	}
	return nil
}

func (s *Supervisor) run(ctx context.Context, logFile *os.File) {
	defer close(s.done)
	defer logFile.Close()
	for ctx.Err() == nil {
		s.mu.Lock()
		s.starts++
		cmd := exec.Command(s.Path)
		cmd.Env = append(cleanEnv(),
			fmt.Sprintf("FAULT_CRASH_RATE=%g", s.CrashRate),
			fmt.Sprintf("FAULT_SEED=%d", s.Seed*100000+int64(s.starts)))
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		err := cmd.Start()
		if err == nil {
			s.cmd = cmd
		}
		s.mu.Unlock()

		if err != nil {
			fmt.Fprintf(logFile, "supervisor: start failed: %v\n", err)
		} else {
			cmd.Wait()
			s.mu.Lock()
			s.cmd = nil
			s.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Supervisor) killRandomly(ctx context.Context) {
	rng := rand.New(rand.NewSource(s.Seed))
	for {
		wait := time.Duration(rng.Int63n(int64(2*s.KillInterval))) + time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.mu.Lock()
		if s.cmd != nil && s.cmd.Process != nil {
			if s.cmd.Process.Kill() == nil {
				s.kills++
			}
		}
		s.mu.Unlock()
	}
}

// Stop kills the program and stops restarting it.
func (s *Supervisor) Stop() {
	s.cancel()
	s.mu.Lock()
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.mu.Unlock()
	<-s.done
}

// Restarts is how many times the program had to be started again.
func (s *Supervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.starts == 0 {
		return 0
	}
	return s.starts - 1
}

func (s *Supervisor) Kills() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kills
}

// cleanEnv drops the single-purpose crash switches so a leftover shell
// variable cannot make every start crash at the same point.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "WORKER_CRASH_AFTER_GRANT=") ||
			strings.HasPrefix(kv, "NAIVE_CRASH_AFTER_GRANT=") ||
			strings.HasPrefix(kv, "FAULT_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
