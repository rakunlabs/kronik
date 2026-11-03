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
	"github.com/rakunlabs/kronik/internal/scheduler"
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

	// Settings: the configuration, overridden from the UI, shared through
	// the database and followed by every replica.
	set := settings.NewManager(settings.StoreSource{Store: st}, cfg.Values())
	if err := set.Load(ctx); err != nil {
		return fmt.Errorf("load settings; %w", err)
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

		return scheduler.Retention{Runs: v.RunRetention, RunsPerJob: v.RunsPerJob, Audit: v.AuditRetention, At: v.CleanupAt, Zone: v.CleanupZone}
	})

	svc := service.New(st, executors, sch)

	ui, err := server.UI()
	if err != nil {
		return fmt.Errorf("load ui; %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Whichever stops first stops the other; the scheduler is waited for so
	// running jobs can finish.
	go set.Run(ctx)

	schDone := make(chan error, 1)
	go func() {
		defer cancel()
		schDone <- sch.Run(ctx)
	}()

	srvErr := server.Start(ctx, cfg, svc, ui, set, sch)
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

// buildExecutors makes the executors the settings turn on.
func buildExecutors(cfg *config.Config, v settings.Values) executor.Registry {
	return executor.Build(executor.Limits{
		HTTP: v.HTTP, JS: v.JS, Command: v.Command, Docker: v.Docker, Swarm: v.Swarm, Kubernetes: v.Kubernetes,
		Shell: cfg.Executor.Command.Shell,
		JSLimits: executor.JSLimits{
			MaxDuration: v.JSMaxDuration,
			MaxRequests: v.JSMaxRequests,
			MaxResponse: v.JSMaxResponse,
			Secrets:     cfg.Executor.JS.Secrets,
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
