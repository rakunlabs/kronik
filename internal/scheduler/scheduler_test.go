package scheduler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/scheduler"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/store"
)

// newStore opens a store on KRONIK_TEST_DSN with its own table prefix.
func newStore(t *testing.T) (*store.Store, config.Store) {
	t.Helper()

	dsn := os.Getenv("KRONIK_TEST_DSN")
	if dsn == "" {
		t.Skip("KRONIK_TEST_DSN is not set")
	}

	prefix := fmt.Sprintf("t%d_", time.Now().UnixNano())
	cfg := config.Store{DSN: dsn, TablePrefix: prefix, MaxOpenConns: 10}

	st, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = st.Close()

		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = db.Close() }()

		for _, table := range []string{"notifications", "channels", "secrets", "secret_keys", "runs", "jobs", "nodes", "audit", "settings", "migrations"} {
			_, _ = db.Exec("DROP TABLE IF EXISTS " + prefix + table + " CASCADE")
		}
	})

	return st, cfg
}

func schedCfg() config.Scheduler {
	return config.Scheduler{
		PollInterval:    100 * time.Millisecond,
		MaxConcurrent:   10,
		MaxOutput:       1 << 16,
		ShutdownTimeout: 5 * time.Second,
		NodeTimeout:     15 * time.Second,
	}
}

func registry() executor.Registry {
	return executor.Registry{
		executor.TypeHTTP:    executor.NewHTTP(),
		executor.TypeCommand: executor.NewCommand("/bin/sh"),
	}
}

type node struct {
	sch  *scheduler.Scheduler
	stop func()
}

func startNode(t *testing.T, st *store.Store, name string) *node {
	t.Helper()

	sch := scheduler.New(st, registry(), schedCfg(), name)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sch.Run(ctx)
	}()

	n := &node{sch: sch, stop: sync.OnceFunc(func() { cancel(); <-done })}
	t.Cleanup(n.stop)

	return n
}

