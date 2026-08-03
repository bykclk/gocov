-- The path prefix (e.g. the Go module path) maps module-qualified profile
-- paths to repo-relative paths; the source view needs it to fetch files
-- from the forge.
ALTER TABLE uploads ADD COLUMN path_prefix TEXT NOT NULL DEFAULT '';
