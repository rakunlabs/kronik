package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/rakunlabs/into"
	"github.com/rakunlabs/logi"
	"github.com/rakunlabs/tell"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/metrics"
	"github.com/rakunlabs/kronik/internal/notify"
	"github.com/rakunlabs/kronik/internal/scheduler"
	"github.com/rakunlabs/kronik/internal/secrets"
	"github.com/rakunlabs/kronik/internal/server"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

// Set by the linker; the version goes straight into config.Version.
var (
	commit = "-"
	date   = "-"
)

func main() {
	config.Build = buildInfo()

	into.Init(run,
		into.WithLogger(logi.InitializeLog(logi.WithCaller(false))),
		into.WithMsgf("%s version:[%s] commit:[%s] date:[%s]",
			config.ServiceName, config.Build.Version, config.Build.Commit, config.Build.Date),
	)
}

func run(ctx context.Context) error {
	cfg, err := config.Load(ctx)
	if err != nil {
		return err
	}

	collector, err := tell.New(ctx, cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("init telemetry; %w", err)
	}
	defer func() { _ = collector.Shutdown() }()

	st, err := store.New(ctx, cfg.Store)
	if err != nil {
		return fmt.Errorf("init store; %w", err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Changes reach the other replicas at once through LISTEN/NOTIFY; they
	// poll too, so a lost connection only makes them slower.
	bus := st.NewBus(cfg.Store.DSN, cfg.Store.TablePrefix)
	st.SetBus(bus)

	// Settings: the configuration, overridden from the UI, shared through
	// the database and followed by every replica.
	set := settings.NewManager(settings.StoreSource{Store: st, Bus: bus}, cfg.Values())
	if err := set.Load(ctx); err != nil {
		return fmt.Errorf("load settings; %w", err)
	}

	vault, err := newVault(ctx, cfg, st)
	if err != nil {
		return err
	}

	executors := executor.NewLive(buildExecutors(cfg, set.Values()))
	defer func() { executors.Registry().Close() }()

	set.OnChange(func(v settings.Values) {
		old := executors.Swap(buildExecutors(cfg, v))
		slog.Info("settings changed", "executors", executors.Names(), "environment", v.Environment)

		// Runs going on keep their executor; its connection goes with it
		// after a while.
		time.AfterFunc(time.Hour, old.Close)
	})

	sch := scheduler.New(st, executors, cfg.Scheduler, cfg.NodeName)
	sch.SetRetention(func() scheduler.Retention {
		v := set.Values()

		return scheduler.Retention{Runs: v.RunRetention, RunsPerJob: v.RunsPerJob, Audit: v.AuditRetention,
			Notifications: v.NotificationRetention, At: v.CleanupAt, Zone: v.CleanupZone}
	})
	sch.SetOptions(func() scheduler.Options {
		v := set.Values()

		return scheduler.Options{Paused: v.Paused, Windows: v.Windows(), TagLimits: v.Limits()}
	})
	sch.SetSecrets(vault.Values)

	svc := service.New(st, executors, sch)
	svc.SetSecrets(vault.Values)

	mets, err := metrics.New(collector.MeterProvider, cfg.NodeName, func() metrics.Options {
		v := set.Values()

		return metrics.Options{Enabled: v.MetricsEnabled && !collector.IsMetricNoop(), PerJob: v.MetricsPerJob}
	})
	if err != nil {
		return fmt.Errorf("init metrics; %w", err)
	}

	sch.SetObserver(mets)

	sender := notify.NewSender(st, func() notify.Options {
		v := set.Values()
		secrets := vault.Values()

		return notify.Options{
			Enabled: v.NotifyEnabled, BaseURL: v.NotifyBaseURL, Secrets: secrets, Environment: v.Environment,
			SMTP: notify.SMTP{Host: v.SMTPHost, Port: v.SMTPPort, User: v.SMTPUser, Password: secrets[v.SMTPPasswordSecret],
				From: v.SMTPFrom, TLS: v.SMTPTLS},
		}
	})
	sender.Observer = mets.NotificationSent

	ui, err := server.UI()
	if err != nil {
		return fmt.Errorf("load ui; %w", err)
	}

	live := server.NewLive()

	bus.On(store.TopicWake, func(string) { sch.Wake() })
	bus.On(store.TopicSettings, func(string) { set.Refresh() })
	bus.On(store.TopicSecrets, func(string) { go loadSecrets(ctx, vault) })
	bus.On(store.TopicRun, live.Publish)

	go bus.Run(ctx)
	go set.Run(ctx)
	go sender.Run(ctx)
	go mets.Watch(ctx, st, 15*time.Second)
	go secretsLoop(ctx, vault)

	// Whichever stops first stops the other; the scheduler is waited for so
	// running jobs can finish.
	schDone := make(chan error, 1)
	go func() {
		defer cancel()
		schDone <- sch.Run(ctx)
	}()

	srvErr := server.Start(ctx, cfg, server.Deps{
		Service: svc, UI: ui, Settings: set, Cleaner: sch, Vault: vault, Sender: sender, Live: live, Bus: bus,
		MetricsActive: !collector.IsMetricNoop(),
	})
	cancel()

	schErr := <-schDone

	if srvErr != nil {
		return fmt.Errorf("start server; %w", srvErr)
	}

	if schErr != nil {
		return fmt.Errorf("scheduler; %w", schErr)
	}

	return nil
}

// newVault reads the stored secrets with the master key from the
// configuration.
func newVault(ctx context.Context, cfg *config.Config, st *store.Store) (*secrets.Vault, error) {
	v := secrets.NewVault(st, secrets.NewMasterKey(cfg.Secrets.Key), cfg.Executor.JS.Secrets)
	if _, err := v.Load(ctx); err != nil {
		return nil, fmt.Errorf("load secrets; %w", err)
	}

	if cfg.Secrets.Key == "" {
		slog.Warn("secrets.key (KRONIK_SECRETS_KEY) is empty: the secrets set from the UI are encrypted with the key of an empty secret; set one, or change it from Settings → Secrets")
	}

	if p := v.Problem(); p != "" {
		slog.Error(p)
	}

	return v, nil
}

func loadSecrets(ctx context.Context, v *secrets.Vault) {
	if _, err := v.Load(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("reload secrets", "error", err)
	}
}

// secretsLoop reloads the secrets now and then.
func secretsLoop(ctx context.Context, v *secrets.Vault) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		loadSecrets(ctx, v)
	}
}

