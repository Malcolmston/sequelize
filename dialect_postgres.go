package sequelize

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Postgres is the PostgreSQL [Dialect]: double-quoted identifiers, positional
// "$1" placeholders, SERIAL/BIGSERIAL auto-increment, native BOOLEAN, TIMESTAMPTZ,
// NUMERIC, JSONB, UUID and BYTEA columns, native ENUM types created with
// CREATE TYPE, RETURNING on INSERT and UPDATE, upsert through
// ON CONFLICT ... DO UPDATE, and ILIKE.
//
// It is registered under the names "postgres" and "postgresql" when the package
// is loaded. Like the SQLite dialect it pulls in no driver: register a
// database/sql driver yourself — github.com/lib/pq or
// github.com/jackc/pgx/v5/stdlib — and pass its DSN to [New], or hand an open
// *sql.DB to [Open]. Keeping the driver out of the module is what lets the core
// stay cgo-free and dependency-light, and it is why [Postgres] recognises driver
// errors structurally (see [PostgresErrorCode]) rather than by importing a
// driver's error type.
var Postgres Dialect = postgresDialect{name: "postgres"}

func init() {
	// Registration errors here would mean this file registers the same name
	// twice, which is a programming error in this file alone.
	if err := Register(postgresDialect{name: "postgres"}); err != nil {
		panic(err)
	}
	if err := Register(postgresDialect{name: "postgresql"}); err != nil {
		panic(err)
	}
}

// postgresDialect implements Dialect, plus the optional [ColumnTyper],
// [Returner], [Upserter], [OperatorSupporter] and [TableDDLer] capabilities. The
// name field lets the same implementation answer to both "postgres" and
// "postgresql".
type postgresDialect struct{ name string }

func (d postgresDialect) Name() string { return d.name }

// Quote validates name and wraps it in double quotes. As everywhere in the
// package a name that fails validation is refused, never escaped — note in
// particular that PostgreSQL folds an unquoted identifier to lower case while a
// quoted one is case-sensitive, so a model using mixed-case column names must
// keep using them consistently.
func (d postgresDialect) Quote(name string) (string, error) {
	if err := ValidateIdentifier(name); err != nil {
		return "", err
	}
	if err := postgresIdentifierLength(name); err != nil {
		return "", err
	}
	return `"` + name + `"`, nil
}

// postgresMaxIdentifier is PostgreSQL's NAMEDATALEN-1. A longer identifier is
// silently truncated by the server, which would make two distinct columns
// collide, so the dialect refuses it instead.
const postgresMaxIdentifier = 63

func postgresIdentifierLength(name string) error {
	if len(name) > postgresMaxIdentifier {
		return fmt.Errorf("%w: %q is %d bytes, postgres truncates above %d", ErrInvalidIdentifier, name, len(name), postgresMaxIdentifier)
	}
	return nil
}

// Placeholder renders PostgreSQL's positional bind marker, "$1" for the first
// argument. The builder numbers arguments as it appends them, so it needs no
// knowledge of which style a dialect uses.
func (d postgresDialect) Placeholder(position int) string {
	return "$" + strconv.Itoa(position)
}

func (d postgresDialect) ColumnType(t DataType) (string, error) {
	return d.columnType(t, "")
}

// ColumnTypeFor renders the concrete type for t on table.column. It differs from
// [postgresDialect.ColumnType] only for [KindEnum], which becomes the named type
// [PostgresEnumTypeName] derives from table and column and which
// [postgresDialect.BeforeCreateTable] emits a CREATE TYPE for.
func (d postgresDialect) ColumnTypeFor(table, column string, t DataType) (string, error) {
	if t.Kind != KindEnum {
		return d.columnType(t, "")
	}
	typeName, err := PostgresEnumTypeName(table, column)
	if err != nil {
		return "", err
	}
	if len(t.Values) == 0 {
		return "", fmt.Errorf("%w: ENUM with no values", ErrUnsupported)
	}
	return d.columnType(t, typeName)
}

