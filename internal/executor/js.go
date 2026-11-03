package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// JSConfig is a job that runs JavaScript.
type JSConfig struct {
	Script string            `json:"script"`
	Env    map[string]string `json:"env"`
}

// JSLimits bound what one run of a script may do.
type JSLimits struct {
	// MaxDuration stops a script of a job without a timeout.
	MaxDuration time.Duration
	// MaxRequests is the number of fetch calls a run may make.
	MaxRequests int
	// MaxResponse is the largest response body fetch reads.
	MaxResponse int64
	// Secrets are given to every script as secrets.NAME. They come from the
	// replica's configuration and are never stored with a job or shown.
	Secrets map[string]string
}

type JS struct {
	limits JSLimits
	fetch  *fetcher
}

func NewJS(limits JSLimits) *JS {
	return &JS{limits: limits, fetch: newFetcher(limits.MaxResponse)}
}

func (j *JS) secretValues() []string {
	out := make([]string, 0, len(j.limits.Secrets))
	for _, v := range j.limits.Secrets {
		out = append(out, v)
	}

	return out
}

// SecretNames lists the secrets a script can read, for the editor.
func (j *JS) SecretNames() []string {
	names := make([]string, 0, len(j.limits.Secrets))
	for k := range j.limits.Secrets {
		names = append(names, k)
	}

	return names
}

const (
	// maxCallStack turns runaway recursion into a JavaScript error instead
	// of taking the replica down.
	maxCallStack = 1000
	maxScript    = 256 << 10
)

func (j *JS) Validate(raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := decode[JSConfig](raw)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(cfg.Script) == "" {
		return nil, invalid("script is required")
	}

	if len(cfg.Script) > maxScript {
		return nil, invalid("script is larger than %d KiB", maxScript>>10)
	}

	if _, err := compileJS(cfg.Script); err != nil {
		return nil, invalid("%v", err)
	}

	for k := range cfg.Env {
		if !isIdentifier(k) {
			return nil, invalid("env name %q must be a JavaScript identifier", k)
		}
	}

	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}

	return json.Marshal(cfg)
}

// The script is the body of an async function, so it can await and return.
const (
	wrapperHead = "(async function () {\n"
	wrapperTail = "\n})"
)

// Problem is a script error at a place in the user's text (1-based).
type Problem struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

func (p *Problem) Error() string {
	return fmt.Sprintf("line %d:%d: %s", p.Line, p.Column, p.Message)
}

func compileJS(src string) (*goja.Program, error) {
	wrapped := wrapperHead + src + wrapperTail
	lines := strings.Count(src, "\n") + 1

	parsed, err := parser.ParseFile(nil, "job.js", wrapped, 0)
	if err != nil {
		var list parser.ErrorList
		if errors.As(err, &list) && len(list) > 0 {
			return nil, position(src, lines, list[0].Position.Line, list[0].Position.Column, list[0].Message)
		}

		return nil, &Problem{Line: 1, Column: 1, Message: err.Error()}
	}

	// A script holding "})" could close the wrapper and put statements at
	// the top level; only one function literal may load.
	if !singleFunction(parsed) {
		return nil, &Problem{Line: 1, Column: 1, Message: "the script closes the function it runs in"}
	}

	prg, err := goja.CompileAST(parsed, true)
	if err != nil {
		var syntax *goja.CompilerSyntaxError
		if errors.As(err, &syntax) && syntax.File != nil {
			p := syntax.File.Position(syntax.Offset)

			return nil, position(src, lines, p.Line, p.Column, syntax.Message)
		}

		var ref *goja.CompilerReferenceError
		if errors.As(err, &ref) && ref.File != nil {
			p := ref.File.Position(ref.Offset)

			return nil, position(src, lines, p.Line, p.Column, ref.Message)
		}

		return nil, &Problem{Line: 1, Column: 1, Message: err.Error()}
	}

	return prg, nil
}

func singleFunction(program *ast.Program) bool {
	if len(program.Body) != 1 {
		return false
	}

	statement, ok := program.Body[0].(*ast.ExpressionStatement)
	if !ok {
		return false
	}

	_, ok = statement.Expression.(*ast.FunctionLiteral)

	return ok
}

// position maps a place in the wrapped program to the user's text: the
// wrapper adds one line before it.
func position(src string, lines, line, column int, message string) *Problem {
	line--

	switch {
	case line < 1:
		line, column = 1, 1
	case line > lines:
		line = lines
		last := src[strings.LastIndex(src, "\n")+1:]
		column = len([]rune(last)) + 1
	}

	return &Problem{Line: line, Column: column, Message: message}
}

// CheckJS reports why a script would be refused, for the editor.
func CheckJS(src string) []Problem {
	if _, err := compileJS(src); err != nil {
		var p *Problem
		if errors.As(err, &p) {
			return []Problem{*p}
		}

		return []Problem{{Line: 1, Column: 1, Message: err.Error()}}
	}

	return []Problem{}
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}

	for i, r := range s {
		switch {
		case r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}

	return true
}

func (j *JS) Run(ctx context.Context, info Info, raw json.RawMessage, out *Output) (*int, error) {
	cfg, err := decode[JSConfig](raw)
	if err != nil {
		return nil, err
	}

	if _, ok := ctx.Deadline(); !ok && j.limits.MaxDuration > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeoutCause(ctx, j.limits.MaxDuration,
			fmt.Errorf("stopped after %s, the longest a script may run without a job timeout", j.limits.MaxDuration))
		defer cancel()
	}

	res, err := j.Exec(ctx, info, cfg, out)
	out.SetResult(res)

	if err != nil {
		err = errors.New(out.Clean(err.Error()))
	}

	return nil, err
}

// Exec runs the script and returns its return value as JSON, "" when it
// returned nothing.
func (j *JS) Exec(ctx context.Context, info Info, cfg *JSConfig, out *Output) (string, error) {
	prg, err := compileJS(cfg.Script)
	if err != nil {
		return "", err
	}

	out.Redact(j.secretValues())

	l := newLoop(ctx, j, out)
	defer l.close()

	return l.run(prg, info, maps.Clone(cfg.Env))
}
