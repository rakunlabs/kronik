// Package settings holds the settings that can be changed from the UI while
// kronik runs. Each has a default, may be set in the configuration (file or
// environment) and may be overridden from the UI; the override is stored in
// PostgreSQL and read by every replica.
package settings

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Type is how a setting's value is written.
type Type string

const (
	TypeBool     Type = "bool"
	TypeInt      Type = "int"
	TypeString   Type = "string"
	TypeDuration Type = "duration" // Go duration, "720h", "30s"
	TypeList     Type = "list"     // list of strings
	TypeTime     Type = "time"     // time of day, "03:00"
	TypeChoice   Type = "choice"
)

// Def describes one setting.
type Def struct {
	Key         string   `json:"key"`
	Group       string   `json:"group"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Type        Type     `json:"type"`
	Choices     []string `json:"choices,omitempty"`
	Min         *int64   `json:"min,omitempty"`
	Max         *int64   `json:"max,omitempty"`
	// Placeholder is shown in an empty field.
	Placeholder string `json:"placeholder,omitempty"`
	// Danger marks a setting that gives jobs access to the host or cluster.
	Danger bool `json:"danger,omitempty"`
	// Unit is shown after a number, e.g. "bytes".
	Unit string `json:"unit,omitempty"`
}

func i64(v int64) *int64 { return &v }

// Groups orders the groups of the settings page.
var Groups = []struct {
	ID, Label, Description string
}{
	{"general", "General", "How this installation shows itself."},
	{"scheduling", "Scheduling", "Pause everything, maintenance windows and how many runs of a tag go at once."},
	{"retention", "Retention", "What is kept and when it is cleaned up."},
	{"notifications", "Notifications", "Messages when jobs fail, recover or do not run. Channels are set under Settings → Notifications."},
	{"secrets", "Secrets", "How the secrets set from the UI are encrypted."},
	{"metrics", "Metrics", "Run and job metrics sent through OpenTelemetry."},
	{"executors", "Executors", "What jobs may run. A replica runs only the executors turned on here."},
	{"js", "JavaScript", "Limits of JavaScript jobs."},
	{"docker", "Docker", "Containers on a Docker Engine (the replica's docker.sock or a remote daemon)."},
	{"swarm", "Docker Swarm", "One-off services on a Swarm cluster, scheduled by Swarm on any node."},
	{"kubernetes", "Kubernetes", "Jobs (batch/v1) in a Kubernetes cluster."},
	{"mcp", "MCP", "The endpoint AI agents use."},
}

// Defs lists every setting, in the order shown.
var Defs = []Def{
	{Key: "environment", Group: "general", Label: "Environment", Type: TypeString, Placeholder: "e.g. prod, preprod, sandbox",
		Description: "Shown at the top of every page and told to MCP agents, so nobody mistakes production for a test system."},
	{Key: "environment_color", Group: "general", Label: "Environment colour", Type: TypeString, Placeholder: "from the name",
		Description: "A CSS colour (#dc2626). Empty: red for prod, amber for preprod/staging, green for the rest."},

	{Key: "maintenance.paused", Group: "scheduling", Label: "Pause all schedules", Type: TypeBool,
		Description: "Scheduled runs are skipped (recorded as skipped) until this is turned off. Runs started by hand, webhooks and chains still run."},
	{Key: "maintenance.windows", Group: "scheduling", Label: "Maintenance windows", Type: TypeList, Placeholder: "0 2 * * 0|2h",
		Description: "schedule|duration[|tag,tag]: from each time of the cron schedule, for the duration, scheduled runs are skipped — of the jobs with one of the tags, or of all. 0 2 * * 0|2h is Sundays 02:00–04:00."},
	{Key: "maintenance.timezone", Group: "scheduling", Label: "Window time zone", Type: TypeString, Placeholder: "UTC",
		Description: "IANA time zone of the window schedules."},
	{Key: "limits.tags", Group: "scheduling", Label: "Runs at once per tag", Type: TypeList, Placeholder: "reports=2",
		Description: "tag=n: at most n runs of the jobs with this tag run at the same time, across all replicas. The others wait in the queue."},

	{Key: "retention.runs", Group: "retention", Label: "Keep runs for", Type: TypeDuration, Placeholder: "720h",
		Description: "Finished runs older than this are deleted. 0 keeps them. Use hours: 720h is 30 days."},
	{Key: "retention.runs_per_job", Group: "retention", Label: "Runs kept per job", Type: TypeInt, Min: i64(0), Max: i64(1_000_000),
		Description: "At most this many finished runs per job; older ones are deleted. 0 keeps all."},
	{Key: "retention.audit", Group: "retention", Label: "Keep the audit log for", Type: TypeDuration, Placeholder: "2160h",
		Description: "Audit entries older than this are deleted. 0 keeps them."},
	{Key: "retention.at", Group: "retention", Label: "Clean up at", Type: TypeTime, Placeholder: "01:00",
		Description: "Time of day the cleanup runs, once a day, on one replica."},
	{Key: "retention.timezone", Group: "retention", Label: "Cleanup time zone", Type: TypeString, Placeholder: "UTC",
		Description: "IANA time zone of the cleanup time."},

	{Key: "retention.notifications", Group: "retention", Label: "Keep notifications for", Type: TypeDuration, Placeholder: "720h",
		Description: "Sent and failed notifications older than this are deleted. 0 keeps them."},

	{Key: "notify.enabled", Group: "notifications", Label: "Send notifications", Type: TypeBool,
		Description: "Off: notifications are still recorded, nothing is sent until this is turned on again."},
	{Key: "notify.base_url", Group: "notifications", Label: "kronik's address", Type: TypeString, Placeholder: "https://cron.example.com",
		Description: "The address users open kronik at, for links to the job and run in messages."},
	{Key: "notify.smtp_host", Group: "notifications", Label: "SMTP server", Type: TypeString, Placeholder: "smtp.example.com",
		Description: "For e-mail channels."},
	{Key: "notify.smtp_port", Group: "notifications", Label: "SMTP port", Type: TypeInt, Min: i64(1), Max: i64(65535)},
	{Key: "notify.smtp_tls", Group: "notifications", Label: "SMTP security", Type: TypeChoice, Choices: []string{"starttls", "tls", "none"}},
	{Key: "notify.smtp_user", Group: "notifications", Label: "SMTP user", Type: TypeString},
	{Key: "notify.smtp_password_secret", Group: "notifications", Label: "SMTP password secret", Type: TypeString, Placeholder: "SMTP_PASSWORD",
		Description: "Name of the secret (Settings → Secrets) that holds the SMTP password."},
	{Key: "notify.smtp_from", Group: "notifications", Label: "From address", Type: TypeString, Placeholder: "kronik@example.com"},

	{Key: "secrets.rotate", Group: "secrets", Label: "Rotate the data key after", Type: TypeDuration, Placeholder: "90d",
		Description: "Once the data key is this old, one replica makes a new one and re-encrypts every secret with it; old keys are deleted when no secret uses them. 0 rotates only by hand."},

	{Key: "metrics.enabled", Group: "metrics", Label: "Send metrics", Type: TypeBool,
		Description: "Runs, durations, queue and job health as OpenTelemetry metrics. Sent only when a collector is set (OTEL_EXPORTER_OTLP_ENDPOINT)."},
	{Key: "metrics.per_job", Group: "metrics", Label: "Per job", Type: TypeBool,
		Description: "Add the job's name to the run metrics. Off keeps the number of series small with many jobs."},

	{Key: "executor.http", Group: "executors", Label: "HTTP requests", Type: TypeBool,
		Description: "Jobs that send an HTTP request."},
	{Key: "executor.js", Group: "executors", Label: "JavaScript", Type: TypeBool,
		Description: "Jobs that run a script with fetch. Scripts can read every secret."},
	{Key: "executor.command", Group: "executors", Label: "Shell commands", Type: TypeBool, Danger: true,
		Description: "Jobs that run a shell command inside the kronik container."},
	{Key: "executor.docker", Group: "executors", Label: "Docker containers", Type: TypeBool, Danger: true,
		Description: "Jobs that run a container on a Docker Engine. Access to the engine is root on its host."},
	{Key: "executor.swarm", Group: "executors", Label: "Docker Swarm services", Type: TypeBool, Danger: true,
		Description: "Jobs that run a one-off service on a Swarm cluster (needs a manager node's engine)."},
	{Key: "executor.kubernetes", Group: "executors", Label: "Kubernetes Jobs", Type: TypeBool, Danger: true,
		Description: "Jobs that create a batch/v1 Job in a Kubernetes cluster."},

	{Key: "js.max_duration", Group: "js", Label: "Longest run without a timeout", Type: TypeDuration, Placeholder: "10m",
		Description: "A script whose job has no timeout is stopped after this."},
	{Key: "js.max_requests", Group: "js", Label: "fetch calls per run", Type: TypeInt, Min: i64(0), Max: i64(1_000_000),
		Description: "0 is no limit."},
	{Key: "js.max_response", Group: "js", Label: "Largest response body", Type: TypeInt, Min: i64(1024), Max: i64(1 << 30), Unit: "bytes",
		Description: "A larger response fails the fetch."},

	{Key: "docker.host", Group: "docker", Label: "Engine", Type: TypeString, Placeholder: "unix:///var/run/docker.sock",
		Description: "Address of the Docker Engine. Empty uses DOCKER_HOST or the local socket."},
	{Key: "docker.allowed_images", Group: "docker", Label: "Allowed images", Type: TypeList, Placeholder: "any image",
		Description: "Image patterns jobs may use, e.g. ghcr.io/acme/* or alpine:*. Empty allows any image."},
	{Key: "docker.network", Group: "docker", Label: "Default network", Type: TypeString, Placeholder: "bridge",
		Description: "Network a container joins when its job names none."},
	{Key: "docker.allow_privileged", Group: "docker", Label: "Allow privileged containers", Type: TypeBool, Danger: true,
		Description: "Let jobs ask for privileged mode."},
	{Key: "docker.allow_mounts", Group: "docker", Label: "Allow host mounts", Type: TypeBool, Danger: true,
		Description: "Let jobs mount host paths and volumes into their container."},
	{Key: "docker.pull", Group: "docker", Label: "Pull images", Type: TypeChoice, Choices: []string{"missing", "always", "never"},
		Description: "When an image is pulled before the container starts. A job may choose its own."},

	{Key: "swarm.host", Group: "swarm", Label: "Manager engine", Type: TypeString, Placeholder: "same as Docker",
		Description: "A Swarm manager's Docker Engine. Empty uses the Docker engine above."},
	{Key: "swarm.allowed_images", Group: "swarm", Label: "Allowed images", Type: TypeList, Placeholder: "any image",
		Description: "Image patterns Swarm jobs may use. Empty allows any image."},
	{Key: "swarm.networks", Group: "swarm", Label: "Default networks", Type: TypeList, Placeholder: "none",
		Description: "Overlay networks a service joins when its job names none."},
	{Key: "swarm.allow_mounts", Group: "swarm", Label: "Allow host mounts", Type: TypeBool, Danger: true,
		Description: "Let jobs mount host paths and volumes into their service."},

	{Key: "kubernetes.kubeconfig", Group: "kubernetes", Label: "kubeconfig", Type: TypeString, Placeholder: "in-cluster",
		Description: "Path of a kubeconfig file. Empty uses the pod's service account (in-cluster), then ~/.kube/config."},
	{Key: "kubernetes.context", Group: "kubernetes", Label: "kubeconfig context", Type: TypeString, Placeholder: "current context"},
	{Key: "kubernetes.namespace", Group: "kubernetes", Label: "Default namespace", Type: TypeString, Placeholder: "kronik's own namespace",
		Description: "Namespace of a Job whose job names none."},
	{Key: "kubernetes.allowed_namespaces", Group: "kubernetes", Label: "Allowed namespaces", Type: TypeList, Placeholder: "the default only",
		Description: "Namespaces jobs may choose. Empty allows only the default namespace."},
	{Key: "kubernetes.allowed_images", Group: "kubernetes", Label: "Allowed images", Type: TypeList, Placeholder: "any image",
		Description: "Image patterns Kubernetes jobs may use. Empty allows any image."},
	{Key: "kubernetes.service_account", Group: "kubernetes", Label: "Default service account", Type: TypeString, Placeholder: "default",
		Description: "Service account of a Job's pod when its job names none."},
	{Key: "kubernetes.ttl", Group: "kubernetes", Label: "Keep finished Jobs for", Type: TypeDuration, Placeholder: "10m",
		Description: "A finished Job is deleted after this (ttlSecondsAfterFinished). kronik keeps the output."},

	{Key: "mcp.enabled", Group: "mcp", Label: "MCP endpoint", Type: TypeBool,
		Description: "Serve MCP on <base_path>/mcp."},
	{Key: "mcp.read_only", Group: "mcp", Label: "Read-only", Type: TypeBool,
		Description: "Offer only the tools that read: agents cannot change, run or test jobs."},
}

// Find returns the definition of key.
func Find(key string) (Def, bool) {
	i := slices.IndexFunc(Defs, func(d Def) bool { return d.Key == key })
	if i < 0 {
		return Def{}, false
	}

	return Defs[i], true
}

var timeOfDay = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Parse checks a value given for d and returns it normalized as JSON.
func (d Def) Parse(raw json.RawMessage) (json.RawMessage, error) {
	switch d.Type {
	case TypeBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be true or false", d.Label)
		}

		return json.Marshal(v)
	case TypeInt:
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be a whole number", d.Label)
		}

		if d.Min != nil && v < *d.Min || d.Max != nil && v > *d.Max {
			return nil, fmt.Errorf("%s must be between %d and %d", d.Label, *d.Min, *d.Max)
		}

		return json.Marshal(v)
	case TypeDuration:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s must be a duration such as 30s, 10m or 720h", d.Label)
		}

		s = strings.TrimSpace(s)

		v, err := ParseDuration(s)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("%s must be a duration such as 30s, 10m, 720h or 30d", d.Label)
		}

		return json.Marshal(s)
	case TypeTime:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || !timeOfDay.MatchString(strings.TrimSpace(s)) {
			return nil, fmt.Errorf("%s must be a time of day such as 01:00", d.Label)
		}

		return json.Marshal(strings.TrimSpace(s))
	case TypeList:
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be a list", d.Label)
		}

		out := []string{}
		for _, x := range v {
			if x = strings.TrimSpace(x); x != "" && !slices.Contains(out, x) {
				out = append(out, x)
			}
		}

		return json.Marshal(out)
	case TypeChoice:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || !slices.Contains(d.Choices, s) {
			return nil, fmt.Errorf("%s must be one of %s", d.Label, strings.Join(d.Choices, ", "))
		}

		return json.Marshal(s)
	default:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s must be text", d.Label)
		}

		if len(s) > 2000 || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 }) {
			return nil, fmt.Errorf("%s is too long or has control characters", d.Label)
		}

		return json.Marshal(strings.TrimSpace(s))
	}
}

// ParseDuration is time.ParseDuration that also takes days ("30d").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}

	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int64
		if _, err := fmt.Sscanf(n, "%d", &days); err == nil && fmt.Sprint(days) == n && days >= 0 && days < 100000 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}

	return time.ParseDuration(s)
}