// columnType maps an abstract type onto PostgreSQL. enumType, when non-empty, is
// the already-derived name of the native enum type to use for [KindEnum].
func (d postgresDialect) columnType(t DataType, enumType string) (string, error) {
	switch t.Kind {
	case KindInteger:
		return "INTEGER", nil
	case KindBigInt:
		return "BIGINT", nil
	case KindFloat:
		return "DOUBLE PRECISION", nil
	case KindDecimal:
		if t.Precision > 0 {
			return fmt.Sprintf("NUMERIC(%d,%d)", t.Precision, t.Scale), nil
		}
		return "NUMERIC", nil
	case KindString:
		length := t.Length
		if length <= 0 {
			length = 255
		}
		return fmt.Sprintf("VARCHAR(%d)", length), nil
	case KindText:
		return "TEXT", nil
	case KindBoolean:
		return "BOOLEAN", nil
	case KindDate:
		// TIMESTAMPTZ, not TIMESTAMP: the port promises UTC round-trips, and a
		// naive TIMESTAMP would silently reinterpret them in the session zone.
		return "TIMESTAMPTZ", nil
	case KindDateOnly:
		return "DATE", nil
	case KindJSON:
		// JSONB rather than JSON: it is indexable and normalises the document,
		// and the port hands JSON back decoded either way.
		return "JSONB", nil
	case KindBlob:
		return "BYTEA", nil
	case KindUUID:
		return "UUID", nil
	case KindEnum:
		if len(t.Values) == 0 {
			return "", fmt.Errorf("%w: ENUM with no values", ErrUnsupported)
		}
		if enumType != "" {
			return `"` + enumType + `"`, nil
		}
		// Without a table and column there is no name to give the native type, so
		// the portable fallback is TEXT. Callers that can name the column should go
		// through [DialectColumnType] to get the native enum.
		return "TEXT", nil
	default:
		return "", fmt.Errorf("%w: postgres cannot express %s", ErrUnsupported, t)
	}
}

// AutoIncrementColumn renders SERIAL or BIGSERIAL. PostgreSQL 10 and later also
// spell this GENERATED BY DEFAULT AS IDENTITY; SERIAL is used because it is
// accepted by every supported server version and produces the same sequence.
func (d postgresDialect) AutoIncrementColumn(quoted string, t DataType) (string, error) {
	switch t.Kind {
	case KindInteger, KindInvalid:
		return quoted + " SERIAL PRIMARY KEY", nil
	case KindBigInt:
		return quoted + " BIGSERIAL PRIMARY KEY", nil
	default:
		return "", fmt.Errorf("%w: postgres auto-increment requires an integer column, got %s", ErrUnsupported, t)
	}
}

// BindValue narrows Go's numeric zoo to the types a PostgreSQL driver accepts.
// Unlike SQLite it keeps bool as bool, because the column really is BOOLEAN, and
// it widens every sized integer to int64 because database/sql's default
// converter rejects the rest.
func (d postgresDialect) BindValue(t DataType, v any) (any, error) {
	switch tv := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return tv, nil
	case int:
		return int64(tv), nil
	case int8:
		return int64(tv), nil
	case int16:
		return int64(tv), nil
	case int32:
		return int64(tv), nil
	case uint:
		return postgresUint(uint64(tv))
	case uint8:
		return int64(tv), nil
	case uint16:
		return int64(tv), nil
	case uint32:
		return int64(tv), nil
	case uint64:
		return postgresUint(tv)
	case float32:
		return float64(tv), nil
	case time.Time:
		return tv.UTC(), nil
	}
	return v, nil
}

// postgresUint widens an unsigned value, refusing the ones that do not fit a
// signed 64-bit column rather than wrapping them into a negative number.
func postgresUint(v uint64) (any, error) {
	const maxInt64 = uint64(1)<<63 - 1
	if v > maxInt64 {
		return nil, fmt.Errorf("%w: %d does not fit a postgres bigint", ErrInvalidType, v)
	}
	return int64(v), nil
}

// SupportsReturning reports true: RETURNING is how PostgreSQL reports a
// generated key, since its drivers cannot implement LastInsertId.
func (d postgresDialect) SupportsReturning() bool { return true }

// ReturningClause renders " RETURNING a, b".
func (d postgresDialect) ReturningClause(columns []string) (string, error) {
	if len(columns) == 0 {
		return "", fmt.Errorf("%w: RETURNING needs at least one column", ErrInvalidQuery)
	}
	return standardReturningClause(d, columns)
}

// SupportsOperator reports true for the whole [Operator] vocabulary: PostgreSQL
// is the backend that has ILIKE.
func (d postgresDialect) SupportsOperator(op Operator) bool {
	switch op {
	case OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpLike, OpNotLike, OpILike, OpNotILike:
		return true
	default:
		return false
	}
}

