package settings

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kronik/internal/schedule"
)

// Window is a maintenance window: from each time of Schedule, for Duration,
// scheduled runs of the jobs with one of Tags (or of all jobs) are skipped.
type Window struct {
	Expr     string
	Schedule *schedule.Schedule
	Duration time.Duration
	Tags     []string
}

// ParseWindow reads "schedule|duration[|tag,tag]".
func ParseWindow(s, zone string) (Window, error) {
	parts := strings.Split(s, "|")
	if len(parts) < 2 || len(parts) > 3 {
		return Window{}, fmt.Errorf("window %q: write schedule|duration or schedule|duration|tags", s)
	}

	sch, err := schedule.Parse(strings.TrimSpace(parts[0]), zone)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %w", s, err)
	}

	d, err := ParseDuration(parts[1])
	if err != nil || d <= 0 {
		return Window{}, fmt.Errorf("window %q: the duration must be positive, such as 2h", s)
	}

	w := Window{Expr: s, Schedule: sch, Duration: d}

	if len(parts) == 3 {
		for t := range strings.SplitSeq(parts[2], ",") {
			if t = strings.TrimSpace(t); t != "" {
				w.Tags = append(w.Tags, t)
			}
		}
	}

	return w, nil
}

// Active reports whether t is in the window: a start of the schedule is at
// most Duration before t.
func (w Window) Active(t time.Time) (bool, time.Time) {
	start := w.Schedule.Prev(t.Add(time.Second))
	for !start.IsZero() && start.After(t) {
		start = w.Schedule.Prev(start)
	}

	if start.IsZero() || t.Sub(start) >= w.Duration {
		return false, time.Time{}
	}

	return true, start.Add(w.Duration)
}

// Applies reports whether the window covers a job with these tags.
func (w Window) Applies(tags []string) bool {
	if len(w.Tags) == 0 {
		return true
	}

	for _, t := range w.Tags {
		if slices.Contains(tags, t) {
			return true
		}
	}

	return false
}

// Windows parses the maintenance windows; bad ones are left out.
func (v *Values) Windows() []Window {
	var out []Window

	for _, s := range v.MaintenanceWindows {
		if w, err := ParseWindow(s, v.zone()); err == nil {
			out = append(out, w)
		}
	}

	return out
}

func (v *Values) zone() string {
	if v.MaintenanceZone == "" {
		return "UTC"
	}

	return v.MaintenanceZone
}

// ParseTagLimit reads "tag=n".
func ParseTagLimit(s string) (string, int, error) {
	tag, n, ok := strings.Cut(s, "=")
	tag = strings.TrimSpace(tag)

	limit, err := strconv.Atoi(strings.TrimSpace(n))
	if !ok || tag == "" || err != nil || limit < 1 || limit > 10000 {
		return "", 0, fmt.Errorf("tag limit %q: write tag=n with n from 1", s)
	}

	return tag, limit, nil
}

// Limits parses the tag limits; bad ones are left out.
func (v *Values) Limits() map[string]int {
	out := map[string]int{}

	for _, s := range v.TagLimits {
		if t, n, err := ParseTagLimit(s); err == nil {
			out[t] = n
		}
	}

	return out
}

func (v *Values) checkScheduling() error {
	if _, err := time.LoadLocation(v.zone()); err != nil {
		return fmt.Errorf("unknown window time zone %q", v.MaintenanceZone)
	}

	for _, s := range v.MaintenanceWindows {
		if _, err := ParseWindow(s, v.zone()); err != nil {
			return err
		}
	}

	for _, s := range v.TagLimits {
		if _, _, err := ParseTagLimit(s); err != nil {
			return err
		}
	}

	return nil
}
