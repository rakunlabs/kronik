package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// DockerOptions are the Docker settings in effect.
type DockerOptions struct {
	Host            string
	AllowedImages   []string
	Network         string
	AllowPrivileged bool
	AllowMounts     bool
	Pull            string
}

// Docker runs a job's container on a Docker Engine and removes it after.
type Docker struct {
	opts DockerOptions

	mu     sync.Mutex
	client *client.Client
}

func NewDocker(opts DockerOptions) *Docker {
	return &Docker{opts: opts}
}

// dockerClient connects on first use, so a replica without an engine starts
// fine and only Docker runs fail.
func dockerClient(host string) (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}

	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker engine %s: %w", host, err)
	}

	return c, nil
}

func (d *Docker) cli() (*client.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.client != nil {
		return d.client, nil
	}

	c, err := dockerClient(d.opts.Host)
	if err != nil {
		return nil, err
	}

	d.client = c

	return c, nil
}

// Close drops the connection to the engine.
func (d *Docker) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.client == nil {
		return nil
	}

	err := d.client.Close()
	d.client = nil

	return err
}

func (d *Docker) policy() Policy {
	return Policy{AllowedImages: d.opts.AllowedImages, AllowPrivileged: d.opts.AllowPrivileged, AllowMounts: d.opts.AllowMounts}
}

func (d *Docker) Validate(raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := validateContainer(raw, d.policy())
	if err != nil {
		return nil, err
	}

	if cfg.Namespace != "" || cfg.ServiceAccount != "" || len(cfg.NodeSelector) > 0 || len(cfg.SecretEnv) > 0 {
		return nil, invalid("namespace, service_account, node_selector and secret_env are for Kubernetes jobs")
	}

	if len(cfg.Constraints) > 0 || len(cfg.Networks) > 0 {
		return nil, invalid("constraints and networks are for Swarm jobs; use network")
	}

	return json.Marshal(cfg)
}

// stopGrace is how long a container has to exit after SIGTERM.
const stopGrace = 10

func (d *Docker) Run(ctx context.Context, info Info, raw json.RawMessage, out *Output) (*int, error) {
	cfg, err := decode[ContainerConfig](raw)
	if err != nil {
		return nil, err
	}

	// The config was checked when saved; the policy may have changed since.
	if _, err := validateContainer(raw, d.policy()); err != nil {
		return nil, err
	}

	cli, err := d.cli()
	if err != nil {
		return nil, err
	}

	pull := cfg.Pull
	if pull == "" {
		pull = d.opts.Pull
	}

	if err := pullImage(ctx, cli, cfg.Image, pull, out); err != nil {
		return nil, err
	}

	hc := &container.HostConfig{Privileged: cfg.Privileged, Init: ptrTrue()}

	network := cfg.Network
	if network == "" {
		network = d.opts.Network
	}

	if network != "" {
		hc.NetworkMode = container.NetworkMode(network)
	}

	if cfg.CPU != "" {
		hc.NanoCPUs, _ = parseCPU(cfg.CPU)
	}

	if cfg.Memory != "" {
		hc.Memory, _ = parseBytes(cfg.Memory)
	}

	for _, m := range cfg.Mounts {
		hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.Type(m.Type), Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "kronik-" + shortID(info.RunID),
		Config: &container.Config{
			Image:      cfg.Image,
			Entrypoint: cfg.Command,
			Cmd:        cfg.Args,
			Env:        envList(runEnv(info, cfg.Env)),
			WorkingDir: cfg.Workdir,
			User:       cfg.User,
			Labels:     labels(info, cfg.Labels),
		},
		HostConfig: hc,
	})
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	id := created.ID
	for _, w := range created.Warnings {
		out.Printf("[docker] %s\n", w)
	}

	// Removed whatever happens, also when the run is canceled.
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if _, err := cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			out.Printf("[docker] removing container %s: %v\n", shortID(id), err)
		}
	}()

	// Wait is set up before the start, so a fast exit is not missed.
	wait := cli.ContainerWait(context.WithoutCancel(ctx), id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	if _, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	out.Printf("[docker] container %s started from %s\n", shortID(id), cfg.Image)

	logsDone := make(chan struct{})

	go func() {
		defer close(logsDone)
		streamLogs(context.WithoutCancel(ctx), cli, id, out)
	}()

	var exit int64

	select {
	case res := <-wait.Result:
		exit = res.StatusCode
		if res.Error != nil && res.Error.Message != "" {
			out.Printf("[docker] %s\n", res.Error.Message)
		}
	case err := <-wait.Error:
		return nil, fmt.Errorf("wait for container: %w", err)
	case <-ctx.Done():
		// A cancel or timeout stops the container: SIGTERM, then SIGKILL.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), (stopGrace+10)*time.Second)
		defer cancel()

		grace := stopGrace
		_, _ = cli.ContainerStop(stopCtx, id, client.ContainerStopOptions{Timeout: &grace})

		select {
		case <-logsDone:
		case <-time.After(5 * time.Second):
		}

		return nil, ctx.Err()
	}

	select {
	case <-logsDone:
	case <-time.After(10 * time.Second):
	}

	code := int(exit)
	if code != 0 {
		return &code, fmt.Errorf("exit code %d", code)
	}

	return &code, nil
}

// streamLogs copies the container's stdout and stderr into the output while
// it runs.
func streamLogs(ctx context.Context, cli *client.Client, id string, out *Output) {
	logs, err := cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		out.Printf("[docker] reading logs: %v\n", err)

		return
	}
	defer func() { _ = logs.Close() }()

	if _, err := stdcopy.StdCopy(out, out, logs); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		out.Printf("[docker] reading logs: %v\n", err)
	}
}

// pullImage pulls image per policy: missing (when not there), always or never.
func pullImage(ctx context.Context, cli *client.Client, image, policy string, out *Output) error {
	if policy == "never" {
		return nil
	}

	if policy != "always" {
		if _, err := cli.ImageInspect(ctx, image); err == nil {
			return nil
		} else if !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("inspect image %s: %w", image, err)
		}
	}

	out.Printf("[docker] pulling %s\n", image)

	resp, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}

	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}

	return nil
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return out
}

func ptrTrue() *bool {
	t := true

	return &t
}
