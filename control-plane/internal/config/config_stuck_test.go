package config

// S-197: stuck-scanner configuration knobs.
//   SKQUAD_STUCK_SCAN_INTERVAL_SECONDS  (default 300 = 5 min)
//   SKQUAD_TASK_STUCK_THRESHOLD_SECONDS (default 86400 = 24 h; also the
//                                       per-task notification dedupe window)

import (
	"testing"
	"time"
)

func TestStuckScannerDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StuckScanInterval != 300*time.Second {
		t.Fatalf("StuckScanInterval = %s, want 300s", cfg.StuckScanInterval)
	}
	if cfg.TaskStuckThreshold != 24*time.Hour {
		t.Fatalf("TaskStuckThreshold = %s, want 24h", cfg.TaskStuckThreshold)
	}
}

func TestStuckScannerEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("SKQUAD_STUCK_SCAN_INTERVAL_SECONDS", "42")
	t.Setenv("SKQUAD_TASK_STUCK_THRESHOLD_SECONDS", "7200")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StuckScanInterval != 42*time.Second {
		t.Fatalf("StuckScanInterval = %s, want 42s", cfg.StuckScanInterval)
	}
	if cfg.TaskStuckThreshold != 2*time.Hour {
		t.Fatalf("TaskStuckThreshold = %s, want 2h", cfg.TaskStuckThreshold)
	}
}
