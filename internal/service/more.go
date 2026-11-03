package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/schedule"
	"github.com/rakunlabs/kronik/internal/store"
)

const (
	ActionJobHook   = "job.webhook"
	ActionJobReport = "job.report"
	ActionBulk      = "jobs.bulk"
)

var notifyEvents = []string{store.NotifyFailure, store.NotifyRecovery, store.NotifyNoRun, store.NotifySuccess}

func (s *Service) checkNotify(n store.Notify) (store.Notify, error) {
	out := store.Notify{Channels: []string{}, On: []string{}}

	for _, e := range n.On {
		if !slices.Contains(notifyEvents, e) {
			return out, invalid("notify on %q: use failure, recovery, no_run or success", e)
		}

		if !slices.Contains(out.On, e) {
			out.On = append(out.On, e)
		}
	}

	if len(n.Channels) == 0 {
		return out, nil
	}

	channels, err := s.store.ListChannels(context.Background())
	if err != nil {
		return out, err
	}

	for _, id := range n.Channels {
		if !slices.ContainsFunc(channels, func(c *store.Channel) bool { return c.ID == id }) {
			return out, invalid("notification channel %q does not exist", id)
		}

		if !slices.Contains(out.Channels, id) {
			out.Channels = append(out.Channels, id)
		}
	}

	return out, nil
}

// checkAfter checks the jobs a job runs after: they exist, and no chain
// goes round back to the job.
func (s *Service) checkAfter(j *store.Job, in JobInput) error {
	j.AfterOn = in.AfterOn
	if j.AfterOn == "" {
		j.AfterOn = store.AfterSucceeded
	}

	if !slices.Contains([]string{store.AfterSucceeded, store.AfterFailed, store.AfterFinished}, j.AfterOn) {
		return invalid("run after on must be succeeded, failed or finished")
	}

	j.AfterJobs = []string{}

	for _, id := range in.AfterJobs {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(j.AfterJobs, id) {
			j.AfterJobs = append(j.AfterJobs, id)
		}
	}

	if len(j.AfterJobs) == 0 {
		return nil
	}

	if len(j.AfterJobs) > maxAfterJobs {
		return invalid("a job runs after at most %d jobs", maxAfterJobs)
	}

	if slices.Contains(j.AfterJobs, j.ID) {
		return invalid("a job cannot run after itself")
	}

	all, err := s.store.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		return err
	}

	after := map[string][]string{}
	for _, o := range all {
		after[o.ID] = o.AfterJobs
	}

	for _, id := range j.AfterJobs {
		if _, ok := after[id]; !ok {
			return invalid("run after: job %q does not exist", id)
		}
	}

	after[j.ID] = j.AfterJobs

	// A run of j starts its followers; a follower that leads back to j
	// would run forever.
	seen := map[string]bool{}

	var walk func(id string) bool

	walk = func(id string) bool {
		if id == j.ID && len(seen) > 0 {
			return true
		}

		if seen[id] {
			return false
		}

		seen[id] = true

		for _, p := range after[id] {
			if walk(p) {
				return true
			}
		}

		return false
	}

	if walk(j.ID) {
		return invalid("run after: the chain goes round back to this job")
	}

	return nil
}

// TriggerWith queues a run of the job with params, by hand.
func (s *Service) TriggerWith(ctx context.Context, jobID string, params map[string]string) (string, error) {
	if err := executor.CheckParams(params); err != nil {
		return "", invalid("params: %v", err)
	}

	j, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return "", err
	}

	id, err := s.store.StartRun(ctx, store.StartRun{JobID: jobID, Trigger: store.TriggerManual, By: ActorFrom(ctx).User, Params: params}, store.Now())
	if err != nil {
		return "", err
	}

	var detail any
	if len(params) > 0 {
		detail = map[string]any{"params": slices.Sorted(maps.Keys(params))}
	}

	s.audit(ctx, ActionJobTrigger, j, id, detail)
	s.waker.Wake()

	return id, nil
}

// HookHash is how a webhook token is stored.
func HookHash(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// NewHook gives the job a new webhook token and returns it; the token is
// shown once, only its hash is kept. An earlier token stops working.
func (s *Service) NewHook(ctx context.Context, jobID string) (string, error) {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return "", err
	}

	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	token := "kh_" + base64.RawURLEncoding.EncodeToString(b)

	if err := s.store.SetHook(ctx, jobID, HookHash(token)); err != nil {
		return "", err
	}

	s.audit(ctx, ActionJobHook, j, "", map[string]any{"token": "created"})

	return token, nil
}

// RemoveHook takes the job's webhook away.
func (s *Service) RemoveHook(ctx context.Context, jobID string) error {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}

	if err := s.store.SetHook(ctx, jobID, ""); err != nil {
		return err
	}

	s.audit(ctx, ActionJobHook, j, "", map[string]any{"token": "removed"})

	return nil
}

// ErrBadHook is an unknown or removed webhook token.
var ErrBadHook = errors.New("unknown webhook token")

