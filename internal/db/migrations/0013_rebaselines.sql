-- How many times a rollout row's soak was started over because the control
-- plane restarted the bot. A tenant that keeps restarting its bot must not be
-- able to hide a crash loop behind the restarts, so the updater halts after a
-- few.
ALTER TABLE rollout_tenants ADD COLUMN rebaselines integer NOT NULL DEFAULT 0;
