package scheduler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/store"
)

func TestUpdateKeepsPauseAndRefusesStaleVersion(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("v", "true", func(in *service.JobInput) { in.Schedule = "@hourly" }))
	if err != nil {
		t.Fatal(err)
	}

	opened := job.UpdatedAt

	// Paused while an edit form is open.
	if _, err := svc.SetEnabled(ctx, job.ID, false); err != nil {
		t.Fatal(err)
	}

	// Saving that form sends the version it was opened at: refused.
	in := cmdJob("v", "echo changed", func(in *service.JobInput) { in.Schedule = "@hourly"; in.UpdatedAt = &opened })
	if _, err := svc.UpdateJob(ctx, job.ID, in); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update: want conflict, got %v", err)
	}

	// With the current version and no enabled field, the pause is kept.
	cur, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}

	in.UpdatedAt = &cur.UpdatedAt

	updated, err := svc.UpdateJob(ctx, job.ID, in)
	if err != nil {
		t.Fatal(err)
	}

	if updated.Enabled || updated.NextRunAt != nil {
		t.Fatalf("update resumed a paused job: %+v", updated)
	}

	// The version returned is the one stored, so the next save works too.
	again, err := st.GetJob(ctx, job.ID)
	if err != nil || !again.UpdatedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("version %v vs stored %v (err %v)", updated.UpdatedAt, again.UpdatedAt, err)
	}

	if time.Since(again.UpdatedAt) > time.Minute {
		t.Fatalf("updated_at not moved: %v", again.UpdatedAt)
	}
}
