// Package schedule turns a job's cron expression and time zone into run times,
// using hardloop's parser.
package schedule

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rakunlabs/hardloop"
)

type Schedule struct {
	spec *hardloop.SpecSchedule
}

// Parse reads a cron expression (five fields or a descriptor such as @daily
// or @every 10m). timezone is an IANA name; empty means UTC. A CRON_TZ= or
// TZ= prefix in the expression wins over timezone.
func Parse(expr, timezone string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, errors.New("schedule is empty")
	}

	if !strings.HasPrefix(expr, "CRON_TZ=") && !strings.HasPrefix(expr, "TZ=") {
		tz := strings.TrimSpace(timezone)
		if tz == "" {
			tz = "UTC"
		}

		expr = "CRON_TZ=" + tz + " " + expr
	}

	// "Local" is the replica's own zone, which differs between replicas
	// (and hardloop reads it as "the zone of the time given", always UTC).
	if zone := zoneOf(expr); strings.EqualFold(zone, "Local") {
		return nil, errors.New(`time zone "Local" is not allowed, name the zone (e.g. Europe/Istanbul)`)
	} else if _, err := time.LoadLocation(zone); err != nil {
		return nil, fmt.Errorf("unknown time zone %q", zone)
	}

	spec, err := hardloop.Parse(expr)
	if err != nil {
		return nil, err
	}

	return &Schedule{spec: spec}, nil
}

// zoneOf is the zone of a CRON_TZ= or TZ= prefixed expression.
func zoneOf(expr string) string {
	_, rest, _ := strings.Cut(expr, "=")
	zone, _, _ := strings.Cut(rest, " ")

	return zone
}

// Next is the first run time after t, zero when there is none.
func (s *Schedule) Next(t time.Time) time.Time {
	return s.spec.Next(t)
}

// Prev is the last run time before t, zero when there is none.
func (s *Schedule) Prev(t time.Time) time.Time {
	return s.spec.Prev(t)
}

// NextAfter is the next run time after now, following on from the times
// just run: an @every schedule keeps its phase (last + d) instead of moving
// to now + d when the scheduler was late.
func (s *Schedule) NextAfter(ran []time.Time, now time.Time) time.Time {
	d := s.spec.ConstantDelaySchedule
	if d <= 0 || len(ran) == 0 {
		return s.spec.Next(now)
	}

	next := ran[len(ran)-1].Add(d)
	if !next.After(now) {
		next = next.Add((now.Sub(next)/d + 1) * d)
	}

	return next
}

// Missed returns the run times from first up to now, at most limit of them,
// the latest ones. It walks back from now, so a long pause costs no more
// than limit steps.
func (s *Schedule) Missed(first, now time.Time, limit int) []time.Time {
	if first.IsZero() || first.After(now) || limit <= 0 {
		return nil
	}

	// The usual case: only first is due.
	if next := s.spec.Next(first); next.IsZero() || next.After(now) {
		return []time.Time{first}
	}

	latest := s.latest(first, now)

	var out []time.Time

	for t := latest; !t.IsZero() && !t.Before(first) && len(out) < limit; t = s.spec.Prev(t) {
		out = append(out, t)
	}

	if len(out) == 0 {
		return []time.Time{first}
	}

	slices.Reverse(out)

	return out
}

// latest is the last run time at or before now, counting from first.
func (s *Schedule) latest(first, now time.Time) time.Time {
	// @every runs at first + n*d.
	if d := s.spec.ConstantDelaySchedule; d > 0 {
		return first.Add(now.Sub(first) / d * d)
	}

	latest := s.spec.Prev(now.Add(time.Second))
	for latest.After(now) {
		latest = s.spec.Prev(latest)
	}

	return latest
}

// NextN lists up to n run times after t.
func (s *Schedule) NextN(t time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for range n {
		t = s.spec.Next(t)
		if t.IsZero() {
			break
		}

		out = append(out, t)
	}

	return out
}
