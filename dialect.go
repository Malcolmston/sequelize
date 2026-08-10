package sequelize

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// identifierPattern is the only shape of identifier the port will emit. Anything
// else is rejected with [ErrInvalidIdentifier] rather than escaped.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateIdentifier reports whether name is a legal SQL identifier for this
// package, returning an error wrapping [ErrInvalidIdentifier] if it is not.
//
// The rule is deliberately strict — ^[A-Za-z_][A-Za-z0-9_]*$ — because the port
// never interpolates a user value into SQL text, and an identifier is the one
// piece of a statement that cannot become a placeholder. A name that fails here
// is a bug or an attack, never something to quote around.
func ValidateIdentifier(name string) error {
	if !identifierPattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidIdentifier, name)
	}
	return nil
}

// Dialect captures everything that differs between database backends: how
// identifiers are quoted, how bind parameters are spelled, how abstract
// [DataType] values map to concrete SQL, how driver errors map to the package's
// typed errors, and how Go values are bound.
//
// Implement it to support a backend the package does not ship, then make it
// available to [New] and [Open] with [Register]. The package ships one
// implementation, [SQLite].
type Dialect interface {
	// Name returns the dialect's short lowercase name, as passed to [New].
	Name() string

	// Quote validates name with [ValidateIdentifier] and returns it quoted for
	// the backend. It must not attempt to escape an invalid identifier.
	Quote(name string) (string, error)

	// Placeholder renders the bind parameter marker for a 1-based argument
	// position, for example "?" or "$1".
	Placeholder(position int) string

	// ColumnType renders the concrete SQL type for an abstract [DataType].
	ColumnType(t DataType) (string, error)

	// AutoIncrementColumn returns the complete column definition for a
	// single-column auto-increment primary key. quoted is the already-quoted
	// column name.
	AutoIncrementColumn(quoted string, t DataType) (string, error)

	// BindValue converts an already portably-encoded value (see the DataType
	// encoding rules) into something the backend's driver accepts.
	BindValue(t DataType, v any) (any, error)

	// TranslateError maps a driver error onto [UniqueConstraintError],
	// [ForeignKeyError] or, when it recognises neither, returns err unchanged.
	TranslateError(err error) error

	// SupportsReturning reports whether INSERT ... RETURNING is available.
	SupportsReturning() bool
}

// The interfaces below are *optional* extensions to [Dialect]. A dialect that
// implements one gains a capability; a dialect that does not keeps working, and
// the package-level Dialect* helpers supply a portable fallback or a clear
// [ErrUnsupported].
//
// They are separate interfaces rather than extra methods on [Dialect] on
// purpose: [Dialect] is the seam every backend and every third-party dialect is
// written against, so growing it would break every existing implementation.
// Callers should always go through the Dialect* helper functions rather than
// type-asserting themselves, so that the fallback lives in one place.

// ColumnTyper is implemented by a [Dialect] whose concrete SQL type for a column
// depends on where the column lives — a PostgreSQL native ENUM, for instance, is
// a named type derived from the table and column name. Prefer
// [DialectColumnType], which falls back to [Dialect.ColumnType].
type ColumnTyper interface {
	// ColumnTypeFor renders the concrete SQL type for t on table.column. Both
	// names must be validated with [ValidateIdentifier].
	ColumnTypeFor(table, column string, t DataType) (string, error)
}

// Returner is implemented by a [Dialect] that spells the clause returning the
// written row differently from the standard " RETURNING a, b". Prefer
// [DialectReturning].
type Returner interface {
	// ReturningClause renders the clause, including its leading space, that makes
	// an INSERT or UPDATE return the given (unquoted) columns.
	ReturningClause(columns []string) (string, error)
}

// Upserter is implemented by a [Dialect] with a native insert-or-update
// conflict clause, such as PostgreSQL's ON CONFLICT ... DO UPDATE or MySQL's
// ON DUPLICATE KEY UPDATE. Prefer [DialectUpsert].
type Upserter interface {
	// UpsertClause renders the clause, including its leading space, that turns an
	// INSERT into an insert-or-update. conflictColumns names the unique columns to
	// match on and updateColumns the columns to overwrite; an empty
	// updateColumns means "leave the existing row alone".
	UpsertClause(conflictColumns, updateColumns []string) (string, error)
}

