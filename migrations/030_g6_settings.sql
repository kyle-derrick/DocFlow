-- 030: G6 settings keys (upload.version_retention_days, upload.blocked_extensions,
-- folder.max_depth default) are registered by the application on read; existing
-- rows are unaffected. This migration is reserved for deployment ordering.
SELECT 1;
