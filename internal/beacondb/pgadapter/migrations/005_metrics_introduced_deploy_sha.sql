ALTER TABLE beacon_metrics
  ADD COLUMN IF NOT EXISTS introduced_deploy_sha VARCHAR(64);
