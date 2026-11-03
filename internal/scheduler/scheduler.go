// Package scheduler turns due jobs into runs and runs them. Every replica
// runs the same loop against the shared store; row locks decide which
// replica takes what.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/schedule"
	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

const (
	// scheduleBatch is how many due jobs one transaction takes.
	scheduleBatch = 100
	// maxCatchUp caps the runs created for times missed by one job.
	maxCatchUp = 100
	// outputFlush is how often the output of a run going on is saved.
	outputFlush = 2 * time.Second
	// stopCheck is how often running runs are checked for cancel requests.
	stopCheck = 2 * time.Second
	// finishFor is how long a run's result is retried when the database is
	// not reachable.
	finishFor = 2 * time.Minute
)

type Scheduler struct {
	store     *store.Store
	executors executor.Executors
	// retention is read when the daily cleanup runs; nil uses cfg.
	retention func() Retention
	// options are read at every tick; nil is no pause, window or limit.
	options func() Options
	// secrets are given to every run; nil is none.
	secrets func() map[string]string
	// observer is told about every run that ends; nil is nobody.
	observer  Observer
	cfg       config.Scheduler
	node      string
	startedAt time.Time

	// ctx is canceled when shutdown runs out of time; runs use it.
	runCtx    context.Context
	runCancel context.CancelFunc

	mu      sync.Mutex
	running map[string]*active
	wg      sync.WaitGroup
	slots   chan struct{}
	// wake makes the loop look for work at once, after a trigger.
	wake chan struct{}
}

type active struct {
	cancel context.CancelCauseFunc
}

func New(st *store.Store, executors executor.Executors, cfg config.Scheduler, node string) *Scheduler {
	runCtx, runCancel := context.WithCancel(context.Background())

	return &Scheduler{
		store:     st,
		executors: executors,
		cfg:       cfg,
		node:      node,
		startedAt: store.Now(),
		runCtx:    runCtx,
		runCancel: runCancel,
		running:   map[string]*active{},
		slots:     make(chan struct{}, cfg.MaxConcurrent),
		wake:      make(chan struct{}, 1),
	}
}

func (s *Scheduler) Node() string { return s.node }

// Options are the settings the scheduler follows while it runs.
type Options struct {
	// Paused skips every scheduled run.
	Paused bool
	// Windows skip the scheduled runs of the jobs they cover.
	Windows []settings.Window
	// TagLimits caps the runs going at once per tag, across replicas.
	TagLimits map[string]int
}

// Observer is told when a run ends, for metrics.
type Observer interface {
	RunFinished(job *store.Job, run *store.Run, status string, d time.Duration)
}

// SetOptions makes the scheduler read pause, windows and limits from fn.
func (s *Scheduler) SetOptions(fn func() Options) { s.options = fn }

// SetSecrets makes the scheduler give runs the secrets fn returns.
func (s *Scheduler) SetSecrets(fn func() map[string]string) { s.secrets = fn }

// SetObserver tells o about every run that ends.
func (s *Scheduler) SetObserver(o Observer) { s.observer = o }

func (s *Scheduler) opts() Options {
	if s.options == nil {
		return Options{}
	}

	return s.options()
}

func (s *Scheduler) secretValues() map[string]string {
	if s.secrets == nil {
		return nil
	}

	return s.secrets()
}

