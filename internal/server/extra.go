package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kronik/internal/notify"
	"github.com/rakunlabs/kronik/internal/schedule"
	"github.com/rakunlabs/kronik/internal/secrets"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/store"
)

func decodeStrict(c *ada.Context, v any) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return ada.NewHTTPError(http.StatusBadRequest, "invalid body: "+err.Error())
	}

	return nil
}

func (s *Server) secretNames() []string {
	return slices.Sorted(maps.Keys(s.vault.Values()))
}

// Secrets.

type secretView struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Source      string     `json:"source"` // "ui" or "env"
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	UpdatedBy   string     `json:"updated_by,omitempty"`
	// Readable is false when this replica cannot decrypt the value.
	Readable bool `json:"readable"`
	// Shadows is set on a stored secret with the name of one from the
	// environment, which it replaces.
	Shadows bool `json:"shadows,omitempty"`
}

type secretsPage struct {
	secrets.Status
	Secrets []secretView       `json:"secrets"`
	Keys    []*store.SecretKey `json:"keys"`
}

func (s *Server) listSecrets(c *ada.Context) error {
	ctx := c.Request.Context()

	page := secretsPage{Status: s.vault.Status(), Secrets: []secretView{}, Keys: []*store.SecretKey{}}

	values := s.vault.Values()
	static := s.vault.StaticNames()

	stored, err := s.svc.Store().ListSecrets(ctx)
	if err != nil {
		return err
	}

	seen := map[string]bool{}

	for _, x := range stored {
		_, readable := values[x.Name]
		v := secretView{Name: x.Name, Description: x.Description, Source: "ui", CreatedAt: &x.CreatedAt, CreatedBy: x.CreatedBy,
			UpdatedAt: &x.UpdatedAt, UpdatedBy: x.UpdatedBy, Readable: readable, Shadows: slices.Contains(static, x.Name)}
		page.Secrets = append(page.Secrets, v)
		seen[x.Name] = true
	}

	for _, n := range static {
		if !seen[n] {
			page.Secrets = append(page.Secrets, secretView{Name: n, Source: "env", Readable: true})
		}
	}

	slices.SortFunc(page.Secrets, func(a, b secretView) int { return strings.Compare(a.Name, b.Name) })

	if page.Keys, err = s.svc.Store().SecretKeys(ctx); err != nil {
		return err
	}

	return c.SendJSON(page)
}

func (s *Server) changedSecrets(ctx context.Context) {
	s.bus.Send(context.WithoutCancel(ctx), store.TopicSecrets, "")
}

func (s *Server) putSecret(c *ada.Context) error {
	var in struct {
		Description string  `json:"description"`
		Value       *string `json:"value"`
	}

	if err := decodeStrict(c, &in); err != nil {
		return err
	}

	ctx := c.Request.Context()
	name := c.Request.PathValue("name")

	_, err := s.svc.Store().GetSecret(ctx, name)
	exists := err == nil

	if !exists && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	if !exists && in.Value == nil {
		return ada.NewHTTPError(http.StatusBadRequest, "a new secret needs a value")
	}

	if err := s.vault.Put(ctx, name, in.Description, in.Value, service.ActorFrom(ctx).User); err != nil {
		return secretErr(err)
	}

	change := "description"
	if in.Value != nil {
		change = "value"
	}

	action := service.ActionSecretUpdate
	if !exists {
		action = service.ActionSecretCreate
	}

	s.svc.AuditSimple(ctx, action, map[string]any{"name": name, "changed": change})
	s.changedSecrets(ctx)

	return s.listSecrets(c)
}

func secretErr(err error) error {
	if errors.Is(err, secrets.ErrInvalid) {
		return ada.WrapHTTPError(http.StatusBadRequest, err)
	}

	return apiErr(err)
}

func (s *Server) deleteSecret(c *ada.Context) error {
	ctx := c.Request.Context()
	name := c.Request.PathValue("name")

	if err := s.vault.Delete(ctx, name); err != nil {
		return secretErr(err)
	}

	s.svc.AuditSimple(ctx, service.ActionSecretDelete, map[string]any{"name": name})
	s.changedSecrets(ctx)

	return s.listSecrets(c)
}