func waitFor(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func runs(t *testing.T, st *store.Store, jobID string) []*store.Run {
	t.Helper()

	rs, err := st.ListRuns(context.Background(), store.RunFilter{JobID: jobID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}

	return rs
}

func cmdJob(name, command string, mod func(*service.JobInput)) service.JobInput {
	in := service.JobInput{
		Name:     name,
		Schedule: "@every 1s",
		Executor: executor.TypeCommand,
		Config:   json.RawMessage(fmt.Sprintf(`{"command":%q}`, command)),
	}
	if mod != nil {
		mod(&in)
	}

	return in
}

// Two replicas on one store run every scheduled time exactly once.
func TestTwoReplicasRunEachTimeOnce(t *testing.T) {
	st, _ := newStore(t)

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	a := startNode(t, st, "a")
	b := startNode(t, st, "b")

	svc := service.New(st, registry(), a.sch)

	job, err := svc.CreateJob(context.Background(), service.JobInput{
		Name:     "ping",
		Schedule: "@every 1s",
		Executor: executor.TypeHTTP,
		Config:   json.RawMessage(`{"url":"` + srv.URL + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(4500 * time.Millisecond)

	if _, err := svc.SetEnabled(context.Background(), job.ID, false); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, "runs to finish", func() bool {
		for _, r := range runs(t, st, job.ID) {
			if !r.Finished() {
				return false
			}
		}

		return true
	})

	a.stop()
	b.stop()

	rs := runs(t, st, job.ID)
	seen := map[time.Time]bool{}
	nodes := map[string]bool{}

	for _, r := range rs {
		if seen[r.ScheduledAt] {
			t.Fatalf("time %s ran twice", r.ScheduledAt)
		}

		seen[r.ScheduledAt] = true

		if r.Status != store.StatusSucceeded {
			t.Errorf("run %s: %s %s", r.ID, r.Status, r.Error)
		}

		nodes[r.Node] = true
	}

	if len(rs) < 3 || len(rs) > 6 {
		t.Fatalf("got %d runs in ~4.5s", len(rs))
	}

	if int(hits.Load()) != len(rs) {
		t.Fatalf("server hit %d times for %d runs", hits.Load(), len(rs))
	}

	t.Logf("%d runs on replicas %v", len(rs), nodes)
}

func TestForbidSkipsAndCancel(t *testing.T) {
	st, _ := newStore(t)
	n := startNode(t, st, "a")
	svc := service.New(st, registry(), n.sch)
	ctx := context.Background()

	job, err := svc.CreateJob(ctx, cmdJob("slow", "sleep 30", nil))
	if err != nil {
		t.Fatal(err)
	}

	var running *store.Run

	waitFor(t, 5*time.Second, "a running run and a skipped one", func() bool {
		var skipped bool

		running = nil

		for _, r := range runs(t, st, job.ID) {
			switch r.Status {
			case store.StatusRunning:
				running = r
			case store.StatusSkipped:
				skipped = true
			}
		}

		return running != nil && skipped
	})

	if _, err := svc.SetEnabled(ctx, job.ID, false); err != nil {
		t.Fatal(err)
	}

	if err := svc.CancelRun(ctx, running.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 8*time.Second, "the run to be canceled", func() bool {
		r, err := st.GetRun(ctx, running.ID)

		return err == nil && r.Status == store.StatusCanceled
	})
}

func TestRetriesAndTimeout(t *testing.T) {
	st, _ := newStore(t)
	n := startNode(t, st, "a")
	svc := service.New(st, registry(), n.sch)
	ctx := context.Background()

	zero := 0

	job, err := svc.CreateJob(ctx, cmdJob("flaky", "echo try $KRONIK_ATTEMPT; exit 1", func(in *service.JobInput) {
		in.Schedule = "0 0 1 1 *"
		in.Retries = 2
		in.RetryDelaySeconds = &zero
	}))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Trigger(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, "three failed attempts", func() bool {
		rs := runs(t, st, job.ID)
		if len(rs) != 3 {
			return false
		}

		for _, r := range rs {
			if r.Status != store.StatusFailed {
				return false
			}
		}

		return true
	})

	r, err := st.GetRun(ctx, runs(t, st, job.ID)[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	if r.Attempt != 3 || r.Trigger != store.TriggerRetry || strings.TrimSpace(r.Output) != "try 3" {
		t.Fatalf("last attempt: %+v", r)
	}

	timed, err := svc.CreateJob(ctx, cmdJob("timed", "sleep 10", func(in *service.JobInput) {
		in.Schedule = "0 0 1 1 *"
		in.TimeoutSeconds = 1
	}))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Trigger(ctx, timed.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 8*time.Second, "the run to time out", func() bool {
		rs := runs(t, st, timed.ID)

		return len(rs) == 1 && rs[0].Status == store.StatusTimedOut
	})
}

// A replica that disappears has its running runs marked lost by another.
func TestLostRuns(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()

	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("x", "true", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *" }))
	if err != nil {
		t.Fatal(err)
	}

	runID, err := svc.Trigger(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}

	old := time.Now().UTC().Add(-time.Hour)
	if err := st.Heartbeat(ctx, "gone", "v0", old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := st.ClaimRuns(ctx, "gone", time.Now().UTC(), 1, []string{executor.TypeCommand}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()

	n, err := st.RecoverLost(ctx, now.Add(-time.Minute), now)
	if err != nil || n != 1 {
		t.Fatalf("recovered %d, err %v", n, err)
	}

	r, err := st.GetRun(ctx, runID)
	if err != nil || r.Status != store.StatusLost {
		t.Fatalf("run %+v err %v", r, err)
	}

	ok, err := st.FinishRun(ctx, runID, "gone", store.Result{Status: store.StatusSucceeded}, now)
	if err != nil || ok {
		t.Fatalf("a lost run must not be finished: ok=%v err=%v", ok, err)
	}
}

func TestCatchUp(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	for _, catchUp := range []bool{false, true} {
		job, err := svc.CreateJob(ctx, cmdJob(fmt.Sprintf("c-%v", catchUp), "true", func(in *service.JobInput) {
			in.Schedule = "* * * * *"
			in.CatchUp = catchUp
			in.Concurrency = store.ConcurrencyAllow
		}))
		if err != nil {
			t.Fatal(err)
		}

		// Pretend it was due 10 minutes ago.
		past := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
		job.NextRunAt = &past

		if err := st.UpdateJob(ctx, job, job.UpdatedAt); err != nil {
			t.Fatal(err)
		}
	}

	sch := scheduler.New(st, registry(), schedCfg(), "a")
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	_ = sch.Run(cctx)

	jobs, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}

	for _, j := range jobs {
		got := len(runs(t, st, j.ID))

		want := 1
		if j.CatchUp {
			want = 11
		}

		if got != want {
			t.Errorf("job %s: %d runs, want %d", j.Name, got, want)
		}

		if j.NextRunAt == nil || !j.NextRunAt.After(time.Now()) {
			t.Errorf("job %s: next run %v not in the future", j.Name, j.NextRunAt)
		}
	}
}

type noWake struct{}

func (noWake) Wake() {}
