package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/service"
)

func newTestServer(t *testing.T, base string) http.Handler {
	t.Helper()

	cfg := &config.Config{NodeName: "n1", Server: config.Server{BasePath: base}}
	svc := service.New(nil, executor.Registry{executor.TypeHTTP: executor.NewHTTP()}, nil)
	ui := fstest.MapFS{"index.html": {Data: []byte("<html>kronik</html>")}}

	srv, err := New(cfg, Deps{Service: svc, UI: ui})
	if err != nil {
		t.Fatal(err)
	}

	return srv.Mux
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestInfoAndSchedulePreview(t *testing.T) {
	h := newTestServer(t, "")

	rec := do(h, http.MethodGet, "/api/v1/info", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("info: %d %s", rec.Code, rec.Body)
	}

	var info struct {
		Node  string           `json:"node"`
		Build config.BuildInfo `json:"build"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info.Node != "n1" || info.Build.Repository == "" {
		t.Fatalf("info body %s, err %v", rec.Body, err)
	}

	rec = do(h, http.MethodGet, "/api/v1/schedule/next?schedule=0+9+*+*+*&timezone=UTC&n=2", "")

	var p service.Preview
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || len(p.Times) != 2 || p.Times[0].Hour() != 9 {
		t.Fatalf("preview %s, err %v", rec.Body, err)
	}

	rec = do(h, http.MethodGet, "/api/v1/schedule/next?schedule=bad", "")
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("bad schedule: %s", rec.Body)
	}
}

func TestRouting(t *testing.T) {
	h := newTestServer(t, "/cron")

	if rec := do(h, http.MethodGet, "/cron/api/v1/nope", ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("unknown api path: %d %s", rec.Code, rec.Body)
	}

	if rec := do(h, http.MethodGet, "/cron/", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kronik") {
		t.Fatalf("ui: %d %s", rec.Code, rec.Body)
	}

	if rec := do(h, http.MethodGet, "/cron", ""); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("base redirect: %d", rec.Code)
	}

	// Bad bodies are refused before reaching the store.
	if rec := do(h, http.MethodPost, "/cron/api/v1/jobs", `{"nme":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", rec.Code, rec.Body)
	}

	if rec := do(h, http.MethodPost, "/cron/api/v1/jobs", `{"name":"x","schedule":"* * *","executor":"http"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad schedule: %d %s", rec.Code, rec.Body)
	}
}

func TestWritesNeedJSONAndSameOrigin(t *testing.T) {
	h := newTestServer(t, "")

	// A no-cors fetch from another site sends text/plain.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/test", strings.NewReader(`{"executor":"http"}`))
	req.Header.Set("Content-Type", "text/plain")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain body: %d", rec.Code)
	}

	// A cross-site request is refused before anything runs.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/jobs/test", strings.NewReader(`{"executor":"http"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", rec.Code)
	}
}

func TestUserHeader(t *testing.T) {
	s := &Server{userHeader: "X-User"}

	for _, tc := range []struct {
		values []string
		want   string
	}{
		{nil, ""},
		{[]string{" alice "}, "alice"},
		{[]string{"mallory", "alice"}, ""},
		{[]string{"a\x00b\nc"}, "abc"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, v := range tc.values {
			r.Header.Add("X-User", v)
		}

		if got := s.userOf(r); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.values, got, tc.want)
		}
	}
}