// changeSecretsKey moves the secrets to a new master key. Every replica
// must get the new key in its configuration before it starts again.
func (s *Server) changeSecretsKey(c *ada.Context) error {
	var in struct {
		Key string `json:"key"`
	}

	if err := decodeStrict(c, &in); err != nil {
		return err
	}

	ctx := c.Request.Context()

	st, err := s.vault.ChangeKey(ctx, in.Key)
	if err != nil {
		return secretErr(err)
	}

	s.svc.AuditSimple(ctx, service.ActionSecretKey, st)
	s.changedSecrets(ctx)

	return s.listSecrets(c)
}

// Notification channels.

type channelInput struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

func (in channelInput) check() (channelInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return in, service.Invalid("a channel needs a name of up to 100 characters")
	}

	cfg, err := notify.CheckChannel(in.Type, in.Config)
	if err != nil {
		return in, service.Invalid("%v", err)
	}

	in.Config = cfg

	return in, nil
}

func (s *Server) listChannels(c *ada.Context) error {
	chs, err := s.svc.Store().ListChannels(c.Request.Context())
	if err != nil {
		return err
	}

	return c.SendJSON(chs)
}

func (s *Server) createChannel(c *ada.Context) error {
	var in channelInput
	if err := decodeStrict(c, &in); err != nil {
		return err
	}

	in, err := in.check()
	if err != nil {
		return apiErr(err)
	}

	ctx := c.Request.Context()
	now := store.Now()
	user := service.ActorFrom(ctx).User

	ch := &store.Channel{ID: store.NewID(), Name: in.Name, Type: in.Type, Config: in.Config, CreatedAt: now, CreatedBy: user, UpdatedAt: now, UpdatedBy: user}
	if err := s.svc.Store().PutChannel(ctx, ch, true); err != nil {
		return apiErr(err)
	}

	s.svc.AuditSimple(ctx, service.ActionChannelCreate, map[string]any{"id": ch.ID, "name": ch.Name, "type": ch.Type})

	return c.SetStatus(http.StatusCreated).SendJSON(ch)
}

func (s *Server) updateChannel(c *ada.Context) error {
	var in channelInput
	if err := decodeStrict(c, &in); err != nil {
		return err
	}

	in, err := in.check()
	if err != nil {
		return apiErr(err)
	}

	ctx := c.Request.Context()

	ch, err := s.svc.Store().GetChannel(ctx, c.Request.PathValue("id"))
	if err != nil {
		return apiErr(err)
	}

	ch.Name, ch.Type, ch.Config, ch.UpdatedAt, ch.UpdatedBy = in.Name, in.Type, in.Config, store.Now(), service.ActorFrom(ctx).User
	if err := s.svc.Store().PutChannel(ctx, ch, false); err != nil {
		return apiErr(err)
	}

	s.svc.AuditSimple(ctx, service.ActionChannelUpdate, map[string]any{"id": ch.ID, "name": ch.Name, "type": ch.Type})

	return c.SendJSON(ch)
}

func (s *Server) deleteChannel(c *ada.Context) error {
	ctx := c.Request.Context()

	ch, err := s.svc.Store().GetChannel(ctx, c.Request.PathValue("id"))
	if err != nil {
		return apiErr(err)
	}

	if err := s.svc.Store().DeleteChannel(ctx, ch.ID); err != nil {
		return apiErr(err)
	}

	s.svc.AuditSimple(ctx, service.ActionChannelDelete, map[string]any{"id": ch.ID, "name": ch.Name})

	return c.SendNoContent()
}

// testChannel sends a test message now, from this replica, and says how it
// went.
func (s *Server) testChannel(c *ada.Context) error {
	ctx := c.Request.Context()

	ch, err := s.svc.Store().GetChannel(ctx, c.Request.PathValue("id"))
	if err != nil {
		return apiErr(err)
	}

	if s.sender == nil {
		return ada.NewHTTPError(http.StatusConflict, "notifications are not set up on this server")
	}

	ev := store.Event{Event: "test", JobName: "kronik", At: store.Now()}
	payload, _ := json.Marshal(ev)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	err = s.sender.Send(ctx, s.notifyOptions(), ch, &store.Notification{Event: "test", Payload: payload})
	if err != nil {
		return c.SendJSON(map[string]any{"ok": false, "message": err.Error()})
	}

	return c.SendJSON(map[string]any{"ok": true, "message": "sent"})
}

