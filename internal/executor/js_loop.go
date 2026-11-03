package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// loop runs one script. JavaScript runs only on the goroutine that called
// run; fetch and sleep wait on goroutines of their own and hand their result
// back through done, to be settled on the script's goroutine.
type loop struct {
	ctx  context.Context
	js   *JS
	out  *Output
	vm   *goja.Runtime
	done chan func() error
	// pending counts the waits not settled yet; the script has finished when
	// its promise is settled and none are left that could change that.
	pending  int
	requests int
	// inflight bounds the fetches waiting at once, so Promise.all over many
	// requests does not hold all their bodies in memory together.
	inflight chan struct{}
	wg       sync.WaitGroup
	stop     context.CancelFunc
	// helpers are captured before the script runs, so it cannot replace
	// them; calling them turns a throw inside them into an error, never a
	// panic.
	helpers struct {
		inspect, describe, toJSON goja.Callable
	}
	unhandled int
}

// maxInflight is the number of fetches one run may wait on at once.
const maxInflight = 16

func newLoop(ctx context.Context, js *JS, out *Output) *loop {
	ctx, stop := context.WithCancel(ctx)

	vm := goja.New()
	vm.SetMaxCallStackSize(maxCallStack)
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	return &loop{
		ctx: ctx, js: js, out: out, vm: vm, stop: stop,
		done:     make(chan func() error, 16),
		inflight: make(chan struct{}, maxInflight),
	}
}

// close stops the waits still going and waits for their goroutines.
func (l *loop) close() {
	l.stop()
	l.wg.Wait()
}

// errStopped is the interrupt value; the cause of ctx is reported instead.
var errStopped = errors.New("stopped")

func (l *loop) run(prg *goja.Program, info Info, env map[string]string) (result string, err error) {
	// Nothing a script does may take the replica down: a panic that gets
	// past goja (it should not) fails this run only.
	defer func() {
		if r := recover(); r != nil {
			result, err = "", fmt.Errorf("the script stopped with an internal error: %v", r)
		}
	}()

	// Stop the script when ctx ends, also inside a busy loop.
	watch := context.AfterFunc(l.ctx, func() { l.vm.Interrupt(errStopped) })
	defer watch()

	if err := l.globals(info, env); err != nil {
		return "", err
	}

	l.vm.SetPromiseRejectionTracker(func(_ *goja.Promise, op goja.PromiseRejectionOperation) {
		if op == goja.PromiseRejectionReject {
			l.unhandled++
		} else {
			l.unhandled--
		}
	})

	fnValue, err := l.vm.RunProgram(prg)
	if err != nil {
		return "", l.scriptErr(err)
	}

	fn, ok := goja.AssertFunction(fnValue)
	if !ok {
		return "", errors.New("the script did not load as a function")
	}

	ret, err := fn(goja.Undefined())
	if err != nil {
		return "", l.scriptErr(err)
	}

	promise, ok := ret.Export().(*goja.Promise)
	if !ok {
		return "", errors.New("the script did not return a promise")
	}

	for promise.State() == goja.PromiseStatePending {
		if l.pending == 0 {
			return "", errors.New("the script is waiting for a promise that nothing will settle")
		}

		select {
		case <-l.ctx.Done():
			return "", l.cause()
		case settle := <-l.done:
			l.pending--

			if err := settle(); err != nil {
				return "", l.scriptErr(err)
			}
		}
	}

	if promise.State() == goja.PromiseStateRejected {
		// The script's own promise is counted as unhandled too.
		l.unhandled--
		l.warnUnhandled()

		return "", l.thrown(promise.Result())
	}

	l.warnUnhandled()

	return l.resultJSON(promise.Result())
}

func (l *loop) warnUnhandled() {
	if l.unhandled > 0 {
		l.out.Printf("WARN %d promise(s) were rejected and never handled (a missing await or .catch?)\n", l.unhandled)
	}
}

// cause is why ctx ended: the job timeout, a cancel, or the replica stopping.
func (l *loop) cause() error {
	if c := context.Cause(l.ctx); c != nil {
		return c
	}

	return context.Canceled
}

func (l *loop) scriptErr(err error) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return l.cause()
	}

	var ex *goja.Exception
	if errors.As(err, &ex) {
		return l.thrown(ex.Value())
	}

	var overflow *goja.StackOverflowError
	if errors.As(err, &overflow) {
		return errors.New("maximum call stack size exceeded")
	}

	return err
}

// call runs a captured helper; a throw inside it (a getter of the value, an
// interrupt) comes back as ok=false.
func (l *loop) call(fn goja.Callable, v goja.Value) (string, bool) {
	if fn == nil {
		return "", false
	}

	if l.ctx.Err() != nil {
		// Helpers run after the script; an interrupt from ctx would stop
		// them at once, so they run on a clean flag and a short budget.
		l.vm.ClearInterrupt()
	}

	timer := time.AfterFunc(helperBudget, func() { l.vm.Interrupt(errStopped) })
	defer func() {
		timer.Stop()
		l.vm.ClearInterrupt()
	}()

	r, err := fn(goja.Undefined(), v)
	if err != nil || r == nil || goja.IsUndefined(r) {
		return "", false
	}

	return r.String(), true
}

