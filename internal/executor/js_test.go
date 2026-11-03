package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestJS() *JS {
	return NewJS(JSLimits{
		MaxDuration: 5 * time.Second,
		MaxRequests: 10,
		MaxResponse: 1 << 20,
	})
}

func runJS(t *testing.T, j *JS, ctx context.Context, script string) (string, string, error) {
	t.Helper()

	raw, err := j.Validate(json.RawMessage(`{"script":` + quote(script) + `,"env":{"NAME":"kronik"}}`))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	out := NewOutput(1 << 16)
	_, err = j.Run(ctx, Info{JobName: "j1", RunID: "r1", Attempt: 2, ScheduledAt: time.Unix(0, 0),
		Secrets: map[string]string{"TOKEN": "s3cret"}, Params: map[string]string{"region": "eu"}}, raw, out)
	text, _, _ := out.Snapshot()

	return text, out.Result(), err
}

func quote(s string) string {
	b, _ := json.Marshal(s)

	return string(b)
}

func TestJSFetchParallelAndResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users":
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`[{"id":1},{"id":2}]`))
		case "/orders":
			time.Sleep(200 * time.Millisecond)

			if r.Header.Get("Authorization") != "Bearer s3cret" {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			_, _ = w.Write([]byte(`[{"s":"late"},{"s":"ok"},{"s":"late"}]`))
		case "/hook":
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Got", r.Header.Get("Content-Type")+"|"+string(b))
		}
	}))
	defer srv.Close()

	start := time.Now()

	out, res, err := runJS(t, newTestJS(), context.Background(), `
const base = '`+srv.URL+`'
const [users, orders] = await Promise.all([
  fetch(base + '/users').then(r => r.json()),
  fetch(base + '/orders', { headers: { Authorization: 'Bearer ' + secrets.TOKEN } }).then(r => r.json()),
])
const late = orders.filter(o => o.s === 'late')
console.log(users.length, 'users,', late.length, 'late', { by: env.NAME, attempt: job.attempt })
const hook = await fetch(base + '/hook', { method: 'POST', body: { n: late.length } })
console.log(hook.ok, hook.headers.get('x-got'))
return { users: users.length, late: late.length }
`)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if d := time.Since(start); d > 380*time.Millisecond {
		t.Errorf("requests were not parallel: %s", d)
	}

	want := "2 users, 2 late {\"by\":\"kronik\",\"attempt\":2}\ntrue application/json|{\"n\":2}\n"
	if out != want {
		t.Errorf("output:\n%q\nwant\n%q", out, want)
	}

	if res != `{"users":2,"late":2}` {
		t.Errorf("result %s", res)
	}
}

func TestJSInsecureAndErrors(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
	}))
	defer tlsSrv.Close()

	j := newTestJS()

	if _, _, err := runJS(t, j, context.Background(), `await fetch('`+tlsSrv.URL+`')`); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("self-signed without insecure: %v", err)
	}

	out, _, err := runJS(t, j, context.Background(), `console.log(await (await fetch('`+tlsSrv.URL+`', { insecure: true })).text())`)
	if err != nil || out != "hi\n" {
		t.Errorf("insecure: %q %v", out, err)
	}

	// A caught error does not fail the run.
	if _, _, err := runJS(t, j, context.Background(), `try { await fetch('http://127.0.0.1:1') } catch (e) { console.log('caught') }`); err != nil {
		t.Errorf("caught fetch error failed the run: %v", err)
	}

	_, _, err = runJS(t, j, context.Background(), `throw new Error('boom')`)
	if err == nil || err.Error() != "Error: boom" {
		t.Errorf("throw: %v", err)
	}

	_, _, err = runJS(t, j, context.Background(), `await new Promise(() => {})`)
	if err == nil || !strings.Contains(err.Error(), "nothing will settle") {
		t.Errorf("never settled: %v", err)
	}

	_, _, err = runJS(t, j, context.Background(), `secrets.TOKEN = 'x'; return secrets.TOKEN`)
	if err == nil {
		t.Errorf("secrets must be read-only in strict mode")
	}
}

