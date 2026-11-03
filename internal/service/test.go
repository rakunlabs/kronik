package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/store"
)

// TestRun is the outcome of a job tried once without saving it.
type TestRun struct {
	Status     string `json:"status"`
	Error      string `json:"error"`
	Output     string `json:"output"`
	Truncated  bool   `json:"output_truncated"`
	Result     string `json:"result"`
	ResultCode *int   `json:"result_code"`
	DurationMs int64  `json:"duration_ms"`
}

const (
	testTimeout   = time.Minute
	testMaxOutput = 256 << 10
)

// Test runs a job's executor once on this replica, as given and without
// saving anything: what the editor's "Test run" shows. It is bound by the
// job's timeout, and by a minute at most.
func (s *Service) Test(ctx context.Context, in JobInput) (*TestRun, error) {
	exec, err := s.executors.Get(in.Executor)
	if err != nil {
		return nil, invalid("%v", err)
	}

	cfg, err := exec.Validate(in.Config)
	if err != nil {
		return nil, invalid("config: %v", err)
	}

	select {
	case s.tests <- struct{}{}:
		defer func() { <-s.tests }()
	default:
		return nil, fmt.Errorf("%w: %d test runs are going on already, try again in a moment", ErrBusy, maxTests)
	}

	timeout := testTimeout
	if in.TimeoutSeconds > 0 {
		timeout = min(timeout, time.Duration(in.TimeoutSeconds)*time.Second)
	}

	ctx, cancel := context.WithTimeoutCause(ctx, timeout, fmt.Errorf("the test run stopped after %s", timeout))
	defer cancel()

	name := in.Name
	if name == "" {
		name = "test"
	}

	s.audit(ctx, ActionScriptTest, &store.Job{Name: name}, "", map[string]any{"executor": in.Executor, "config": auditConfig(in.Executor, cfg)})

	out := executor.NewOutput(testMaxOutput)
	start := time.Now()

	code, runErr := runSafely(ctx, exec, executor.Info{JobName: name, RunID: "test", Attempt: 1, ScheduledAt: start.UTC()}, cfg, out)

	t := &TestRun{Status: store.StatusSucceeded, ResultCode: code, DurationMs: time.Since(start).Milliseconds(), Result: out.Result()}
	t.Output, t.Truncated, _ = out.Snapshot()

	if runErr != nil {
		t.Status, t.Error = store.StatusFailed, runErr.Error()
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			t.Status, t.Error = store.StatusTimedOut, cause.Error()
		}
	}

	return t, nil
}

// CheckScript reports the problems of a script for the editor.
func CheckScript(src string) []executor.Problem {
	return executor.CheckJS(src)
}

func runSafely(ctx context.Context, exec executor.Executor, info executor.Info, cfg json.RawMessage, out *executor.Output) (code *int, err error) {
	defer func() {
		if r := recover(); r != nil {
			code, err = nil, fmt.Errorf("internal error in the executor: %v", r)
		}
	}()

	return exec.Run(ctx, info, cfg, out)
}

// auditConfig is what a test ran, for the audit log: the script or command,
// and the URL without headers or body, which may hold credentials.
func auditConfig(executorName string, cfg json.RawMessage) any {
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil {
		return nil
	}

	const maxText = 4000

	cut := func(v any) any {
		if s, ok := v.(string); ok && len(s) > maxText {
			return s[:maxText] + "…"
		}

		return v
	}

	switch executorName {
	case executor.TypeHTTP:
		return map[string]any{"method": m["method"], "url": m["url"]}
	case executor.TypeCommand:
		return map[string]any{"command": cut(m["command"])}
	case executor.TypeJS:
		return map[string]any{"script": cut(m["script"])}
	}

	return nil
}
