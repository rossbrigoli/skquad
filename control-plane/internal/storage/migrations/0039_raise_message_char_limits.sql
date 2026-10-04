-- 0039: raise the per-message character limit to 10,000 (S-229).
--
-- The server-side inbox cap (maxInboxMessageChars) moved 2000 -> 10000 and
-- the runtime tool defaults for send_inbox / notify_owner / send_message
-- follow it. The seeded builtin-tool policies must follow too: the DB row
-- overrides the code default at wake time, so without this bump agents keep
-- the old cap. Only values still equal to the old seeded defaults are
-- rewritten, so any admin customisation survives untouched.

-- notify_owner was seeded with the old 2000 cap (0030): bump the seeded default.
UPDATE builtin_tools_config
SET policy = jsonb_set(policy, '{maxMessageChars}', '10000'::jsonb, true)
WHERE name = 'notify_owner'
  AND policy->>'maxMessageChars' = '2000';

-- send_inbox / send_message were seeded without an explicit cap (0023/0029),
-- so the code default applied. Record the new 10,000 default explicitly so
-- the admin UI and the runtime agree on one value. Rows carrying a custom
-- value (anything other than missing / the old defaults) are left alone.
UPDATE builtin_tools_config
SET policy = jsonb_set(policy, '{maxMessageChars}', '10000'::jsonb, true)
WHERE name IN ('send_inbox', 'send_message')
  AND (policy->>'maxMessageChars' IS NULL
       OR policy->>'maxMessageChars' IN ('2000', '8000'));
