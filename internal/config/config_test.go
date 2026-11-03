package config

import "testing"

func TestSecretsFromEnv(t *testing.T) {
	got := secretsFromEnv([]string{
		"KRONIK_SECRET_API_TOKEN=abc=def",
		"KRONIK_SECRET_=ignored",
		"KRONIK_SECRETX=ignored",
		"OTHER=1",
	}, map[string]string{"FROM_FILE": "f", "API_TOKEN": "old"})

	want := map[string]string{"API_TOKEN": "abc=def", "FROM_FILE": "f"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}

	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}
