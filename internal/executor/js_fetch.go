package executor

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// fetcher makes the HTTP requests of scripts. Responses are read whole (up to
// max) before they are handed to the script.
type fetcher struct {
	max      int64
	client   *http.Client
	insecure *http.Client
}

func newFetcher(maxBody int64) *fetcher {
	base := http.DefaultTransport.(*http.Transport).Clone()
	insecure := base.Clone()
	insecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // asked for by the script

	return &fetcher{
		max:      maxBody,
		client:   &http.Client{Transport: base},
		insecure: &http.Client{Transport: insecure},
	}
}

type fetchRequest struct {
	URL     string              `json:"url"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
	HasBody bool                `json:"hasBody"`
	// Insecure skips the TLS certificate check.
	Insecure bool `json:"insecure"`
	// Redirect is "follow" (default), "manual" or "error", as in fetch.
	Redirect string `json:"redirect"`
	// TimeoutMs bounds this request; 0 uses the run's time.
	TimeoutMs float64 `json:"timeout"`
}

type fetchResponse struct {
	Status     int                 `json:"status"`
	StatusText string              `json:"statusText"`
	URL        string              `json:"url"`
	Redirected bool                `json:"redirected"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
}

const defaultFetchTimeout = 5 * time.Minute

// redactURL drops the query and the user info, which may hold credentials.
func redactURL(u *url.URL) string {
	c := *u
	c.User = nil

	if c.RawQuery != "" {
		c.RawQuery = "…"
	}

	c.Fragment = ""

	return c.String()
}

func (f *fetcher) do(ctx context.Context, r fetchRequest) (*fetchResponse, error) {
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("fetch: %q is not an absolute http or https URL", r.URL)
	}

	method := strings.ToUpper(r.Method)
	if method == "" {
		method = http.MethodGet
	}

	timeout := defaultFetchTimeout
	if r.TimeoutMs > 0 {
		timeout = clampMs(r.TimeoutMs, defaultFetchTimeout)
	}

	// Errors name the host and path, not the query, where tokens often are.
	shown := redactURL(u)

	ctx, cancel := context.WithTimeoutCause(ctx, timeout, fmt.Errorf("fetch %s: timed out after %s", shown, timeout))
	defer cancel()

	var body io.Reader
	if r.HasBody {
		body = strings.NewReader(r.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	req.Header.Set("User-Agent", "kronik")

	for k, values := range r.Headers {
		if strings.EqualFold(k, "Host") {
			if len(values) > 0 {
				req.Host = values[len(values)-1]
			}

			continue
		}

		req.Header.Del(k)

		for _, v := range values {
			req.Header.Add(k, v)
		}
	}

	client := f.client
	if r.Insecure {
		client = f.insecure
	}

	redirected := false

	c := *client
	c.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		switch r.Redirect {
		case "manual":
			return http.ErrUseLastResponse
		case "error":
			return errors.New("redirected, and redirect is \"error\"")
		}

		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}

		redirected = true

		return nil
	}

	resp, err := c.Do(req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && ctx.Err() != nil {
			return nil, cause
		}

		return nil, fmt.Errorf("fetch %s: %w", shown, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.max+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: reading the body: %w", shown, err)
	}

	if int64(len(data)) > f.max {
		return nil, fmt.Errorf("fetch %s: the response is larger than %d bytes", shown, f.max)
	}

	headers := make(map[string][]string, len(resp.Header))
	for k, v := range resp.Header {
		headers[strings.ToLower(k)] = v
	}

	return &fetchResponse{
		Status:     resp.StatusCode,
		StatusText: strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode))),
		URL:        resp.Request.URL.String(),
		Redirected: redirected,
		Headers:    headers,
		Body:       string(data),
	}, nil
}

// fetchPrelude builds fetch on top of the native request: a Response with
// text(), json(), ok and headers.get(), as in browsers. It returns
// {fetch, Headers, Response}.
const fetchPrelude = `(function (native) {
  const own = (o, k) => Object.prototype.hasOwnProperty.call(o, k)
  class Headers {
    constructor(init) {
      this._h = Object.create(null)
      if (!init) return
      if (init instanceof Headers) init.forEach((v, k) => this.append(k, v))
      else if (Array.isArray(init)) for (const [k, v] of init) this.append(k, v)
      else for (const k of Object.keys(init)) {
        const v = init[k]
        if (Array.isArray(v)) for (const x of v) this.append(k, x)
        else this.append(k, v)
      }
    }
    append(name, value) {
      const k = String(name).toLowerCase()
      if (!own(this._h, k)) this._h[k] = []
      this._h[k].push(String(value))
    }
    set(name, value) { this._h[String(name).toLowerCase()] = [String(value)] }
    delete(name) { delete this._h[String(name).toLowerCase()] }
    get(name) { const k = String(name).toLowerCase(); return own(this._h, k) ? this._h[k].join(', ') : null }
    has(name) { return own(this._h, String(name).toLowerCase()) }
    entries() { return Object.keys(this._h).map((k) => [k, this._h[k].join(', ')]) }
    keys() { return Object.keys(this._h) }
    forEach(fn) { for (const [k, v] of this.entries()) fn(v, k, this) }
    toJSON() { return Object.fromEntries(this.entries()) }
  }
  class Response {
    constructor(r) {
      this.status = r.status
      this.statusText = r.statusText
      this.ok = r.status >= 200 && r.status < 300
      this.url = r.url
      this.redirected = r.redirected
      this.headers = new Headers(r.headers)
      this._body = r.body
    }
    async text() { return this._body }
    async json() {
      try { return JSON.parse(this._body) }
      catch (e) { throw new SyntaxError('the response of ' + this.url + ' is not JSON: ' + e.message) }
    }
  }
  async function fetch(input, init) {
    init = init || {}
    const headers = new Headers(init.headers)
    const req = {
      url: String(input),
      method: init.method ? String(init.method) : 'GET',
      headers: headers._h,
      hasBody: init.body !== undefined && init.body !== null,
      body: '',
      insecure: !!init.insecure,
      redirect: init.redirect ? String(init.redirect) : 'follow',
      timeout: Number(init.timeout) || 0,
    }
    if (req.hasBody) {
      if (typeof init.body === 'string') req.body = init.body
      else {
        req.body = JSON.stringify(init.body)
        if (!headers.has('content-type')) headers.set('Content-Type', 'application/json')
      }
    }
    return new Response(await native(req))
  }
  return { fetch, Headers, Response }
})`

// fetchJS is the native side of fetch: it takes the request built by the
// prelude and returns a promise of the raw response.
func (l *loop) fetchJS(call goja.FunctionCall) goja.Value {
	var r fetchRequest
	if err := l.vm.ExportTo(call.Argument(0), &r); err != nil {
		panic(l.vm.NewTypeError("fetch: %v", err))
	}

	l.requests++
	if limit := l.js.limits.MaxRequests; limit > 0 && l.requests > limit {
		panic(l.vm.NewTypeError(fmt.Sprintf("fetch: a run may make at most %d requests", limit)))
	}

	return l.wait(func(ctx context.Context) (any, error) {
		select {
		case l.inflight <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-l.inflight }()

		return l.js.fetch.do(ctx, r)
	}, func(v any) (goja.Value, error) {
		resp := v.(*fetchResponse)

		return l.vm.ToValue(map[string]any{
			"status":     resp.Status,
			"statusText": resp.StatusText,
			"url":        resp.URL,
			"redirected": resp.Redirected,
			"headers":    resp.Headers,
			"body":       resp.Body,
		}), nil
	})
}