// buildExecutors makes the executors the settings turn on.
func buildExecutors(cfg *config.Config, v settings.Values) executor.Registry {
	return executor.Build(executor.Limits{
		HTTP: v.HTTP, JS: v.JS, Command: v.Command, Docker: v.Docker, Swarm: v.Swarm, Kubernetes: v.Kubernetes,
		Shell: cfg.Executor.Command.Shell,
		JSLimits: executor.JSLimits{
			MaxDuration: v.JSMaxDuration,
			MaxRequests: v.JSMaxRequests,
			MaxResponse: v.JSMaxResponse,
		},
		DockerOptions: executor.DockerOptions{
			Host: v.DockerHost, AllowedImages: v.DockerAllowedImages, Network: v.DockerNetwork,
			AllowPrivileged: v.DockerAllowPrivileged, AllowMounts: v.DockerAllowMounts, Pull: v.DockerPull,
		},
		SwarmOptions: executor.SwarmOptions{
			Host: firstNonEmpty(v.SwarmHost, v.DockerHost), AllowedImages: v.SwarmAllowedImages, Networks: v.SwarmNetworks,
			AllowMounts: v.SwarmAllowMounts,
		},
		KubernetesOptions: executor.KubernetesOptions{
			Kubeconfig: v.KubeConfig, Context: v.KubeContext, Namespace: v.KubeNamespace,
			AllowedNamespaces: v.KubeAllowedNamespaces, AllowedImages: v.KubeAllowedImages,
			ServiceAccount: v.KubeServiceAccount, TTL: v.KubeTTL,
		},
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

// buildInfo uses what the release build set, and for a local build what Go
// recorded from version control.
func buildInfo() config.BuildInfo {
	b := config.BuildInfo{
		Version:    config.Version,
		Commit:     commit,
		Date:       date,
		GoVersion:  runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Repository: config.Repository,
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}

	local := b.Commit == "" || b.Commit == "-"
	dirty := false

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if local {
				b.Commit = s.Value[:min(len(s.Value), 7)]
			}
		case "vcs.time":
			if b.Date == "" || b.Date == "-" {
				b.Date = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	if local && dirty && b.Commit != "-" {
		b.Commit += "-dirty"
	}

	return b
}
