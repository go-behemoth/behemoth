// Package mysql is behemoth's MySQL integration, in its own module so the core
// module does not depend on the MySQL driver:
//
//   - MySQLAdapter implements behemoth.Database (application reads/writes).
//   - MySQLDriver implements the migration interfaces: migration.SchemaDriver,
//     migration.MigrationRenderer, migration.SchemaIntrospector and migration.ColumnNormalizer.
//   - MigrationBackend returns both as one migration.Backend, for
//     migration.Generate, migration.Migrate and the behemoth command line.
//
// Both take the same behemoth.SchemaResolver, so application queries and
// migrations agree on physical table and column names.
//
// The adapter converts what the driver returns into the Go types the models
// read (adapters.ScanMySQLRow), so the DSN does not need parseTime. A signed TINYINT column
// is read as a bool.
//
// The migration driver works with any database/sql MySQL driver. The adapter
// classifies errors using github.com/go-sql-driver/mysql's error type, which
// is what makes this module depend on it.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/storage/adapters"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/utils"
	"github.com/go-sql-driver/mysql"
)

// MySQLAdapter implements the behemoth.Database interface for MySQL.
//
// Models are addressed by canonical names; every table and column name in the
// generated SQL goes through Resolver first. A nil Resolver maps every name to
// itself.
type MySQLAdapter struct {
	DB       adapters.Querier
	Resolver behemoth.SchemaResolver

	// Logger receives the text of every statement at Debug. nil = none.
	// Set it with WithLogger.
	Logger telemetry.Logger
}

// NewMySQLAdapter wraps db (a *sql.DB, or a *sql.Tx). resolver maps canonical
// names to physical ones; nil uses them as-is.
func NewMySQLAdapter(db adapters.Querier, resolver behemoth.SchemaResolver) *MySQLAdapter {
	return &MySQLAdapter{DB: db, Resolver: resolver}
}

// WithLogger makes the adapter write the text of every statement it runs to
// logger at Debug, and returns the adapter. Argument values are never
// logged: they hold password hashes and token hashes. Adapters bound to a
// transaction (see Transaction) log to the same logger.
func (my *MySQLAdapter) WithLogger(logger telemetry.Logger) *MySQLAdapter {
	my.Logger = logger
	return my
}

// q is the connection statements run on: DB, logging each statement when a
// Logger is set.
func (my *MySQLAdapter) q() adapters.Querier {
	return adapters.LogQueries(my.DB, my.Logger, "storage.mysql")
}

func (my *MySQLAdapter) names() behemoth.SchemaResolver {
	return adapters.ResolverOrIdentity(my.Resolver)
}

// where renders expr with its fields resolved to physical columns.
func (my *MySQLAdapter) where(m behemoth.Model, expr *clause.Expression) (string, []any) {
	return adapters.BuildSQLWhereClause(adapters.PhysicalExpression(my.names(), m, expr), adapters.DefaultClauseOption)
}

// keys tells which unique key a duplicate key error broke. A duplicate key
// leaves a MySQL transaction usable, so the catalog lookup runs on the
// adapter's own connection.
func (my *MySQLAdapter) keys() adapters.KeyFinder {
	return adapters.KeyFinder{Dialect: adapters.DialectMySQL, Resolver: my.names(), Catalog: adapters.CatalogOn(my.DB)}
}

// mapErrors classifies MySQL server errors by error number
// (https://dev.mysql.com/doc/mysql-errors/8.0/en/server-error-reference.html).
// A duplicate entry names the columns of its key.
func (my *MySQLAdapter) mapErrors(op, entity string, err error) error {
	if classified, ok := adapters.MapStdSQLErrors(op, entity, err); ok {
		return classified
	}

	myErr, ok := errors.AsType[*mysql.MySQLError](err)
	if !ok {
		return adapters.Classify(op, entity, adapters.SentinelUnknown, err)
	}
	switch myErr.Number {
	case 1062: // ER_DUP_ENTRY
		return my.keys().DuplicateKey(op, entity, err)
	case 1451, 1452, 1216, 1217: // ER_ROW_IS_REFERENCED_2, ER_NO_REFERENCED_ROW_2, and their pre-5.1 forms
		return adapters.Classify(op, entity, adapters.SentinelForeignKey, err)
	case 1146: // ER_NO_SUCH_TABLE
		return adapters.Classify(op, entity, adapters.SentinelUndefinedTable, err)
	case 1048, 3819: // ER_BAD_NULL_ERROR, ER_CHECK_CONSTRAINT_VIOLATED
		return adapters.Classify(op, entity, adapters.SentinelConstraintViolation, err)
	default:
		return adapters.Classify(op, entity, adapters.SentinelUnknown, err)
	}
}

