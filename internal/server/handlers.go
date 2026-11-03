package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kronik/internal/config"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/store"
)

// apiErr gives store and service errors their HTTP status.
func apiErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return ada.NewHTTPError(http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		return ada.WrapHTTPError(http.StatusConflict, err)
	case errors.Is(err, service.ErrInvalid):
		return ada.WrapHTTPError(http.StatusBadRequest, err)
	case errors.Is(err, service.ErrBusy):
		return ada.WrapHTTPError(http.StatusTooManyRequests, err)
	}

	return err
}

func (s *Server) info(c *ada.Context) error {
	return c.SendJSON(map[string]any{
		"name":        config.ServiceName,
		"version":     config.Build.Version,
		"build":       config.Build,
		"node":        s.node,
		"executors":   s.svc.Executors(),
		"scheduler":   s.scheduler,
		"user":        s.userOf(c.Request),
		"user_header": s.userHeader,
		"secrets":     s.secrets,
		"mcp":         map[string]any{"enabled": s.values().MCPEnabled, "read_only": s.values().MCPReadOnly},
		"environment": s.env(),
		"retention":   s.retentionInfo(),
		"settings":    map[string]any{"editable": s.set != nil},
	})
}

func (s *Server) stats(c *ada.Context) error {
	st, err := s.svc.Store().Stats(c.Request.Context(), time.Now().UTC())
	if err != nil {
		return err
	}

	return c.SendJSON(st)
}

func (s *Server) nodes(c *ada.Context) error {
	nodes, err := s.svc.Store().ListNodes(c.Request.Context(), time.Now().UTC().Add(-s.nodeTimeout))
	if err != nil {
		return err
	}

	return c.SendJSON(nodes)
}

func (s *Server) tags(c *ada.Context) error {
	tags, err := s.svc.Store().Tags(c.Request.Context())
	if err != nil {
		return err
	}

	return c.SendJSON(tags)
}

func (s *Server) scheduleNext(c *ada.Context) error {
	q := c.Request.URL.Query()

	n := 5
	if v, err := strconv.Atoi(q.Get("n")); err == nil && v > 0 && v <= 50 {
		n = v
	}

	return c.SendJSON(service.Next(q.Get("schedule"), q.Get("timezone"), n))
}

func (s *Server) listJobs(c *ada.Context) error {
	q := c.Request.URL.Query()

	jobs, err := s.svc.Store().ListJobs(c.Request.Context(), store.JobFilter{
		Search: strings.TrimSpace(q.Get("search")),
		Tag:    q.Get("tag"),
	})
	if err != nil {
		return err
	}

	return c.SendJSON(jobs)
}

func (s *Server) getJob(c *ada.Context) error {
	j, err := s.svc.Store().GetJob(c.Request.Context(), c.Request.PathValue("id"))
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(j)
}

func bindJob(c *ada.Context) (service.JobInput, error) {
	var in service.JobInput

	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&in); err != nil {
		return in, ada.NewHTTPError(http.StatusBadRequest, "invalid body: "+err.Error())
	}

	return in, nil
}

func (s *Server) createJob(c *ada.Context) error {
	in, err := bindJob(c)
	if err != nil {
		return err
	}

	j, err := s.svc.CreateJob(c.Request.Context(), in)
	if err != nil {
		return apiErr(err)
	}

	return c.SetStatus(http.StatusCreated).SendJSON(j)
}

func (s *Server) updateJob(c *ada.Context) error {
	in, err := bindJob(c)
	if err != nil {
		return err
	}

	j, err := s.svc.UpdateJob(c.Request.Context(), c.Request.PathValue("id"), in)
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(j)
}

func (s *Server) deleteJob(c *ada.Context) error {
	if err := s.svc.DeleteJob(c.Request.Context(), c.Request.PathValue("id")); err != nil {
		return apiErr(err)
	}

	return c.SendNoContent()
}

func (s *Server) enableJob(c *ada.Context) error  { return s.setEnabled(c, true) }
func (s *Server) disableJob(c *ada.Context) error { return s.setEnabled(c, false) }

