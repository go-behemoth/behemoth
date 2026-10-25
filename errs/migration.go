package errs

// Migration error codes: the Code of every *DomainError in CategoryMigration
// (see NewMigrationError). Callers branch on them with IsCode, so each value
// is part of the public contract — rename a constant freely, but never change
// its string.
const (
	// ---- Planning & generation ----

	// Nothing differs between the declared schema and the live schema/snapshot.
	CodeMigrationNothingToGenerate = "nothing_to_generate"
	// One or more plan issues have no (non-stale) decision in the resolution draft.
	CodeMigrationUnresolvedIssues = "unresolved_issues"
	// A live column's type has no mapping to a canonical ColumnType.
	CodeMigrationUnmappableColumnType = "unmappable_column_type"
	// A declared table exists live as a view or another non-table object.
	CodeMigrationIncompatibleObject = "incompatible_object"
	// Migrations depend on each other in a cycle.
	CodeMigrationDependencyCycle = "dependency_cycle"
	// The resolution draft file could not be read.
	CodeMigrationDraftReadFailed = "draft_read_failed"
	// The answers a written migration used could not be removed from the
	// resolution draft.
	CodeMigrationDraftWriteFailed = "draft_write_failed"
	// A migration's Up could not be projected onto the snapshot.
	CodeMigrationSnapshotProjectionFailed = "snapshot_projection_failed"

	// ---- Run state (PathManaged) ----

	// A migration file exists on disk that the snapshot doesn't reflect yet.
	CodeMigrationUnappliedPending = "unapplied_migrations_pending"
	// Migration files exist but no ledger table does: no safe starting point.
	CodeMigrationUnappliedNoLedger = "unapplied_migrations_no_ledger"
	// A ledger table exists but the migration folder is empty.
	CodeMigrationLedgerWithoutFiles = "ledger_without_files"
	// More than one migration is pending where at most one is expected.
	CodeMigrationMultipleUnapplied = "multiple_unapplied_migrations"
	// A baseline awaiting confirmation no longer has any live table behind it.
	CodeMigrationBaselineCandidatesVanished = "baseline_candidates_vanished"
	// The driver failed to apply a migration.
	CodeMigrationApplyFailed = "apply_failed"

	// ---- Migration files on disk ----

	CodeMigrationMkdirFailed     = "mkdir_failed"
	CodeMigrationReadDirFailed   = "read_dir_failed"
	CodeMigrationReadFileFailed  = "read_file_failed"
	CodeMigrationWriteFailed     = "write_failed"
	CodeMigrationMarshalFailed   = "marshal_failed"
	CodeMigrationUnmarshalFailed = "unmarshal_failed"

	// ---- Rendering (MigrationRenderer) ----

	CodeMigrationRenderFailed = "render_failed"
	// A MigrationRenderer returned an empty FileExtension.
	CodeMigrationMissingFileExtension = "missing_file_extension"

	// ---- Ledger & snapshot rows ----

	CodeMigrationInvalidLedgerEntry = "invalid_ledger_entry"
	CodeMigrationInvalidSnapshot    = "invalid_snapshot"

	// ---- Introspection ----

	CodeMigrationExistenceCheckFailed = "existence_check_failed"
	CodeMigrationIntrospectionFailed  = "introspection_failed"

	// ---- Driver: schema model support ----

	// No native rendering for a canonical ColumnType.
	CodeMigrationUnsupportedCanonicalType = "unsupported_canonical_type"
	// Column.Default holds a value type that can't be rendered as a literal.
	CodeMigrationUnsupportedDefaultType = "unsupported_default_type"
	// AutoInc on a column the database can't auto-increment.
	CodeMigrationUnsupportedAutoIncrement = "unsupported_autoincrement"
	CodeMigrationInvalidIndex             = "invalid_index"
	CodeMigrationInvalidForeignKey        = "invalid_foreign_key"
	// An operation targets a table that doesn't exist.
	CodeMigrationTableNotFound = "table_not_found"

	// ---- Driver: execution ----

	CodeMigrationConnFailed     = "conn_failed"
	CodeMigrationBeginTxFailed  = "begin_tx_failed"
	CodeMigrationCommitFailed   = "commit_failed"
	CodeMigrationRollbackFailed = "rollback_failed"
	CodeMigrationLockFailed     = "lock_failed"
	CodeMigrationPragmaFailed   = "pragma_failed"
	CodeMigrationExecFailed     = "exec_failed"
	CodeMigrationQueryFailed    = "query_failed"
	CodeMigrationScanFailed     = "scan_failed"
	// Committing would leave rows that violate a foreign key.
	CodeMigrationForeignKeyViolation = "foreign_key_violation"
	// SQLite: a table's stored CREATE TABLE statement couldn't be parsed or edited for a rebuild.
	CodeMigrationParseFailed   = "parse_failed"
	CodeMigrationRebuildFailed = "rebuild_failed"
)
