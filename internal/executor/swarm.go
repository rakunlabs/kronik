package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// SwarmOptions are the Swarm settings in effect.
type SwarmOptions struct {
	Host          string
	AllowedImages []string
	Networks      []string
	AllowMounts   bool
}

// Swarm runs a job as a one-off service (mode replicated-job) on a Swarm
// cluster, which places its task on any node, and removes it after.
type Swarm struct {
	opts SwarmOptions

	mu     sync.Mutex
	client *client.Client
}

func NewSwarm(opts SwarmOptions) *Swarm {
	return &Swarm{opts: opts}
}

func (s *Swarm) cli() (*client.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		return s.client, nil
	}

	c, err := dockerClient(s.opts.Host)
	if err != nil {
		return nil, err
	}

	s.client = c

	return c, nil
}

func (s *Swarm) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil {
		return nil
	}

	err := s.client.Close()
	s.client = nil

	return err
}

func (s *Swarm) policy() Policy {
	return Policy{AllowedImages: s.opts.AllowedImages, AllowMounts: s.opts.AllowMounts}
}

func (s *Swarm) Validate(raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := validateContainer(raw, s.policy())
	if err != nil {
		return nil, err
	}

	if cfg.Namespace != "" || cfg.ServiceAccount != "" || len(cfg.NodeSelector) > 0 || len(cfg.SecretEnv) > 0 {
		return nil, invalid("namespace, service_account, node_selector and secret_env are for Kubernetes jobs; use constraints")
	}

	if cfg.Privileged {
		return nil, invalid("Swarm services cannot be privileged")
	}

	if cfg.Network != "" {
		return nil, invalid("use networks (a list) for a Swarm job")
	}

	if cfg.Pull != "" && cfg.Pull != "always" {
		// Swarm nodes pull the image themselves.
		cfg.Pull = ""
	}

	return json.Marshal(cfg)
}

// swarmPoll is how often a task's state is read.
const swarmPoll = time.Second

