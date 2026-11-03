package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/rakunlabs/ada"
	"github.com/rakunlabs/ada/handler/folder"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"
	mserver "github.com/rakunlabs/ada/middleware/server"
	mtelemetry "github.com/rakunlabs/ada/middleware/telemetry"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/notify"
	"github.com/rakunlabs/kronik/internal/scheduler"
	"github.com/rakunlabs/kronik/internal/secrets"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

const (
	apiPrefix = "/api/v1"
	maxBody   = 1 << 20
	// viteAsset matches Vite's content-hashed asset names.
	viteAsset = `-[\w-]{8}\.(js|css|woff2?)$`
)

type Server struct {
	svc         *service.Service
	node        string
	nodeTimeout time.Duration
	scheduler   schedulerInfo
	userHeader  string
	// set holds the settings changed from the UI; nil uses the config.
	set     *settings.Manager
	base    settings.Values
	cleaner Cleaner
	vault   *secrets.Vault
	sender  *notify.Sender
	live    *Live
	bus     *store.Bus
	metrics bool
}

// Deps are the parts the server works with. Only Service is required.
type Deps struct {
	Service  *service.Service
	UI       fs.FS
	Settings *settings.Manager
	Cleaner  Cleaner
	Vault    *secrets.Vault
	Sender   *notify.Sender
	Live     *Live
	Bus      *store.Bus
	// MetricsActive is set when a collector receives the metrics.
	MetricsActive bool
}

// Cleaner runs the retention cleanup on demand.
type Cleaner interface {
	Cleanup(ctx context.Context, r scheduler.Retention) (scheduler.CleanupStats, error)
}

// values are the settings in effect.
func (s *Server) values() settings.Values {
	if s.set != nil {
		return s.set.Values()
	}

	return s.base
}

func (s *Server) env() environment {
	v := s.values()

	return newEnvironment(v.Environment, v.EnvironmentColor)
}

// environment is the installation's label, for the UI and MCP.
type environment struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

func newEnvironment(name, color string) environment {
	name = strings.TrimSpace(name)
	if name == "" {
		return environment{}
	}

	if color == "" {
		switch l := strings.ToLower(name); {
		case strings.Contains(l, "preprod"), strings.Contains(l, "stag"), strings.Contains(l, "uat"), strings.Contains(l, "qa"):
			color = "#d97706"
		case strings.Contains(l, "prod"), strings.Contains(l, "live"):
			color = "#dc2626"
		default:
			color = "#16a34a"
		}
	}

	return environment{Name: name, Color: color}
}

// schedulerInfo is the part of the replica's configuration that is set per
// replica (not from the UI) and shown under Settings.
type schedulerInfo struct {
	PollInterval    string `json:"poll_interval"`
	MaxConcurrent   int    `json:"max_concurrent"`
	MaxOutput       int    `json:"max_output"`
	ShutdownTimeout string `json:"shutdown_timeout"`
	NodeTimeout     string `json:"node_timeout"`
}

func newSchedulerInfo(c config.Scheduler) schedulerInfo {
	return schedulerInfo{
		PollInterval:    c.PollInterval.String(),
		MaxConcurrent:   c.MaxConcurrent,
		MaxOutput:       c.MaxOutput,
		ShutdownTimeout: c.ShutdownTimeout.String(),
		NodeTimeout:     c.NodeTimeout.String(),
	}
}

func Start(ctx context.Context, cfg *config.Config, d Deps) error {
	server, err := New(cfg, d)
	if err != nil {
		return err
	}

	return server.StartWithContext(ctx, cfg.Server.Host+":"+cfg.Server.Port)
}

// New builds the server. Settings, Cleaner and the rest may be nil
// (tests): the settings are then the configuration's and cannot be changed.
func New(cfg *config.Config, d Deps) (*ada.Server, error) {
	s := &Server{
		svc:         d.Service,
		node:        cfg.NodeName,
		nodeTimeout: cfg.Scheduler.NodeTimeout,
		scheduler:   newSchedulerInfo(cfg.Scheduler),
		userHeader:  cfg.Server.UserHeader,
		set:         d.Settings,
		base:        cfg.Values(),
		cleaner:     d.Cleaner,
		vault:       d.Vault,
		sender:      d.Sender,
		live:        d.Live,
		bus:         d.Bus,
		metrics:     d.MetricsActive,
	}

	if s.vault == nil {
		s.vault = secrets.NewVault(nil, secrets.NewMasterKey(cfg.Secrets.Key), cfg.Executor.JS.Secrets)
	}

	if s.live == nil {
		s.live = NewLive()
	}

	ui := d.UI

	server := ada.New()
	server.Use(
		mrecover.Middleware(),
		mserver.Middleware(config.ServiceName+"/"+config.Build.Version),
		mrequestid.Middleware(),
		mlog.Middleware(),
		mtelemetry.Middleware(),
	)

	server.GET("/healthz", s.health)

	base := cfg.Server.BasePath
	root := server.Group(base)

	// A page on another site must not make the user's browser change jobs
	// (it carries the proxy's session cookie): cross-origin writes are
	// refused, and JSON bodies must be sent as JSON, which a plain form or
	// no-cors fetch cannot do.
	csrf := http.NewCrossOriginProtection()

	s.routes(root.Group(apiPrefix, limitBody, csrf.Handler, requireJSON, s.withActor))

	// Webhooks are called by other systems, without the proxy's session:
	// the token in the path is the credential, so no CSRF check applies.
	hooks := root.Group("/hook", limitBody, s.withActor)
	hooks.POST("/{token}", s.hookTrigger)
	hooks.GET("/{token}", s.hookTrigger)
	hooks.POST("/{token}/report", s.hookReport)
	hooks.GET("/{token}/report", s.hookReport)

	// Always mounted: turning MCP off or read-only in the settings applies
	// at once, without a restart.
	root.Handle("/mcp", s.mcpHandler(), limitBody, csrf.Handler)

	if err := mountUI(root, base, ui); err != nil {
		return nil, err
	}

	return server, nil
}

