package config

import "testing"

func TestClientFromEnv(t *testing.T) {
	c := ClientFromEnv("axis", "https://herald.example.com")
	if c.Project != "axis" || c.Gateway != "https://herald.example.com" {
		t.Fatalf("got %+v", c)
	}
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv("HERALD_TOKEN", "herald_axis_secret")
	if got := Token(); got != "herald_axis_secret" {
		t.Fatalf("got %q", got)
	}
}