// Wake makes the scheduler look for work now instead of at the next tick.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is canceled, then waits for the runs going on up to
// the shutdown timeout and cancels the rest.
func (s *Scheduler) Run(ctx context.Context) error {
	// Another live process with this name would share its runs with us.
	other, err := s.store.NodeStartedAt(ctx, s.node, time.Now().UTC().Add(-s.cfg.NodeTimeout))
	if err != nil {
		return fmt.Errorf("check replica name; %w", err)
	}

	if !other.IsZero() {
		slog.Warn("a replica with this name was seen recently: a restart, or two replicas share the name (set KRONIK_NODE_NAME uniquely)",
			"node", s.node, "started_at", other)
	}

	if err := s.heartbeat(ctx); err != nil {
		return fmt.Errorf("register replica; %w", err)
	}

	// Runs left running by an earlier process under this name (a container
	// restart keeps the pod name) are not running anymore.
	if n, err := s.store.RecoverOwn(ctx, s.node, []string{}, s.startedAt, time.Now().UTC()); err != nil {
		return fmt.Errorf("recover runs of an earlier process; %w", err)
	} else if n > 0 {
		slog.Warn("runs of an earlier process with this name marked lost", "count", n)
	}

	slog.Info("scheduler started", "node", s.node, "max_concurrent", s.cfg.MaxConcurrent, "executors", s.executors.Names())

	// Heartbeat and cancel checks keep going while running runs finish on
	// shutdown, so other replicas do not take them as lost and a cancel
	// from the UI still reaches them.
	keepCtx, stopKeep := context.WithCancel(context.WithoutCancel(ctx))

	var bg, keep sync.WaitGroup

	keep.Go(func() { s.every(keepCtx, s.heartbeatInterval(), s.heartbeat) })
	keep.Go(func() { s.every(keepCtx, stopCheck, s.checkStops) })
	bg.Go(func() { s.every(ctx, s.heartbeatInterval(), s.recover) })
	bg.Go(func() { s.cleanupLoop(ctx) })
	bg.Go(func() { s.every(ctx, alertCheck, s.alertSilent) })

	s.loop(ctx)
	bg.Wait()

	s.shutdown(stopKeep, &keep)

	return nil
}

func (s *Scheduler) heartbeatInterval() time.Duration {
	return max(s.cfg.NodeTimeout/3, time.Second)
}

func (s *Scheduler) every(ctx context.Context, d time.Duration, fn func(context.Context) error) {
	t := time.NewTicker(d)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				slog.Error("scheduler task failed", "error", err)
			}
		}
	}
}

func (s *Scheduler) loop(ctx context.Context) {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()

	for {
		s.tick(ctx)

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	o := s.opts()

	for {
		n, err := s.store.ScheduleDue(ctx, now, scheduleBatch, s.plan(now, o))
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("schedule due jobs", "error", err)
			}

			break
		}

		if n < scheduleBatch {
			break
		}
	}

	s.claim(ctx, o)
}

// plan returns the run times of a due job: every missed time with catch-up,
// else only the latest, and its next run time after now. A schedule this
// replica cannot read (a time zone missing here) is an error: the job is
// tried again later, by any replica, instead of being turned off.
func (s *Scheduler) plan(now time.Time, o Options) func(*store.Job) (store.Plan, error) {
	return func(j *store.Job) (store.Plan, error) {
		sch, err := schedule.Parse(j.Schedule, j.Timezone)
		if err != nil {
			return store.Plan{}, fmt.Errorf("schedule %q in %s; %w", j.Schedule, j.Timezone, err)
		}

		limit := 1
		if j.CatchUp {
			limit = maxCatchUp
		}

		times := sch.Missed(*j.NextRunAt, now, limit)
		next := sch.NextAfter(times, now)

		p := store.Plan{Times: times, Skip: skipReason(o, j, now)}
		if !next.IsZero() {
			p.Next = &next
		}

		return p, nil
	}
}

// skipReason is why the scheduled runs of a job due now are skipped, "" when
// they run.
func skipReason(o Options, j *store.Job, now time.Time) string {
	if o.Paused {
		return "skipped: all schedules are paused (Settings → Configuration → Scheduling)"
	}

	for _, w := range o.Windows {
		if !w.Applies(j.Tags) {
			continue
		}

		if in, until := w.Active(now); in {
			return fmt.Sprintf("skipped: maintenance window %q until %s", w.Expr, until.UTC().Format(time.RFC3339))
		}
	}

	return ""
}