// OperatorSupporter is implemented by a [Dialect] that wants to declare which
// members of the [Operator] vocabulary it can render. Prefer
// [DialectSupportsOperator], which answers conservatively for dialects that do
// not implement this.
type OperatorSupporter interface {
	// SupportsOperator reports whether the dialect can render op.
	SupportsOperator(op Operator) bool
}

// ColumnDef names one column and its abstract type. It is the minimum a dialect
// needs in order to emit the statements that must surround a CREATE TABLE, and
// deliberately carries no [Attribute] so that dialects do not depend on the
// model layer.
type ColumnDef struct {
	// Column is the physical column name.
	Column string
	// Type is the column's abstract type.
	Type DataType
}

// TableDDLer is implemented by a [Dialect] that needs statements executed
// around a CREATE TABLE or DROP TABLE — PostgreSQL's CREATE TYPE ... AS ENUM
// being the motivating case, since a native enum is a schema object with a
// lifetime of its own. Prefer [DialectBeforeCreateTable] and
// [DialectAfterDropTable], which return nothing for dialects that need nothing.
type TableDDLer interface {
	// BeforeCreateTable returns statements to execute, in order, immediately
	// before the table's CREATE TABLE.
	BeforeCreateTable(table string, columns []ColumnDef) ([]string, error)
	// AfterDropTable returns statements to execute, in order, immediately after
	// the table's DROP TABLE.
	AfterDropTable(table string, columns []ColumnDef) ([]string, error)
}

// DialectColumnType renders the concrete SQL type for t on table.column, using
// d's [ColumnTyper] implementation when it has one and [Dialect.ColumnType]
// otherwise. Callers that genuinely have no column in hand may pass empty
// strings, which selects the plain [Dialect.ColumnType] rendering.
func DialectColumnType(d Dialect, table, column string, t DataType) (string, error) {
	if d == nil {
		return "", fmt.Errorf("%w: nil dialect", ErrUnknownDialect)
	}
	if ct, ok := d.(ColumnTyper); ok && table != "" && column != "" {
		return ct.ColumnTypeFor(table, column, t)
	}
	return d.ColumnType(t)
}

// DialectReturning renders the clause, leading space included, that makes an
// INSERT or UPDATE hand back the given columns — the only way PostgreSQL reports
// a generated key, and a way to avoid a second round trip everywhere it works.
//
// It returns an error wrapping [ErrUnsupported] when d reports no RETURNING
// support, and one wrapping [ErrInvalidQuery] when columns is empty. Column
// names are quoted by d, so an invalid one is [ErrInvalidIdentifier].
func DialectReturning(d Dialect, columns []string) (string, error) {
	if d == nil {
		return "", fmt.Errorf("%w: nil dialect", ErrUnknownDialect)
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("%w: RETURNING needs at least one column", ErrInvalidQuery)
	}
	if r, ok := d.(Returner); ok {
		return r.ReturningClause(columns)
	}
	if !d.SupportsReturning() {
		return "", fmt.Errorf("%w: %s cannot RETURNING", ErrUnsupported, d.Name())
	}
	return standardReturningClause(d, columns)
}

// standardReturningClause renders the SQL-standard spelling, which SQLite,
// PostgreSQL and MariaDB all accept.
func standardReturningClause(d Dialect, columns []string) (string, error) {
	var b strings.Builder
	b.WriteString(" RETURNING ")
	for i, col := range columns {
		if i > 0 {
			b.WriteString(", ")
		}
		quoted, err := d.Quote(col)
		if err != nil {
			return "", err
		}
		b.WriteString(quoted)
	}
	return b.String(), nil
}

