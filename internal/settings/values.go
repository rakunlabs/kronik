package settings

import (
	"encoding/json"
	"strconv"
	"time"
)

// Values are the settings in effect: the defaults and the configuration,
// overridden by what was set from the UI.
type Values struct {
	Environment      string `json:"environment"`
	EnvironmentColor string `json:"environment_color"`

	RunRetention   time.Duration `json:"-"`
	RunsPerJob     int           `json:"-"`
	AuditRetention time.Duration `json:"-"`
	// CleanupAt is the time of day ("03:00") of the daily cleanup.
	CleanupAt   string `json:"-"`
	CleanupZone string `json:"-"`

	HTTP, JS, Command, Docker, Swarm, Kubernetes bool `json:"-"`

	JSMaxDuration time.Duration `json:"-"`
	JSMaxRequests int           `json:"-"`
	JSMaxResponse int64         `json:"-"`

	DockerHost            string
	DockerAllowedImages   []string
	DockerNetwork         string
	DockerAllowPrivileged bool
	DockerAllowMounts     bool
	DockerPull            string

	SwarmHost          string
	SwarmAllowedImages []string
	SwarmNetworks      []string
	SwarmAllowMounts   bool

	KubeConfig            string
	KubeContext           string
	KubeNamespace         string
	KubeAllowedNamespaces []string
	KubeAllowedImages     []string
	KubeServiceAccount    string
	KubeTTL               time.Duration

	MCPEnabled  bool
	MCPReadOnly bool

	Paused             bool
	MaintenanceWindows []string
	MaintenanceZone    string
	TagLimits          []string

	NotificationRetention time.Duration
	NotifyEnabled         bool
	NotifyBaseURL         string
	SMTPHost              string
	SMTPPort              int
	SMTPTLS               string
	SMTPUser              string
	SMTPPasswordSecret    string
	SMTPFrom              string

	MetricsEnabled bool
	MetricsPerJob  bool
}

// fields maps each key to the value it sets.
func (v *Values) fields() map[string]any {
	return map[string]any{
		"environment":       &v.Environment,
		"environment_color": &v.EnvironmentColor,

		"retention.runs":         &v.RunRetention,
		"retention.runs_per_job": &v.RunsPerJob,
		"retention.audit":        &v.AuditRetention,
		"retention.at":           &v.CleanupAt,
		"retention.timezone":     &v.CleanupZone,

		"executor.http":       &v.HTTP,
		"executor.js":         &v.JS,
		"executor.command":    &v.Command,
		"executor.docker":     &v.Docker,
		"executor.swarm":      &v.Swarm,
		"executor.kubernetes": &v.Kubernetes,

		"js.max_duration": &v.JSMaxDuration,
		"js.max_requests": &v.JSMaxRequests,
		"js.max_response": &v.JSMaxResponse,

		"docker.host":             &v.DockerHost,
		"docker.allowed_images":   &v.DockerAllowedImages,
		"docker.network":          &v.DockerNetwork,
		"docker.allow_privileged": &v.DockerAllowPrivileged,
		"docker.allow_mounts":     &v.DockerAllowMounts,
		"docker.pull":             &v.DockerPull,

		"swarm.host":           &v.SwarmHost,
		"swarm.allowed_images": &v.SwarmAllowedImages,
		"swarm.networks":       &v.SwarmNetworks,
		"swarm.allow_mounts":   &v.SwarmAllowMounts,

		"kubernetes.kubeconfig":         &v.KubeConfig,
		"kubernetes.context":            &v.KubeContext,
		"kubernetes.namespace":          &v.KubeNamespace,
		"kubernetes.allowed_namespaces": &v.KubeAllowedNamespaces,
		"kubernetes.allowed_images":     &v.KubeAllowedImages,
		"kubernetes.service_account":    &v.KubeServiceAccount,
		"kubernetes.ttl":                &v.KubeTTL,

		"mcp.enabled":   &v.MCPEnabled,
		"mcp.read_only": &v.MCPReadOnly,

		"maintenance.paused":   &v.Paused,
		"maintenance.windows":  &v.MaintenanceWindows,
		"maintenance.timezone": &v.MaintenanceZone,
		"limits.tags":          &v.TagLimits,

		"retention.notifications":     &v.NotificationRetention,
		"notify.enabled":              &v.NotifyEnabled,
		"notify.base_url":             &v.NotifyBaseURL,
		"notify.smtp_host":            &v.SMTPHost,
		"notify.smtp_port":            &v.SMTPPort,
		"notify.smtp_tls":             &v.SMTPTLS,
		"notify.smtp_user":            &v.SMTPUser,
		"notify.smtp_password_secret": &v.SMTPPasswordSecret,
		"notify.smtp_from":            &v.SMTPFrom,

		"metrics.enabled": &v.MetricsEnabled,
		"metrics.per_job": &v.MetricsPerJob,
	}
}

// Get returns the value of key as JSON.
func (v *Values) Get(key string) json.RawMessage {
	p, ok := v.fields()[key]
	if !ok {
		return nil
	}

	var out any

	switch x := p.(type) {
	case *time.Duration:
		out = FormatDuration(*x)
	case *[]string:
		if *x == nil {
			out = []string{}
		} else {
			out = *x
		}
	default:
		out = x
	}

	b, _ := json.Marshal(out)

	return b
}

// Set applies a value already checked by Def.Parse.
func (v *Values) Set(key string, raw json.RawMessage) error {
	p, ok := v.fields()[key]
	if !ok {
		return nil
	}

	if d, ok := p.(*time.Duration); ok {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}

		dur, err := ParseDuration(s)
		if err != nil {
			return err
		}

		*d = dur

		return nil
	}

	return json.Unmarshal(raw, p)
}

// FormatDuration writes a duration the way it is usually typed: whole days
// as "30d", else Go's form without trailing zero units ("10m", "1h30m").
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}

	if d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}

	s := d.String()
	for _, suffix := range []string{"m0s", "h0m"} {
		if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
			s = s[:len(s)-len(suffix)+1]
		}
	}

	return s
}

// Clone copies v, lists included.
func (v Values) Clone() Values {
	c := v
	c.DockerAllowedImages = append([]string(nil), v.DockerAllowedImages...)
	c.SwarmAllowedImages = append([]string(nil), v.SwarmAllowedImages...)
	c.SwarmNetworks = append([]string(nil), v.SwarmNetworks...)
	c.KubeAllowedNamespaces = append([]string(nil), v.KubeAllowedNamespaces...)
	c.KubeAllowedImages = append([]string(nil), v.KubeAllowedImages...)
	c.MaintenanceWindows = append([]string(nil), v.MaintenanceWindows...)
	c.TagLimits = append([]string(nil), v.TagLimits...)

	return c
}