func (s *Scheduler) claim(ctx context.Context, o Options) {
	free := cap(s.slots) - len(s.slots)
	if free <= 0 {
		return
	}

	claimed, err := s.store.ClaimRunsLimited(ctx, s.node, time.Now().UTC(), free, s.executors.Names(), o.TagLimits)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("claim runs", "error", err)
		}

		return
	}

	for _, c := range claimed {
		s.slots <- struct{}{}

		runCtx, cancel := context.WithCancelCause(s.runCtx)

		s.mu.Lock()
		s.running[c.Run.ID] = &active{cancel: cancel}
		s.mu.Unlock()

		s.wg.Go(func() {
			defer func() { <-s.slots }()
			defer cancel(nil)

			s.execute(runCtx, c)

			s.mu.Lock()
			delete(s.running, c.Run.ID)
			s.mu.Unlock()
		})
	}
}

var (
	errTimeout  = errors.New("timed out")
	errShutdown = errors.New("kronik is shutting down")
)

func (s *Scheduler) execute(ctx context.Context, c store.Claimed) {
	run, job := c.Run, c.Job
	start := time.Now()
	log := slog.With("job", job.Name, "run", run.ID, "attempt", run.Attempt)

	out := executor.NewOutput(s.cfg.MaxOutput)
	res := store.Result{}

	exec, err := s.executors.Get(job.Executor)
	if err != nil {
		res.Status, res.Error = store.StatusFailed, err.Error()
	} else {
		if job.TimeoutSeconds > 0 {
			var cancel context.CancelFunc

			ctx, cancel = context.WithTimeoutCause(ctx, time.Duration(job.TimeoutSeconds)*time.Second, errTimeout)
			defer cancel()
		}

		log.Info("run started")

		stopFlush := s.flushOutput(run.ID, out)

		code, runErr := safeRun(ctx, exec, executor.Info{
			JobID:       job.ID,
			JobName:     job.Name,
			RunID:       run.ID,
			Attempt:     run.Attempt,
			ScheduledAt: run.ScheduledAt,
			Secrets:     s.secretValues(),
			Params:      run.Params,
		}, job.Config, out)

		stopFlush()

		res.ResultCode = code
		res.Status = store.StatusSucceeded

		if runErr != nil {
			res.Status, res.Error = store.StatusFailed, runErr.Error()

			if cause := context.Cause(ctx); cause != nil {
				res.Error = cause.Error()

				switch {
				case errors.Is(cause, errTimeout):
					res.Status = store.StatusTimedOut
				case errors.Is(cause, context.Canceled) && s.runCtx.Err() != nil:
					res.Status, res.Error = store.StatusFailed, errShutdown.Error()
				default:
					res.Status = store.StatusCanceled
				}
			}
		}
	}

	res.Output, res.OutputTruncated, _ = out.Snapshot()
	res.Value = out.Result()

	ok, err := s.finish(run.ID, res)
	if err != nil {
		log.Error("run result not recorded; the run is marked lost later", "status", res.Status, "error", err)

		return
	}

	if !ok {
		log.Warn("run result not recorded, it is not running on this replica anymore")

		return
	}

	log.Info("run finished", "status", res.Status, "error", res.Error)

	if s.observer != nil {
		s.observer.RunFinished(job, run, res.Status, time.Since(start))
	}

	s.Wake()
}

// safeRun runs an executor; a panic in it fails the run instead of taking
// the replica, and every run on it, down.
func safeRun(ctx context.Context, exec executor.Executor, info executor.Info, cfg json.RawMessage, out *executor.Output) (code *int, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("executor panic", "job", info.JobName, "run", info.RunID, "panic", r, "stack", string(debug.Stack()))
			code, err = nil, fmt.Errorf("internal error in the executor: %v", r)
		}
	}()

	return exec.Run(ctx, info, cfg, out)
}

// finish records a run's result, trying again while the database is not
// reachable. It goes on while shutting down, so the run does not end up lost.
func (s *Scheduler) finish(id string, res store.Result) (bool, error) {
	deadline := time.Now().Add(finishFor)
	wait := 250 * time.Millisecond

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok, err := s.store.FinishRun(ctx, id, s.node, res, time.Now().UTC())
		cancel()

		if err == nil || time.Now().Add(wait).After(deadline) {
			return ok, err
		}

		slog.Warn("record run result, trying again", "run", id, "error", err, "in", wait)
		time.Sleep(wait)
		wait = min(wait*2, 10*time.Second)
	}
}