func (s *Server) notifyOptions() notify.Options {
	v := s.values()
	sec := s.vault.Values()

	return notify.Options{
		Enabled: true, BaseURL: v.NotifyBaseURL, Secrets: sec, Environment: v.Environment,
		SMTP: notify.SMTP{Host: v.SMTPHost, Port: v.SMTPPort, User: v.SMTPUser, Password: sec[v.SMTPPasswordSecret], From: v.SMTPFrom, TLS: v.SMTPTLS},
	}
}

func (s *Server) listNotifications(c *ada.Context) error {
	limit := 100
	if v, err := strconv.Atoi(c.Request.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}

	ns, err := s.svc.Store().ListNotifications(c.Request.Context(), c.Request.URL.Query().Get("job_id"), limit)
	if err != nil {
		return err
	}

	return c.SendJSON(ns)
}

// Webhooks.

func (s *Server) newHook(c *ada.Context) error {
	token, err := s.svc.NewHook(c.Request.Context(), c.Request.PathValue("id"))
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(map[string]string{"token": token})
}

func (s *Server) removeHook(c *ada.Context) error {
	if err := s.svc.RemoveHook(c.Request.Context(), c.Request.PathValue("id")); err != nil {
		return apiErr(err)
	}

	return c.SendNoContent()
}

// hookParams reads params from the query and from a JSON body: an object of
// strings, or {"params": {...}}.
func hookParams(c *ada.Context) (map[string]string, error) {
	params := map[string]string{}

	for k, v := range c.Request.URL.Query() {
		if len(v) > 0 {
			params[k] = v[len(v)-1]
		}
	}

	if c.Request.Method != http.MethodPost || c.Request.ContentLength == 0 {
		return params, nil
	}

	b, err := io.ReadAll(c.Request.Body)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return params, nil //nolint:nilerr // an unreadable body is an empty one
	}

	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, service.Invalid("the body must be a JSON object")
	}

	if p, ok := body["params"].(map[string]any); ok && len(body) == 1 {
		body = p
	}

	for k, v := range body {
		switch x := v.(type) {
		case string:
			params[k] = x
		case nil:
		default:
			j, _ := json.Marshal(x)
			params[k] = string(j)
		}
	}

	return params, nil
}

func hookErr(err error) error {
	if errors.Is(err, service.ErrBadHook) {
		return ada.NewHTTPError(http.StatusNotFound, "unknown webhook")
	}

	return apiErr(err)
}

func (s *Server) hookTrigger(c *ada.Context) error {
	params, err := hookParams(c)
	if err != nil {
		return apiErr(err)
	}

	j, id, err := s.svc.HookTrigger(c.Request.Context(), c.Request.PathValue("token"), params)
	if err != nil {
		return hookErr(err)
	}

	return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"job": j.Name, "run_id": id})
}

func (s *Server) hookReport(c *ada.Context) error {
	var r service.Report

	if c.Request.Method == http.MethodPost && c.Request.ContentLength != 0 {
		if err := json.NewDecoder(c.Request.Body).Decode(&r); err != nil && !errors.Is(err, io.EOF) {
			return ada.NewHTTPError(http.StatusBadRequest, "invalid body: "+err.Error())
		}
	}

	q := c.Request.URL.Query()
	if v := q.Get("status"); v != "" {
		r.Status = v
	}

	if v := q.Get("exit_code"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return ada.NewHTTPError(http.StatusBadRequest, "exit_code must be a number")
		}

		r.ExitCode = &n
	}

	j, id, err := s.svc.HookReport(c.Request.Context(), c.Request.PathValue("token"), r)
	if err != nil {
		return hookErr(err)
	}

	return c.SendJSON(map[string]string{"job": j.Name, "run_id": id})
}

// Job pages.

func (s *Server) jobStats(c *ada.Context) error {
	n := 100
	if v, err := strconv.Atoi(c.Request.URL.Query().Get("n")); err == nil && v > 0 {
		n = min(v, 1000)
	}

	pts, err := s.svc.Store().RunPoints(c.Request.Context(), c.Request.PathValue("id"), n)
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(pts)
}

