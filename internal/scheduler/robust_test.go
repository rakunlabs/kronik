package scheduler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/scheduler"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/store"
)

func TestManualTriggerFollowsForbid(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("f", "sleep 5", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *" }))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Trigger(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Trigger(ctx, job.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second manual run of a forbid job: %v", err)
	}

	if _, err := svc.Trigger(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

// A process restarting under the same name marks the runs the old one left
// running as lost, instead of leaving them running forever.
func TestRestartRecoversOwnRuns(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("r", "true", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *" }))
	if err != nil {
		t.Fatal(err)
	}

	runID, err := svc.Trigger(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The old process claimed it, then died without a trace.
	if _, err := st.ClaimRuns(ctx, "pod-0", time.Now().UTC(), 1, []string{executor.TypeCommand}); err != nil {
		t.Fatal(err)
	}

	n := startNode(t, st, "pod-0")

	waitFor(t, 3*time.Second, "the orphan run to be marked lost", func() bool {
		r, err := st.GetRun(ctx, runID)

		return err == nil && r.Status == store.StatusLost
	})

	n.stop()
}

// A replica without an executor leaves its runs to the others.
func TestClaimOnlyOwnExecutors(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("c", "true", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *" }))
	if err != nil {
		t.Fatal(err)
	}

	runID, err := svc.Trigger(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}

	httpOnly := scheduler.New(st, executor.Registry{executor.TypeHTTP: executor.NewHTTP()}, schedCfg(), "http-only")
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	_ = httpOnly.Run(cctx)

	r, err := st.GetRun(ctx, runID)
	if err != nil || r.Status != store.StatusQueued {
		t.Fatalf("run taken by a replica without the executor: %+v %v", r, err)
	}
}

// Editing something else does not move a run that is due right now.
func TestEditKeepsDueRun(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("e", "true", func(in *service.JobInput) { in.Schedule = "0 10 * * *" }))
	if err != nil {
		t.Fatal(err)
	}

	due := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	job.NextRunAt = &due

	if err := st.UpdateJob(ctx, job, job.UpdatedAt); err != nil {
		t.Fatal(err)
	}

	cur, _ := st.GetJob(ctx, job.ID)
	in := cmdJob("e", "true", func(in *service.JobInput) {
		in.Schedule = "0 10 * * *"
		in.Description = "changed"
		in.UpdatedAt = &cur.UpdatedAt
	})

	updated, err := svc.UpdateJob(ctx, job.ID, in)
	if err != nil {
		t.Fatal(err)
	}

	if updated.NextRunAt == nil || !updated.NextRunAt.Equal(due) {
		t.Fatalf("next run moved by a description edit: %v, want %v", updated.NextRunAt, due)
	}

	// A new schedule does move it.
	cur, _ = st.GetJob(ctx, job.ID)
	in.Schedule = "0 11 * * *"
	in.UpdatedAt = &cur.UpdatedAt

	if updated, err = svc.UpdateJob(ctx, job.ID, in); err != nil || !updated.NextRunAt.After(time.Now()) {
		t.Fatalf("schedule change: %v %v", updated, err)
	}
}

func TestScheduleValidation(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	for _, mod := range []func(*service.JobInput){
		func(in *service.JobInput) { in.Timezone = "Local" },
		func(in *service.JobInput) { in.Schedule = "CRON_TZ=Local 0 * * * *" },
		func(in *service.JobInput) { in.Schedule = "0 0 30 2 *" },
		func(in *service.JobInput) { in.Name = "bad\nname" },
	} {
		in := cmdJob("v", "true", func(in *service.JobInput) { in.Schedule = "@hourly" })
		mod(&in)

		if _, err := svc.CreateJob(ctx, in); !errors.Is(err, service.ErrInvalid) {
			t.Errorf("%+v: want invalid, got %v", in, err)
		}
	}
}
