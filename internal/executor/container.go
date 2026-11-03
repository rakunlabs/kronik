package executor

import (
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	TypeDocker     = "docker"
	TypeSwarm      = "swarm"
	TypeKubernetes = "kubernetes"
)

// ContainerConfig is a job that runs a container: on Docker, as a Swarm
// service or as a Kubernetes Job. Fields that one runtime does not have are
// refused for it.
type ContainerConfig struct {
	Image string `json:"image"`
	// Command replaces the image's entrypoint; Args its command (CMD).
	Command []string          `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	// Workdir is the working directory in the container.
	Workdir string `json:"workdir"`
	// User runs the process as this user (uid, uid:gid or a name).
	User string `json:"user"`
	// CPU is a number of CPUs ("0.5", "2"); Memory a size ("512Mi", "1Gi").
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	// Pull is missing, always or never; empty uses the setting.
	Pull string `json:"pull"`
	// Labels are put on the container, service or Job.
	Labels map[string]string `json:"labels"`

	// Docker and Swarm.
	Network    string   `json:"network"`
	Networks   []string `json:"networks"`
	Mounts     []Mount  `json:"mounts"`
	Privileged bool     `json:"privileged"`
	// Constraints place a Swarm service, e.g. "node.role==worker".
	Constraints []string `json:"constraints"`

	// Kubernetes.
	Namespace      string            `json:"namespace"`
	ServiceAccount string            `json:"service_account"`
	NodeSelector   map[string]string `json:"node_selector"`
	// SecretEnv sets env variables from Kubernetes Secrets: NAME → secret/key.
	SecretEnv map[string]string `json:"secret_env"`
}

// Mount is a host path or a named volume in the container.
type Mount struct {
	// Type is bind (a host path) or volume (a named volume).
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// Policy is what an installation allows container jobs to do.
type Policy struct {
	AllowedImages     []string
	AllowPrivileged   bool
	AllowMounts       bool
	AllowedNamespaces []string
	DefaultNamespace  string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateContainer checks the fields every runtime shares, against the
// policy, and normalizes them.
func validateContainer(raw json.RawMessage, p Policy) (*ContainerConfig, error) {
	cfg, err := decode[ContainerConfig](raw)
	if err != nil {
		return nil, err
	}

	cfg.Image = strings.TrimSpace(cfg.Image)
	if cfg.Image == "" {
		return nil, invalid("image is required")
	}

	if strings.ContainsAny(cfg.Image, " \t\n") {
		return nil, invalid("image %q is not valid", cfg.Image)
	}

	if !imageAllowed(cfg.Image, p.AllowedImages) {
		return nil, invalid("image %q is not allowed here; allowed: %s", cfg.Image, strings.Join(p.AllowedImages, ", "))
	}

	for k := range cfg.Env {
		if !envName.MatchString(k) {
			return nil, invalid("env name %q is not valid", k)
		}
	}

	switch cfg.Pull {
	case "", "missing", "always", "never":
	default:
		return nil, invalid("pull must be missing, always or never")
	}

	if cfg.CPU != "" {
		if _, err := parseCPU(cfg.CPU); err != nil {
			return nil, invalid("cpu %q: %v", cfg.CPU, err)
		}
	}

	if cfg.Memory != "" {
		if _, err := parseBytes(cfg.Memory); err != nil {
			return nil, invalid("memory %q: %v", cfg.Memory, err)
		}
	}

	if cfg.Privileged && !p.AllowPrivileged {
		return nil, invalid("privileged containers are not allowed here (Settings → Configuration)")
	}

	if len(cfg.Mounts) > 0 && !p.AllowMounts {
		return nil, invalid("mounts are not allowed here (Settings → Configuration)")
	}

	for i, m := range cfg.Mounts {
		switch m.Type {
		case "":
			cfg.Mounts[i].Type = "bind"
		case "bind", "volume":
		default:
			return nil, invalid("mount type must be bind or volume")
		}

		if m.Source == "" || !strings.HasPrefix(m.Target, "/") {
			return nil, invalid("a mount needs a source and an absolute target")
		}
	}

	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}

	return cfg, nil
}

// imageAllowed matches an image against patterns such as "alpine:*",
// "ghcr.io/acme/*" or "*". A pattern without a tag also allows every tag
// of that image. No patterns allows any image.
func imageAllowed(image string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}

	name := image
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		name = image[:i]
	}

	name, _, _ = strings.Cut(name, "@")

	for _, p := range patterns {
		if ok, _ := path.Match(p, image); ok {
			return true
		}

		if ok, _ := path.Match(p, name); ok {
			return true
		}

		// "ghcr.io/acme/*" allows "ghcr.io/acme/team/tool:1" too.
		if prefix, found := strings.CutSuffix(p, "/*"); found && strings.HasPrefix(image, prefix+"/") {
			return true
		}
	}

	return false
}

// parseCPU reads "0.5", "2" or the Kubernetes form "500m" as nano CPUs.
func parseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)

	if m, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.ParseInt(m, 10, 64)
		if err != nil || n <= 0 {
			return 0, errBadNumber
		}

		return n * 1_000_000, nil
	}

	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f > 1024 {
		return 0, errBadNumber
	}

	return int64(f * 1e9), nil
}

var errBadNumber = invalid("not a positive number")

// parseBytes reads "512Mi", "1Gi", "256M", "1G" or a number of bytes.
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)

	units := []struct {
		suffix string
		mult   int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	}

	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(n, 64)
			if err != nil || v <= 0 {
				return 0, errBadNumber
			}

			return int64(v * float64(u.mult)), nil
		}
	}

	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, errBadNumber
	}

	return v, nil
}

// labels are what kronik puts on what it starts, to find and clean it up.
func labels(info Info, extra map[string]string) map[string]string {
	out := make(map[string]string, len(extra)+4)
	for k, v := range extra {
		out[k] = v
	}

	out["kronik.run"] = info.RunID
	out["kronik.job"] = info.JobID
	out["kronik.managed"] = "true"

	return out
}

// runEnv is the job's env plus what kronik tells every run.
func runEnv(info Info, env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+5)
	for k, v := range env {
		out[k] = v
	}

	out["KRONIK_JOB_ID"] = info.JobID
	out["KRONIK_JOB_NAME"] = info.JobName
	out["KRONIK_RUN_ID"] = info.RunID
	out["KRONIK_ATTEMPT"] = strconv.Itoa(info.Attempt)
	out["KRONIK_SCHEDULED_AT"] = info.ScheduledAt.UTC().Format("2006-01-02T15:04:05Z07:00")

	return out
}

// shortID is a name-safe piece of a run id.
func shortID(id string) string {
	id = strings.ToLower(id)
	if len(id) > 12 {
		id = id[len(id)-12:]
	}

	return id
}