// generateMySQLPlaceholders returns a VALUES clause with n '?' placeholders: (?, ?, ?)
func generateMySQLPlaceholders(n int) string {
	placeholders := make([]string, n)
	for i := range placeholders {
		placeholders[i] = "?"
	}
	return "(" + strings.Join(placeholders, ", ") + ")"
}

// generateMySQLSETClause returns "col1 = ?, col2 = ?, ..." for an UPDATE SET clause.
func generateMySQLSETClause(columns []string) string {
	parts := make([]string, len(columns))
	for i, col := range columns {
		parts[i] = fmt.Sprintf("%s = ?", col)
	}
	return strings.Join(parts, ", ")
}

func (my *MySQLAdapter) Create(ctx context.Context, m behemoth.Model) error {
	if _, ok := m.(behemoth.Serializable); !ok {
		return errs.SerializableNotImplemented()
	}

	columns, values, _ := models.GenerateColumnValuePairs(m)
	placeholders := generateMySQLPlaceholders(len(columns))

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES %s",
		adapters.PhysicalTable(my.names(), m),
		strings.Join(adapters.PhysicalColumns(my.names(), m, columns), ", "),
		placeholders,
	)

	_, err := my.q().ExecContext(ctx, query, values...)
	return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
}

func (my *MySQLAdapter) FindOne(
	ctx context.Context,
	m behemoth.Model,
	whereExpression clause.Expression,
) (behemoth.Model, error) {
	if _, ok := m.(behemoth.Serializable); !ok {
		return nil, errs.SerializableNotImplemented()
	}

	// columns stay canonical: they key the map handed to FromMap.
	columns := adapters.ReadColumns(my.names(), m, nil)

	query := fmt.Sprintf(
		"SELECT %s FROM %s",
		strings.Join(adapters.PhysicalColumns(my.names(), m, columns), ", "),
		adapters.PhysicalTable(my.names(), m),
	)
	whereClause, args := my.where(m, &whereExpression)
	if whereClause != "" {
		query += " WHERE " + whereClause
	}
	query += " LIMIT 1"

	rows, err := my.q().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
		}
		return nil, adapters.WrapWithCaller(sql.ErrNoRows, m.SchemaName(), my.mapErrors)
	}
	values, err := adapters.ScanMySQLRow(rows, types)
	if err != nil {
		return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}

	return models.GenerateModelFromRows(m, columns, values)

}

func (my *MySQLAdapter) FindMany(
	ctx context.Context,
	m behemoth.Model,
	whereExpression clause.Expression,
	options *behemoth.QueryOptions,
) ([]behemoth.Model, error) {
	if _, ok := m.(behemoth.Serializable); !ok {
		return nil, errs.SerializableNotImplemented()
	}

	var (
		columns        []string
		distinctClause string
		query          string
	)

	// columns stay canonical: they key the map handed to FromMap.
	var selected []string
	if options != nil {
		selected = options.Select
	}
	columns = adapters.ReadColumns(my.names(), m, selected)

	if options != nil && options.Distinct {
		distinctClause = "DISTINCT "
	}

	whereClause, args := my.where(m, &whereExpression)

	query = fmt.Sprintf(
		"SELECT %s%s FROM %s",
		distinctClause,
		strings.Join(adapters.PhysicalColumns(my.names(), m, columns), ", "),
		adapters.PhysicalTable(my.names(), m),
	)
	if whereClause != "" {
		query += " WHERE " + whereClause
	}

	if options != nil {
		if options.OrderBy.Field != "" {
			query += fmt.Sprintf(" ORDER BY %s %s", adapters.PhysicalColumn(my.names(), m, options.OrderBy.Field), options.OrderBy.Direction)
		}
		// MySQL supports LIMIT / OFFSET in the same way as SQLite.
		if options.Limit != 0 {
			query += fmt.Sprintf(" LIMIT %d", options.Limit)
		}
		if options.Offset != 0 {
			query += fmt.Sprintf(" OFFSET %d", options.Offset)
		}
	}

	rows, err := my.q().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}

	var results []behemoth.Model
	for rows.Next() {
		values, err := adapters.ScanMySQLRow(rows, types)
		if err != nil {
			return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
		}
		result, err := models.GenerateModelFromRows(m, columns, values)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}

	return results, nil

}

