package scheduler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/secrets"
	"github.com/rakunlabs/kronik/internal/server"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

type api struct {
	t   *testing.T
	url string
}

func (a api) do(method, path, body string) (int, map[string]any) {
	a.t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}

	req, _ := http.NewRequest(method, a.url+path, r)
	req.Header.Set("X-User", "alice")

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	b, _ := io.ReadAll(resp.Body)

	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		var list []any
		if json.Unmarshal(b, &list) == nil {
			out = map[string]any{"list": list}
		}
	}

	if out == nil {
		out = map[string]any{"raw": string(b)}
	}

	return resp.StatusCode, out
}

// The API of secrets, webhooks, bulk actions, the calendar and the live
// stream of a run.
func TestAPIFeatures(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()

	cfg := &config.Config{NodeName: "n1", Server: config.Server{UserHeader: "X-User"}}
	cfg.Executor.HTTP.Enabled, cfg.Executor.Command.Enabled = true, true
	cfg.Executor.JS.MaxDuration = time.Minute
	cfg.Executor.JS.Secrets = map[string]string{"FROM_ENV": "env-value"}

	set := settings.NewManager(settings.StoreSource{Store: st}, cfg.Values())
	if err := set.Load(ctx); err != nil {
		t.Fatal(err)
	}

	key, _ := secrets.ParseMasterKey("a test master key here")
	vault := secrets.NewVault(st, []secrets.MasterKey{key}, cfg.Executor.JS.Secrets)

	n := startNode(t, st, "n1")
	n.sch.SetSecrets(vault.Values)

	svc := service.New(st, registry(), n.sch)
	svc.SetSecrets(vault.Values)

	live := server.NewLive()

	srv, err := server.New(cfg, server.Deps{Service: svc, Settings: set, Vault: vault, Live: live})
	if err != nil {
		t.Fatal(err)
	}

	hs := httptest.NewServer(srv.Mux)
	defer hs.Close()

	a := api{t: t, url: hs.URL}

	// Secrets: stored, listed without values, used by a run, hidden in it.
	if code, out := a.do(http.MethodPut, "/api/v1/secrets/DB_PASS", `{"value":"hunter22","description":"db"}`); code != 200 {
		t.Fatalf("put secret: %d %v", code, out)
	}

	_, page := a.do(http.MethodGet, "/api/v1/secrets", "")
	if b, _ := json.Marshal(page); strings.Contains(string(b), "hunter22") || !strings.Contains(string(b), "FROM_ENV") || !strings.Contains(string(b), "DB_PASS") {
		t.Fatalf("secrets page: %s", b)
	}

	if code, _ := a.do(http.MethodPut, "/api/v1/secrets/NEW", `{"description":"no value"}`); code != 400 {
		t.Fatalf("secret without value: %d", code)
	}

	code, job := a.do(http.MethodPost, "/api/v1/jobs",
		`{"name":"uses-secret","schedule":"0 0 1 1 *","executor":"command","config":{"command":"echo pass=$P day=$KRONIK_PARAM_DAY","env":{"P":"{{secret.DB_PASS}}"}}}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, job)
	}

	jobID := job["id"].(string)

	code, trig := a.do(http.MethodPost, "/api/v1/jobs/"+jobID+"/trigger", `{"params":{"day":"mon"}}`)
	if code != 202 {
		t.Fatalf("trigger: %d %v", code, trig)
	}

	runID := trig["run_id"].(string)

	// The live stream sends the run until it has finished.
	resp, err := http.Get(hs.URL + "/api/v1/runs/" + runID + "/events")
	if err != nil {
		t.Fatal(err)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	var last store.Run

	deadline := time.Now().Add(15 * time.Second)

	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if err := json.Unmarshal([]byte(data), &last); err != nil {
				t.Fatal(err)
			}

			// Without the bus the stream reads the run every few seconds.
			if last.Finished() {
				break
			}
		}
	}

	_ = resp.Body.Close()

	if last.Status != store.StatusSucceeded || last.Output != "pass=[secret] day=mon\n" {
		t.Fatalf("run %+v", last)
	}

	// Webhook: token shown once; trigger with query and body params.
	code, hook := a.do(http.MethodPost, "/api/v1/jobs/"+jobID+"/hook", "")
	if code != 200 {
		t.Fatalf("hook: %d %v", code, hook)
	}

	token := hook["token"].(string)

	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/hook/"+token+"?day=tue", strings.NewReader(`{"extra":"1"}`))
	req.Header.Set("Content-Type", "text/plain") // other systems send what they like

	hr, err := http.DefaultClient.Do(req)
	if err != nil || hr.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook: %v %v", hr.StatusCode, err)
	}

	var hookRun map[string]string
	_ = json.NewDecoder(hr.Body).Decode(&hookRun)
	_ = hr.Body.Close()

	r, _ := st.GetRun(ctx, hookRun["run_id"])
	if r.Params["day"] != "tue" || r.Params["extra"] != "1" || r.Trigger != store.TriggerWebhook {
		t.Fatalf("webhook run %+v", r)
	}

	if bad, _ := http.Post(hs.URL+"/hook/kh_nope", "application/json", nil); bad.StatusCode != 404 {
		t.Fatalf("bad token: %d", bad.StatusCode)
	}

	rep, err := http.Get(hs.URL + "/hook/" + token + "/report?exit_code=1")
	if err != nil || rep.StatusCode != 200 {
		t.Fatalf("report: %v %v", rep.StatusCode, err)
	}

	// Removed: the token stops working.
	if code, _ := a.do(http.MethodDelete, "/api/v1/jobs/"+jobID+"/hook", ""); code != 204 {
		t.Fatalf("remove hook: %d", code)
	}

	if gone, _ := http.Post(hs.URL+"/hook/"+token, "application/json", nil); gone.StatusCode != 404 {
		t.Fatalf("removed token: %d", gone.StatusCode)
	}

	// Calendar: the next day of an hourly job.
	_, other := a.do(http.MethodPost, "/api/v1/jobs", `{"name":"hourly","schedule":"0 * * * *","executor":"command","config":{"command":"true"}}`)

	_, cal := a.do(http.MethodGet, "/api/v1/calendar?from=2026-11-03T00:00:00Z&to=2026-11-04T00:00:00Z", "")
	if items := cal["items"].([]any); len(items) != 24 {
		t.Fatalf("calendar items: %d", len(items))
	}

	// Bulk pause.
	code, bulk := a.do(http.MethodPost, "/api/v1/jobs/bulk", `{"action":"pause","ids":["`+jobID+`","`+other["id"].(string)+`","nope"]}`)
	if code != 200 || len(bulk["done"].([]any)) != 2 || bulk["failed"].(map[string]any)["nope"] == nil {
		t.Fatalf("bulk: %d %v", code, bulk)
	}

	if j, _ := st.GetJob(ctx, jobID); j.Enabled {
		t.Fatal("not paused")
	}

	// Stats and history.
	_, pts := a.do(http.MethodGet, "/api/v1/jobs/"+jobID+"/stats", "")
	if len(pts["list"].([]any)) < 2 {
		t.Fatalf("stats: %v", pts)
	}

	_, hist := a.do(http.MethodGet, "/api/v1/jobs/"+jobID+"/history", "")
	if len(hist["list"].([]any)) != 1 {
		t.Fatalf("history: %v", hist)
	}

	// Rotation keeps the secret readable.
	if code, rot := a.do(http.MethodPost, "/api/v1/secrets/rotate", ""); code != 200 || rot["reencrypted"].(float64) != 1 {
		t.Fatalf("rotate: %d %v", code, rot)
	}

	if vault.Values()["DB_PASS"] != "hunter22" {
		t.Fatal("secret lost in rotation")
	}

	// Channels.
	code, ch := a.do(http.MethodPost, "/api/v1/channels", `{"name":"ops","type":"slack","config":{"url":"https://hooks.slack.com/x"}}`)
	if code != 201 {
		t.Fatalf("channel: %d %v", code, ch)
	}

	if code, _ := a.do(http.MethodPost, "/api/v1/channels", `{"name":"bad","type":"slack","config":{"url":"nope"}}`); code != 400 {
		t.Fatalf("bad channel: %d", code)
	}

	code, upd := a.do(http.MethodPut, "/api/v1/jobs/"+other["id"].(string),
		`{"name":"hourly","schedule":"0 * * * *","executor":"command","config":{"command":"true"},"notify":{"channels":["`+ch["id"].(string)+`"],"on":["failure"]}}`)
	if code != 200 {
		t.Fatalf("notify on job: %d %v", code, upd)
	}

	if code, out := a.do(http.MethodDelete, "/api/v1/channels/"+ch["id"].(string), ""); code != 204 {
		t.Fatalf("delete channel: %d %v", code, out)
	}

	if j, _ := st.GetJob(ctx, other["id"].(string)); len(j.Notify.Channels) != 0 {
		t.Fatalf("deleted channel left on the job: %v", j.Notify)
	}
}