// UpsertClause renders " ON CONFLICT (cols) DO UPDATE SET c = EXCLUDED.c, ...",
// or " ON CONFLICT (cols) DO NOTHING" when updateColumns is empty.
//
// The conflict target must be covered by a unique index or a primary key, which
// is PostgreSQL's rule and not something the dialect can check; a target that is
// not raises an error from the server at execution time.
func (d postgresDialect) UpsertClause(conflictColumns, updateColumns []string) (string, error) {
	if len(conflictColumns) == 0 {
		return "", fmt.Errorf("%w: upsert needs at least one conflict column", ErrInvalidQuery)
	}
	var b strings.Builder
	b.WriteString(" ON CONFLICT (")
	for i, col := range conflictColumns {
		if i > 0 {
			b.WriteString(", ")
		}
		quoted, err := d.Quote(col)
		if err != nil {
			return "", err
		}
		b.WriteString(quoted)
	}
	b.WriteString(")")
	if len(updateColumns) == 0 {
		b.WriteString(" DO NOTHING")
		return b.String(), nil
	}
	b.WriteString(" DO UPDATE SET ")
	for i, col := range updateColumns {
		if i > 0 {
			b.WriteString(", ")
		}
		quoted, err := d.Quote(col)
		if err != nil {
			return "", err
		}
		b.WriteString(quoted)
		b.WriteString(" = EXCLUDED.")
		b.WriteString(quoted)
	}
	return b.String(), nil
}

// PostgresEnumTypeName returns the name of the native enum type the dialect uses
// for table.column, "<table>_<column>_enum". Both names are validated, and the
// result is rejected if it would exceed PostgreSQL's identifier length — a
// truncated type name would collide with a neighbouring column's.
func PostgresEnumTypeName(table, column string) (string, error) {
	if err := ValidateIdentifier(table); err != nil {
		return "", err
	}
	if err := ValidateIdentifier(column); err != nil {
		return "", err
	}
	name := table + "_" + column + "_enum"
	if err := postgresIdentifierLength(name); err != nil {
		return "", err
	}
	return name, nil
}

// BeforeCreateTable returns one CREATE TYPE ... AS ENUM statement per enum
// column, in the order the columns are given.
//
// PostgreSQL has no CREATE TYPE IF NOT EXISTS, so each statement is wrapped in
// the standard DO block that swallows duplicate_object. That keeps Sync
// idempotent, which is the same promise CREATE TABLE IF NOT EXISTS makes.
//
// A pre-existing type is left exactly as it is: this never adds a label to an
// enum whose value list has grown. Widening an enum is an ALTER TYPE, and
// belongs to the migration path rather than to Sync.
func (d postgresDialect) BeforeCreateTable(table string, columns []ColumnDef) ([]string, error) {
	if err := ValidateIdentifier(table); err != nil {
		return nil, err
	}
	var out []string
	for _, col := range columns {
		if col.Type.Kind != KindEnum {
			continue
		}
		stmt, err := d.createEnumSQL(table, col)
		if err != nil {
			return nil, err
		}
		out = append(out, stmt)
	}
	return out, nil
}

func (d postgresDialect) createEnumSQL(table string, col ColumnDef) (string, error) {
	if len(col.Type.Values) == 0 {
		return "", fmt.Errorf("%w: ENUM with no values on %s.%s", ErrUnsupported, table, col.Column)
	}
	typeName, err := PostgresEnumTypeName(table, col.Column)
	if err != nil {
		return "", err
	}
	labels := make([]string, 0, len(col.Type.Values))
	for _, v := range col.Type.Values {
		literal, err := postgresStringLiteral(v)
		if err != nil {
			return "", fmt.Errorf("enum label on %s.%s: %w", table, col.Column, err)
		}
		labels = append(labels, literal)
	}
	// DDL takes no bind parameters, so the labels are rendered as literals — which
	// is why postgresStringLiteral refuses anything it cannot prove is safe.
	return `DO $$ BEGIN CREATE TYPE "` + typeName + `" AS ENUM (` + strings.Join(labels, ", ") +
		`); EXCEPTION WHEN duplicate_object THEN NULL; END $$`, nil
}

// AfterDropTable returns one DROP TYPE IF EXISTS statement per enum column, so
// that dropping a table does not leave its enum types behind and a later Sync
// with a changed value list starts clean.
func (d postgresDialect) AfterDropTable(table string, columns []ColumnDef) ([]string, error) {
	if err := ValidateIdentifier(table); err != nil {
		return nil, err
	}
	var out []string
	for _, col := range columns {
		if col.Type.Kind != KindEnum {
			continue
		}
		typeName, err := PostgresEnumTypeName(table, col.Column)
		if err != nil {
			return nil, err
		}
		out = append(out, `DROP TYPE IF EXISTS "`+typeName+`"`)
	}
	return out, nil
}

// postgresLiteralLimit caps how long a DDL string literal may be.
const postgresLiteralLimit = 255

// postgresStringLiteral renders s as a single-quoted SQL literal, refusing
// anything it cannot prove safe: only printable ASCII, and no quote or
// backslash. This is deliberately the same rule the DDL default renderer uses —
// a literal is only ever written into DDL, which cannot take a placeholder, so
// "reject" is the only safe answer to an awkward value.
func postgresStringLiteral(s string) (string, error) {
	if len(s) > postgresLiteralLimit {
		return "", fmt.Errorf("%w: literal %q is longer than %d bytes", ErrUnsupported, s, postgresLiteralLimit)
	}
	if s == "" {
		return "", fmt.Errorf("%w: empty literal", ErrUnsupported)
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e || r == '\'' || r == '"' || r == '\\' {
			return "", fmt.Errorf("%w: literal %q cannot be rendered in DDL", ErrUnsupported, s)
		}
	}
	return "'" + s + "'", nil
}