// flushOutput saves the output of a run every few seconds while it changes.
func (s *Scheduler) flushOutput(runID string, out *executor.Output) func() {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		t := time.NewTicker(outputFlush)
		defer t.Stop()

		saved := 0

		for {
			select {
			case <-done:
				return
			case <-t.C:
				text, truncated, version := out.Snapshot()
				if version == saved {
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := s.store.SaveOutput(ctx, runID, text, truncated); err != nil {
					slog.Warn("save run output", "run", runID, "error", err)
				} else {
					saved = version
				}

				cancel()
			}
		}
	}()

	return func() {
		close(done)
		<-finished
	}
}

// checkStops cancels the runs going on here that were canceled from the API,
// replaced, deleted or taken as lost.
func (s *Scheduler) checkStops(ctx context.Context) error {
	ids := s.runningIDs()
	if len(ids) == 0 {
		return nil
	}

	stop, err := s.store.StopRequested(ctx, s.node, ids)
	if err != nil {
		return fmt.Errorf("check canceled runs; %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for id, reason := range stop {
		if a, ok := s.running[id]; ok {
			a.cancel(errors.New(reason))
		}
	}

	return nil
}

func (s *Scheduler) heartbeat(ctx context.Context) error {
	return s.store.Heartbeat(ctx, s.node, config.Build.Version, s.startedAt, time.Now().UTC())
}

// claimGrace keeps a run claimed a moment ago, and not yet listed as
// running, from being taken for an orphan.
const claimGrace = time.Minute

// recover marks lost the running runs of replicas that stopped, and the ones
// of this replica that it does not run anymore (their result could not be
// saved, or a claim's reply was lost).
func (s *Scheduler) recover(ctx context.Context) error {
	now := time.Now().UTC()

	lost, err := s.store.RecoverLost(ctx, now.Add(-s.cfg.NodeTimeout), now)
	if err != nil {
		return fmt.Errorf("recover lost runs; %w", err)
	}

	if lost > 0 {
		slog.Warn("runs of stopped replicas marked lost", "count", lost)
	}

	own, err := s.store.RecoverOwn(ctx, s.node, s.runningIDs(), now.Add(-claimGrace), now)
	if err != nil {
		return fmt.Errorf("recover own runs; %w", err)
	}

	if own > 0 {
		slog.Warn("runs this replica no longer runs marked lost", "count", own)
	}

	return nil
}

func (s *Scheduler) runningIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}

	return ids
}

// alertCheck is how often jobs are checked for the "did not run" alert.
const alertCheck = 30 * time.Second

// alertSilent raises the alert of jobs without a successful run for their
// alert time. Each job is taken by one replica (row locks).
func (s *Scheduler) alertSilent(ctx context.Context) error {
	alerted, err := s.store.AlertSilent(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("check jobs that did not run; %w", err)
	}

	for _, a := range alerted {
		slog.Warn("job did not succeed within its alert time", "job_id", a.JobID, "last_success", a.Since)
	}

	return nil
}

// Retention is what the daily cleanup keeps, and when it runs.
type Retention struct {
	Runs       time.Duration
	RunsPerJob int
	Audit      time.Duration
	// Notifications deletes sent and failed notifications older than this.
	Notifications time.Duration
	// At is the time of day ("01:00") in Zone.
	At   string
	Zone string
}

// SetRetention makes the cleanup read its settings from fn, so a change
// from the UI applies at the next cleanup.
func (s *Scheduler) SetRetention(fn func() Retention) { s.retention = fn }

func (s *Scheduler) retentionNow() Retention {
	if s.retention != nil {
		return s.retention()
	}

	return Retention{Runs: s.cfg.RunRetention, RunsPerJob: s.cfg.RunsPerJob, Audit: s.cfg.AuditRetention,
		At: s.cfg.CleanupAt, Zone: s.cfg.CleanupTimezone}
}

