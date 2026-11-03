package scheduler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/server"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/settings"
	"github.com/rakunlabs/kronik/internal/store"
)

// userTransport sends the proxy's user header on every request.
type userTransport struct{ user string }

func (t userTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-User", t.user)

	return http.DefaultTransport.RoundTrip(r)
}

// Jobs changed through the API and MCP are recorded with the proxy's user.
func TestAuditAndMCP(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()

	reg := registry()
	reg[executor.TypeJS] = executor.NewJS(executor.JSLimits{MaxResponse: 1 << 20, MaxDuration: 10 * 1e9, Secrets: map[string]string{"K": "v"}})

	cfg := &config.Config{
		NodeName: "n1",
		Server:   config.Server{UserHeader: "X-User"},
		MCP:      config.MCP{Enabled: true},
	}
	cfg.Executor.JS.Secrets = map[string]string{"K": "v"}
	cfg.Executor.HTTP.Enabled, cfg.Executor.JS.Enabled, cfg.Executor.JS.MaxDuration = true, true, time.Minute

	set := settings.NewManager(settings.StoreSource{Store: st}, cfg.Values())
	if err := set.Load(ctx); err != nil {
		t.Fatal(err)
	}

	srv, err := server.New(cfg, service.New(st, reg, noWake{}), nil, set, nil)
	if err != nil {
		t.Fatal(err)
	}

	hs := httptest.NewServer(srv.Mux)
	defer hs.Close()

	// Through the API, as alice.
	body := `{"name":"api-job","schedule":"@hourly","executor":"js","config":{"script":"return secrets.K"}}`
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/jobs", strings.NewReader(body))
	req.Header.Set("X-User", "alice")
	req.Header.Set("X-Kronik-Client", "ui")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %v %v", resp.StatusCode, err)
	}

	var job store.Job
	_ = json.NewDecoder(resp.Body).Decode(&job)
	_ = resp.Body.Close()

	if job.CreatedBy != "alice" {
		t.Fatalf("created_by %q", job.CreatedBy)
	}

	// Test run of a script through the API.
	req, _ = http.NewRequest(http.MethodPost, hs.URL+"/api/v1/jobs/test", strings.NewReader(
		`{"executor":"js","config":{"script":"console.log('hi', job.name); return {k: secrets.K}"}}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	var tr service.TestRun
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	_ = resp.Body.Close()

	// The secret's value is too short to be hidden (under 4 bytes).
	if tr.Status != store.StatusSucceeded || tr.Result != `{"k":"v"}` || tr.Output != "hi test\n" {
		t.Fatalf("test run: %+v", tr)
	}

	// Through MCP, as bob.
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   hs.URL + "/mcp",
		HTTPClient: &http.Client{Transport: userTransport{user: "bob"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) < 10 {
		t.Fatalf("tools: %v %v", tools, err)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "kronik_pause_job", Arguments: map[string]any{"id": "api-job"}})
	if err != nil || res.IsError {
		t.Fatalf("pause: %v %+v", err, res)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "kronik_get_job", Arguments: map[string]any{"id": job.ID}})
	if err != nil || res.IsError {
		t.Fatalf("get: %v %+v", err, res)
	}

	got, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(got), `"enabled":false`) {
		t.Fatalf("job after pause: %s", got)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "kronik_get_job", Arguments: map[string]any{"id": "nope"}})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("missing job: %+v %v", res, err)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "kronik_audit_log", Arguments: map[string]any{"job": "api-job"}})
	if err != nil || res.IsError {
		t.Fatalf("audit_log: %v %+v", err, res)
	}

	entries, err := st.ListAudit(ctx, store.AuditFilter{JobID: job.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	for _, e := range entries {
		seen = append(seen, e.Action+":"+e.Actor+":"+e.Via)
	}

	want := []string{"job.pause:bob:mcp", "job.create:alice:ui"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("audit %v, want %v", seen, want)
	}
}
