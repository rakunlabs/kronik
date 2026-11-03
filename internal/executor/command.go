package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type CommandConfig struct {
	// Command is run by the shell, e.g. `/bin/sh -c "<command>"`.
	Command string            `json:"command"`
	Env     map[string]string `json:"env"`
	Dir     string            `json:"dir"`
}

type Command struct {
	shell string
}

func NewCommand(shell string) *Command {
	return &Command{shell: shell}
}

func (c *Command) Validate(raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := decode[CommandConfig](raw)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(cfg.Command) == "" {
		return nil, invalid("command is required")
	}

	for k := range cfg.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return nil, invalid("environment variable name %q is not valid", k)
		}
	}

	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}

	return json.Marshal(cfg)
}

// commandEnv is the replica's environment without kronik's own settings,
// which hold the database password and the script secrets.
func commandEnv(environ []string) []string {
	out := make([]string, 0, len(environ))

	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")

		switch {
		case strings.HasPrefix(k, "KRONIK_"), strings.HasPrefix(k, "CONFIG_"), strings.HasPrefix(k, "OTEL_"),
			strings.HasPrefix(k, "VAULT_"), strings.HasPrefix(k, "CONSUL_"):
			continue
		}

		out = append(out, kv)
	}

	return out
}

// waitDelay is how long a stopped command has to exit after SIGTERM before
// it is killed.
const waitDelay = 10 * time.Second

func (c *Command) Run(ctx context.Context, info Info, raw json.RawMessage, out *Output) (*int, error) {
	cfg, err := decode[CommandConfig](raw)
	if err != nil {
		return nil, err
	}

	out.Redact(SecretValues(info.Secrets))

	env, err := expandMap(cfg.Env, info)
	if err != nil {
		return nil, fmt.Errorf("env %w", err)
	}

	cmd := exec.CommandContext(ctx, c.shell, "-c", cfg.Command)
	cmd.Dir = cfg.Dir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = waitDelay

	cmd.Env = append(commandEnv(os.Environ()),
		"KRONIK_JOB_ID="+info.JobID,
		"KRONIK_JOB_NAME="+info.JobName,
		"KRONIK_RUN_ID="+info.RunID,
		"KRONIK_ATTEMPT="+strconv.Itoa(info.Attempt),
		"KRONIK_SCHEDULED_AT="+info.ScheduledAt.UTC().Format(time.RFC3339),
	)
	for k, v := range paramEnv(info) {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	cleanup := processGroup(cmd)
	err = cmd.Run()
	cleanup()

	// The shell exited fine, but something it started in the background
	// kept the output open; that is stopped, the run is not failed for it.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		out.Printf("\n[the command left a process running that kept the output open; it was stopped]\n")

		err = nil
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			if code < 0 {
				return nil, fmt.Errorf("stopped: %s", exitErr.String())
			}

			return &code, fmt.Errorf("exit code %d", code)
		}

		return nil, err
	}

	code := 0

	return &code, nil
}
