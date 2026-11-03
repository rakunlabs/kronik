package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kronik/internal/executor"
	"github.com/rakunlabs/kronik/internal/scheduler"
	"github.com/rakunlabs/kronik/internal/service"
	"github.com/rakunlabs/kronik/internal/settings"
)

type retentionInfo struct {
	Runs       string `json:"runs"`
	RunsPerJob int    `json:"runs_per_job"`
	Audit      string `json:"audit"`
	At         string `json:"at"`
	Zone       string `json:"zone"`
}

func (s *Server) retentionInfo() retentionInfo {
	v := s.values()

	return retentionInfo{
		Runs: settings.FormatDuration(v.RunRetention), RunsPerJob: v.RunsPerJob,
		Audit: settings.FormatDuration(v.AuditRetention), At: v.CleanupAt, Zone: v.CleanupZone,
	}
}

func (s *Server) retention() scheduler.Retention {
	v := s.values()

	return scheduler.Retention{Runs: v.RunRetention, RunsPerJob: v.RunsPerJob, Audit: v.AuditRetention, At: v.CleanupAt, Zone: v.CleanupZone}
}

type settingsPage struct {
	Editable bool             `json:"editable"`
	Groups   []settingsGroup  `json:"groups"`
	Entries  []settings.Entry `json:"entries"`
}

type settingsGroup struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

func (s *Server) getSettings(c *ada.Context) error {
	page := settingsPage{Editable: s.set != nil}

	for _, g := range settings.Groups {
		page.Groups = append(page.Groups, settingsGroup{ID: g.ID, Label: g.Label, Description: g.Description})
	}

	if s.set != nil {
		page.Entries = s.set.Entries()
	} else {
		for _, d := range settings.Defs {
			page.Entries = append(page.Entries, settings.Entry{Def: d, Value: s.base.Get(d.Key), Base: s.base.Get(d.Key)})
		}
	}

	return c.SendJSON(page)
}

func (s *Server) putSettings(c *ada.Context) error {
	if s.set == nil {
		return ada.NewHTTPError(http.StatusConflict, "settings cannot be changed on this server")
	}

	var in settings.Change

	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&in); err != nil {
		return ada.NewHTTPError(http.StatusBadRequest, "invalid body: "+err.Error())
	}

	before := s.set.Values()
	actor := service.ActorFrom(c.Request.Context())

	keys, err := s.set.Apply(c.Request.Context(), in, actor.User)
	if err != nil {
		return ada.WrapHTTPError(http.StatusBadRequest, err)
	}

	s.svc.AuditSettings(c.Request.Context(), keys, before, s.set.Values())

	return s.getSettings(c)
}

func (s *Server) cleanupNow(c *ada.Context) error {
	if s.cleaner == nil {
		return ada.NewHTTPError(http.StatusConflict, "no scheduler on this server")
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()

	st, err := s.cleaner.Cleanup(ctx, s.retention())
	if err != nil {
		return err
	}

	s.svc.AuditCleanup(c.Request.Context(), st.Runs, st.Audit)

	return c.SendJSON(st)
}

// checkExecutor tries the connection of a container executor with the
// settings in effect, so a wrong address shows before a job fails on it.
func (s *Server) checkExecutor(c *ada.Context) error {
	name := c.Request.PathValue("executor")

	exec, err := s.svc.Executor(name)
	if err != nil {
		return c.SendJSON(map[string]any{"ok": false, "message": err.Error()})
	}

	checker, ok := exec.(executor.Checker)
	if !ok {
		return c.SendJSON(map[string]any{"ok": true, "message": "nothing to connect to"})
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	msg, err := checker.Check(ctx)
	if err != nil {
		text := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			text = "no answer within 10 seconds"
		}

		return c.SendJSON(map[string]any{"ok": false, "message": strings.TrimSpace(text)})
	}

	return c.SendJSON(map[string]any{"ok": true, "message": msg})
}
