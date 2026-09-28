-- 0024: chat turn cancellation (S-175).
--
-- Widens the messages status CHECK to admit 'cancelled': the chat UI's
-- stop button marks the newest live user turn cancelled; the agent runtime
-- polls the status and abandons the turn without posting a reply.
ALTER TABLE messages
    DROP CONSTRAINT IF EXISTS messages_status_check;

ALTER TABLE messages
    ADD CONSTRAINT messages_status_check
    CHECK (status IN ('pending','delivered','expired','dead','cancelled'));
