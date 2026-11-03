package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	info := Info{Secrets: map[string]string{"TOKEN": "s3cret"}, Params: map[string]string{"day": "mon"}}

	got, err := expand("Bearer {{secret.TOKEN}} on {{ param.day }}{{param.none}}", info)
	if err != nil || got != "Bearer s3cret on mon" {
		t.Fatalf("%q %v", got, err)
	}

	if _, err := expand("{{secret.NOPE}}", info); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("missing secret: %v", err)
	}

	if got, _ := expand("{{ other }}", info); got != "{{ other }}" {
		t.Fatalf("left alone: %q", got)
	}

	if err := CheckParams(map[string]string{"ok_1": "x"}); err != nil {
		t.Fatal(err)
	}

	if err := CheckParams(map[string]string{"1bad": "x"}); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestHTTPSecretsAndParams(t *testing.T) {
	var auth, path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte("token was " + r.Header.Get("Authorization")))
	}))
	defer srv.Close()

	h := NewHTTP()

	cfg, err := h.Validate(json.RawMessage(`{"url":"` + srv.URL + `/{{param.path}}","headers":{"Authorization":"Bearer {{secret.TOKEN}}"}}`))
	if err != nil {
		t.Fatal(err)
	}

	out := NewOutput(1 << 12)

	info := Info{Secrets: map[string]string{"TOKEN": "s3cret"}, Params: map[string]string{"path": "reports"}}
	if _, err := h.Run(context.Background(), info, cfg, out); err != nil {
		t.Fatal(err)
	}

	text, _, _ := out.Snapshot()
	if auth != "Bearer s3cret" || path != "/reports" || strings.Contains(text, "s3cret") || !strings.Contains(text, "[secret]") {
		t.Fatalf("auth=%q path=%q out=%q", auth, path, text)
	}
}

func TestCommandParamsEnv(t *testing.T) {
	c := NewCommand("/bin/sh")

	cfg, err := c.Validate(json.RawMessage(`{"command":"echo $KRONIK_PARAM_DAY $TOKEN","env":{"TOKEN":"{{secret.TOKEN}}"}}`))
	if err != nil {
		t.Fatal(err)
	}

	out := NewOutput(1 << 12)
	if _, err := c.Run(context.Background(), Info{Secrets: map[string]string{"TOKEN": "s3cret"}, Params: map[string]string{"day": "mon"}}, cfg, out); err != nil {
		t.Fatal(err)
	}

	if text, _, _ := out.Snapshot(); text != "mon [secret]\n" {
		t.Fatalf("%q", text)
	}
}
