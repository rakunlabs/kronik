package executor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// dockerOrSkip skips unless a Docker Engine answers.
func dockerOrSkip(t *testing.T) {
	t.Helper()

	if os.Getenv("KRONIK_TEST_DOCKER") == "" {
		t.Skip("KRONIK_TEST_DOCKER is not set")
	}

	c, err := dockerClient("")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skip("no docker engine: ", err)
	}
}

const testImage = "alpine:3.22"

func TestImageAllowed(t *testing.T) {
	for _, tc := range []struct {
		image    string
		patterns []string
		want     bool
	}{
		{"alpine:3.22", nil, true},
		{"alpine:3.22", []string{"alpine:*"}, true},
		{"alpine:3.22", []string{"alpine"}, true},
		{"ghcr.io/acme/team/tool:1", []string{"ghcr.io/acme/*"}, true},
		{"ghcr.io/evil/tool:1", []string{"ghcr.io/acme/*"}, false},
		{"busybox", []string{"alpine:*"}, false},
		{"registry:5000/app:1", []string{"registry:5000/app"}, true},
		{"alpine@sha256:abc", []string{"alpine"}, true},
	} {
		if got := imageAllowed(tc.image, tc.patterns); got != tc.want {
			t.Errorf("imageAllowed(%q, %v) = %v", tc.image, tc.patterns, got)
		}
	}
}

func TestContainerValidate(t *testing.T) {
	d := NewDocker(DockerOptions{AllowedImages: []string{"alpine:*"}})

	for _, raw := range []string{
		`{}`,
		`{"image":"busybox"}`,
		`{"image":"alpine:3","privileged":true}`,
		`{"image":"alpine:3","mounts":[{"source":"/","target":"/host"}]}`,
		`{"image":"alpine:3","cpu":"lots"}`,
		`{"image":"alpine:3","memory":"-1"}`,
		`{"image":"alpine:3","namespace":"x"}`,
		`{"image":"alpine:3","env":{"BAD NAME":"x"}}`,
	} {
		if _, err := d.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("Validate(%s) succeeded", raw)
		}
	}

	if _, err := d.Validate(json.RawMessage(`{"image":"alpine:3","cpu":"0.5","memory":"64Mi","command":["sh","-c"],"args":["echo hi"]}`)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		in   string
		want int64
	}{{"0.5", 5e8}, {"2", 2e9}, {"500m", 5e8}} {
		if got, _ := parseCPU(tc.in); got != tc.want {
			t.Errorf("parseCPU(%s) = %d", tc.in, got)
		}
	}

	for _, tc := range []struct {
		in   string
		want int64
	}{{"64Mi", 64 << 20}, {"1Gi", 1 << 30}, {"1G", 1e9}, {"1024", 1024}} {
		if got, _ := parseBytes(tc.in); got != tc.want {
			t.Errorf("parseBytes(%s) = %d", tc.in, got)
		}
	}
}

func TestSwarmRun(t *testing.T) {
	dockerOrSkip(t)

	s := NewSwarm(SwarmOptions{})
	defer func() { _ = s.Close() }()

	cli, err := s.cli()
	if err != nil {
		t.Fatal(err)
	}

	if info, err := cli.Info(context.Background(), client.InfoOptions{}); err != nil || info.Info.Swarm.LocalNodeState != "active" {
		t.Skip("not a swarm manager")
	}

	info := Info{JobID: "job-swarm", JobName: "swarm-test", RunID: "01testrunswarm", Attempt: 1, ScheduledAt: time.Now()}

	cfg, err := s.Validate(json.RawMessage(`{"image":"` + testImage + `","command":["sh","-c"],"args":["echo swarm $KRONIK_RUN_ID; exit 4"]}`))
	if err != nil {
		t.Fatal(err)
	}

	out := NewOutput(1 << 16)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	code, err := s.Run(ctx, info, cfg, out)

	text, _, _ := out.Snapshot()
	if err == nil || code == nil || *code != 4 || !strings.Contains(text, "swarm 01testrunswarm") {
		t.Fatalf("code=%v err=%v output:\n%s", code, err, text)
	}

	cfg, _ = s.Validate(json.RawMessage(`{"image":"` + testImage + `","command":["true"]}`))
	info.RunID = "01testrunswarm2"

	if code, err := s.Run(ctx, info, cfg, NewOutput(1000)); err != nil || code == nil || *code != 0 {
		t.Fatalf("success: code=%v err=%v", code, err)
	}

	list, err := cli.ServiceList(context.Background(), client.ServiceListOptions{Filters: make(client.Filters).Add("label", "kronik.job=job-swarm")})
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("services left: %v %v", list, err)
	}
}

func TestDockerRun(t *testing.T) {
	dockerOrSkip(t)

	d := NewDocker(DockerOptions{Pull: "missing"})
	defer func() { _ = d.Close() }()

	info := Info{JobID: "job1", JobName: "docker-test", RunID: "01testrundocker", Attempt: 1, ScheduledAt: time.Now()}

	cfg, err := d.Validate(json.RawMessage(`{"image":"` + testImage + `","command":["sh","-c"],` +
		`"args":["echo out $KRONIK_JOB_NAME $FOO; echo err >&2; exit 3"],"env":{"FOO":"bar"},"memory":"64Mi"}`))
	if err != nil {
		t.Fatal(err)
	}

	out := NewOutput(1 << 16)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	code, err := d.Run(ctx, info, cfg, out)

	text, _, _ := out.Snapshot()
	if err == nil || code == nil || *code != 3 || !strings.Contains(text, "out docker-test bar") || !strings.Contains(text, "err") {
		t.Fatalf("code=%v err=%v output:\n%s", code, err, text)
	}

	// Canceled: the container is stopped and removed.
	cfg, _ = d.Validate(json.RawMessage(`{"image":"` + testImage + `","command":["sleep","300"]}`))
	info.RunID = "01testrundocker2"

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	start := time.Now()
	if _, err := d.Run(ctx2, info, cfg, NewOutput(1000)); err == nil {
		t.Fatal("canceled run succeeded")
	}

	if time.Since(start) > 30*time.Second {
		t.Fatalf("took %s to stop", time.Since(start))
	}

	cli, _ := d.cli()

	list, err := cli.ContainerList(context.Background(), client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", "kronik.job=job1")})
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("containers left: %v %v", list, err)
	}
}
