package scheduler_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/scheduler"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

func baseConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Executor.HTTP.Enabled = true
	cfg.Executor.Command.Enabled = true
	cfg.Executor.JS.Enabled = true
	cfg.Executor.JS.MaxDuration = time.Minute
	cfg.Scheduler.RunRetention = 30 * 24 * time.Hour
	cfg.Scheduler.CleanupAt = "01:00"
	cfg.Scheduler.CleanupTimezone = "UTC"

	return cfg
}

// A setting changed on one replica reaches another one.
func TestSettingsSharedAndChecked(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	cfg := baseConfig()

	a := settings.NewManager(settings.StoreSource{Store: st}, cfg.Values())
	b := settings.NewManager(settings.StoreSource{Store: st}, cfg.Values())

	for _, m := range []*settings.Manager{a, b} {
		if err := m.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var seen settings.Values

	b.OnChange(func(v settings.Values) { seen = v })

	keys, err := a.Apply(ctx, settings.Change{Values: map[string]json.RawMessage{
		"environment":           json.RawMessage(`"prod"`),
		"retention.runs":        json.RawMessage(`"7d"`),
		"executor.command":      json.RawMessage(`false`),
		"docker.pull":           json.RawMessage(`"always"`),
		"docker.allowed_images": json.RawMessage(`["alpine:*"]`),
	}}, "alice")
	if err != nil || len(keys) != 5 {
		t.Fatalf("apply: %v %v", keys, err)
	}

	if err := b.Load(ctx); err != nil {
		t.Fatal(err)
	}

	if seen.Environment != "prod" || seen.RunRetention != 7*24*time.Hour || seen.Command || seen.DockerPull != "always" {
		t.Fatalf("other replica sees %+v", seen)
	}

	for _, e := range b.Entries() {
		if e.Key == "environment" && (!e.Overridden || e.UpdatedBy != "alice" || string(e.Base) != `""`) {
			t.Fatalf("entry %+v", e)
		}
	}

	// Bad values and impossible combinations are refused, nothing stored.
	for _, bad := range []map[string]json.RawMessage{
		{"retention.runs": json.RawMessage(`"forever"`)},
		{"retention.timezone": json.RawMessage(`"Mars/Base"`)},
		{"nope": json.RawMessage(`1`)},
		{"executor.http": json.RawMessage(`false`), "executor.js": json.RawMessage(`false`)},
	} {
		if _, err := a.Apply(ctx, settings.Change{Values: bad}, "x"); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}

	// Reset goes back to the configuration.
	if _, err := a.Apply(ctx, settings.Change{Reset: []string{"environment", "executor.command"}}, "alice"); err != nil {
		t.Fatal(err)
	}

	if v := a.Values(); v.Environment != "" || !v.Command {
		t.Fatalf("after reset: %+v", v)
	}
}

// Only one replica runs the cleanup of a day; it deletes what retention says.
func TestDailyCleanup(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()

	if got, err := st.ClaimDaily(ctx, "cleanup", "2026-11-03"); err != nil || !got {
		t.Fatalf("first claim: %v %v", got, err)
	}

	if got, _ := st.ClaimDaily(ctx, "cleanup", "2026-11-03"); got {
		t.Fatal("the same day claimed twice")
	}

	if got, _ := st.ClaimDaily(ctx, "cleanup", "2026-11-04"); !got {
		t.Fatal("next day not claimed")
	}

	svc := service.New(st, registry(), noWake{})

	job, err := svc.CreateJob(ctx, cmdJob("old", "true", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *"; in.Concurrency = "allow" }))
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		id, err := svc.Trigger(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := st.ClaimRuns(ctx, "n", time.Now().UTC(), 10, []string{executor.TypeCommand}); err != nil {
			t.Fatal(err)
		}

		old := time.Now().UTC().Add(-40 * 24 * time.Hour)
		if _, err := st.FinishRun(ctx, id, "n", store.Result{Status: store.StatusSucceeded}, old); err != nil {
			t.Fatal(err)
		}
	}

	sch := scheduler.New(st, registry(), schedCfg(), "n")

	stats, err := sch.Cleanup(ctx, scheduler.Retention{Runs: 30 * 24 * time.Hour})
	if err != nil || stats.Runs != 3 {
		t.Fatalf("cleanup deleted %+v, %v", stats, err)
	}

	// Settings keys of the daily claim are not shown as settings.
	all, _ := st.Settings(ctx)
	for k := range all {
		if strings.HasPrefix(k, "_") {
			t.Fatalf("bookkeeping key %s listed", k)
		}
	}
}

func TestRunSearch(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	svc := service.New(st, registry(), noWake{})

	mk := func(name, by string) string {
		in := cmdJob(name, "true", func(in *service.JobInput) { in.Schedule = "0 0 1 1 *"; in.Concurrency = "allow" })
		j, err := svc.CreateJob(service.WithActor(ctx, service.Actor{User: by}), in)
		if err != nil {
			t.Fatal(err)
		}

		return j.ID
	}

	backup := mk("nightly-backup", "alice")
	report := mk("weekly-report", "bob")

	if _, err := svc.Trigger(service.WithActor(ctx, service.Actor{User: "carol"}), backup); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Trigger(service.WithActor(ctx, service.Actor{User: "bob"}), report); err != nil {
		t.Fatal(err)
	}

	count := func(f store.RunFilter) int {
		f.Limit = 100

		rs, err := st.ListRuns(ctx, f)
		if err != nil {
			t.Fatal(err)
		}

		return len(rs)
	}

	now := time.Now().UTC()

	for name, tc := range map[string]struct {
		f    store.RunFilter
		want int
	}{
		"name":           {store.RunFilter{Search: "BACKUP"}, 1},
		"by whom":        {store.RunFilter{Search: "carol"}, 1},
		"started by":     {store.RunFilter{TriggeredBy: "bob"}, 1},
		"mine (jobs)":    {store.RunFilter{JobsBy: "alice"}, 1},
		"mine (started)": {store.RunFilter{JobsBy: "carol"}, 1},
		"in range":       {store.RunFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, 2},
		"before range":   {store.RunFilter{To: now.Add(-time.Hour)}, 0},
		"like escaped":   {store.RunFilter{Search: "%"}, 0},
	} {
		if got := count(tc.f); got != tc.want {
			t.Errorf("%s: %d runs, want %d", name, got, tc.want)
		}
	}
}
