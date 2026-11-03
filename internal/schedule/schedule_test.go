package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr, tz string) *Schedule {
	t.Helper()

	s, err := Parse(expr, tz)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", expr, tz, err)
	}

	return s
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ expr, tz string }{
		{"", "UTC"},
		{"* * *", "UTC"},
		{"61 * * * *", "UTC"},
		{"0 0 * * *", "Mars/Olympus"},
		{"@every -1m", "UTC"},
	} {
		if _, err := Parse(tc.expr, tc.tz); err == nil {
			t.Errorf("Parse(%q, %q) succeeded, want error", tc.expr, tc.tz)
		}
	}
}

func TestNextTimezone(t *testing.T) {
	s := mustParse(t, "0 7 * * *", "Europe/Istanbul")

	got := s.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	want := time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC)

	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got.UTC(), want)
	}

	// A CRON_TZ prefix wins over the time zone field.
	s = mustParse(t, "CRON_TZ=UTC 0 7 * * *", "Europe/Istanbul")
	if got := s.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); got.UTC().Hour() != 7 {
		t.Fatalf("Next with CRON_TZ = %s, want 07:00 UTC", got.UTC())
	}
}

func TestNextN(t *testing.T) {
	s := mustParse(t, "*/15 * * * *", "")

	got := s.NextN(time.Date(2026, 1, 1, 10, 7, 0, 0, time.UTC), 3)
	if len(got) != 3 {
		t.Fatalf("NextN returned %d times", len(got))
	}

	for i, m := range []int{15, 30, 45} {
		if got[i].Minute() != m {
			t.Errorf("time %d = %s, want minute %d", i, got[i], m)
		}
	}
}

func TestMissed(t *testing.T) {
	s := mustParse(t, "* * * * *", "")
	first := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	if got := s.Missed(first, first.Add(-time.Second), 10); got != nil {
		t.Fatalf("not due yet: got %v", got)
	}

	if got := s.Missed(first, first.Add(30*time.Second), 10); len(got) != 1 || !got[0].Equal(first) {
		t.Fatalf("only first due: got %v", got)
	}

	now := first.Add(5*time.Minute + 30*time.Second)

	got := s.Missed(first, now, 100)
	if len(got) != 6 || !got[0].Equal(first) || !got[5].Equal(first.Add(5*time.Minute)) {
		t.Fatalf("all missed: got %v", got)
	}

	got = s.Missed(first, now, 2)
	if len(got) != 2 || !got[1].Equal(first.Add(5*time.Minute)) || !got[0].Equal(first.Add(4*time.Minute)) {
		t.Fatalf("limited to the latest: got %v", got)
	}

	// A long pause of a fast schedule stays cheap.
	fast := mustParse(t, "@every 1s", "")

	got = fast.Missed(first, first.Add(365*24*time.Hour), 1)
	if len(got) != 1 {
		t.Fatalf("fast schedule: got %d times", len(got))
	}

	// @every stays on the grid of first.
	every := mustParse(t, "@every 10m", "")
	start := time.Date(2026, 1, 1, 10, 3, 0, 0, time.UTC)

	got = every.Missed(start, start.Add(35*time.Minute), 10)
	if len(got) != 4 || !got[0].Equal(start) || !got[3].Equal(start.Add(30*time.Minute)) {
		t.Fatalf("@every: got %v", got)
	}

	// After a late tick the next time stays on the grid.
	if next := every.NextAfter(got, start.Add(35*time.Minute)); !next.Equal(start.Add(40 * time.Minute)) {
		t.Fatalf("@every next: got %v", next)
	}

	if next := every.NextAfter(got[:1], start.Add(25*time.Minute+time.Second)); !next.Equal(start.Add(30 * time.Minute)) {
		t.Fatalf("@every next after a long gap: got %v", next)
	}
}
