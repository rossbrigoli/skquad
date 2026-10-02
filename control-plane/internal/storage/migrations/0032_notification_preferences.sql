-- 0032: S-199 per-user notification preferences.
-- One row per user that has explicitly changed their notification
-- settings; muted_types lists the NotificationType values the user does
-- NOT want delivered to the bell. Absence of a row means "all types
-- enabled" (the default is deliberately not materialized).
CREATE TABLE IF NOT EXISTS user_notification_preferences (
    user_id     uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    muted_types text[] NOT NULL DEFAULT '{}',
    updated_at  timestamptz NOT NULL DEFAULT now()
);
