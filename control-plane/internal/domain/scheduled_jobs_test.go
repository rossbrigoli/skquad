package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func mustSpec(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return raw
}

func TestComputeNextRunAt(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)

	t.Run("future at returns the fixed instant in UTC", func(t *testing.T) {
		spec := mustSpec(t, AtSpec{At: "2026-10-21T19:30:00+09:30"})
		got, err := ComputeNextRun(ScheduleAt, spec, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC) // 19:30 +09:30
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("past at is rejected", func(t *testing.T) {
		spec := mustSpec(t, AtSpec{At: "2026-10-21T07:00:00Z"})
		if _, err := ComputeNextRun(ScheduleAt, spec, now); err == nil {
			t.Fatal("expected error for past at")
		}
	})

	t.Run("malformed at is rejected", func(t *testing.T) {
		spec := mustSpec(t, AtSpec{At: "tomorrow-ish"})
		if _, err := ComputeNextRun(ScheduleAt, spec, now); err == nil {
			t.Fatal("expected error for malformed at")
		}
	})
}

func TestComputeNextRunEvery(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)

	t.Run("now plus interval", func(t *testing.T) {
		spec := mustSpec(t, EverySpec{IntervalSeconds: 3600})
		got, err := ComputeNextRun(ScheduleEvery, spec, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := now.Add(time.Hour); !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("below minimum interval rejected", func(t *testing.T) {
		spec := mustSpec(t, EverySpec{IntervalSeconds: MinEverySeconds - 1})
		if _, err := ComputeNextRun(ScheduleEvery, spec, now); err == nil {
			t.Fatal("expected error for sub-minimum interval")
		}
	})

	t.Run("above maximum interval rejected", func(t *testing.T) {
		spec := mustSpec(t, EverySpec{IntervalSeconds: MaxEverySeconds + 1})
		if _, err := ComputeNextRun(ScheduleEvery, spec, now); err == nil {
			t.Fatal("expected error for over-maximum interval")
		}
	})
}

func TestComputeNextRunCron(t *testing.T) {
	t.Run("daily 09:00 UTC before the hour", func(t *testing.T) {
		after := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
		spec := mustSpec(t, CronSpec{Expression: "0 9 * * *"})
		got, err := ComputeNextRun(ScheduleCron, spec, after)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("daily 09:00 UTC after the hour rolls to tomorrow", func(t *testing.T) {
		after := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
		spec := mustSpec(t, CronSpec{Expression: "0 9 * * *"})
		got, err := ComputeNextRun(ScheduleCron, spec, after)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("weekdays only skips the weekend", func(t *testing.T) {
		// Friday 2026-10-23 10:00 UTC -> next weekday match is Monday 2026-10-26 09:00 UTC.
		after := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
		spec := mustSpec(t, CronSpec{Expression: "0 9 * * 1-5"})
		got, err := ComputeNextRun(ScheduleCron, spec, after)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("IANA timezone evaluated on the local wall clock", func(t *testing.T) {
		// 09:00 Australia/Adelaide (ACST, UTC+9:30) = 23:30Z the day before.
		after := time.Date(2026, 6, 1, 22, 0, 0, 0, time.UTC) // 07:30 ACST Jun 2? no: Jun 1 22:00Z = Jun 2 07:30 ACST
		spec := mustSpec(t, CronSpec{Expression: "0 9 * * *", Timezone: "Australia/Adelaide"})
		got, err := ComputeNextRun(ScheduleCron, spec, after)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Next 09:00 ACST after Jun 2 07:30 ACST is Jun 2 09:00 ACST = Jun 1 23:30Z.
		want := time.Date(2026, 6, 1, 23, 30, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("DST spring-forward skips the nonexistent wall-clock time", func(t *testing.T) {
		// Australia/Adelaide DST starts Sun 2026-10-04 02:00 ACST -> 03:00 ACDT.
		// "30 2 * * *" has no match on Oct 4; the next valid fire is
		// Oct 5 02:30 ACDT (UTC+10:30) = 2026-10-04T16:00Z.
		after := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC) // Oct 3 22:30 ACST
		spec := mustSpec(t, CronSpec{Expression: "30 2 * * *", Timezone: "Australia/Adelaide"})
		got, err := ComputeNextRun(ScheduleCron, spec, after)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("DST fall-back takes the first wall-clock occurrence", func(t *testing.T) {
		// Australia/Adelaide DST ends Sun 2026-04-05 03:00 ACDT -> 02:00 ACST.
		// "30 2 * * *" matches twice on Apr 5; the first is 02:30 ACDT
		// (UTC+10:30) = 2026-04-04T16:00Z.
		after := time.Date(2026, 4, 4, 15, 0, 0, 0, time.UTC) // Apr 5 01:30 ACDT
		spec := mustSpec(t, CronSpec{Expression: "30 2 * * *", Timezone: "Australia/Adelaide"})
		got, err := ComputeNextRun(ScheduleCron, spec, after)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 4, 4, 16, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("invalid expression rejected", func(t *testing.T) {
		if _, err := ComputeNextRun(ScheduleCron, mustSpec(t, CronSpec{Expression: "not a cron"}), time.Now()); err == nil {
			t.Fatal("expected error for invalid expression")
		}
	})

	t.Run("invalid timezone rejected", func(t *testing.T) {
		spec := mustSpec(t, CronSpec{Expression: "0 9 * * *", Timezone: "Mars/Olympus_Mons"})
		if _, err := ComputeNextRun(ScheduleCron, spec, time.Now()); err == nil {
			t.Fatal("expected error for invalid timezone")
		}
	})
}

func TestBuildScheduleSpec(t *testing.T) {
	t.Run("at requires a parseable RFC3339", func(t *testing.T) {
		if _, err := BuildScheduleSpec(ScheduleAt, "2026-10-21T19:30:00+09:30", 0, "", ""); err != nil {
			t.Fatalf("valid at rejected: %v", err)
		}
		if _, err := BuildScheduleSpec(ScheduleAt, "", 0, "", ""); err == nil {
			t.Fatal("empty at accepted")
		}
		if _, err := BuildScheduleSpec(ScheduleAt, "later", 0, "", ""); err == nil {
			t.Fatal("unparseable at accepted")
		}
	})

	t.Run("every bounds the interval", func(t *testing.T) {
		if _, err := BuildScheduleSpec(ScheduleEvery, "", 3600, "", ""); err != nil {
			t.Fatalf("valid every rejected: %v", err)
		}
		if _, err := BuildScheduleSpec(ScheduleEvery, "", 5, "", ""); err == nil {
			t.Fatal("sub-minimum interval accepted")
		}
	})

	t.Run("cron validates expression and defaults the timezone to UTC", func(t *testing.T) {
		spec, err := BuildScheduleSpec(ScheduleCron, "", 0, "*/15 * * * *", "")
		if err != nil {
			t.Fatalf("valid cron rejected: %v", err)
		}
		var s CronSpec
		if err := json.Unmarshal(spec, &s); err != nil {
			t.Fatalf("spec unmarshal: %v", err)
		}
		if s.Timezone != DefaultScheduleTimezone {
			t.Fatalf("timezone default = %q, want UTC", s.Timezone)
		}
		if _, err := BuildScheduleSpec(ScheduleCron, "", 0, "99 * * * *", ""); err == nil {
			t.Fatal("out-of-range cron accepted")
		}
		if _, err := BuildScheduleSpec(ScheduleCron, "", 0, "0 9 * * *", "Nowhere/Special"); err == nil {
			t.Fatal("bad timezone accepted")
		}
	})

	t.Run("unknown kind rejected", func(t *testing.T) {
		if _, err := BuildScheduleSpec(ScheduleKind("moonphase"), "", 0, "", ""); err == nil {
			t.Fatal("unknown kind accepted")
		}
	})
}

func TestScheduleKindValid(t *testing.T) {
	for _, k := range []ScheduleKind{ScheduleAt, ScheduleEvery, ScheduleCron} {
		if !k.Valid() {
			t.Fatalf("%q should be valid", k)
		}
	}
	if ScheduleKind("nope").Valid() {
		t.Fatal("bogus kind must be invalid")
	}
}