// cleanStack keeps the script's own frames of a stack trace: kronik's
// helpers and Go functions say nothing to the script's author.
func cleanStack(stack string) string {
	lines := strings.Split(strings.TrimSpace(stack), "\n")
	out := lines[:0]

	for _, line := range lines {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "at ") && (strings.Contains(t, "(native)") || strings.Contains(t, "<eval>")) {
			continue
		}

		out = append(out, line)
	}

	return strings.Join(out, "\n")
}

// helperBudget bounds the formatting of one value.
const helperBudget = 2 * time.Second

// thrown turns a value thrown by the script into an error with its message,
// and the stack of an Error into the output.
func (l *loop) thrown(v goja.Value) error {
	text, ok := l.call(l.helpers.describe, v)
	if !ok {
		return errors.New("the script threw a value that cannot be shown")
	}

	msg, stack, _ := strings.Cut(text, "\x00")
	if stack = cleanStack(stack); stack != "" {
		l.out.Printf("%s\n", stack)
	}

	return errors.New(msg)
}

func (l *loop) resultJSON(v goja.Value) (string, error) {
	if v == nil || goja.IsUndefined(v) {
		return "", nil
	}

	s, ok := l.call(l.helpers.toJSON, v)
	if !ok {
		// The run did its work; only its result cannot be kept.
		l.out.Printf("WARN the return value could not be turned into JSON, so no result is kept\n")

		return "", nil
	}

	return s, nil
}

// wait runs fn on a goroutine and returns a promise settled with its result
// on the script's goroutine. fn's value is converted there by toJS.
func (l *loop) wait(fn func(ctx context.Context) (any, error), toJS func(any) (goja.Value, error)) goja.Value {
	promise, resolve, reject := l.vm.NewPromise()
	l.pending++
	l.wg.Add(1)

	go func() {
		defer l.wg.Done()

		v, err := fn(l.ctx)

		settle := func() error {
			if err != nil {
				return reject(l.vm.NewGoError(err))
			}

			jv, convErr := toJS(v)
			if convErr != nil {
				return reject(l.vm.NewGoError(convErr))
			}

			return resolve(jv)
		}

		select {
		case l.done <- settle:
		case <-l.ctx.Done():
		}
	}()

	return l.vm.ToValue(promise)
}

// helpersJS formats values for the output and the result, as browsers do:
// JSON.stringify for the result (NaN becomes null, functions are left out),
// and a readable form for console.log that copes with cycles, Map, Set and
// BigInt. It is loaded before the script, which cannot change it.
const helpersJS = `(function () {
  const stringify = JSON.stringify, toStr = String, isArray = Array.isArray
  const MapT = Map, SetT = Set, ErrorT = Error, keys = Object.keys
  const clean = (s) => typeof s === 'string' ? s.split('\n').filter((l) => !/^\s*at .*(\(native\)|<eval>)/.test(l)).join('\n') : s
  function show(v, seen, depth) {
    if (v === undefined) return 'undefined'
    if (v === null) return 'null'
    const t = typeof v
    if (t === 'string') return depth === 0 ? v : stringify(v)
    if (t === 'bigint') return toStr(v) + 'n'
    if (t === 'symbol') return toStr(v)
    if (t === 'function') return '[Function' + (v.name ? ': ' + v.name : '') + ']'
    if (t !== 'object') return toStr(v)
    if (v instanceof ErrorT) return clean(v.stack) || (v.name + ': ' + v.message)
    if (seen.indexOf(v) >= 0) return '[Circular]'
    if (depth > 6) return isArray(v) ? '[Array]' : '[Object]'
    seen.push(v)
    let out
    if (v instanceof MapT) {
      const parts = []
      v.forEach((val, k) => parts.push(show(k, seen, depth + 1) + ' => ' + show(val, seen, depth + 1)))
      out = 'Map(' + v.size + ') {' + parts.join(', ') + '}'
    } else if (v instanceof SetT) {
      const parts = []
      v.forEach((val) => parts.push(show(val, seen, depth + 1)))
      out = 'Set(' + v.size + ') {' + parts.join(', ') + '}'
    } else if (isArray(v)) {
      out = '[' + v.slice(0, 100).map((x) => show(x, seen, depth + 1)).join(',') + (v.length > 100 ? ',…' : '') + ']'
    } else if (typeof v.toJSON === 'function') {
      out = show(v.toJSON(), seen, depth)
    } else {
      out = '{' + keys(v).slice(0, 100).map((k) => stringify(k) + ':' + show(v[k], seen, depth + 1)).join(',') + '}'
    }
    seen.pop()
    return out
  }
  return {
    inspect: (v) => show(v, [], 0),
    describe: (v) => {
      if (v instanceof ErrorT) return (v.name || 'Error') + ': ' + v.message + '\0' + (v.stack || '')
      return 'uncaught ' + show(v, [], 1)
    },
    toJSON: (v) => stringify(v),
  }
})()`