func (s *Server) setEnabled(c *ada.Context, enabled bool) error {
	j, err := s.svc.SetEnabled(c.Request.Context(), c.Request.PathValue("id"), enabled)
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(j)
}

func (s *Server) triggerJob(c *ada.Context) error {
	id, err := s.svc.Trigger(c.Request.Context(), c.Request.PathValue("id"))
	if err != nil {
		return apiErr(err)
	}

	return c.SetStatus(http.StatusAccepted).SendJSON(map[string]string{"run_id": id})
}

const (
	defaultRunLimit = 50
	maxRunLimit     = 500
)

func (s *Server) listRuns(c *ada.Context) error {
	q := c.Request.URL.Query()

	limit := defaultRunLimit
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, maxRunLimit)
	}

	var status []string
	for v := range strings.SplitSeq(q.Get("status"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			status = append(status, v)
		}
	}

	f := store.RunFilter{
		JobID:       q.Get("job_id"),
		Status:      status,
		Search:      strings.TrimSpace(q.Get("q")),
		TriggeredBy: strings.TrimSpace(q.Get("user")),
		Before:      q.Get("before"),
		Limit:       limit,
	}

	// mine: runs of the jobs the requesting user made or changed, and the
	// runs they started.
	if q.Get("mine") == "true" {
		u := s.userOf(c.Request)
		if u == "" {
			return ada.NewHTTPError(http.StatusBadRequest, "no user header on this request, so there is no \"mine\"")
		}

		f.JobsBy = u
	}

	var err error

	if f.From, err = parseTimeParam(q.Get("from")); err != nil {
		return ada.NewHTTPError(http.StatusBadRequest, "from: "+err.Error())
	}

	if f.To, err = parseTimeParam(q.Get("to")); err != nil {
		return ada.NewHTTPError(http.StatusBadRequest, "to: "+err.Error())
	}

	if !f.From.IsZero() && !f.To.IsZero() && !f.To.After(f.From) {
		return ada.NewHTTPError(http.StatusBadRequest, "to must be after from")
	}

	runs, err := s.svc.Store().ListRuns(c.Request.Context(), f)
	if err != nil {
		return err
	}

	return c.SendJSON(runs)
}

// parseTimeParam reads an RFC 3339 time or a date (2026-11-03, UTC).
func parseTimeParam(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}

	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}

	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}

	return time.Time{}, errors.New("use an RFC 3339 time or a date like 2026-11-03")
}

func (s *Server) getRun(c *ada.Context) error {
	r, err := s.svc.Store().GetRun(c.Request.Context(), c.Request.PathValue("id"))
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(r)
}

func (s *Server) cancelRun(c *ada.Context) error {
	if err := s.svc.CancelRun(c.Request.Context(), c.Request.PathValue("id")); err != nil {
		return apiErr(err)
	}

	return c.SendNoContent()
}

func (s *Server) testJob(c *ada.Context) error {
	in, err := bindJob(c)
	if err != nil {
		return err
	}

	t, err := s.svc.Test(c.Request.Context(), in)
	if err != nil {
		return apiErr(err)
	}

	return c.SendJSON(t)
}

func (s *Server) checkScript(c *ada.Context) error {
	var in struct {
		Script string `json:"script"`
	}

	if err := json.NewDecoder(c.Request.Body).Decode(&in); err != nil {
		return ada.NewHTTPError(http.StatusBadRequest, "invalid body: "+err.Error())
	}

	return c.SendJSON(map[string]any{"problems": service.CheckScript(in.Script)})
}

func (s *Server) listAudit(c *ada.Context) error {
	q := c.Request.URL.Query()

	limit := 100
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, maxRunLimit)
	}

	entries, err := s.svc.Store().ListAudit(c.Request.Context(), store.AuditFilter{
		JobID:  q.Get("job_id"),
		Actor:  q.Get("user"),
		Before: q.Get("before"),
		Limit:  limit,
	})
	if err != nil {
		return err
	}

	return c.SendJSON(entries)
}
