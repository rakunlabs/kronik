package executor

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type HTTPConfig struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	// ExpectedStatus lists the statuses that count as success; empty means
	// any 2xx.
	ExpectedStatus     []int `json:"expected_status"`
	InsecureSkipVerify bool  `json:"insecure_skip_verify"`
	// NoRedirect returns redirects as they are instead of following them.
	NoRedirect bool `json:"no_redirect"`
}

type HTTP struct {
	client   *http.Client
	insecure *http.Client
}

// defaultHTTPTimeout bounds a request of a job without a timeout.
const defaultHTTPTimeout = 10 * time.Minute

func NewHTTP() *HTTP {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = 5 * time.Minute
	insecure := base.Clone()
	insecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // asked for by the job

	return &HTTP{
		client:   &http.Client{Transport: base},
		insecure: &http.Client{Transport: insecure},
	}
}

func (h *HTTP) Validate(raw json.RawMessage) (json.RawMessage, error) {
	cfg, err := decode[HTTPConfig](raw)
	if err != nil {
		return nil, err
	}

	cfg.Method = strings.ToUpper(strings.TrimSpace(cfg.Method))
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}

	if strings.ContainsAny(cfg.Method, " \t\r\n") {
		return nil, invalid("method %q is not valid", cfg.Method)
	}

	cfg.URL = strings.TrimSpace(cfg.URL)

	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, invalid("url must be an absolute http or https URL")
	}

	for k := range cfg.Headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, " :\r\n") {
			return nil, invalid("header name %q is not valid", k)
		}
	}

	for _, s := range cfg.ExpectedStatus {
		if s < 100 || s > 599 {
			return nil, invalid("expected status %d is not an HTTP status", s)
		}
	}

	if cfg.Headers == nil {
		cfg.Headers = map[string]string{}
	}

	if cfg.ExpectedStatus == nil {
		cfg.ExpectedStatus = []int{}
	}

	return json.Marshal(cfg)
}

func (h *HTTP) Run(ctx context.Context, info Info, raw json.RawMessage, out *Output) (*int, error) {
	cfg, err := decode[HTTPConfig](raw)
	if err != nil {
		return nil, err
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeoutCause(ctx, defaultHTTPTimeout,
			fmt.Errorf("stopped after %s, the longest a request may take without a job timeout", defaultHTTPTimeout))
		defer cancel()
	}

	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "kronik")
	req.Header.Set("X-Kronik-Job", info.JobName)
	req.Header.Set("X-Kronik-Run", info.RunID)
	req.Header.Set("X-Kronik-Attempt", strconv.Itoa(info.Attempt))
	req.Header.Set("X-Kronik-Scheduled-At", info.ScheduledAt.UTC().Format(time.RFC3339))

	for k, v := range cfg.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v

			continue
		}

		req.Header.Set(k, v)
	}

	client := h.client
	if cfg.InsecureSkipVerify {
		client = h.insecure
	}

	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if cfg.NoRedirect {
			return http.ErrUseLastResponse
		}

		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}

		// The job's headers may hold credentials: they go to its own host
		// only. Go drops Authorization and Cookie itself.
		if req.URL.Host != via[0].URL.Host {
			for k := range cfg.Headers {
				req.Header.Del(k)
			}
		}

		return nil
	}
	client = &c

	out.Printf("%s %s\n", cfg.Method, cfg.URL)

	start := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		return nil, unwrapURLError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	out.Printf("%s %s (%s)\n\n", resp.Proto, resp.Status, time.Since(start).Round(time.Millisecond))

	// The output keeps only its end; reading on past a few times its size
	// costs time and bandwidth for nothing.
	limit := int64(out.Max()) * 4
	if n, err := io.Copy(out, io.LimitReader(resp.Body, limit)); err != nil && ctx.Err() == nil {
		out.Printf("\n[reading the body failed: %v]\n", err)
	} else if n == limit {
		out.Printf("\n[the body was longer; reading stopped after %d bytes]\n", limit)
	}

	code := resp.StatusCode

	if len(cfg.ExpectedStatus) > 0 {
		if !slices.Contains(cfg.ExpectedStatus, code) {
			return &code, fmt.Errorf("status %d is not one of %v", code, cfg.ExpectedStatus)
		}
	} else if code < 200 || code > 299 {
		return &code, fmt.Errorf("status %d", code)
	}

	return &code, nil
}

// unwrapURLError drops the method and URL net/http puts around an error;
// the output already shows them.
func unwrapURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}

	return err
}