func TestJSStops(t *testing.T) {
	j := newTestJS()

	ctx, cancel := context.WithTimeoutCause(context.Background(), 200*time.Millisecond, errors.New("job timed out"))
	defer cancel()

	start := time.Now()

	if _, _, err := runJS(t, j, ctx, `while (true) {}`); err == nil || err.Error() != "job timed out" {
		t.Errorf("busy loop: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()

	if _, _, err := runJS(t, j, ctx2, `await sleep(10000)`); err == nil {
		t.Errorf("sleep not stopped")
	}

	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s to stop", d)
	}

	limited := NewJS(JSLimits{MaxRequests: 1, MaxResponse: 10})
	if _, _, err := runJS(t, limited, context.Background(), `await fetch('http://127.0.0.1:1').catch(() => {}); await fetch('http://127.0.0.1:1')`); err == nil ||
		!strings.Contains(err.Error(), "at most 1") {
		t.Errorf("request limit: %v", err)
	}
}

func TestJSHostileValuesDoNotPanic(t *testing.T) {
	j := newTestJS()

	for _, src := range []string{
		`JSON.stringify = 1; console.log({a: 1}); return {b: 2}`,
		`JSON = null; throw {a: 1}`,
		`throw { get message() { throw new Error('x') } }`,
		`return { get a() { throw new Error('getter') } }`,
		`const e = new Error('e'); Object.defineProperty(e, 'stack', { get() { while (true) {} } }); throw e`,
		`console.log({ toJSON() { throw new Error('t') } })`,
		`const a = {}; a.self = a; console.log(a, new Map([[1, 2]]), new Set([3]), 10n, Symbol('s')); return {avg: NaN, f() {}}`,
		`return 'x'.repeat(70000)`,
		`await sleep(Infinity)`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

		out, res, err := runJS(t, j, ctx, src)
		cancel()

		t.Logf("%s\n  -> out=%q res=%q err=%v", src, out, res, err)
	}

	out, res, err := runJS(t, j, context.Background(), `const a = {}; a.self = a; console.log(a, new Map([[1, 2]]), 10n); return {avg: NaN}`)
	if err != nil || out != "{\"self\":[Circular]} Map(1) {1 => 2} 10n\n" || res != `{"avg":null}` {
		t.Fatalf("formatting: out=%q res=%q err=%v", out, res, err)
	}

	_, res, err = runJS(t, j, context.Background(), `return 'x'.repeat(70000)`)
	if err != nil || !strings.Contains(res, `"kronik"`) || !json.Valid([]byte(res)) {
		t.Fatalf("large result: %q %v", res, err)
	}
}

func TestJSSecretsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	out, res, err := runJS(t, newTestJS(), context.Background(),
		`console.log('token', secrets.TOKEN); await fetch('http://127.0.0.1:1/x?token=' + secrets.TOKEN).catch(e => console.log(e.message)); return secrets`)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out+res, "s3cret") || !strings.Contains(out, "token [secret]") || !strings.Contains(res, "[secret]") {
		t.Fatalf("secret shown: out=%q res=%q", out, res)
	}

	_, _, err = runJS(t, newTestJS(), context.Background(), `throw new Error('bad ' + secrets.TOKEN)`)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("secret in the error: %v", err)
	}
}

func TestJSUnhandledRejectionWarns(t *testing.T) {
	out, _, err := runJS(t, newTestJS(), context.Background(), `Promise.reject(new Error('x')); return 1`)
	if err != nil || !strings.Contains(out, "never handled") {
		t.Fatalf("out=%q err=%v", out, err)
	}

	out, _, _ = runJS(t, newTestJS(), context.Background(), `const h = new Headers({A: '1'}); h.append('a', '2'); console.log(h.get('a'), h.has('constructor'))`)
	if out != "1, 2 false\n" {
		t.Fatalf("headers: %q", out)
	}
}

func TestJSValidate(t *testing.T) {
	j := newTestJS()

	for _, src := range []string{``, `return (`, `}); while(true){}; (function(){`, `await fetch(`} {
		if _, err := j.Validate(json.RawMessage(`{"script":` + quote(src) + `}`)); err == nil {
			t.Errorf("Validate(%q) succeeded", src)
		}
	}

	p := CheckJS("const a = 1\nconst a = 2")
	if len(p) != 1 || p[0].Line != 2 {
		t.Errorf("CheckJS: %+v", p)
	}
}

func TestJSParams(t *testing.T) {
	out, res, err := runJS(t, newTestJS(), context.Background(), `console.log(params.region); return params`)
	if err != nil || out != "eu\n" || res != `{"region":"eu"}` {
		t.Fatalf("out=%q res=%q err=%v", out, res, err)
	}
}
