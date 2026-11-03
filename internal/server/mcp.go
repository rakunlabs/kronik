package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/store"
)

// The MCP endpoint lets AI agents manage jobs with the same rules as the
// UI: changes go through the service, so they are checked the same way and
// written to the audit log with the user the proxy sent.

const mcpPrefix = "kronik_"

func (s *Server) mcpHandler() http.Handler {
	// One server per mode and environment, made again when they change.
	var (
		mu      sync.Mutex
		key     string
		current *mcp.Server
	)

	serverFor := func() *mcp.Server {
		v := s.values()
		k := fmt.Sprintf("%v|%s|%s", v.MCPReadOnly, s.env().Name, strings.Join(s.svc.Executors(), ","))

		mu.Lock()
		defer mu.Unlock()

		if current == nil || k != key {
			key, current = k, s.newMCPServer(v.MCPReadOnly)
		}

		return current
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return serverFor() }, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		PropagateRequestCancellation: true,
		// kronik runs behind a reverse proxy that rewrites Host.
		DisableLocalhostProtection: true,
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.values().MCPEnabled {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"MCP is turned off (Settings → Configuration → MCP)"}`))

			return
		}

		r = r.WithContext(service.WithActor(r.Context(), service.Actor{User: s.userOf(r), Via: "mcp"}))
		handler.ServeHTTP(w, r)
	})
}

func (s *Server) newMCPServer(readOnly bool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    config.ServiceName,
		Title:   "kronik",
		Version: config.Build.Version,
	}, &mcp.ServerOptions{Instructions: s.env().mcpNote() + mcpInstructions(readOnly, s.svc.Executors())})

	s.registerMCPRead(server)

	if !readOnly {
		s.registerMCPWrite(server)
	}

	return server
}

// mcpNote tells the agent which installation it works on.
func (e environment) mcpNote() string {
	if e.Name == "" {
		return ""
	}

	note := "This kronik is the " + e.Name + " environment."
	if strings.Contains(strings.ToLower(e.Name), "prod") && !strings.Contains(strings.ToLower(e.Name), "preprod") {
		note += " It is production: confirm every change with the user before making it."
	}

	return note + "\n\n"
}

func mcpInstructions(readOnly bool, executors []string) string {
	var b strings.Builder

	b.WriteString("kronik runs cron jobs on PostgreSQL across one or more replicas; each scheduled time runs once.\n")
	b.WriteString("A job has a schedule (5-field cron or @hourly, @daily, @every 10m, in an IANA time zone) and an executor: ")
	b.WriteString(strings.Join(executors, ", ") + ".\n\n")
	b.WriteString("Executor configs:\n")
	b.WriteString(`- http: {"method":"POST","url":"https://…","headers":{},"body":"","expected_status":[200]}` + "\n")
	b.WriteString(`- command: {"command":"sh command line","env":{},"dir":""}` + "\n")
	b.WriteString(`- docker, swarm, kubernetes: {"image":"alpine:3.22","command":["sh","-c"],"args":["echo hi"],"env":{},"cpu":"0.5","memory":"256Mi","pull":"missing"}; ` +
		`docker also takes network, mounts, privileged; swarm networks, constraints, mounts; kubernetes namespace, service_account, node_selector, ` +
		`secret_env {"NAME":"secret/key"}. Exit code 0 is success; the container's output is the run's output.` + "\n")
	b.WriteString(`- js: {"script":"…","env":{}}. The script is the body of an async function: use await, console.log for output, return a JSON value as the result. ` +
		"Globals: fetch(url, {method, headers, body, insecure, redirect, timeout}) like the browser's (body objects are sent as JSON; " +
		"insecure: true skips the TLS check), sleep(ms), env, secrets (from the server configuration), job.\n\n")
	b.WriteString("Workflow: list jobs with " + mcpPrefix + "list_jobs, read one with " + mcpPrefix + "get_job, its runs with " +
		mcpPrefix + "list_runs and a run's output with " + mcpPrefix + "get_run. Check a schedule with " + mcpPrefix + "schedule_preview.\n")

	if readOnly {
		b.WriteString("This endpoint is read-only: no tool changes jobs. Tell the user to make changes in kronik.\n")
	} else {
		b.WriteString("Before saving a js job, try it with " + mcpPrefix + "test_job. Create with " + mcpPrefix + "create_job; " +
			mcpPrefix + "update_job replaces every field, so read the job first and send it back changed. " +
			"Ask the user before deleting a job or running one that changes other systems.\n")
	}

	return b.String()
}

// toolErr is the error a tool reports: the SDK returns it as a result with
// IsError set, which the agent reads and can act on.
func toolErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errors.New("not found")
	case errors.Is(err, service.ErrInvalid), errors.Is(err, store.ErrConflict), errors.Is(err, service.ErrBusy):
		return err
	}

	slog.Error("mcp tool", "error", err)

	return errors.New("internal error, see the kronik log")
}

func ptr[T any](v T) *T { return &v }

var closedWorld = ptr(false)

// mcpJob is a job as tools take and return it.
type mcpJob struct {
	ID                string            `json:"id,omitempty"`
	Name              string            `json:"name" jsonschema:"unique name of the job"`
	Description       string            `json:"description,omitempty"`
	Tags              []string          `json:"tags,omitempty"`
	Schedule          string            `json:"schedule" jsonschema:"5-field cron, or @hourly, @daily, @weekly, @monthly, @every 10m"`
	Timezone          string            `json:"timezone,omitempty" jsonschema:"IANA time zone, default UTC"`
	Enabled           *bool             `json:"enabled,omitempty" jsonschema:"whether it runs on its schedule; default true on create"`
	Executor          string            `json:"executor" jsonschema:"http, js, command, docker, swarm or kubernetes"`
	Config            map[string]any    `json:"config" jsonschema:"executor config, see the server instructions"`
	Concurrency       string            `json:"concurrency,omitempty" jsonschema:"when a run is due while one is active: forbid (default), allow or replace"`
	CatchUp           bool              `json:"catch_up,omitempty" jsonschema:"run every time missed while kronik was down (up to 100) instead of only the latest"`
	TimeoutSeconds    int               `json:"timeout_seconds,omitempty" jsonschema:"0 means none"`
	Retries           int               `json:"retries,omitempty"`
	RetryDelaySeconds *int              `json:"retry_delay_seconds,omitempty" jsonschema:"default 10"`
	NextRunAt         *time.Time        `json:"next_run_at,omitempty"`
	UpdatedAt         *time.Time        `json:"updated_at,omitempty" jsonschema:"on update: the updated_at read; the update is refused if the job changed since"`
	LastRun           *store.RunSummary `json:"last_run,omitempty"`
}

func toMCPJob(j *store.Job) mcpJob {
	var cfg map[string]any
	_ = json.Unmarshal(j.Config, &cfg)

	enabled := j.Enabled
	delay := j.RetryDelaySeconds
	updated := j.UpdatedAt

	return mcpJob{
		ID: j.ID, Name: j.Name, Description: j.Description, Tags: j.Tags, Schedule: j.Schedule, Timezone: j.Timezone,
		Enabled: &enabled, Executor: j.Executor, Config: cfg, Concurrency: j.Concurrency, CatchUp: j.CatchUp,
		TimeoutSeconds: j.TimeoutSeconds, Retries: j.Retries, RetryDelaySeconds: &delay, NextRunAt: j.NextRunAt,
		UpdatedAt: &updated, LastRun: j.LastRun,
	}
}

func (j mcpJob) input() service.JobInput {
	cfg, _ := json.Marshal(j.Config)

	return service.JobInput{
		Name: j.Name, Description: j.Description, Tags: j.Tags, Schedule: j.Schedule, Timezone: j.Timezone, Enabled: j.Enabled,
		Executor: j.Executor, Config: cfg, Concurrency: j.Concurrency, CatchUp: j.CatchUp, TimeoutSeconds: j.TimeoutSeconds,
		Retries: j.Retries, RetryDelaySeconds: j.RetryDelaySeconds, UpdatedAt: j.UpdatedAt,
	}
}

type jobRef struct {
	ID string `json:"id" jsonschema:"job id, or the job's name"`
}

// jobOf finds a job by id or, failing that, by its name.
func (s *Server) jobOf(ctx context.Context, ref string) (*store.Job, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, service.Invalid("id is required: the job id or name")
	}

	j, err := s.svc.Store().GetJob(ctx, ref)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return j, err
	}

	jobs, err := s.svc.Store().ListJobs(ctx, store.JobFilter{Search: ref})
	if err != nil {
		return nil, err
	}

	for _, j := range jobs {
		if j.Name == ref {
			return j, nil
		}
	}

	return nil, store.ErrNotFound
}

type listJobsIn struct {
	Search string `json:"search,omitempty" jsonschema:"matches name, description or tag"`
	Tag    string `json:"tag,omitempty"`
}

type listJobsOut struct {
	Jobs []mcpJob `json:"jobs"`
}

type listRunsIn struct {
	Job    string `json:"job,omitempty" jsonschema:"job id or name; empty for all jobs"`
	Status string `json:"status,omitempty" jsonschema:"comma-separated: queued, running, succeeded, failed, timed_out, canceled, skipped, lost"`
	Before string `json:"before,omitempty" jsonschema:"a run id, to page to older runs"`
	Limit  int    `json:"limit,omitempty" jsonschema:"default 20, at most 200"`
}

type listRunsOut struct {
	Runs []*store.Run `json:"runs"`
}

type getRunIn struct {
	ID string `json:"id"`
	// Tail keeps the end of long output.
	MaxOutput int `json:"max_output,omitempty" jsonschema:"bytes of output to return, the end is kept; default 20000"`
}

type previewIn struct {
	Schedule string `json:"schedule"`
	Timezone string `json:"timezone,omitempty"`
	Count    int    `json:"count,omitempty" jsonschema:"default 5"`
}

type auditIn struct {
	Job   string `json:"job,omitempty" jsonschema:"job id or name"`
	User  string `json:"user,omitempty"`
	Limit int    `json:"limit,omitempty" jsonschema:"default 50, at most 500"`
}

type auditOut struct {
	Entries []mcpAudit `json:"entries"`
}

// mcpAudit is an audit entry with its detail as an object: the schema the
// SDK derives from json.RawMessage is a byte array.
type mcpAudit struct {
	ID      string         `json:"id"`
	At      time.Time      `json:"at"`
	Actor   string         `json:"actor"`
	Via     string         `json:"via"`
	Action  string         `json:"action"`
	JobID   string         `json:"job_id,omitempty"`
	JobName string         `json:"job_name,omitempty"`
	RunID   string         `json:"run_id,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

func (s *Server) registerMCPRead(server *mcp.Server) {
	read := func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: closedWorld}
	}

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "list_jobs", Description: "List jobs with their schedule, next run time and last run.",
		Annotations: read("List jobs"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listJobsIn) (*mcp.CallToolResult, listJobsOut, error) {
		jobs, err := s.svc.Store().ListJobs(ctx, store.JobFilter{Search: in.Search, Tag: in.Tag})
		if err != nil {
			return nil, listJobsOut{}, toolErr(err)
		}

		out := listJobsOut{Jobs: make([]mcpJob, len(jobs))}
		for i, j := range jobs {
			out.Jobs[i] = toMCPJob(j)
		}

		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "get_job", Description: "Read one job with its whole config.", Annotations: read("Read a job"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, mcpJob, error) {
		j, err := s.jobOf(ctx, in.ID)
		if err != nil {
			return nil, mcpJob{}, toolErr(err)
		}

		return nil, toMCPJob(j), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "list_runs", Description: "List runs, newest first, without their output.", Annotations: read("List runs"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listRunsIn) (*mcp.CallToolResult, listRunsOut, error) {
		f := store.RunFilter{Before: in.Before, Limit: in.Limit}
		if f.Limit <= 0 {
			f.Limit = 20
		}

		f.Limit = min(f.Limit, 200)

		for v := range strings.SplitSeq(in.Status, ",") {
			if v = strings.TrimSpace(v); v != "" {
				f.Status = append(f.Status, v)
			}
		}

		if in.Job != "" {
			j, err := s.jobOf(ctx, in.Job)
			if err != nil {
				return nil, listRunsOut{}, toolErr(err)
			}

			f.JobID = j.ID
		}

		runs, err := s.svc.Store().ListRuns(ctx, f)
		if err != nil {
			return nil, listRunsOut{}, toolErr(err)
		}

		return nil, listRunsOut{Runs: runs}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "get_run", Description: "Read a run with its output, error and result.", Annotations: read("Read a run"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getRunIn) (*mcp.CallToolResult, *store.Run, error) {
		r, err := s.svc.Store().GetRun(ctx, in.ID)
		if err != nil {
			return nil, nil, toolErr(err)
		}

		limit := in.MaxOutput
		if limit <= 0 {
			limit = 20000
		}

		if len(r.Output) > limit {
			cut := len(r.Output) - limit
			for cut < len(r.Output) && !utf8.RuneStart(r.Output[cut]) {
				cut++
			}

			r.Output = "…" + r.Output[cut:]
			r.OutputTruncated = true
		}

		return nil, r, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "schedule_preview", Description: "Check a cron expression and list its next run times.",
		Annotations: read("Preview a schedule"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in previewIn) (*mcp.CallToolResult, service.Preview, error) {
		n := in.Count
		if n <= 0 || n > 50 {
			n = 5
		}

		return nil, service.Next(in.Schedule, in.Timezone, n), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "audit_log", Description: "Who changed, ran or canceled what, newest first.", Annotations: read("Read the audit log"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditIn) (*mcp.CallToolResult, auditOut, error) {
		f := store.AuditFilter{Actor: in.User, Limit: in.Limit}
		if f.Limit <= 0 {
			f.Limit = 50
		}

		f.Limit = min(f.Limit, 500)

		if in.Job != "" {
			if j, err := s.jobOf(ctx, in.Job); err == nil {
				f.JobID = j.ID
			} else {
				f.JobID = in.Job
			}
		}

		entries, err := s.svc.Store().ListAudit(ctx, f)
		if err != nil {
			return nil, auditOut{}, toolErr(err)
		}

		out := auditOut{Entries: make([]mcpAudit, len(entries))}
		for i, e := range entries {
			var d map[string]any
			_ = json.Unmarshal(e.Detail, &d)

			out.Entries[i] = mcpAudit{ID: e.ID, At: e.At, Actor: e.Actor, Via: e.Via, Action: e.Action,
				JobID: e.JobID, JobName: e.JobName, RunID: e.RunID, Detail: d}
		}

		return nil, out, nil
	})
}

type triggerOut struct {
	RunID string `json:"run_id"`
}

type deleteOut struct {
	Deleted string `json:"deleted"`
}

func (s *Server) registerMCPWrite(server *mcp.Server) {
	write := func(title string, destructive, idempotent bool) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(destructive), IdempotentHint: idempotent, OpenWorldHint: closedWorld}
	}

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "create_job", Description: "Create a job. It is active (scheduled) unless enabled is false.",
		Annotations: write("Create a job", false, false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJob) (*mcp.CallToolResult, mcpJob, error) {
		j, err := s.svc.CreateJob(ctx, in.input())
		if err != nil {
			return nil, mcpJob{}, toolErr(err)
		}

		return nil, toMCPJob(j), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "update_job",
		Description: "Replace a job with the fields given (read it first with get_job). id is the job id. " +
			"Without enabled, the job stays active or paused as it is.",
		Annotations: write("Update a job", true, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJob) (*mcp.CallToolResult, mcpJob, error) {
		ref, err := s.jobOf(ctx, in.ID)
		if err != nil {
			return nil, mcpJob{}, toolErr(err)
		}

		j, err := s.svc.UpdateJob(ctx, ref.ID, in.input())
		if err != nil {
			return nil, mcpJob{}, toolErr(err)
		}

		return nil, toMCPJob(j), nil
	})

	setEnabled := func(enabled bool) func(context.Context, *mcp.CallToolRequest, jobRef) (*mcp.CallToolResult, mcpJob, error) {
		return func(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, mcpJob, error) {
			ref, err := s.jobOf(ctx, in.ID)
			if err != nil {
				return nil, mcpJob{}, toolErr(err)
			}

			j, err := s.svc.SetEnabled(ctx, ref.ID, enabled)
			if err != nil {
				return nil, mcpJob{}, toolErr(err)
			}

			return nil, toMCPJob(j), nil
		}
	}

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "pause_job", Description: "Stop scheduling a job. Runs going on are not stopped.",
		Annotations: write("Pause a job", false, true),
	}, setEnabled(false))

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "resume_job", Description: "Schedule a paused job again, from now on.",
		Annotations: write("Resume a job", false, true),
	}, setEnabled(true))

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "trigger_job", Description: "Queue a run of a job now. Read its result later with get_run.",
		Annotations: &mcp.ToolAnnotations{Title: "Run a job now", DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, triggerOut, error) {
		ref, err := s.jobOf(ctx, in.ID)
		if err != nil {
			return nil, triggerOut{}, toolErr(err)
		}

		id, err := s.svc.Trigger(ctx, ref.ID)
		if err != nil {
			return nil, triggerOut{}, toolErr(err)
		}

		return nil, triggerOut{RunID: id}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "cancel_run", Description: "Cancel a queued or running run.",
		Annotations: write("Cancel a run", true, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getRunIn) (*mcp.CallToolResult, *store.Run, error) {
		if err := s.svc.CancelRun(ctx, in.ID); err != nil {
			return nil, nil, toolErr(err)
		}

		r, err := s.svc.Store().GetRun(ctx, in.ID)
		if err != nil {
			return nil, nil, toolErr(err)
		}

		r.Output = ""

		return nil, r, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "delete_job", Description: "Delete a job and all its runs. Ask the user first.",
		Annotations: write("Delete a job", true, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, deleteOut, error) {
		ref, err := s.jobOf(ctx, in.ID)
		if err != nil {
			return nil, deleteOut{}, toolErr(err)
		}

		if err := s.svc.DeleteJob(ctx, ref.ID); err != nil {
			return nil, deleteOut{}, toolErr(err)
		}

		return nil, deleteOut{Deleted: fmt.Sprintf("%s (%s)", ref.Name, ref.ID)}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: mcpPrefix + "test_job",
		Description: "Run a job's executor once now, as given, without saving it, and return the output and result. " +
			"For js jobs, use it to try a script before create_job. Runs for a minute at most. " +
			"An http or command test does its real work, like a run.",
		Annotations: &mcp.ToolAnnotations{Title: "Try a job", DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpJob) (*mcp.CallToolResult, *service.TestRun, error) {
		t, err := s.svc.Test(ctx, in.input())
		if err != nil {
			return nil, nil, toolErr(err)
		}

		return nil, t, nil
	})
}
