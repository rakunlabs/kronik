// Package service checks and applies changes to jobs and runs for the API.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/schedule"
	"github.com/rakunlabs/kronik/internal/store"
)

// ErrInvalid marks input errors, answered with 400.
var ErrInvalid = errors.New("invalid")

type inputError struct{ msg string }

func (e inputError) Error() string        { return e.msg }
func (e inputError) Is(target error) bool { return target == ErrInvalid }

// Invalid is an input error, answered with 400.
func Invalid(format string, a ...any) error {
	return invalid(format, a...)
}

func invalid(format string, a ...any) error {
	return inputError{msg: fmt.Sprintf(format, a...)}
}

type Waker interface {
	Wake()
}

type Service struct {
	store     *store.Store
	executors executor.Executors
	waker     Waker
	// tests bounds the test runs going on at once on this replica.
	tests chan struct{}
}

// maxTests is the number of test runs one replica runs at once.
const maxTests = 4

func New(st *store.Store, executors executor.Executors, waker Waker) *Service {
	return &Service{store: st, executors: executors, waker: waker, tests: make(chan struct{}, maxTests)}
}

// ErrBusy is a test run refused because others are going on (429).
var ErrBusy = errors.New("busy")

func (s *Service) Store() *store.Store { return s.store }

func (s *Service) Executors() []string { return s.executors.Names() }

// Executor is the executor of a type, as the settings make it now.
func (s *Service) Executor(name string) (executor.Executor, error) { return s.executors.Get(name) }

