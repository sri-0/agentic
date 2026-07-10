package config

import "testing"

func TestExpandEnv(t *testing.T) {
	t.Setenv("FOO", "bar")
	t.Setenv("TOKEN", "abc123")
	cases := []struct{ in, want string }{
		{"Bearer ${TOKEN}", "Bearer abc123"},
		{"${FOO}", "bar"},
		{"${FOO}-${TOKEN}", "bar-abc123"},
		{"no vars", "no vars"},
		{"${UNSET_VAR}", ""}, // undefined → empty
		{"$FOO", "$FOO"},     // only ${...} form expands
	}
	for _, c := range cases {
		if got := ExpandEnv(c.in); got != c.want {
			t.Errorf("ExpandEnv(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestExpandedHeaders(t *testing.T) {
	t.Setenv("K", "v")
	sc := MCPServerConfig{Headers: map[string]string{
		"Authorization": "Bearer ${K}",
		"X-Static":      "plain",
	}}
	h := sc.ExpandedHeaders()
	if h["Authorization"] != "Bearer v" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	if h["X-Static"] != "plain" {
		t.Errorf("X-Static = %q", h["X-Static"])
	}

	// No headers → nil.
	if (MCPServerConfig{}).ExpandedHeaders() != nil {
		t.Error("expected nil for no headers")
	}
}
