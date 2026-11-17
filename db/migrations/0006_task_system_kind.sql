-- Persistent discriminator for ARTEX-owned immutable task fixtures. Existing
-- rows are ordinary operator tasks and therefore retain the empty default.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS system_kind TEXT NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'tasks'::regclass
          AND conname = 'ck_tasks_system_kind'
    ) THEN
        ALTER TABLE tasks
            ADD CONSTRAINT ck_tasks_system_kind
            CHECK (system_kind IN ('', 'synthetic_calibration'));
    END IF;
END $$;
