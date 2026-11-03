package service

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

// Actor is who made a change and how: the user from the reverse proxy's
// header, and "api", "ui" or "mcp".
type Actor struct {
	User string
	Via  string
}

type actorKey struct{}

func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

func ActorFrom(ctx context.Context) Actor {
	a, _ := ctx.Value(actorKey{}).(Actor)
	if a.Via == "" {
		a.Via = "api"
	}

	return a
}

const (
	ActionJobCreate  = "job.create"
	ActionJobUpdate  = "job.update"
	ActionJobDelete  = "job.delete"
	ActionJobPause   = "job.pause"
	ActionJobResume  = "job.resume"
	ActionJobTrigger = "job.trigger"
	ActionRunCancel  = "run.cancel"
	ActionScriptTest = "script.test"
)

// audit records a change in the log and the audit table. A failure to
// record it is logged but does not undo the change.
func (s *Service) audit(ctx context.Context, action string, job *store.Job, runID string, detail any) {
	a := ActorFrom(ctx)

	e := &store.AuditEntry{At: store.Now(), Actor: a.User, Via: a.Via, Action: action, RunID: runID}
	if job != nil {
		e.JobID, e.JobName = job.ID, job.Name
	}

	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			e.Detail = b
		}
	}

	slog.Info("audit", "action", action, "user", a.User, "via", a.Via, "job", e.JobName, "job_id", e.JobID, "run", runID)

	if s.store == nil {
		return
	}

	// Recorded even when the request was canceled right after the change.
	if err := s.store.AddAudit(context.WithoutCancel(ctx), e); err != nil {
		slog.Error("record audit entry", "action", action, "error", err)
	}
}

// changes lists the fields that differ between two versions of a job, for
// the audit detail of an update. Config is compared as a whole.
func changes(before, after *store.Job) []string {
	var out []string

	add := func(name string, changed bool) {
		if changed {
			out = append(out, name)
		}
	}

	add("name", before.Name != after.Name)
	add("description", before.Description != after.Description)
	add("tags", !equalStrings(before.Tags, after.Tags))
	add("schedule", before.Schedule != after.Schedule)
	add("timezone", before.Timezone != after.Timezone)
	add("enabled", before.Enabled != after.Enabled)
	add("executor", before.Executor != after.Executor)
	add("config", string(before.Config) != string(after.Config))
	add("concurrency", before.Concurrency != after.Concurrency)
	add("catch_up", before.CatchUp != after.CatchUp)
	add("timeout_seconds", before.TimeoutSeconds != after.TimeoutSeconds)
	add("retries", before.Retries != after.Retries)
	add("retry_delay_seconds", before.RetryDelaySeconds != after.RetryDelaySeconds)

	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

const (
	ActionSettings = "settings.change"
	ActionCleanup  = "settings.cleanup"
)

// AuditSettings records a change of settings with the old and new values.
func (s *Service) AuditSettings(ctx context.Context, keys []string, before, after settings.Values) {
	changes := make(map[string]any, len(keys))
	for _, k := range keys {
		changes[k] = map[string]json.RawMessage{"from": before.Get(k), "to": after.Get(k)}
	}

	s.audit(ctx, ActionSettings, nil, "", map[string]any{"changed": keys, "values": changes})
}

// AuditCleanup records a cleanup started from the UI.
func (s *Service) AuditCleanup(ctx context.Context, runs, audit int64) {
	s.audit(ctx, ActionCleanup, nil, "", map[string]any{"runs_deleted": runs, "audit_deleted": audit})
}
