ALTER TABLE repos ADD COLUMN min_coverage      DOUBLE PRECISION;
ALTER TABLE repos ADD COLUMN min_diff_coverage DOUBLE PRECISION;
ALTER TABLE repos ADD COLUMN max_coverage_drop DOUBLE PRECISION;

ALTER TABLE workspaces ADD COLUMN min_coverage      DOUBLE PRECISION;
ALTER TABLE workspaces ADD COLUMN min_diff_coverage DOUBLE PRECISION;
ALTER TABLE workspaces ADD COLUMN max_coverage_drop DOUBLE PRECISION;

-- Gate-failing uploads are recorded but never serve as comparison
-- baselines, otherwise re-running CI would launder a failed gate.
ALTER TABLE uploads ADD COLUMN gate_failed BOOLEAN NOT NULL DEFAULT false;