func (my *MySQLAdapter) Update(ctx context.Context, m behemoth.Model) error {
	if _, ok := m.(behemoth.Serializable); !ok {
		return errs.SerializableNotImplemented()
	}

	columns, values, _ := models.GenerateColumnValuePairs(m)

	query := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s = ?",
		adapters.PhysicalTable(my.names(), m),
		generateMySQLSETClause(adapters.PhysicalColumns(my.names(), m, columns)),
		adapters.PhysicalColumn(my.names(), m, m.PrimaryKeyName()),
	)

	// MySQL reports changed rows unless the connection sets clientFoundRows.
	res, err := my.q().ExecContext(ctx, query, append(values, m.PrimaryKeyField())...)
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	return adapters.ExpectOneRow("Update", m, n, func() (int64, error) { return my.count(ctx, m, adapters.ByPrimaryKey(m), true) })
}

func (my *MySQLAdapter) UpdateOne(
	ctx context.Context,
	m behemoth.Model,
	expr clause.Expression,
	updates behemoth.M,
) error {
	if len(updates) == 0 {
		return nil
	}

	columns, values := utils.MapToSlice(updates)
	whereClause, whereArgs := my.where(m, &expr)

	// LIMIT 1 picks the one row, and expr is checked on the row as written
	// (the UpdateOne convention). The statement asks for the row's exclusive
	// lock directly, so concurrent updates of one row run one after another.
	// Selecting the row's key in a subquery first would take a shared lock
	// that two statements can both hold while each waits to upgrade it, which
	// MySQL ends with a deadlock error.
	query := fmt.Sprintf(
		"UPDATE %s SET %s",
		adapters.PhysicalTable(my.names(), m),
		generateMySQLSETClause(adapters.PhysicalColumns(my.names(), m, columns)),
	)
	if whereClause != "" {
		query += " WHERE " + whereClause
	}
	query += " LIMIT 1"
	args := append(values, whereArgs...)

	res, err := my.q().ExecContext(ctx, query, args...)
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	// MySQL reports changed rows unless the connection sets clientFoundRows.
	return adapters.ExpectOneRow("UpdateOne", m, n, func() (int64, error) { return my.count(ctx, m, expr, true) })
}

func (my *MySQLAdapter) UpdateMany(
	ctx context.Context,
	m behemoth.Model,
	expr clause.Expression,
	updates behemoth.M,
) error {
	if len(updates) == 0 {
		return nil
	}

	columns, values := utils.MapToSlice(updates)
	whereClause, whereArgs := my.where(m, &expr)

	query := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s",
		adapters.PhysicalTable(my.names(), m),
		generateMySQLSETClause(adapters.PhysicalColumns(my.names(), m, columns)),
		whereClause,
	)

	_, err := my.q().ExecContext(ctx, query, append(values, whereArgs...)...)
	return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)

}

func (my *MySQLAdapter) Delete(ctx context.Context, m behemoth.Model) error {
	query := fmt.Sprintf(
		"DELETE FROM %s WHERE %s = ?",
		adapters.PhysicalTable(my.names(), m),
		adapters.PhysicalColumn(my.names(), m, m.PrimaryKeyName()),
	)
	res, err := my.q().ExecContext(ctx, query, m.PrimaryKeyField())
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	return adapters.ExpectOneRow("Delete", m, n, nil)
}

