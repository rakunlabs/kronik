package executor

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOutputKeepsTail(t *testing.T) {
	o := NewOutput(5)
	_, _ = o.Write([]byte("abc"))
	_, _ = o.Write([]byte("defg"))

	text, truncated, _ := o.Snapshot()
	if text != "cdefg" || !truncated {
		t.Fatalf("got %q truncated=%v", text, truncated)
	}

	_, _ = o.Write([]byte("0123456789"))

	if text, _, _ = o.Snapshot(); text != "56789" {
		t.Fatalf("got %q", text)
	}
}

func TestOutputRingMatchesTail(t *testing.T) {
	// Random writes give the same text as keeping the tail of everything.
	r := rand.New(rand.NewPCG(1, 2))

	for _, size := range []int{1, 7, 64} {
		o := NewOutput(size)

		var all []byte

		for range 500 {
			p := make([]byte, r.IntN(size*2+1))
			for i := range p {
				p[i] = byte('a' + r.IntN(26))
			}

			_, _ = o.Write(p)
			all = append(all, p...)

			want := all
			if len(want) > size {
				want = want[len(want)-size:]
			}

			got, truncated, _ := o.Snapshot()
			if got != string(want) || truncated != (len(all) > size) {
				t.Fatalf("size %d: got %q (%v), want %q", size, got, truncated, want)
			}
		}
	}
}

func TestCommandEnvDropsKronikSettings(t *testing.T) {
	got := commandEnv([]string{"PATH=/bin", "KRONIK_STORE_DSN=postgres://u:p@h/db", "KRONIK_SECRET_X=1", "OTEL_X=1", "HOME=/h"})
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h" {
		t.Fatalf("env %v", got)
	}

	c := NewCommand("/bin/sh")
	cfg, _ := c.Validate(json.RawMessage(`{"command":"echo \"[$KRONIK_STORE_DSN]\" $KRONIK_JOB_NAME"}`))
	t.Setenv("KRONIK_STORE_DSN", "secret")

	out := NewOutput(100)
	if _, err := c.Run(context.Background(), Info{JobName: "j"}, cfg, out); err != nil {
		t.Fatal(err)
	}

	if text, _, _ := out.Snapshot(); text != "[] j\n" {
		t.Fatalf("output %q", text)
	}
}

func TestCommandBackgroundChildDoesNotFail(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the wait delay")
	}

	c := NewCommand("/bin/sh")
	cfg, _ := c.Validate(json.RawMessage(`{"command":"sleep 60 & echo started"}`))

	code, err := c.Run(context.Background(), Info{}, cfg, NewOutput(1000))
	if err != nil || code == nil || *code != 0 {
		t.Fatalf("code=%v err=%v", code, err)
	}
}

func TestOutputCutRune(t *testing.T) {
	o := NewOutput(3)
	_, _ = o.Write([]byte("aç\x00b")) // ç is two bytes

	text, _, _ := o.Snapshot()
	if text != "\uFFFDb" {
		t.Fatalf("got %q", text)
	}
}

func TestHTTP(t *testing.T) {
	var gotHeader, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Token")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)

		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusBadGateway)
		}

		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	h := NewHTTP()

	cfg, err := h.Validate(json.RawMessage(`{"method":"post","url":"` + srv.URL + `","headers":{"X-Token":"t"},"body":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}

	out := NewOutput(1 << 10)

	code, err := h.Run(context.Background(), Info{RunID: "r1", ScheduledAt: time.Now()}, cfg, out)
	if err != nil || code == nil || *code != 200 {
		t.Fatalf("code=%v err=%v", code, err)
	}

	if gotHeader != "t" || gotBody != "ping" {
		t.Fatalf("header=%q body=%q", gotHeader, gotBody)
	}

	if text, _, _ := out.Snapshot(); !strings.Contains(text, "hello") {
		t.Fatalf("output %q", text)
	}

	cfg, _ = h.Validate(json.RawMessage(`{"url":"` + srv.URL + `/fail"}`))
	if code, err := h.Run(context.Background(), Info{}, cfg, NewOutput(100)); err == nil || *code != 502 {
		t.Fatalf("want failure, code=%v err=%v", code, err)
	}

	cfg, _ = h.Validate(json.RawMessage(`{"url":"` + srv.URL + `/fail","expected_status":[502]}`))
	if _, err := h.Run(context.Background(), Info{}, cfg, NewOutput(100)); err != nil {
		t.Fatalf("expected status: %v", err)
	}
}

func TestHTTPValidate(t *testing.T) {
	for _, raw := range []string{`{}`, `{"url":"ftp://x"}`, `{"url":"/relative"}`, `{"url":"http://x","expected_status":[42]}`, `[`} {
		if _, err := NewHTTP().Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("Validate(%s) succeeded", raw)
		}
	}
}

func TestCommand(t *testing.T) {
	c := NewCommand("/bin/sh")

	cfg, err := c.Validate(json.RawMessage(`{"command":"echo $KRONIK_JOB_NAME $FOO; exit 3","env":{"FOO":"bar"}}`))
	if err != nil {
		t.Fatal(err)
	}

	out := NewOutput(1 << 10)

	code, err := c.Run(context.Background(), Info{JobName: "job1"}, cfg, out)
	if err == nil || code == nil || *code != 3 {
		t.Fatalf("code=%v err=%v", code, err)
	}

	if text, _, _ := out.Snapshot(); text != "job1 bar\n" {
		t.Fatalf("output %q", text)
	}
}

func TestCommandCancelStopsChildren(t *testing.T) {
	c := NewCommand("/bin/sh")
	cfg, _ := c.Validate(json.RawMessage(`{"command":"sleep 30 & sleep 30; wait"}`))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()

	if _, err := c.Run(ctx, Info{}, cfg, NewOutput(100)); err == nil {
		t.Fatal("want error")
	}

	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s to stop", d)
	}
}