// cleanupLoop deletes old runs and audit entries once a day at the set
// time, on one replica: the one that marks the day done first.
func (s *Scheduler) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(cleanupCheck)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		r := s.retentionNow()

		due, day := cleanupDue(r, time.Now())
		if !due {
			continue
		}

		claimed, err := s.store.ClaimDaily(ctx, "cleanup", day)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("claim the daily cleanup", "error", err)
			}

			continue
		}

		if !claimed {
			continue
		}

		if err := s.CleanupNow(ctx, r); err != nil && ctx.Err() == nil {
			slog.Error("daily cleanup", "error", err)
		}
	}
}

// cleanupCheck is how often the cleanup time is looked at.
const cleanupCheck = time.Minute

// cleanupDue reports whether the cleanup time of today (in the zone) has
// passed, and today's date there.
func cleanupDue(r Retention, now time.Time) (bool, string) {
	loc, err := time.LoadLocation(r.Zone)
	if err != nil || r.Zone == "" {
		loc = time.UTC
	}

	local := now.In(loc)

	at, err := time.ParseInLocation("15:04", r.At, loc)
	if err != nil {
		at, _ = time.ParseInLocation("15:04", "01:00", loc)
	}

	when := time.Date(local.Year(), local.Month(), local.Day(), at.Hour(), at.Minute(), 0, 0, loc)

	return !local.Before(when), local.Format("2006-01-02")
}

// CleanupStats is what one cleanup deleted.
type CleanupStats struct {
	Runs          int64 `json:"runs"`
	Audit         int64 `json:"audit"`
	Notifications int64 `json:"notifications"`
}

// CleanupNow deletes old runs and audit entries now.
func (s *Scheduler) CleanupNow(ctx context.Context, r Retention) error {
	_, err := s.Cleanup(ctx, r)

	return err
}

// Cleanup deletes old runs and audit entries now and says how many.
func (s *Scheduler) Cleanup(ctx context.Context, r Retention) (CleanupStats, error) {
	now := time.Now().UTC()

	var (
		st     CleanupStats
		before time.Time
	)

	if r.Runs > 0 {
		before = now.Add(-r.Runs)
	}

	// Each round deletes a batch; go on until nothing is left.
	for {
		n, err := s.store.PruneRuns(ctx, before, r.RunsPerJob)
		if err != nil {
			return st, fmt.Errorf("prune runs; %w", err)
		}

		st.Runs += n

		if n == 0 || ctx.Err() != nil {
			break
		}
	}

	if r.Audit > 0 {
		n, err := s.store.PruneAudit(ctx, now.Add(-r.Audit))
		if err != nil {
			return st, fmt.Errorf("prune audit log; %w", err)
		}

		st.Audit = n
	}

	if r.Notifications > 0 {
		n, err := s.store.PruneNotifications(ctx, now.Add(-r.Notifications))
		if err != nil {
			return st, fmt.Errorf("prune notifications; %w", err)
		}

		st.Notifications = n
	}

	slog.Info("cleanup done", "runs_deleted", st.Runs, "audit_deleted", st.Audit, "notifications_deleted", st.Notifications,
		"keep_runs", r.Runs, "runs_per_job", r.RunsPerJob, "keep_audit", r.Audit)

	return st, nil
}

// shutdown waits for the runs going on, cancels them after the timeout and
// leaves the replica list.
func (s *Scheduler) shutdown(stopKeep context.CancelFunc, keep *sync.WaitGroup) {
	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	s.mu.Lock()
	n := len(s.running)
	s.mu.Unlock()

	if n > 0 {
		slog.Info("waiting for running runs", "count", n, "timeout", s.cfg.ShutdownTimeout)
	}

	select {
	case <-done:
	case <-time.After(s.cfg.ShutdownTimeout):
		slog.Warn("shutdown timeout reached, canceling running runs")
		s.runCancel()
		<-done
	}

	s.runCancel()

	stopKeep()
	keep.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.store.RemoveNode(ctx, s.node, s.startedAt); err != nil {
		slog.Warn("remove replica", "error", err)
	}
}
