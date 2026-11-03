package settings

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDurations(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0"}, {30 * 24 * time.Hour, "30d"}, {10 * time.Minute, "10m"}, {90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"}, {45 * time.Second, "45s"},
	} {
		if got := FormatDuration(tc.in); got != tc.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}

		back, err := ParseDuration(tc.want)
		if err != nil || back != tc.in {
			t.Errorf("ParseDuration(%q) = %s, %v", tc.want, back, err)
		}
	}
}

func TestParse(t *testing.T) {
	ok := map[string]string{
		"retention.runs":         `"30d"`,
		"retention.runs_per_job": `200`,
		"retention.at":           `"03:00"`,
		"executor.docker":        `true`,
		"docker.allowed_images":  `[" alpine:* ", "alpine:*", ""]`,
		"docker.pull":            `"always"`,
		"environment":            `" prod "`,
	}

	for k, raw := range ok {
		d, found := Find(k)
		if !found {
			t.Fatalf("no setting %s", k)
		}

		if _, err := d.Parse(json.RawMessage(raw)); err != nil {
			t.Errorf("%s=%s: %v", k, raw, err)
		}
	}

	d, _ := Find("docker.allowed_images")
	if out, _ := d.Parse(json.RawMessage(`[" alpine:* ", "alpine:*", ""]`)); string(out) != `["alpine:*"]` {
		t.Errorf("list normalized to %s", out)
	}

	bad := map[string]string{
		"retention.runs":         `"forever"`,
		"retention.runs_per_job": `-1`,
		"retention.at":           `"25:00"`,
		"executor.docker":        `"yes"`,
		"docker.pull":            `"sometimes"`,
		"environment":            `"a\nb"`,
	}

	for k, raw := range bad {
		d, _ := Find(k)
		if _, err := d.Parse(json.RawMessage(raw)); err == nil {
			t.Errorf("%s=%s accepted", k, raw)
		}
	}
}

func TestEveryDefHasAField(t *testing.T) {
	var v Values

	fields := v.fields()
	for _, d := range Defs {
		if _, ok := fields[d.Key]; !ok {
			t.Errorf("setting %s has no field", d.Key)
		}
	}

	if len(fields) != len(Defs) {
		t.Errorf("%d fields for %d settings", len(fields), len(Defs))
	}
}
