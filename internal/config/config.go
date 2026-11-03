package config

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rakunlabs/chu"
	"github.com/rakunlabs/chu/loader/loaderenv"
	"github.com/rakunlabs/logi"
	"github.com/rakunlabs/tell"

	"github.com/rakunlabs/kronik/internal/store"
)

const ServiceName = "kronik"

// Repository is where kronik's source lives.
const Repository = "https://github.com/rakunlabs/kronik"

// Version is set by the linker: -X github.com/rakunlabs/kronik/internal/config.Version=v1.2.3.
var Version = "v0.0.0"

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	Date       string `json:"date"`
	GoVersion  string `json:"go_version"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Repository string `json:"repository"`
}

// Build is filled in by main before the server starts.
var Build = BuildInfo{Version: Version, Repository: Repository}

type Config struct {
	LogLevel string `cfg:"log_level,noprefix" default:"info"`
	// NodeName names this replica in runs and the replica list. Empty uses
	// the host name with a random suffix, so restarts are told apart.
	NodeName string `cfg:"node_name"`
	// Environment names this installation, e.g. sandbox, preprod or prod. It
	// is shown at the top of the UI and told to MCP agents, so nobody
	// mistakes production for a test system.
	Environment string `cfg:"environment"`
	// EnvironmentColor is the label's colour (#rrggbb or a CSS colour name).
	// Empty picks one from the name: red for prod, amber for preprod/staging,
	// green for the rest.
	EnvironmentColor string `cfg:"environment_color"`

	Server    Server      `cfg:"server"`
	Store     Store       `cfg:"store"`
	Scheduler Scheduler   `cfg:"scheduler"`
	Executor  Executor    `cfg:"executor"`
	MCP       MCP         `cfg:"mcp"`
	Telemetry tell.Config `cfg:"telemetry,noprefix"`
}

type Server struct {
	Host     string `cfg:"host"`
	Port     string `cfg:"port" default:"8080"`
	BasePath string `cfg:"base_path"`
	// UserHeader is the request header the reverse proxy puts the signed-in
	// user in. It is recorded with every change; kronik does not check it.
	UserHeader string `cfg:"user_header" default:"X-User"`
}

// MCP serves the Model Context Protocol on <base_path>/mcp, so AI agents can
// list, change and run jobs.
type MCP struct {
	Enabled bool `cfg:"enabled" default:"true"`
	// ReadOnly offers only the tools that read.
	ReadOnly bool `cfg:"read_only"`
}

// Store is how to reach PostgreSQL; every table starts with table_prefix
// (kronik_), so kronik can share a schema with other applications.
type Store = store.Config

type Scheduler struct {
	// PollInterval is how often each replica looks for due jobs and queued runs.
	PollInterval time.Duration `cfg:"poll_interval" default:"1s"`
	// MaxConcurrent is the number of runs one replica executes at once.
	MaxConcurrent int `cfg:"max_concurrent" default:"50"`
	// MaxOutput is the number of output bytes kept per run; the tail is kept.
	MaxOutput int `cfg:"max_output" default:"1048576"`
	// ShutdownTimeout is how long running runs may finish on shutdown before
	// they are canceled.
	ShutdownTimeout time.Duration `cfg:"shutdown_timeout" default:"30s"`
	// NodeTimeout marks a replica lost when it has not been seen for this
	// long; its running runs are then failed.
	NodeTimeout time.Duration `cfg:"node_timeout" default:"45s"`
	// RunRetention deletes finished runs older than this; zero keeps them.
	RunRetention time.Duration `cfg:"run_retention" default:"720h"`
	// RunsPerJob keeps at most this many finished runs per job; zero keeps all.
	RunsPerJob int `cfg:"runs_per_job" default:"200"`
	// AuditRetention deletes audit entries older than this; zero keeps them.
	AuditRetention time.Duration `cfg:"audit_retention" default:"2160h"`
	// CleanupAt is the time of day old runs and audit entries are deleted,
	// once a day on one replica, in CleanupTimezone.
	CleanupAt       string `cfg:"cleanup_at" default:"01:00"`
	CleanupTimezone string `cfg:"cleanup_timezone" default:"UTC"`
}

// Executor sets which executors replicas have and their limits. Every value
// here can be changed from the UI (Settings → Configuration); a value set
// there wins over this one.
type Executor struct {
	HTTP       Toggle     `cfg:"http"`
	Command    Command    `cfg:"command"`
	JS         JS         `cfg:"js"`
	Docker     Docker     `cfg:"docker"`
	Swarm      Swarm      `cfg:"swarm"`
	Kubernetes Kubernetes `cfg:"kubernetes"`
}

type Toggle struct {
	Enabled bool `cfg:"enabled" default:"true"`
}

type Docker struct {
	Enabled bool `cfg:"enabled" default:"true"`
	// Host is the Docker Engine; empty uses DOCKER_HOST or the local socket.
	Host            string   `cfg:"host"`
	AllowedImages   []string `cfg:"allowed_images"`
	Network         string   `cfg:"network"`
	AllowPrivileged bool     `cfg:"allow_privileged"`
	AllowMounts     bool     `cfg:"allow_mounts"`
	// Pull is missing, always or never.
	Pull string `cfg:"pull" default:"missing"`
}

type Swarm struct {
	Enabled       bool     `cfg:"enabled" default:"true"`
	Host          string   `cfg:"host"`
	AllowedImages []string `cfg:"allowed_images"`
	Networks      []string `cfg:"networks"`
	AllowMounts   bool     `cfg:"allow_mounts"`
}

type Kubernetes struct {
	Enabled           bool          `cfg:"enabled" default:"true"`
	Kubeconfig        string        `cfg:"kubeconfig"`
	Context           string        `cfg:"context"`
	Namespace         string        `cfg:"namespace"`
	AllowedNamespaces []string      `cfg:"allowed_namespaces"`
	AllowedImages     []string      `cfg:"allowed_images"`
	ServiceAccount    string        `cfg:"service_account"`
	TTL               time.Duration `cfg:"ttl" default:"10m"`
}

type JS struct {
	Enabled bool `cfg:"enabled" default:"true"`
	// MaxDuration stops a script whose job has no timeout.
	MaxDuration time.Duration `cfg:"max_duration" default:"10m"`
	// MaxRequests is the number of fetch calls one run may make.
	MaxRequests int `cfg:"max_requests" default:"1000"`
	// MaxResponse is the largest response body fetch reads, in bytes.
	MaxResponse int64 `cfg:"max_response" default:"10485760"`
	// Secrets are given to every script as secrets.NAME. They stay in the
	// replica's configuration: not stored with jobs and not shown. Set them
	// in the config file, or as KRONIK_SECRET_<NAME> environment variables
	// (KRONIK_SECRET_API_TOKEN is secrets.API_TOKEN).
	Secrets map[string]string `cfg:"secrets" log:"-"`
}

// SecretEnvPrefix starts the environment variables read as script secrets.
const SecretEnvPrefix = "KRONIK_SECRET_"

// secretsFromEnv reads KRONIK_SECRET_<NAME> variables. The whole rest of the
// name is the key; the generic loader would cut it at the first "_".
func secretsFromEnv(environ []string, into map[string]string) map[string]string {
	if into == nil {
		into = map[string]string{}
	}

	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, SecretEnvPrefix) || len(k) == len(SecretEnvPrefix) {
			continue
		}

		into[k[len(SecretEnvPrefix):]] = v
	}

	return into
}

type Command struct {
	Enabled bool   `cfg:"enabled" default:"true"`
	Shell   string `cfg:"shell" default:"/bin/sh"`
}

func Load(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := chu.Load(ctx, ServiceName, &cfg,
		chu.WithLoaderOption(loaderenv.New(
			loaderenv.WithPrefix("KRONIK_"),
		)),
		chu.WithVersion(Version),
	); err != nil {
		return nil, fmt.Errorf("load config; %w", err)
	}

	if err := logi.SetLogLevel(cfg.LogLevel); err != nil {
		return nil, fmt.Errorf("set log level %s; %w", cfg.LogLevel, err)
	}

	slog.Info("loaded configuration", "config", chu.MarshalMap(cfg))

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	if cfg.NodeName == "" {
		cfg.NodeName = nodeName()
	}

	cfg.Executor.JS.Secrets = secretsFromEnv(os.Environ(), cfg.Executor.JS.Secrets)

	if len(cfg.Executor.JS.Secrets) > 0 {
		names := make([]string, 0, len(cfg.Executor.JS.Secrets))
		for k := range cfg.Executor.JS.Secrets {
			names = append(names, k)
		}

		slices.Sort(names)
		slog.Info("script secrets loaded", "names", names)
	}

	return &cfg, nil
}

// identifier is what a table prefix may be: it is put into SQL as it is.
var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c *Config) validate() error {
	if c.Store.DSN == "" {
		return errors.New("store dsn is required, set KRONIK_STORE_DSN")
	}

	if c.Store.TablePrefix != "" && !identifier.MatchString(c.Store.TablePrefix) {
		return fmt.Errorf("store.table_prefix %q may only have letters, digits and _", c.Store.TablePrefix)
	}

	if c.Store.MaxOpenConns <= 0 {
		return errors.New("store.max_open_conns must be positive")
	}

	if b := c.Server.BasePath; b != "" {
		c.Server.BasePath = "/" + strings.Trim(b, "/")
	}

	s := c.Scheduler
	switch {
	case s.PollInterval < 100*time.Millisecond:
		return errors.New("scheduler.poll_interval must be at least 100ms")
	case s.ShutdownTimeout <= 0:
		return errors.New("scheduler.shutdown_timeout must be positive")
	case s.MaxConcurrent <= 0:
		return errors.New("scheduler.max_concurrent must be positive")
	case s.MaxOutput <= 0:
		return errors.New("scheduler.max_output must be positive")
	case s.NodeTimeout < 3*heartbeatFloor:
		return fmt.Errorf("scheduler.node_timeout must be at least %s", 3*heartbeatFloor)
	case s.RunRetention < 0 || s.RunsPerJob < 0 || s.AuditRetention < 0:
		return errors.New("scheduler.run_retention, runs_per_job and audit_retention must not be negative")
	}

	if js := c.Executor.JS; js.Enabled && (js.MaxDuration <= 0 || js.MaxResponse <= 0 || js.MaxRequests < 0) {
		return errors.New("executor.js.max_duration and max_response must be positive, max_requests must not be negative")
	}

	return nil
}

// heartbeatFloor is the shortest heartbeat a replica uses.
const heartbeatFloor = 5 * time.Second

func nodeName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = ServiceName
	}

	b := make([]byte, 3)
	_, _ = rand.Read(b)

	return host + "-" + hex.EncodeToString(b)
}
