package boot

import "testing"

func TestResolveLevelPriority(t *testing.T) {
	cfg := &Config{LogLevel: "debug"}
	if got := resolveLevel(cfg); got != "debug" {
		t.Fatalf("got %q, want debug (log_level wins)", got)
	}
	cfg = &Config{}
	t.Setenv("LOG_LEVEL", "warn")
	if got := resolveLevel(cfg); got != "warn" {
		t.Fatalf("got %q, want warn (LOG_LEVEL wins)", got)
	}
	t.Setenv("LOG_LEVEL", "")
	if got := resolveLevel(cfg); got != "info" {
		t.Fatalf("got %q, want info default", got)
	}
}
