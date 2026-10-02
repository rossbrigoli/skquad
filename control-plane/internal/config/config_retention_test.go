package config

// S-198: notification retention configuration knobs.
//   SKQUAD_NOTIFICATION_SWEEP_INTERVAL_SECONDS (default 3600 = 1 h)
//   SKQUAD_NOTIFICATION_RETENTION_DAYS         (default 90; age past
//                                             which READ notifications
//                                             are purged)

import (
	"testing"
	"time"
)

func TestNotificationRetentionDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NotificationSweepInterval != time.Hour {
		t.Fatalf("NotificationSweepInterval = %s, want 1h", cfg.NotificationSweepInterval)
	}
	if cfg.NotificationRetention != 90*24*time.Hour {
		t.Fatalf("NotificationRetention = %s, want 2160h (90d)", cfg.NotificationRetention)
	}
}

func TestNotificationRetentionEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("SKQUAD_NOTIFICATION_SWEEP_INTERVAL_SECONDS", "120")
	t.Setenv("SKQUAD_NOTIFICATION_RETENTION_DAYS", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NotificationSweepInterval != 120*time.Second {
		t.Fatalf("NotificationSweepInterval = %s, want 120s", cfg.NotificationSweepInterval)
	}
	if cfg.NotificationRetention != 7*24*time.Hour {
		t.Fatalf("NotificationRetention = %s, want 168h (7d)", cfg.NotificationRetention)
	}
}

func TestNotificationRetentionInvalidEnvFallsBack(t *testing.T) {
	clearEnv(t)
	t.Setenv("SKQUAD_NOTIFICATION_RETENTION_DAYS", "-5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NotificationRetention != 90*24*time.Hour {
		t.Fatalf("NotificationRetention = %s, want default 2160h (90d) on invalid input", cfg.NotificationRetention)
	}
}