// HookJob finds the job of a webhook token.
func (s *Service) HookJob(ctx context.Context, token string) (*store.Job, error) {
	if !strings.HasPrefix(token, "kh_") {
		return nil, ErrBadHook
	}

	j, err := s.store.JobByHook(ctx, HookHash(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrBadHook
	}

	return j, err
}

// HookTrigger queues a run through the job's webhook. A paused job is not
// run.
func (s *Service) HookTrigger(ctx context.Context, token string, params map[string]string) (*store.Job, string, error) {
	j, err := s.HookJob(ctx, token)
	if err != nil {
		return nil, "", err
	}

	if !j.Enabled {
		return j, "", store.Conflict("the job is paused")
	}

	if err := executor.CheckParams(params); err != nil {
		return j, "", invalid("params: %v", err)
	}

	id, err := s.store.StartRun(ctx, store.StartRun{JobID: j.ID, Trigger: store.TriggerWebhook, By: hookActor(ctx), Params: params}, store.Now())
	if err != nil {
		return j, "", err
	}

	s.audit(ctx, ActionJobTrigger, j, id, map[string]any{"via": "webhook"})
	s.waker.Wake()

	return j, id, nil
}

func hookActor(ctx context.Context) string {
	if u := ActorFrom(ctx).User; u != "" {
		return u
	}

	return "webhook"
}

// Report is a result reported from outside through the webhook: a job run
// by something else (a cron elsewhere) tells kronik it ran.
type Report struct {
	// Status is succeeded or failed; empty is succeeded.
	Status string `json:"status"`
	// ExitCode, when given, decides the status: 0 succeeded.
	ExitCode *int   `json:"exit_code"`
	Output   string `json:"output"`
	Error    string `json:"error"`
}

const maxReportOutput = 64 << 10

// HookReport records a reported result as a run of the job: it counts for
// the job's health, its "did not run" alert, notifications and chains.
func (s *Service) HookReport(ctx context.Context, token string, r Report) (*store.Job, string, error) {
	j, err := s.HookJob(ctx, token)
	if err != nil {
		return nil, "", err
	}

	res := store.Result{Status: r.Status, ResultCode: r.ExitCode, Error: strings.TrimSpace(r.Error)}

	switch {
	case r.ExitCode != nil && *r.ExitCode == 0:
		res.Status = store.StatusSucceeded
	case r.ExitCode != nil:
		res.Status = store.StatusFailed
	case res.Status == "":
		res.Status = store.StatusSucceeded
	case res.Status != store.StatusSucceeded && res.Status != store.StatusFailed:
		return j, "", invalid("status must be succeeded or failed")
	}

	if res.Status == store.StatusFailed && res.Error == "" {
		res.Error = "reported as failed"
	}

	res.Output = r.Output
	if len(res.Output) > maxReportOutput {
		res.Output = res.Output[len(res.Output)-maxReportOutput:]
	}

	id, err := s.store.Report(ctx, j.ID, res, hookActor(ctx), store.Now())
	if err != nil {
		return j, "", err
	}

	s.audit(ctx, ActionJobReport, j, id, map[string]any{"status": res.Status})
	s.waker.Wake()

	return j, id, nil
}

// Bulk is one action on many jobs.
type Bulk struct {
	IDs []string `json:"ids"`
	// Action is pause, resume, delete or trigger.
	Action string `json:"action"`
}

// BulkResult tells what happened to each job.
type BulkResult struct {
	Done   []string          `json:"done"`
	Failed map[string]string `json:"failed"`
}

const maxBulk = 500

func (s *Service) Bulk(ctx context.Context, b Bulk) (*BulkResult, error) {
	ids := slices.Compact(slices.Sorted(slices.Values(b.IDs)))
	if len(ids) == 0 {
		return nil, invalid("no jobs given")
	}

	if len(ids) > maxBulk {
		return nil, invalid("at most %d jobs at once", maxBulk)
	}

	res := &BulkResult{Done: []string{}, Failed: map[string]string{}}

	switch b.Action {
	case "pause", "resume":
		enable := b.Action == "resume"
		now := store.Now()

		changed, err := s.store.SetEnabledMany(ctx, ids, enable, func(j *store.Job) *time.Time {
			sch, err := schedule.Parse(j.Schedule, j.Timezone)
			if err != nil {
				return nil
			}

			if t := sch.Next(now); !t.IsZero() {
				return &t
			}

			return nil
		}, now, ActorFrom(ctx).User)
		if err != nil {
			return nil, err
		}

		action := ActionJobPause
		if enable {
			action = ActionJobResume
		}

		for _, j := range changed {
			s.audit(ctx, action, j, "", map[string]any{"bulk": true})
			res.Done = append(res.Done, j.ID)
		}

		// Jobs already in that state count as done.
		for _, id := range ids {
			if !slices.Contains(res.Done, id) {
				if _, err := s.store.GetJob(ctx, id); err != nil {
					res.Failed[id] = err.Error()
				} else {
					res.Done = append(res.Done, id)
				}
			}
		}
	case "delete":
		for _, id := range ids {
			if err := s.DeleteJob(ctx, id); err != nil {
				res.Failed[id] = err.Error()
			} else {
				res.Done = append(res.Done, id)
			}
		}
	case "trigger":
		for _, id := range ids {
			if _, err := s.Trigger(ctx, id); err != nil {
				res.Failed[id] = err.Error()
			} else {
				res.Done = append(res.Done, id)
			}
		}
	default:
		return nil, invalid("action must be pause, resume, delete or trigger")
	}

	return res, nil
}
