-- Persistent discriminator for product-owned task capability contracts.
-- Existing rows are ordinary tasks and keep the empty default. The one
-- currently supported nonempty policy is the bounded synthetic-agent
-- calibration lane; ordinary task creation never writes it.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS execution_policy TEXT NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'tasks'::regclass
          AND conname = 'ck_tasks_execution_policy'
    ) THEN
        ALTER TABLE tasks
            ADD CONSTRAINT ck_tasks_execution_policy
            CHECK (execution_policy IN ('', 'synthetic_calibration_v1'));
    END IF;
END $$;

-- A product-owned calibration task is singleton while it is live. The
-- dedicated constructor also takes an advisory lock, while this partial unique
-- index supplies database-level protection against an accidental future caller.
CREATE UNIQUE INDEX IF NOT EXISTS uq_tasks_synthetic_agent_calibration_live
    ON tasks (execution_policy)
    WHERE execution_policy = 'synthetic_calibration_v1' AND deleted_at IS NULL;