func (s *Server) routes(api *ada.Mux) {
	api.GET("/info", s.info)
	api.GET("/stats", s.stats)
	api.GET("/nodes", s.nodes)
	api.GET("/tags", s.tags)
	api.GET("/schedule/next", s.scheduleNext)

	api.GET("/jobs", s.listJobs)
	api.POST("/jobs", s.createJob)
	api.GET("/jobs/{id}", s.getJob)
	api.PUT("/jobs/{id}", s.updateJob)
	api.DELETE("/jobs/{id}", s.deleteJob)
	api.POST("/jobs/{id}/enable", s.enableJob)
	api.POST("/jobs/{id}/disable", s.disableJob)
	api.POST("/jobs/{id}/trigger", s.triggerJob)
	api.POST("/jobs/{id}/hook", s.newHook)
	api.DELETE("/jobs/{id}/hook", s.removeHook)
	api.GET("/jobs/{id}/stats", s.jobStats)
	api.GET("/jobs/{id}/history", s.jobHistory)
	api.POST("/jobs/bulk", s.bulkJobs)
	api.GET("/calendar", s.calendar)

	api.POST("/jobs/test", s.testJob)
	api.POST("/script/check", s.checkScript)

	api.GET("/runs", s.listRuns)
	api.GET("/runs/{id}", s.getRun)
	api.GET("/runs/{id}/events", s.runEvents)
	api.POST("/runs/{id}/cancel", s.cancelRun)

	api.GET("/audit", s.listAudit)

	api.GET("/settings", s.getSettings)
	api.PUT("/settings", s.putSettings)
	api.POST("/settings/cleanup", s.cleanupNow)
	api.POST("/settings/check/{executor}", s.checkExecutor)

	api.GET("/secrets", s.listSecrets)
	api.PUT("/secrets/{name}", s.putSecret)
	api.DELETE("/secrets/{name}", s.deleteSecret)
	api.PUT("/secret-key", s.changeSecretsKey)

	api.GET("/channels", s.listChannels)
	api.POST("/channels", s.createChannel)
	api.PUT("/channels/{id}", s.updateChannel)
	api.DELETE("/channels/{id}", s.deleteChannel)
	api.POST("/channels/{id}/test", s.testChannel)
	api.GET("/notifications", s.listNotifications)

	// Unknown API paths must not fall through to the SPA index.
	api.HandleFuncWildcard("/", func(c *ada.Context) error {
		return ada.NewHTTPError(http.StatusNotFound, "not found")
	})
}

func (s *Server) health(c *ada.Context) error {
	if err := s.svc.Store().Ping(c.Request.Context()); err != nil {
		return ada.NewHTTPError(http.StatusServiceUnavailable, "store unreachable")
	}

	return c.SendString("OK")
}

// maxUser bounds the user name taken from the proxy's header.
const maxUser = 200

// userOf is the user the reverse proxy put in the user header, "" when
// there is none. kronik trusts it as it is: only the proxy may reach kronik.
func (s *Server) userOf(r *http.Request) string {
	if s.userHeader == "" {
		return ""
	}

	values := r.Header.Values(s.userHeader)
	if len(values) != 1 {
		// None, or a proxy that added its value next to one the client sent:
		// neither can be told apart from a forged one.
		if len(values) > 1 {
			slog.Warn("request with more than one user header ignored; the proxy must replace it, not add to it",
				"header", s.userHeader, "path", r.URL.Path)
		}

		return ""
	}

	u := strings.TrimSpace(values[0])
	u = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}

		return c
	}, u)

	if len(u) > maxUser {
		u = u[:maxUser]
	}

	return u
}

// withActor puts the user and the client (the UI sends X-Kronik-Client)
// into the request's context, for the audit log.
func (s *Server) withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		via := "api"
		if r.Header.Get("X-Kronik-Client") == "ui" {
			via = "ui"
		}

		next.ServeHTTP(w, r.WithContext(service.WithActor(r.Context(), service.Actor{User: s.userOf(r), Via: via})))
	})
}

// requireJSON refuses a body sent as anything but JSON.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
			ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
			if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnsupportedMediaType)
				_, _ = w.Write([]byte(`{"message":"send the body as application/json"}`))

				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}

		next.ServeHTTP(w, r)
	})
}

// mountUI serves the built frontend under base; ui is nil when only the
// placeholder was embedded.
func mountUI(root *ada.Mux, base string, ui fs.FS) error {
	if ui == nil {
		return nil
	}

	f, err := folder.New(&folder.Config{
		Index:      true,
		SPA:        true,
		PrefixPath: base,
		// embed.FS has no modtime, so only a content hash can validate.
		ETag: folder.ETagStrong,
		CacheRegex: []*folder.RegexCacheStore{
			{Regex: viteAsset, CacheControl: "public, max-age=31536000, immutable"},
			{Regex: `.*`, CacheControl: "no-cache"},
		},
	})
	if err != nil {
		return fmt.Errorf("ui handler; %w", err)
	}

	f.SetFs(http.FS(ui))

	if base != "" {
		// "/cron" → "/cron/" so relative asset URLs resolve under the base.
		root.HandleFunc("", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, base+"/", http.StatusMovedPermanently)
		})
	}

	root.HandleWildcard("/", f)

	return nil
}