func (l *loop) globals(info Info, env map[string]string) error {
	vm := l.vm

	helpers, err := vm.RunString(helpersJS)
	if err != nil {
		return fmt.Errorf("load helpers; %w", err)
	}

	h := helpers.ToObject(vm)
	l.helpers.inspect, _ = goja.AssertFunction(h.Get("inspect"))
	l.helpers.describe, _ = goja.AssertFunction(h.Get("describe"))
	l.helpers.toJSON, _ = goja.AssertFunction(h.Get("toJSON"))

	console := vm.NewObject()
	for _, name := range []string{"log", "info", "warn", "error", "debug"} {
		prefix := ""
		if name == "warn" || name == "error" {
			prefix = strings.ToUpper(name) + " "
		}

		if err := console.Set(name, func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				s, err := l.inspect(a)
				if err != nil {
					// Rethrown into the script, as the browser would; an
					// interrupt goes on as the interrupt it is.
					var ex *goja.Exception
					if errors.As(err, &ex) {
						panic(ex.Value())
					}

					var interrupted *goja.InterruptedError
					if errors.As(err, &interrupted) {
						vm.Interrupt(errStopped)

						return goja.Undefined()
					}

					panic(vm.NewGoError(err))
				}

				parts[i] = s
			}

			l.out.Printf("%s%s\n", prefix, strings.Join(parts, " "))

			return goja.Undefined()
		}); err != nil {
			return err
		}
	}

	jobObj := vm.NewObject()
	for k, v := range map[string]any{
		"id":          info.JobID,
		"name":        info.JobName,
		"runId":       info.RunID,
		"attempt":     info.Attempt,
		"scheduledAt": info.ScheduledAt.UTC().Format(time.RFC3339),
	} {
		if err := jobObj.Set(k, v); err != nil {
			return err
		}
	}

	secrets := vm.NewObject()
	for k, v := range info.Secrets {
		if err := secrets.Set(k, v); err != nil {
			return err
		}
	}

	params := vm.NewObject()
	for k, v := range info.Params {
		if err := params.Set(k, v); err != nil {
			return err
		}
	}

	envObj := vm.NewObject()
	for k, v := range env {
		if err := envObj.Set(k, v); err != nil {
			return err
		}
	}

	for name, v := range map[string]any{
		"console": console,
		"job":     jobObj,
		"env":     envObj,
		"secrets": secrets,
		"params":  params,
		"sleep":   l.sleep,
	} {
		if err := vm.Set(name, v); err != nil {
			return err
		}
	}

	prelude, err := vm.RunString(fetchPrelude)
	if err != nil {
		return fmt.Errorf("load fetch; %w", err)
	}

	build, _ := goja.AssertFunction(prelude)

	api, err := build(goja.Undefined(), vm.ToValue(l.fetchJS))
	if err != nil {
		return fmt.Errorf("load fetch; %w", err)
	}

	for _, name := range []string{"fetch", "Headers", "Response"} {
		if err := vm.Set(name, api.ToObject(vm).Get(name)); err != nil {
			return err
		}
	}

	// Nothing a script does may be undone by it reassigning these.
	for _, name := range []string{"job", "env", "secrets", "params"} {
		if _, err := vm.RunString("Object.freeze(" + name + ")"); err != nil {
			return err
		}
	}

	return nil
}

// inspect prints a value as console.log does. A throw while formatting (a
// getter) becomes the script's error, as in browsers.
func (l *loop) inspect(v goja.Value) (string, error) {
	r, err := l.helpers.inspect(goja.Undefined(), v)
	if err != nil {
		return "", err
	}

	return r.String(), nil
}

// maxSleep is the longest sleep; the run's timeout ends it earlier.
const maxSleep = 24 * time.Hour

func (l *loop) sleep(call goja.FunctionCall) goja.Value {
	d := clampMs(call.Argument(0).ToFloat(), maxSleep)

	return l.wait(func(ctx context.Context) (any, error) {
		t := time.NewTimer(d)
		defer t.Stop()

		select {
		case <-t.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, func(any) (goja.Value, error) { return goja.Undefined(), nil })
}

// clampMs turns a JavaScript number of milliseconds into a duration between
// 0 and limit; NaN is 0 and Infinity is limit.
func clampMs(ms float64, limit time.Duration) time.Duration {
	switch {
	case math.IsNaN(ms) || ms <= 0:
		return 0
	case ms >= float64(limit/time.Millisecond):
		return limit
	}

	return time.Duration(ms * float64(time.Millisecond))
}
