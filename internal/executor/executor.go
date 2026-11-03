// Package executor runs one attempt of a job: an HTTP request, a script, a
// shell command, or a container on Docker, Swarm or Kubernetes.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	TypeHTTP    = "http"
	TypeCommand = "command"
	TypeJS      = "js"
)

// Info describes the run to the executor, passed on as headers or environment.
type Info struct {
	JobID       string
	JobName     string
	RunID       string
	Attempt     int
	ScheduledAt time.Time
}

type Executor interface {
	// Validate checks a job's executor config and returns it normalized.
	Validate(cfg json.RawMessage) (json.RawMessage, error)
	// Run runs once and writes what it produces to out. code is the HTTP
	// status or exit code, nil when there is none. A nil err means success.
	Run(ctx context.Context, info Info, cfg json.RawMessage, out *Output) (code *int, err error)
}

// Executors finds an executor by type: a Registry, or the Live one that
// follows the settings.
type Executors interface {
	Get(name string) (Executor, error)
	Names() []string
}

type Registry map[string]Executor

func (r Registry) Get(name string) (Executor, error) {
	e, ok := r[name]
	if !ok {
		if slices.Contains(AllTypes, name) {
			return nil, fmt.Errorf("%s jobs are turned off (Settings → Configuration → Executors)", name)
		}

		return nil, fmt.Errorf("unknown executor %q", name)
	}

	return e, nil
}

// AllTypes are every executor kronik has, in the order shown.
var AllTypes = []string{TypeHTTP, TypeJS, TypeCommand, TypeDocker, TypeSwarm, TypeKubernetes}

// Names lists the available executors.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for _, n := range AllTypes {
		if _, ok := r[n]; ok {
			names = append(names, n)
		}
	}

	return names
}

// Closer is an executor holding a connection (to a Docker Engine).
type Closer interface {
	Close() error
}

// Close closes the executors that hold a connection.
func (r Registry) Close() {
	for _, e := range r {
		if c, ok := e.(Closer); ok {
			_ = c.Close()
		}
	}
}

// Live is the registry in effect; it is replaced when settings change, and
// runs going on keep the executor they started with.
type Live struct {
	mu sync.RWMutex
	r  Registry
}

func NewLive(r Registry) *Live {
	return &Live{r: r}
}

func (l *Live) Registry() Registry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.r
}

// Swap puts a new registry in place and returns the old one.
func (l *Live) Swap(r Registry) Registry {
	l.mu.Lock()
	defer l.mu.Unlock()

	old := l.r
	l.r = r

	return old
}

func (l *Live) Get(name string) (Executor, error) { return l.Registry().Get(name) }

func (l *Live) Names() []string { return l.Registry().Names() }

// invalid is a config error; every error Validate returns is one.
func invalid(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}

func decode[T any](cfg json.RawMessage) (*T, error) {
	var v T
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}

	if err := json.Unmarshal(cfg, &v); err != nil {
		return nil, invalid("%v", err)
	}

	return &v, nil
}

// Output keeps the last max bytes written to it and is safe for concurrent
// use. It is a ring: a full buffer costs a write no more than its size.
type Output struct {
	mu        sync.Mutex
	buf       []byte // len(buf) <= max; a ring once full
	head      int    // where the oldest byte is once full
	max       int
	truncated bool
	version   int
	result    string
	redact    *strings.Replacer
}

// Redact hides the given values (script secrets) in the output, the result
// and errors. A value written in another form (base64, split up) is not
// recognised: this keeps an honest mistake out of sight, it does not stop a
// script that means to show a secret.
func (o *Output) Redact(values []string) {
	var pairs []string

	for _, v := range values {
		// Short values would hide ordinary text.
		if len(v) >= minRedact {
			pairs = append(pairs, v, "[secret]")
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if len(pairs) > 0 {
		o.redact = strings.NewReplacer(pairs...)
	}
}

// minRedact is the shortest secret value hidden.
const minRedact = 4

// Clean hides the redacted values in s.
func (o *Output) Clean(s string) string {
	o.mu.Lock()
	r := o.redact
	o.mu.Unlock()

	if r == nil {
		return s
	}

	return r.Replace(s)
}

func NewOutput(maxBytes int) *Output {
	return &Output{max: max(maxBytes, 1)}
}

// Max is the number of bytes kept.
func (o *Output) Max() int { return o.max }

// maxResult caps the return value kept for a run.
const maxResult = 64 << 10

// SetResult keeps a run's return value (JSON). One too large to keep is
// replaced by a note, not cut into invalid JSON.
func (o *Output) SetResult(s string) {
	s = o.Clean(s)
	if len(s) > maxResult {
		s = fmt.Sprintf(`{"kronik":"the result was not kept, it is larger than %d KiB","size":%d}`, maxResult>>10, len(s))
	}

	o.mu.Lock()
	o.result = toValidUTF8([]byte(s))
	o.mu.Unlock()
}

func (o *Output) Result() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.result
}

func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	n := len(p)
	o.version++

	if o.redact != nil {
		p = []byte(o.redact.Replace(string(p)))
	}

	if len(p) >= o.max {
		o.buf = append(o.buf[:0], p[len(p)-o.max:]...)
		o.head = 0
		o.truncated = true

		return n, nil
	}

	// Fill up first.
	if room := o.max - len(o.buf); room > 0 {
		k := min(room, len(p))
		o.buf = append(o.buf, p[:k]...)
		p = p[k:]
	}

	// Then overwrite the oldest bytes.
	for len(p) > 0 {
		o.truncated = true
		k := copy(o.buf[o.head:], p)
		p = p[k:]
		o.head = (o.head + k) % o.max
	}

	return n, nil
}

func (o *Output) Printf(format string, a ...any) {
	_, _ = fmt.Fprintf(o, format, a...)
}

// bytes is the kept output, oldest first.
func (o *Output) bytes() []byte {
	if o.head == 0 {
		return o.buf
	}

	out := make([]byte, 0, len(o.buf))

	return append(append(out, o.buf[o.head:]...), o.buf[:o.head]...)
}

// Snapshot returns the text, whether the start was cut and a counter that
// changes with every write.
func (o *Output) Snapshot() (string, bool, int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return toValidUTF8(o.bytes()), o.truncated, o.version
}