// DialectUpsert renders the clause, leading space included, that turns an INSERT
// into an insert-or-update on a conflict over conflictColumns.
//
// There is no portable spelling, so a dialect that does not implement
// [Upserter] returns an error wrapping [ErrUnsupported] and the caller must fall
// back to a select-then-write inside a transaction. An empty conflictColumns is
// [ErrInvalidQuery]; an empty updateColumns asks for "do nothing on conflict",
// which is a legitimate request rather than an error.
func DialectUpsert(d Dialect, conflictColumns, updateColumns []string) (string, error) {
	if d == nil {
		return "", fmt.Errorf("%w: nil dialect", ErrUnknownDialect)
	}
	if len(conflictColumns) == 0 {
		return "", fmt.Errorf("%w: upsert needs at least one conflict column", ErrInvalidQuery)
	}
	u, ok := d.(Upserter)
	if !ok {
		return "", fmt.Errorf("%w: %s has no native upsert clause", ErrUnsupported, d.Name())
	}
	return u.UpsertClause(conflictColumns, updateColumns)
}

// nonPortableOperators lists the operators no dialect may be assumed to have.
// ILIKE is the whole list today: PostgreSQL has it, SQLite rejects it outright,
// and MySQL gets case-insensitivity from the collation instead.
var nonPortableOperators = map[Operator]bool{
	OpILike:    true,
	OpNotILike: true,
}

// DialectSupportsOperator reports whether d can render op.
//
// A dialect that implements [OperatorSupporter] answers for itself. Otherwise
// the answer is conservative: every operator in the portable core is supported,
// and the non-portable ones ([OpILike], [OpNotILike]) are not. Rendering an
// unsupported operator produces SQL the backend rejects at execution time, so a
// caller building a clause should check here first and fail with a Go error
// instead.
func DialectSupportsOperator(d Dialect, op Operator) bool {
	if d == nil {
		return false
	}
	if os, ok := d.(OperatorSupporter); ok {
		return os.SupportsOperator(op)
	}
	return !nonPortableOperators[op]
}

// DialectBeforeCreateTable returns the statements that must run immediately
// before table's CREATE TABLE, or nil when d needs none.
func DialectBeforeCreateTable(d Dialect, table string, columns []ColumnDef) ([]string, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: nil dialect", ErrUnknownDialect)
	}
	t, ok := d.(TableDDLer)
	if !ok {
		return nil, nil
	}
	return t.BeforeCreateTable(table, columns)
}

// DialectAfterDropTable returns the statements that must run immediately after
// table's DROP TABLE, or nil when d needs none.
func DialectAfterDropTable(d Dialect, table string, columns []ColumnDef) ([]string, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: nil dialect", ErrUnknownDialect)
	}
	t, ok := d.(TableDDLer)
	if !ok {
		return nil, nil
	}
	return t.AfterDropTable(table, columns)
}

var (
	dialectsMu sync.RWMutex
	dialects   = map[string]Dialect{}
)

// Register makes d available to [New] and [Open] under d.Name(). It returns an
// error wrapping [ErrDialectRegistered] if the name is taken, or
// [ErrInvalidIdentifier] if the name is empty. Registration is safe for
// concurrent use and is typically done from an init function.
func Register(d Dialect) error {
	if d == nil {
		return fmt.Errorf("%w: nil dialect", ErrUnknownDialect)
	}
	name := strings.ToLower(strings.TrimSpace(d.Name()))
	if name == "" {
		return fmt.Errorf("%w: empty dialect name", ErrInvalidIdentifier)
	}
	dialectsMu.Lock()
	defer dialectsMu.Unlock()
	if _, ok := dialects[name]; ok {
		return fmt.Errorf("%w: %q", ErrDialectRegistered, name)
	}
	dialects[name] = d
	return nil
}

// DialectByName returns the dialect registered under name, matched
// case-insensitively, or an error wrapping [ErrUnknownDialect].
func DialectByName(name string) (Dialect, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	dialectsMu.RLock()
	d, ok := dialects[key]
	dialectsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownDialect, name)
	}
	return d, nil
}

// Dialects returns the names of every registered dialect, sorted.
func Dialects() []string {
	dialectsMu.RLock()
	names := make([]string, 0, len(dialects))
	for name := range dialects {
		names = append(names, name)
	}
	dialectsMu.RUnlock()
	sort.Strings(names)
	return names
}