func (my *MySQLAdapter) DeleteOne(ctx context.Context, m behemoth.Model, expr clause.Expression) error {
	whereClause, args := my.where(m, &expr)
	if whereClause == "" {
		return errs.NewValidationError(adapters.OpDeleteOne, "clause", nil)
	}

	// LIMIT 1 and no subquery, for the reason given in UpdateOne.
	query := fmt.Sprintf(
		"DELETE FROM %s WHERE %s LIMIT 1",
		adapters.PhysicalTable(my.names(), m),
		whereClause,
	)
	res, err := my.q().ExecContext(ctx, query, args...)
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	return adapters.ExpectOneRow("DeleteOne", m, n, nil)
}

func (my *MySQLAdapter) DeleteMany(ctx context.Context, m behemoth.Model, expr clause.Expression) error {
	whereClause, args := my.where(m, &expr)
	if whereClause == "" {
		return errs.NewValidationError(adapters.OpDeleteMany, "clause", nil)
	}

	query := fmt.Sprintf(
		"DELETE FROM %s WHERE %s",
		adapters.PhysicalTable(my.names(), m),
		whereClause,
	)

	_, err := my.q().ExecContext(ctx, query, args...)
	return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
}

func (my *MySQLAdapter) DeleteAll(ctx context.Context, m behemoth.Model) error {
	query := fmt.Sprintf("DELETE FROM %s", adapters.PhysicalTable(my.names(), m))
	_, err := my.q().ExecContext(ctx, query)
	return adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
}

func (my *MySQLAdapter) Count(ctx context.Context, m behemoth.Model, expr clause.Expression) (int64, error) {
	return my.count(ctx, m, expr, false)
}

// count is Count, with current set for the re-count after an update that
// reported no changed rows. Inside a transaction a plain SELECT reads the
// transaction's snapshot, which can still show a row that another transaction
// has since changed: a writer that lost a guarded update would count its
// guard as matching and report success. FOR SHARE reads the committed row
// instead, as the UPDATE itself did.
func (my *MySQLAdapter) count(ctx context.Context, m behemoth.Model, expr clause.Expression, current bool) (int64, error) {
	whereClause, args := my.where(m, &expr)

	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM %s",
		adapters.PhysicalTable(my.names(), m),
	)
	if whereClause != "" {
		query += " WHERE " + whereClause
	}
	if current {
		query += " FOR SHARE"
	}

	row, err := my.q().QueryContext(ctx, query, args...)
	if err != nil {
		return 0, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
	}
	defer row.Close()

	var count int64
	if row.Next() {
		if err := row.Scan(&count); err != nil {
			return 0, adapters.WrapWithCaller(err, m.SchemaName(), my.mapErrors)
		}
	}

	return count, nil
}

// Transaction runs fn with an adapter bound to one database transaction. fn's
// error rolls the transaction back and is returned as-is.
//
// Called on an adapter that is already bound to a transaction (the one
// Transaction hands to fn), it runs fn in that same transaction: nothing is
// begun, committed or rolled back here, and fn's error is returned for the
// outer call to roll back on. Code that is handed an adapter, such as a hook
// handler, can therefore call Transaction without knowing whether one is open.
func (my *MySQLAdapter) Transaction(ctx context.Context, fn behemoth.TransactionFunc) error {
	var db *sql.DB
	switch q := my.DB.(type) {
	case *sql.DB:
		db = q
	case *sql.Tx:
		_, err := fn(ctx, my)
		return err
	default:
		return errs.NewTransactionError("Transaction",
			fmt.Errorf("cannot begin a transaction on a %T; the adapter needs a *sql.DB", my.DB))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()

	txAdapter := NewMySQLAdapter(tx, my.Resolver).WithLogger(my.Logger)
	_, err = fn(ctx, txAdapter)

	if err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return errs.NewTransactionError("Transaction", rollbackErr)
		}
		return err
	}

	return tx.Commit()

}
