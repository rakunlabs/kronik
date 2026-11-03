package scheduler_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rakunlabs/kronik/internal/notify"
	"github.com/rakunlabs/kronik/internal/scheduler"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

func addChannel(t *testing.T, st *store.Store, url string) string {
	t.Helper()

	cfg, err := notify.CheckChannel(notify.TypeWebhook, json.RawMessage(`{"url":"`+url+`"}`))
	if err != nil {
		t.Fatal(err)
	}

	now := store.Now()
	ch := &store.Channel{ID: store.NewID(), Name: "hook-" + store.NewID(), Type: notify.TypeWebhook, Config: cfg, CreatedAt: now, UpdatedAt: now}

	if err := st.PutChannel(context.Background(), ch, true); err != nil {
		t.Fatal(err)
	}

	return ch.ID
}

func events(t *testing.T, st *store.Store, jobID string) []string {
	t.Helper()

	ns, err := st.ListNotifications(context.Background(), jobID, 100)
	if err != nil {
		t.Fatal(err)
	}

	var out []string
	for i := len(ns) - 1; i >= 0; i-- {
		out = append(out, ns[i].Event)
	}

	return out
}

// A failing job notifies once, not on every failure; it notifies again
// when it recovers; the sender delivers them.
func TestNotifyOnChange(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()

	var (
		mu     sync.Mutex
		bodies []string
	)

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer hook.Close()

	ch := addChannel(t, st, hook.URL)
	svc := service.New(st, registry(), noWake{})

	marker := t.TempDir() + "/ok"

	job, err := svc.CreateJob(ctx, cmdJob("flaky", "test -f "+marker, func(in *service.JobInput) {
		in.Schedule = "0 0 1 1 *"
		in.Concurrency = "allow"
		in.Notify = &store.Notify{Channels: []string{ch}, On: []string{"failure", "recovery"}}
	}))
	if err != nil {
		t.Fatal(err)
	}

	startNode(t, st, "n1")

	runOnce := func() {
		t.Helper()

		id, err := svc.Trigger(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}

		waitFor(t, 10*time.Second, "run "+id, func() bool {
			r, err := st.GetRun(ctx, id)

			return err == nil && r.Finished()
		})
	}

	runOnce()
	runOnce()

	if got := strings.Join(events(t, st, job.ID), ","); got != "failure" {
		t.Fatalf("after two failures: %q", got)
	}

	if j, _ := st.GetJob(ctx, job.ID); j.Health != store.HealthFailing {
		t.Fatalf("health %q", j.Health)
	}

	if err := writeFile(marker); err != nil {
		t.Fatal(err)
	}

	runOnce()
	runOnce()

	if got := strings.Join(events(t, st, job.ID), ","); got != "failure,recovery" {
		t.Fatalf("after recovery: %q", got)
	}

	sender := notify.NewSender(st, func() notify.Options { return notify.Options{Enabled: true, Environment: "test"} })
	if err := sender.Round(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 2 || !strings.Contains(bodies[0], "flaky failed") || !strings.Contains(bodies[1], "flaky works again") {
		t.Fatalf("delivered: %v", bodies)
	}

	ns, _ := st.ListNotifications(ctx, job.ID, 10)
	for _, x := range ns {
		if x.Status != store.NotificationSent {
			t.Fatalf("notification %s is %s: %s", x.ID, x.Status, x.Error)
		}
	}
}

// A job with an alert time and no successful run in it is alerted once.
func TestDidNotRunAlert(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	ch := addChannel(t, st, "http://127.0.0.1:1/never")
	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("silent", "true", func(in *service.JobInput) {
		in.Schedule = "0 0 1 1 *"
		in.AlertAfterSeconds = 60
		in.Notify = &store.Notify{Channels: []string{ch}, On: []string{"no_run"}}
	}))
	if err != nil {
		t.Fatal(err)
	}

	if a, _ := st.AlertSilent(ctx, time.Now().UTC()); len(a) != 0 {
		t.Fatalf("alerted at once: %v", a)
	}

	later := time.Now().UTC().Add(2 * time.Minute)

	if a, err := st.AlertSilent(ctx, later); err != nil || len(a) != 1 {
		t.Fatalf("not alerted: %v %v", a, err)
	}

	if a, _ := st.AlertSilent(ctx, later.Add(time.Hour)); len(a) != 0 {
		t.Fatal("alerted twice")
	}

	if got := strings.Join(events(t, st, job.ID), ","); got != "no_run" {
		t.Fatalf("events %q", got)
	}

	// A success clears it: the job can be alerted again later.
	startNode(t, st, "n1")

	id, _ := svc.Trigger(ctx, job.ID)
	waitFor(t, 10*time.Second, "run", func() bool {
		r, err := st.GetRun(ctx, id)

		return err == nil && r.Finished()
	})

	if j, _ := st.GetJob(ctx, job.ID); j.AlertedAt != nil || j.Health != store.HealthOK {
		t.Fatalf("after success: alerted %v health %q", j.AlertedAt, j.Health)
	}
}