func (s *Server) jobHistory(c *ada.Context) error {
	entries, err := s.svc.Store().ListAudit(c.Request.Context(), store.AuditFilter{
		JobID: c.Request.PathValue("id"), Actions: []string{service.ActionJobCreate, service.ActionJobUpdate}, Limit: 100,
	})
	if err != nil {
		return err
	}

	return c.SendJSON(entries)
}

func (s *Server) bulkJobs(c *ada.Context) error {
	var in service.Bulk
	if err := decodeStrict(c, &in); err != nil {
		return err
	}

	res, err := s.svc.Bulk(c.Request.Context(), in)
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(res)
}

// Calendar.

type calendarItem struct {
	JobID   string    `json:"job_id"`
	JobName string    `json:"job_name"`
	At      time.Time `json:"at"`
	// Skipped says why the run will be skipped (pause, window), "" when it
	// runs.
	Skipped string `json:"skipped,omitempty"`
}

type calendarPage struct {
	From  time.Time      `json:"from"`
	To    time.Time      `json:"to"`
	Items []calendarItem `json:"items"`
	// Capped is set when a job had more runs in the range than were listed.
	Capped  []string     `json:"capped"`
	Windows []windowSpan `json:"windows"`
}

type windowSpan struct {
	Expr  string    `json:"expr"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Tags  []string  `json:"tags"`
	Pause bool      `json:"pause,omitempty"`
}

const (
	maxCalendarRange = 31 * 24 * time.Hour
	maxPerJob        = 500
	maxCalendarItems = 20000
)

// calendar lists the run times of the active jobs in a range: from and to
// (RFC 3339), at most a month.
func (s *Server) calendar(c *ada.Context) error {
	q := c.Request.URL.Query()

	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		return ada.NewHTTPError(http.StatusBadRequest, "from: "+err.Error())
	}

	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		return ada.NewHTTPError(http.StatusBadRequest, "to: "+err.Error())
	}

	if from.IsZero() {
		from = time.Now().UTC()
	}

	if to.IsZero() {
		to = from.Add(24 * time.Hour)
	}

	if !to.After(from) || to.Sub(from) > maxCalendarRange {
		return ada.NewHTTPError(http.StatusBadRequest, "the range must be positive and at most 31 days")
	}

	jobs, err := s.svc.Store().ListJobs(c.Request.Context(), store.JobFilter{Tag: q.Get("tag")})
	if err != nil {
		return err
	}

	v := s.values()
	windows := v.Windows()

	page := calendarPage{From: from, To: to, Items: []calendarItem{}, Capped: []string{}, Windows: []windowSpan{}}

	if v.Paused {
		page.Windows = append(page.Windows, windowSpan{Expr: "all schedules paused", From: from, To: to, Pause: true, Tags: []string{}})
	}

	for _, w := range windows {
		for t := w.Schedule.Prev(from); ; {
			if t.IsZero() {
				t = w.Schedule.Next(from)
			}

			if t.IsZero() || !t.Before(to) {
				break
			}

			if t.Add(w.Duration).After(from) {
				tags := w.Tags
				if tags == nil {
					tags = []string{}
				}

				page.Windows = append(page.Windows, windowSpan{Expr: w.Expr, From: t, To: t.Add(w.Duration), Tags: tags})
			}

			t = w.Schedule.Next(t)

			if len(page.Windows) > 1000 {
				break
			}
		}
	}

	for _, j := range jobs {
		if !j.Enabled {
			continue
		}

		sch, err := schedule.Parse(j.Schedule, j.Timezone)
		if err != nil {
			continue
		}

		n := 0

		for t := sch.Next(from.Add(-time.Second)); !t.IsZero() && t.Before(to); t = sch.Next(t) {
			if n >= maxPerJob || len(page.Items) >= maxCalendarItems {
				page.Capped = append(page.Capped, j.ID)

				break
			}

			item := calendarItem{JobID: j.ID, JobName: j.Name, At: t}
			if v.Paused {
				item.Skipped = "paused"
			} else {
				for _, w := range windows {
					if in, _ := w.Active(t); in && w.Applies(j.Tags) {
						item.Skipped = w.Expr

						break
					}
				}
			}

			page.Items = append(page.Items, item)
			n++
		}
	}

	slices.SortStableFunc(page.Items, func(a, b calendarItem) int { return a.At.Compare(b.At) })

	return c.SendJSON(page)
}