// JobInput is a job as the API takes it.
type JobInput struct {
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	Tags              []string        `json:"tags"`
	Schedule          string          `json:"schedule"`
	Timezone          string          `json:"timezone"`
	Enabled           *bool           `json:"enabled"`
	Executor          string          `json:"executor"`
	Config            json.RawMessage `json:"config"`
	Concurrency       string          `json:"concurrency"`
	CatchUp           bool            `json:"catch_up"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	Retries           int             `json:"retries"`
	RetryDelaySeconds *int            `json:"retry_delay_seconds"`
	// UpdatedAt, on an update, is the version the change was made from. A
	// job changed since then is not overwritten (409).
	UpdatedAt *time.Time `json:"updated_at"`
}

const (
	maxRetries     = 100
	maxNameLength  = 200
	maxTags        = 20
	defaultRetryIn = 10
)

func (s *Service) apply(j *store.Job, in JobInput, now time.Time, create bool) error {
	j.Name = strings.TrimSpace(in.Name)
	if j.Name == "" {
		return invalid("name is required")
	}

	if len(j.Name) > maxNameLength {
		return invalid("name is longer than %d characters", maxNameLength)
	}

	// The name goes into the X-Kronik-Job header and the logs.
	if strings.ContainsFunc(j.Name, unicode.IsControl) {
		return invalid("name must not contain control characters")
	}

	j.Description = strings.TrimSpace(in.Description)

	j.Tags = []string{}
	for _, t := range in.Tags {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(j.Tags, t) {
			j.Tags = append(j.Tags, t)
		}
	}

	if len(j.Tags) > maxTags {
		return invalid("at most %d tags", maxTags)
	}

	prevSchedule, prevZone, prevEnabled, prevNext := j.Schedule, j.Timezone, j.Enabled, j.NextRunAt

	j.Schedule = strings.TrimSpace(in.Schedule)
	j.Timezone = strings.TrimSpace(in.Timezone)
	if j.Timezone == "" {
		j.Timezone = "UTC"
	}

	sch, err := schedule.Parse(j.Schedule, j.Timezone)
	if err != nil {
		return invalid("schedule: %v", err)
	}

	// A new job is active unless asked otherwise; an update without the
	// field keeps the job as it is (pause and resume have their own calls).
	if in.Enabled != nil {
		j.Enabled = *in.Enabled
	} else if create {
		j.Enabled = true
	}

	j.Executor = in.Executor

	exec, err := s.executors.Get(in.Executor)
	if err != nil {
		return invalid("%v", err)
	}

	cfg, err := exec.Validate(in.Config)
	if err != nil {
		return invalid("config: %v", err)
	}

	j.Config = cfg

	j.Concurrency = in.Concurrency
	if j.Concurrency == "" {
		j.Concurrency = store.ConcurrencyForbid
	}

	if !slices.Contains([]string{store.ConcurrencyAllow, store.ConcurrencyForbid, store.ConcurrencyReplace}, j.Concurrency) {
		return invalid("concurrency must be allow, forbid or replace")
	}

	j.CatchUp = in.CatchUp

	if in.TimeoutSeconds < 0 {
		return invalid("timeout must not be negative")
	}

	j.TimeoutSeconds = in.TimeoutSeconds

	if in.Retries < 0 || in.Retries > maxRetries {
		return invalid("retries must be between 0 and %d", maxRetries)
	}

	j.Retries = in.Retries

	j.RetryDelaySeconds = defaultRetryIn
	if in.RetryDelaySeconds != nil {
		if *in.RetryDelaySeconds < 0 {
			return invalid("retry delay must not be negative")
		}

		j.RetryDelaySeconds = *in.RetryDelaySeconds
	}

	j.NextRunAt = nil

	switch {
	case !j.Enabled:
	case !create && prevEnabled && prevNext != nil && j.Schedule == prevSchedule && j.Timezone == prevZone:
		// Same schedule: a run due right now is not moved to its next time
		// by an edit of something else.
		j.NextRunAt = prevNext
	default:
		next := sch.Next(now)
		if next.IsZero() {
			return invalid("the schedule has no run time in the next years")
		}

		j.NextRunAt = &next
	}

	j.UpdatedAt = now

	return nil
}

func (s *Service) CreateJob(ctx context.Context, in JobInput) (*store.Job, error) {
	now := store.Now()
	j := &store.Job{ID: store.NewID(), CreatedAt: now}

	if err := s.apply(j, in, now, true); err != nil {
		return nil, err
	}

	j.CreatedBy = ActorFrom(ctx).User
	j.UpdatedBy = j.CreatedBy

	if err := s.store.CreateJob(ctx, j); err != nil {
		return nil, err
	}

	s.audit(ctx, ActionJobCreate, j, "", map[string]any{"schedule": j.Schedule, "executor": j.Executor, "enabled": j.Enabled})

	return j, nil
}

// UpdateJob replaces the job. Its next run is computed again from now.
func (s *Service) UpdateJob(ctx context.Context, id string, in JobInput) (*store.Job, error) {
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}

	if in.UpdatedAt != nil && !in.UpdatedAt.Equal(j.UpdatedAt) {
		return nil, store.Conflict("the job was changed since it was opened; reload it and apply the change again")
	}

	prev := j.UpdatedAt
	before := *j

	if err := s.apply(j, in, store.Now(), false); err != nil {
		return nil, err
	}

	j.UpdatedBy = ActorFrom(ctx).User

	if err := s.store.UpdateJob(ctx, j, prev); err != nil {
		return nil, err
	}

	s.audit(ctx, ActionJobUpdate, j, "", map[string]any{"changed": changes(&before, j)})

	return j, nil
}

func (s *Service) DeleteJob(ctx context.Context, id string) error {
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return err
	}

	if err := s.store.DeleteJob(ctx, id); err != nil {
		return err
	}

	s.audit(ctx, ActionJobDelete, j, "", nil)

	return nil
}

func (s *Service) SetEnabled(ctx context.Context, id string, enabled bool) (*store.Job, error) {
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}

	now := store.Now()

	var next *time.Time

	if enabled {
		sch, err := schedule.Parse(j.Schedule, j.Timezone)
		if err != nil {
			return nil, invalid("schedule: %v", err)
		}

		if t := sch.Next(now); !t.IsZero() {
			next = &t
		}
	}

	if err := s.store.SetEnabled(ctx, id, enabled, next, now, ActorFrom(ctx).User); err != nil {
		return nil, err
	}

	action := ActionJobPause
	if enabled {
		action = ActionJobResume
	}

	s.audit(ctx, action, j, "", nil)

	return s.store.GetJob(ctx, id)
}

func (s *Service) Trigger(ctx context.Context, jobID string) (string, error) {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return "", err
	}

	id, err := s.store.TriggerRun(ctx, jobID, time.Now().UTC(), ActorFrom(ctx).User)
	if err != nil {
		return "", err
	}

	s.audit(ctx, ActionJobTrigger, j, id, nil)
	s.waker.Wake()

	return id, nil
}

func (s *Service) CancelRun(ctx context.Context, id string) error {
	if err := s.store.CancelRun(ctx, id, time.Now().UTC(), ActorFrom(ctx).User); err != nil {
		return err
	}

	r, err := s.store.GetRun(ctx, id)
	if err == nil {
		s.audit(ctx, ActionRunCancel, &store.Job{ID: r.JobID, Name: r.JobName}, id, nil)
	}

	return nil
}

// Preview lists the next run times of a schedule.
type Preview struct {
	Times []time.Time `json:"times"`
	Error string      `json:"error,omitempty"`
}

func Next(expr, timezone string, n int) Preview {
	sch, err := schedule.Parse(expr, timezone)
	if err != nil {
		return Preview{Times: []time.Time{}, Error: err.Error()}
	}

	return Preview{Times: sch.NextN(time.Now().UTC(), n)}
}