// A job runs after another with the result as params; a report through
// the webhook counts as a run.
func TestChainAndReport(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})
	out := t.TempDir() + "/chain"

	first, err := svc.CreateJob(ctx, cmdJob("first", "true", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *" }))
	if err != nil {
		t.Fatal(err)
	}

	second, err := svc.CreateJob(ctx, cmdJob("second", `echo "$KRONIK_PARAM_AFTER_JOB $KRONIK_PARAM_AFTER_STATUS" > `+out, func(in *service.JobInput) {
		in.Schedule = "0 0 1 1 *"
		in.AfterJobs = []string{first.ID}
	}))
	if err != nil {
		t.Fatal(err)
	}

	// A chain going round is refused.
	in := cmdJob("first", "true", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *"; in.AfterJobs = []string{second.ID} })
	if _, err := svc.UpdateJob(ctx, first.ID, in); err == nil || !strings.Contains(err.Error(), "round") {
		t.Fatalf("cycle accepted: %v", err)
	}

	startNode(t, st, "n1")

	if _, err := svc.Trigger(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "the second job", func() bool {
		rs := runs(t, st, second.ID)

		return len(rs) == 1 && rs[0].Finished()
	})

	if rs := runs(t, st, second.ID); rs[0].Trigger != store.TriggerAfter || rs[0].Status != store.StatusSucceeded || rs[0].TriggeredBy != "first" {
		t.Fatalf("second run %+v", rs[0])
	}

	if b, _ := readFile(out); b != "first succeeded\n" {
		t.Fatalf("params: %q", b)
	}

	// The report webhook: a failed result starts nothing (on succeeded),
	// a succeeded one starts the second job again.
	token, err := svc.NewHook(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}

	code := 3
	if _, _, err := svc.HookReport(ctx, token, service.Report{ExitCode: &code, Output: "boom"}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.HookReport(ctx, token, service.Report{}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "the second run of the second job", func() bool { return len(runs(t, st, second.ID)) == 2 })

	rs := runs(t, st, first.ID)
	if len(rs) != 3 || rs[0].Trigger != store.TriggerPing || rs[1].Status != store.StatusFailed || rs[1].Error != "reported as failed" {
		t.Fatalf("first runs: %+v %+v", rs[0], rs[1])
	}

	if _, _, err := svc.HookTrigger(ctx, "kh_wrong", nil); err == nil {
		t.Fatal("wrong token accepted")
	}

	id := ""
	if _, id, err = svc.HookTrigger(ctx, token, map[string]string{"day": "mon"}); err != nil {
		t.Fatal(err)
	}

	r, _ := st.GetRun(ctx, id)
	if r.Trigger != store.TriggerWebhook || r.Params["day"] != "mon" {
		t.Fatalf("webhook run %+v", r)
	}
}

// Paused schedules and maintenance windows skip scheduled runs; a tag limit
// keeps runs of a tag to n at once across replicas.
func TestPauseWindowsAndTagLimits(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	skipped, err := svc.CreateJob(ctx, cmdJob("in-window", "true", func(in *service.JobInput) { in.Tags = []string{"db"} }))
	if err != nil {
		t.Fatal(err)
	}

	free, err := svc.CreateJob(ctx, cmdJob("other", "true", nil))
	if err != nil {
		t.Fatal(err)
	}

	w, err := settings.ParseWindow("* * * * *|2m|db", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	n := startNode(t, st, "n1")
	n.sch.SetOptions(func() scheduler.Options { return scheduler.Options{Windows: []settings.Window{w}} })

	waitFor(t, 10*time.Second, "runs", func() bool { return len(runs(t, st, free.ID)) >= 2 && len(runs(t, st, skipped.ID)) >= 2 })

	for _, r := range runs(t, st, skipped.ID) {
		if r.Status != store.StatusSkipped || !strings.Contains(r.Error, "maintenance window") {
			t.Fatalf("run in the window: %+v", r)
		}
	}

	for _, r := range runs(t, st, free.ID) {
		if r.Status == store.StatusSkipped {
			t.Fatalf("run outside the window skipped: %+v", r)
		}
	}

	n.stop()

	// Tag limit: five slow jobs tagged "heavy", at most two at once, over
	// two replicas.
	var running, peak atomic.Int64

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := running.Add(1)
		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}

		time.Sleep(700 * time.Millisecond)
		running.Add(-1)
	}))
	defer slow.Close()

	var ids []string

	for i := range 5 {
		j, err := svc.CreateJob(ctx, service.JobInput{
			Name: "heavy-" + string(rune('a'+i)), Schedule: "0 0 1 1 *", Executor: "http", Tags: []string{"heavy"},
			Config: json.RawMessage(`{"url":"` + slow.URL + `"}`),
		})
		if err != nil {
			t.Fatal(err)
		}

		ids = append(ids, j.ID)
	}

	limit := func() scheduler.Options { return scheduler.Options{TagLimits: map[string]int{"heavy": 2}} }

	for _, name := range []string{"a", "b"} {
		nd := startNode(t, st, name)
		nd.sch.SetOptions(limit)
	}

	for _, id := range ids {
		if _, err := svc.Trigger(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, 20*time.Second, "heavy runs", func() bool {
		for _, id := range ids {
			rs := runs(t, st, id)
			if len(rs) == 0 || !rs[0].Finished() {
				return false
			}
		}

		return true
	})

	if p := peak.Load(); p > 2 {
		t.Fatalf("%d heavy runs at once, limit 2", p)
	}

	// Paused: every scheduled run is skipped.
	paused := startNode(t, st, "p")
	paused.sch.SetOptions(func() scheduler.Options { return scheduler.Options{Paused: true} })

	before := len(runs(t, st, free.ID))

	waitFor(t, 10*time.Second, "a paused run", func() bool { return len(runs(t, st, free.ID)) > before })

	if r := runs(t, st, free.ID)[0]; r.Status != store.StatusSkipped || !strings.Contains(r.Error, "paused") {
		t.Fatalf("paused run %+v", r)
	}
}

// A run of a job with jitter waits a random time.
func TestJitter(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	j, err := svc.CreateJob(ctx, cmdJob("jitter", "true", func(in *service.JobInput) { in.JitterSeconds = 30 }))
	if err != nil {
		t.Fatal(err)
	}

	startNode(t, st, "n1")

	waitFor(t, 10*time.Second, "runs", func() bool { return len(runs(t, st, j.ID)) >= 3 })

	var delayed int

	for _, r := range runs(t, st, j.ID) {
		if r.NotBefore.Sub(r.ScheduledAt) > 0 {
			delayed++
		}

		if d := r.NotBefore.Sub(r.ScheduledAt); d < 0 || d > 30*time.Second {
			t.Fatalf("delay %s", d)
		}
	}

	if delayed == 0 {
		t.Fatal("no run delayed")
	}

	if _, err := svc.CreateJob(ctx, cmdJob("too-much", "true", func(in *service.JobInput) { in.JitterSeconds = 99999 })); err == nil {
		t.Fatal("jitter over an hour accepted")
	}
}

// The bus tells another replica at once.
func TestBus(t *testing.T) {
	st, cfg := newStore(t)

	bus := st.NewBus(cfg.DSN, cfg.TablePrefix)

	got := make(chan string, 10)
	bus.On(store.TopicRun, func(p string) { got <- p })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go bus.Run(ctx)

	time.Sleep(300 * time.Millisecond)
	bus.Send(ctx, store.TopicRun, "r-1")

	select {
	case p := <-got:
		if p != "r-1" {
			t.Fatalf("payload %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func writeFile(p string) error { return os.WriteFile(p, []byte("x"), 0o600) }

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)

	return string(b), err
}
