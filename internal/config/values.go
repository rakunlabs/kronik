package config

import "github.com/rakunlabs/kronik/internal/settings"

// Values are the settings as the configuration sets them; the UI overrides
// them key by key.
func (c *Config) Values() settings.Values {
	e := c.Executor

	return settings.Values{
		Environment:      c.Environment,
		EnvironmentColor: c.EnvironmentColor,

		RunRetention:   c.Scheduler.RunRetention,
		RunsPerJob:     c.Scheduler.RunsPerJob,
		AuditRetention: c.Scheduler.AuditRetention,
		CleanupAt:      c.Scheduler.CleanupAt,
		CleanupZone:    c.Scheduler.CleanupTimezone,

		HTTP:       e.HTTP.Enabled,
		JS:         e.JS.Enabled,
		Command:    e.Command.Enabled,
		Docker:     e.Docker.Enabled,
		Swarm:      e.Swarm.Enabled,
		Kubernetes: e.Kubernetes.Enabled,

		JSMaxDuration: e.JS.MaxDuration,
		JSMaxRequests: e.JS.MaxRequests,
		JSMaxResponse: e.JS.MaxResponse,

		DockerHost:            e.Docker.Host,
		DockerAllowedImages:   e.Docker.AllowedImages,
		DockerNetwork:         e.Docker.Network,
		DockerAllowPrivileged: e.Docker.AllowPrivileged,
		DockerAllowMounts:     e.Docker.AllowMounts,
		DockerPull:            e.Docker.Pull,

		SwarmHost:          e.Swarm.Host,
		SwarmAllowedImages: e.Swarm.AllowedImages,
		SwarmNetworks:      e.Swarm.Networks,
		SwarmAllowMounts:   e.Swarm.AllowMounts,

		KubeConfig:            e.Kubernetes.Kubeconfig,
		KubeContext:           e.Kubernetes.Context,
		KubeNamespace:         e.Kubernetes.Namespace,
		KubeAllowedNamespaces: e.Kubernetes.AllowedNamespaces,
		KubeAllowedImages:     e.Kubernetes.AllowedImages,
		KubeServiceAccount:    e.Kubernetes.ServiceAccount,
		KubeTTL:               e.Kubernetes.TTL,

		MCPEnabled:  c.MCP.Enabled,
		MCPReadOnly: c.MCP.ReadOnly,

		Paused:             c.Maintenance.Paused,
		MaintenanceWindows: c.Maintenance.Windows,
		MaintenanceZone:    c.Maintenance.Timezone,
		TagLimits:          c.Scheduler.TagLimits,

		NotificationRetention: c.Notify.Retention,
		NotifyEnabled:         c.Notify.Enabled,
		NotifyBaseURL:         c.Notify.BaseURL,
		SMTPHost:              c.Notify.SMTPHost,
		SMTPPort:              c.Notify.SMTPPort,
		SMTPTLS:               c.Notify.SMTPTLS,
		SMTPUser:              c.Notify.SMTPUser,
		SMTPPasswordSecret:    c.Notify.SMTPPasswordSecret,
		SMTPFrom:              c.Notify.SMTPFrom,

		SecretsRotate: c.Secrets.Rotate,

		MetricsEnabled: c.Metrics.Enabled,
		MetricsPerJob:  c.Metrics.PerJob,
	}
}
