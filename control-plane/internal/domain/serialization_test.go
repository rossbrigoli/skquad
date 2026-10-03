package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// S-209 retest regression: time.Time + omitempty does NOT omit the zero
// value, so unread notifications serialized "read_at":"0001-01-01T00:00:00Z".
// The frontend treats any read_at string as read, hiding the unread badge
// and "Mark all read". ReadAt is now *time.Time so nil is omitted.

func TestNotificationReadAtOmittedWhenUnread(t *testing.T) {
	t.Parallel()

	n := Notification{
		ID:        "n1",
		UserID:  "u1",
		SquadID: "s1",
		Type:    NotificationTaskFailed,
		Message: "hello",
	}
	if n.IsRead() {
		t.Fatal("unset ReadAt must report unread")
	}
	raw, err := json.Marshal(&n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "read_at") {
		t.Fatalf("unread notification must not serialize read_at, got %s", raw)
	}
}

func TestNotificationReadAtPresentWhenRead(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	n := Notification{ID: "n2", UserID: "u1", SquadID: "s1", ReadAt: &now}
	if !n.IsRead() {
		t.Fatal("set ReadAt must report read")
	}
	raw, err := json.Marshal(&n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"read_at":"`+now.Format(time.RFC3339)+`"`) {
		t.Fatalf("read notification must serialize read_at, got %s", raw)
	}
}

func TestInboxMessageReadAtOmittedWhenUnread(t *testing.T) {
	t.Parallel()

	m := InboxMessage{ID: "m1", SquadID: "s1", UserID: "u1", Message: "hi"}
	if m.IsRead() {
		t.Fatal("unset ReadAt must report unread")
	}
	raw, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "read_at") {
		t.Fatalf("unread inbox message must not serialize read_at, got %s", raw)
	}
}

func TestInboxMessageReadAtPresentWhenRead(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	m := InboxMessage{ID: "m2", SquadID: "s1", UserID: "u1", ReadAt: &now}
	if !m.IsRead() {
		t.Fatal("set ReadAt must report read")
	}
	raw, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"read_at":"`+now.Format(time.RFC3339)+`"`) {
		t.Fatalf("read inbox message must serialize read_at, got %s", raw)
	}
}