// The SQLSTATE codes the dialect recognises. PostgreSQL's class 23 is
// "integrity constraint violation"; the codes are stable across every server
// version and are the documented way to tell these failures apart, which parsing
// the English message text is not.
const (
	// PostgresNotNullViolation is SQLSTATE 23502.
	PostgresNotNullViolation = "23502"
	// PostgresForeignKeyViolation is SQLSTATE 23503.
	PostgresForeignKeyViolation = "23503"
	// PostgresUniqueViolation is SQLSTATE 23505.
	PostgresUniqueViolation = "23505"
)

// TranslateError maps a PostgreSQL driver error onto the package's typed errors
// by its SQLSTATE code: 23505 to [UniqueConstraintError], 23503 to
// [ForeignKeyError], and 23502 to a [ValidationError] naming the column that was
// null. Anything else, including an error carrying no SQLSTATE, is returned
// unchanged.
//
// 23502 becomes a validation failure because that is the error the port already
// has for "this column may not be empty", and it is what upstream Sequelize
// reports for the same condition. There is no dedicated not-null sentinel.
func (d postgresDialect) TranslateError(err error) error {
	if err == nil {
		return nil
	}
	code := PostgresErrorCode(err)
	if code == "" {
		return err
	}
	column, constraint := postgresErrorFields(err)
	switch code {
	case PostgresUniqueViolation:
		return &UniqueConstraintError{Fields: postgresFieldList(column, constraint), Cause: err}
	case PostgresForeignKeyViolation:
		return &ForeignKeyError{Fields: postgresFieldList(column, constraint), Cause: err}
	case PostgresNotNullViolation:
		attribute := column
		if attribute == "" {
			attribute = constraint
		}
		if attribute == "" {
			return err
		}
		return &ValidationError{Errors: []FieldError{{
			Attribute: attribute,
			Message:   "must not be null",
		}}}
	default:
		return err
	}
}

// postgresFieldList prefers the column the server named, falling back to the
// constraint name, and returns nil when it has neither.
func postgresFieldList(column, constraint string) []string {
	if column != "" {
		return []string{column}
	}
	if constraint != "" {
		return []string{constraint}
	}
	return nil
}

// PostgresSQLState is implemented by driver errors that expose their SQLSTATE
// code, as github.com/jackc/pgx/v5's *pgconn.PgError does. Implement it on your
// own error type to make [Postgres] translate it.
type PostgresSQLState interface {
	// SQLState returns the five-character SQLSTATE code.
	SQLState() string
}

// PostgresErrorCode extracts the five-character SQLSTATE code from a driver
// error, or returns "" when it carries none.
//
// It works without importing a driver, which is what keeps the module free of
// driver dependencies. It walks the [errors.Unwrap] chain looking for, in order,
// an error implementing [PostgresSQLState] (pgx) and an error struct with a
// string-kinded Code field (lib/pq). Only a well-formed code — five characters
// of digits and upper-case letters — is accepted, so an unrelated Code field on
// some other error type cannot be mistaken for a SQLSTATE.
func PostgresErrorCode(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := e.(PostgresSQLState); ok {
			if code := s.SQLState(); isPostgresSQLState(code) {
				return code
			}
		}
		if code := postgresStringField(e, "Code"); isPostgresSQLState(code) {
			return code
		}
	}
	return ""
}

// postgresErrorFields pulls the column and constraint names out of a driver
// error, under either the lib/pq spelling (Column, Constraint) or the pgx one
// (ColumnName, ConstraintName). Either may be absent.
func postgresErrorFields(err error) (column, constraint string) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if column == "" {
			column = firstNonEmpty(postgresStringField(e, "ColumnName"), postgresStringField(e, "Column"))
		}
		if constraint == "" {
			constraint = firstNonEmpty(postgresStringField(e, "ConstraintName"), postgresStringField(e, "Constraint"))
		}
		if column != "" && constraint != "" {
			break
		}
	}
	return column, constraint
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// postgresStringField reads a named string-kinded exported field off an error
// value, dereferencing a pointer first. It returns "" for anything else,
// including a nil pointer or a non-struct.
func postgresStringField(err error, field string) string {
	v := reflect.ValueOf(err)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	f := v.FieldByName(field)
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}

// isPostgresSQLState reports whether code has the shape of a SQLSTATE: exactly
// five characters, each a digit or an upper-case letter.
func isPostgresSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