func (s *Swarm) Run(ctx context.Context, info Info, raw json.RawMessage, out *Output) (*int, error) {
	cfg, err := decode[ContainerConfig](raw)
	if err != nil {
		return nil, err
	}

	if _, err := validateContainer(raw, s.policy()); err != nil {
		return nil, err
	}

	cli, err := s.cli()
	if err != nil {
		return nil, err
	}

	one := uint64(1)
	grace := stopGrace * time.Second

	spec := swarm.ContainerSpec{
		Image:           cfg.Image,
		Command:         cfg.Command,
		Args:            cfg.Args,
		Env:             envList(runEnv(info, cfg.Env)),
		Dir:             cfg.Workdir,
		User:            cfg.User,
		Labels:          labels(info, cfg.Labels),
		Init:            ptrTrue(),
		StopGracePeriod: &grace,
	}

	for _, m := range cfg.Mounts {
		spec.Mounts = append(spec.Mounts, mount.Mount{Type: mount.Type(m.Type), Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}

	task := swarm.TaskSpec{
		ContainerSpec: &spec,
		RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
	}

	if len(cfg.Constraints) > 0 {
		task.Placement = &swarm.Placement{Constraints: cfg.Constraints}
	}

	if cfg.CPU != "" || cfg.Memory != "" {
		limit := &swarm.Limit{}
		limit.NanoCPUs, _ = parseCPU(cfg.CPU)
		limit.MemoryBytes, _ = parseBytes(cfg.Memory)
		task.Resources = &swarm.ResourceRequirements{Limits: limit}
	}

	networks := cfg.Networks
	if len(networks) == 0 {
		networks = s.opts.Networks
	}

	for _, n := range networks {
		task.Networks = append(task.Networks, swarm.NetworkAttachmentConfig{Target: n})
	}

	name := "kronik-" + shortID(info.RunID)

	created, err := cli.ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec: swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: name, Labels: labels(info, cfg.Labels)},
			TaskTemplate: task,
			Mode:         swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{MaxConcurrent: &one, TotalCompletions: &one}},
		},
		QueryRegistry: cfg.Pull == "always",
	})
	if err != nil {
		return nil, fmt.Errorf("create service: %w", err)
	}

	id := created.ID
	for _, w := range created.Warnings {
		out.Printf("[swarm] %s\n", w)
	}

	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if _, err := cli.ServiceRemove(rmCtx, id, client.ServiceRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			out.Printf("[swarm] removing service %s: %v\n", name, err)
		}
	}()

	out.Printf("[swarm] service %s created from %s\n", name, cfg.Image)

	logCtx, stopLogs := context.WithCancel(context.WithoutCancel(ctx))
	defer stopLogs()

	logsDone := make(chan struct{})

	go func() {
		defer close(logsDone)
		s.streamLogs(logCtx, cli, id, out)
	}()

	t := time.NewTicker(swarmPoll)
	defer t.Stop()

	lastState := swarm.TaskState("")

	for {
		select {
		case <-ctx.Done():
			// Removing the service stops its task (SIGTERM, then the grace).
			return nil, ctx.Err()
		case <-t.C:
		}

		tasks, err := cli.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", id)})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			out.Printf("[swarm] reading the task: %v\n", err)

			continue
		}

		if len(tasks.Items) == 0 {
			continue
		}

		tk := latestTask(tasks.Items)
		st := tk.Status

		if st.State != lastState {
			lastState = st.State
			if st.State != swarm.TaskStateRunning && st.State != swarm.TaskStateComplete {
				msg := st.Message
				if st.Err != "" {
					msg += ": " + st.Err
				}

				out.Printf("[swarm] task %s on node %s%s\n", st.State, shortID(tk.NodeID), prefixed(msg))
			}
		}

		code := 0
		if st.ContainerStatus != nil {
			code = st.ContainerStatus.ExitCode
		}

		switch st.State {
		case swarm.TaskStateComplete:
			waitLogs(logsDone, stopLogs)

			return &code, nil
		case swarm.TaskStateFailed:
			waitLogs(logsDone, stopLogs)

			if code != 0 {
				return &code, fmt.Errorf("exit code %d", code)
			}

			return nil, fmt.Errorf("the task failed: %s", taskErr(st))
		case swarm.TaskStateRejected, swarm.TaskStateOrphaned, swarm.TaskStateShutdown, swarm.TaskStateRemove:
			waitLogs(logsDone, stopLogs)

			return nil, fmt.Errorf("the task was %s: %s", st.State, taskErr(st))
		}
	}
}

func prefixed(s string) string {
	if s == "" {
		return ""
	}

	return ": " + s
}

func taskErr(st swarm.TaskStatus) string {
	if st.Err != "" {
		return st.Err
	}

	if st.Message != "" {
		return st.Message
	}

	return "no reason given"
}

func latestTask(tasks []swarm.Task) swarm.Task {
	latest := tasks[0]
	for _, t := range tasks[1:] {
		if t.CreatedAt.After(latest.CreatedAt) {
			latest = t
		}
	}

	return latest
}

// waitLogs gives the log stream a moment to deliver the last lines.
func waitLogs(done <-chan struct{}, stop context.CancelFunc) {
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		stop()
		<-done
	}
}

// streamLogs follows the service's log until the service goes or ctx ends.
func (s *Swarm) streamLogs(ctx context.Context, cli *client.Client, id string, out *Output) {
	logs, err := cli.ServiceLogs(ctx, id, client.ServiceLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		if ctx.Err() == nil && !cerrdefs.IsNotFound(err) {
			out.Printf("[swarm] reading logs: %v\n", err)
		}

		return
	}
	defer func() { _ = logs.Close() }()

	if _, err := stdcopy.StdCopy(out, out, logs); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		out.Printf("[swarm] reading logs: %v\n", err)
	}
}
