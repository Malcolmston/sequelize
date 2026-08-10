package sequelize

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// pg returns the concrete postgres dialect, for the methods that are not on the
// Dialect interface.
func pg() postgresDialect { return postgresDialect{name: "postgres"} }

// newPostgresGen returns a connection whose dialect is Postgres but whose handle
// is the SQLite test database.
//
// It exists because there is no PostgreSQL server in this environment, and the
// bulk of a dialect is pure string generation that needs none. Tests using it
// must only ever call the SQL *builders* — CreateTableSQL, buildSelect and
// friends — and never execute what they produce. Anything that needs a server to
// prove is called out in the report rather than faked here.
func newPostgresGen(t *testing.T) *Sequelize {
	t.Helper()
	sqliteConn := newTestDB(t)
	gen, err := Open("postgres", sqliteConn.DB())
	if err != nil {
		t.Fatalf("Open(postgres): %v", err)
	}
	if _, ok := gen.Dialect().(postgresDialect); !ok {
		t.Fatalf("Dialect() = %T, want postgresDialect", gen.Dialect())
	}
	return gen
}

func TestPostgresRegisteredUnderBothNames(t *testing.T) {
	for _, name := range []string{"postgres", "Postgres", " postgresql "} {
		d, err := DialectByName(name)
		if err != nil {
			t.Fatalf("DialectByName(%q) = %v", name, err)
		}
		if !strings.HasPrefix(d.Name(), "postgres") {
			t.Errorf("DialectByName(%q).Name() = %q", name, d.Name())
		}
	}
	names := Dialects()
	found := 0
	for _, n := range names {
		if n == "postgres" || n == "postgresql" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("Dialects() = %v, want postgres and postgresql", names)
	}
}

func TestPostgresQuote(t *testing.T) {
	got, err := Postgres.Quote("user_name")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if want := `"user_name"`; got != want {
		t.Errorf("Quote = %q, want %q", got, want)
	}
}

func TestPostgresQuoteRefusesToEscape(t *testing.T) {
	bad := []string{
		`a" ; DROP TABLE users --`,
		"a b",
		"",
		"user-name",
		"1id",
		"naïve",
		// 64 bytes: postgres would truncate to 63 and collide with a neighbour.
		strings.Repeat("a", postgresMaxIdentifier+1),
	}
	for _, name := range bad {
		got, err := Postgres.Quote(name)
		if !errors.Is(err, ErrInvalidIdentifier) {
			t.Errorf("Quote(%q) = %q, %v; want ErrInvalidIdentifier", name, got, err)
		}
		if got != "" {
			t.Errorf("Quote(%q) returned %q alongside an error", name, got)
		}
	}
	// Exactly at the limit is fine.
	if _, err := Postgres.Quote(strings.Repeat("a", postgresMaxIdentifier)); err != nil {
		t.Errorf("Quote(63 bytes) = %v, want nil", err)
	}
}

func TestPostgresPlaceholderIsPositional(t *testing.T) {
	for i, want := range map[int]string{1: "$1", 2: "$2", 10: "$10", 137: "$137"} {
		if got := Postgres.Placeholder(i); got != want {
			t.Errorf("Placeholder(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestPostgresColumnTypes(t *testing.T) {
	cases := []struct {
		dt   DataType
		want string
	}{
		{INTEGER(), "INTEGER"},
		{BIGINT(), "BIGINT"},
		{FLOAT(), "DOUBLE PRECISION"},
		{DECIMAL(), "NUMERIC"},
		{DECIMAL(10, 2), "NUMERIC(10,2)"},
		{DECIMAL(8), "NUMERIC(8,0)"},
		{STRING(64), "VARCHAR(64)"},
		{STRING(0), "VARCHAR(255)"},
		{TEXT(), "TEXT"},
		{BOOLEAN(), "BOOLEAN"},
		{DATE(), "TIMESTAMPTZ"},
		{DATEONLY(), "DATE"},
		{JSON(), "JSONB"},
		{BLOB(), "BYTEA"},
		{UUID(), "UUID"},
		// With no column to name, an enum falls back to TEXT.
		{ENUM("a", "b"), "TEXT"},
	}
	for _, c := range cases {
		got, err := Postgres.ColumnType(c.dt)
		if err != nil {
			t.Errorf("ColumnType(%s) = %v", c.dt, err)
			continue
		}
		if got != c.want {
			t.Errorf("ColumnType(%s) = %q, want %q", c.dt, got, c.want)
		}
	}
}

func TestPostgresColumnTypeRejectsUnmapped(t *testing.T) {
	if _, err := Postgres.ColumnType(DataType{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ColumnType(zero) = %v, want ErrUnsupported", err)
	}
	if _, err := Postgres.ColumnType(DataType{Kind: KindEnum}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ColumnType(empty ENUM) = %v, want ErrUnsupported", err)
	}
}

func TestPostgresColumnTypeForEnumIsNative(t *testing.T) {
	got, err := DialectColumnType(Postgres, "users", "role", ENUM("admin", "user"))
	if err != nil {
		t.Fatalf("DialectColumnType: %v", err)
	}
	if want := `"users_role_enum"`; got != want {
		t.Errorf("DialectColumnType = %q, want %q", got, want)
	}
	// A non-enum type is unaffected by the column it lives on.
	got, err = DialectColumnType(Postgres, "users", "age", INTEGER())
	if err != nil {
		t.Fatalf("DialectColumnType: %v", err)
	}
	if got != "INTEGER" {
		t.Errorf("DialectColumnType(INTEGER) = %q, want INTEGER", got)
	}
}

func TestPostgresColumnTypeForRejectsBadNames(t *testing.T) {
	if _, err := pg().ColumnTypeFor("users; DROP", "role", ENUM("a")); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("ColumnTypeFor(bad table) = %v, want ErrInvalidIdentifier", err)
	}
	if _, err := pg().ColumnTypeFor("users", `role"`, ENUM("a")); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("ColumnTypeFor(bad column) = %v, want ErrInvalidIdentifier", err)
	}
	if _, err := pg().ColumnTypeFor("users", "role", ENUM()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ColumnTypeFor(empty ENUM) = %v, want ErrUnsupported", err)
	}
}

func TestPostgresEnumTypeName(t *testing.T) {
	got, err := PostgresEnumTypeName("users", "role")
	if err != nil {
		t.Fatalf("PostgresEnumTypeName: %v", err)
	}
	if want := "users_role_enum"; got != want {
		t.Errorf("PostgresEnumTypeName = %q, want %q", got, want)
	}
	// table + "_" + column + "_enum" must still fit postgres's 63 bytes.
	if _, err := PostgresEnumTypeName(strings.Repeat("t", 40), strings.Repeat("c", 40)); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("PostgresEnumTypeName(too long) = %v, want ErrInvalidIdentifier", err)
	}
}

func TestPostgresAutoIncrementColumn(t *testing.T) {
	cases := []struct {
		dt   DataType
		want string
	}{
		{INTEGER(), `"id" SERIAL PRIMARY KEY`},
		{BIGINT(), `"id" BIGSERIAL PRIMARY KEY`},
		// The zero type is what model.go's implicit primary key carries.
		{DataType{}, `"id" SERIAL PRIMARY KEY`},
	}
	for _, c := range cases {
		got, err := Postgres.AutoIncrementColumn(`"id"`, c.dt)
		if err != nil {
			t.Errorf("AutoIncrementColumn(%s) = %v", c.dt, err)
			continue
		}
		if got != c.want {
			t.Errorf("AutoIncrementColumn(%s) = %q, want %q", c.dt, got, c.want)
		}
	}
	for _, dt := range []DataType{STRING(10), UUID(), BOOLEAN(), TEXT()} {
		if _, err := Postgres.AutoIncrementColumn(`"id"`, dt); !errors.Is(err, ErrUnsupported) {
			t.Errorf("AutoIncrementColumn(%s) = %v, want ErrUnsupported", dt, err)
		}
	}
}

func TestPostgresBindValue(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	cases := []struct {
		in   any
		want any
	}{
		{nil, nil},
		// Unlike SQLite, a bool stays a bool: the column really is BOOLEAN.
		{true, true},
		{false, false},
		{int(7), int64(7)},
		{int8(7), int64(7)},
		{int16(7), int64(7)},
		{int32(7), int64(7)},
		{int64(7), int64(7)},
		{uint(7), int64(7)},
		{uint8(7), int64(7)},
		{uint16(7), int64(7)},
		{uint32(7), int64(7)},
		{uint64(7), int64(7)},
		{float32(1.5), float64(1.5)},
		{float64(1.5), float64(1.5)},
		{"text", "text"},
		{[]byte{1, 2}, []byte{1, 2}},
		{when, when.UTC()},
	}
	for _, c := range cases {
		got, err := Postgres.BindValue(DataType{}, c.in)
		if err != nil {
			t.Errorf("BindValue(%#v) = %v", c.in, err)
			continue
		}
		if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", c.want) {
			t.Errorf("BindValue(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestPostgresBindValueRejectsOversizedUnsigned(t *testing.T) {
	// Wrapping this into a negative bigint would corrupt the value silently.
	if _, err := Postgres.BindValue(DataType{}, uint64(1)<<63); !errors.Is(err, ErrInvalidType) {
		t.Errorf("BindValue(1<<63) = %v, want ErrInvalidType", err)
	}
	if _, err := Postgres.BindValue(DataType{}, ^uint64(0)); !errors.Is(err, ErrInvalidType) {
		t.Errorf("BindValue(max uint64) = %v, want ErrInvalidType", err)
	}
}

func TestPostgresReturningClause(t *testing.T) {
	if !Postgres.SupportsReturning() {
		t.Fatal("SupportsReturning() = false")
	}
	got, err := DialectReturning(Postgres, []string{"id", "created_at"})
	if err != nil {
		t.Fatalf("DialectReturning: %v", err)
	}
	if want := ` RETURNING "id", "created_at"`; got != want {
		t.Errorf("DialectReturning = %q, want %q", got, want)
	}
	if _, err := DialectReturning(Postgres, nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("DialectReturning(nil) = %v, want ErrInvalidQuery", err)
	}
	if _, err := DialectReturning(Postgres, []string{"id", "a b"}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("DialectReturning(bad column) = %v, want ErrInvalidIdentifier", err)
	}
}

func TestPostgresUpsertClause(t *testing.T) {
	cases := []struct {
		name     string
		conflict []string
		updates  []string
		want     string
	}{
		{
			name:     "single conflict column",
			conflict: []string{"email"},
			updates:  []string{"name", "age"},
			want:     ` ON CONFLICT ("email") DO UPDATE SET "name" = EXCLUDED."name", "age" = EXCLUDED."age"`,
		},
		{
			name:     "composite conflict target",
			conflict: []string{"user_id", "group_id"},
			updates:  []string{"role"},
			want:     ` ON CONFLICT ("user_id", "group_id") DO UPDATE SET "role" = EXCLUDED."role"`,
		},
		{
			name:     "nothing to update",
			conflict: []string{"id"},
			updates:  nil,
			want:     ` ON CONFLICT ("id") DO NOTHING`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DialectUpsert(Postgres, c.conflict, c.updates)
			if err != nil {
				t.Fatalf("DialectUpsert: %v", err)
			}
			if got != c.want {
				t.Errorf("DialectUpsert =\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

func TestPostgresUpsertClauseRejectsBadInput(t *testing.T) {
	if _, err := DialectUpsert(Postgres, nil, []string{"a"}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("DialectUpsert(no conflict) = %v, want ErrInvalidQuery", err)
	}
	if _, err := DialectUpsert(Postgres, []string{"a b"}, []string{"c"}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("DialectUpsert(bad conflict column) = %v, want ErrInvalidIdentifier", err)
	}
	if _, err := DialectUpsert(Postgres, []string{"a"}, []string{"c; DROP"}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("DialectUpsert(bad update column) = %v, want ErrInvalidIdentifier", err)
	}
}

func TestPostgresSupportsILike(t *testing.T) {
	for _, op := range []Operator{OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpLike, OpNotLike, OpILike, OpNotILike} {
		if !DialectSupportsOperator(Postgres, op) {
			t.Errorf("DialectSupportsOperator(postgres, %s) = false", op)
		}
	}
	if DialectSupportsOperator(Postgres, Operator("~~*")) {
		t.Error("DialectSupportsOperator accepted an operator outside the vocabulary")
	}
	// The counterpart: SQLite must not claim ILIKE, which it rejects at parse time.
	for _, op := range []Operator{OpILike, OpNotILike} {
		if DialectSupportsOperator(SQLite, op) {
			t.Errorf("DialectSupportsOperator(sqlite, %s) = true; sqlite has no ILIKE", op)
		}
	}
}

func TestPostgresBeforeCreateTableEmitsEnumTypes(t *testing.T) {
	cols := []ColumnDef{
		{Column: "id", Type: INTEGER()},
		{Column: "role", Type: ENUM("admin", "user")},
		{Column: "state", Type: ENUM("on")},
		{Column: "name", Type: TEXT()},
	}
	got, err := DialectBeforeCreateTable(Postgres, "users", cols)
	if err != nil {
		t.Fatalf("DialectBeforeCreateTable: %v", err)
	}
	want := []string{
		`DO $$ BEGIN CREATE TYPE "users_role_enum" AS ENUM ('admin', 'user'); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`DO $$ BEGIN CREATE TYPE "users_state_enum" AS ENUM ('on'); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
	}
	if len(got) != len(want) {
		t.Fatalf("DialectBeforeCreateTable = %v, want %d statements", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d =\n%s\nwant\n%s", i, got[i], want[i])
		}
	}
}

func TestPostgresTableDDLNeedsNothingWithoutEnums(t *testing.T) {
	cols := []ColumnDef{{Column: "id", Type: INTEGER()}, {Column: "name", Type: TEXT()}}
	before, err := DialectBeforeCreateTable(Postgres, "users", cols)
	if err != nil || before != nil {
		t.Errorf("DialectBeforeCreateTable = %v, %v; want nil, nil", before, err)
	}
	after, err := DialectAfterDropTable(Postgres, "users", cols)
	if err != nil || after != nil {
		t.Errorf("DialectAfterDropTable = %v, %v; want nil, nil", after, err)
	}
}

func TestPostgresAfterDropTableDropsEnumTypes(t *testing.T) {
	got, err := DialectAfterDropTable(Postgres, "users", []ColumnDef{
		{Column: "role", Type: ENUM("a")},
		{Column: "id", Type: INTEGER()},
	})
	if err != nil {
		t.Fatalf("DialectAfterDropTable: %v", err)
	}
	want := []string{`DROP TYPE IF EXISTS "users_role_enum"`}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("DialectAfterDropTable = %v, want %v", got, want)
	}
}

func TestPostgresEnumDDLRejectsUnrenderableLabels(t *testing.T) {
	bad := [][]string{
		{"it's"},                      // a quote would end the literal early
		{`a\b`},                       // a backslash is an escape in some settings
		{"a'); DROP TABLE users; --"}, // the reason the rule exists
		{""},                          // an empty label is not worth guessing at
		{"tab\there"},                 // a control character
		{"héllo"},                     // non-ASCII, which the rule does not cover
		{strings.Repeat("x", 256)},    // longer than the literal limit
	}
	for _, values := range bad {
		_, err := DialectBeforeCreateTable(Postgres, "users", []ColumnDef{{Column: "role", Type: ENUM(values...)}})
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("BeforeCreateTable(ENUM(%q)) = %v, want ErrUnsupported", values, err)
		}
	}
	if _, err := DialectBeforeCreateTable(Postgres, "users", []ColumnDef{{Column: "role", Type: ENUM()}}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("BeforeCreateTable(empty ENUM) = %v, want ErrUnsupported", err)
	}
	if _, err := DialectBeforeCreateTable(Postgres, "bad table", nil); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("BeforeCreateTable(bad table) = %v, want ErrInvalidIdentifier", err)
	}
	if _, err := DialectAfterDropTable(Postgres, "bad table", nil); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("AfterDropTable(bad table) = %v, want ErrInvalidIdentifier", err)
	}
}

// pgxStyleError mirrors github.com/jackc/pgx/v5/pgconn.PgError, which exposes its
// SQLSTATE through a method.
type pgxStyleError struct {
	Code           string
	Message        string
	ColumnName     string
	ConstraintName string
	TableName      string
}

func (e *pgxStyleError) Error() string    { return "ERROR: " + e.Message + " (SQLSTATE " + e.Code + ")" }
func (e *pgxStyleError) SQLState() string { return e.Code }

// pqErrorCode mirrors lib/pq's ErrorCode, a named string type.
type pqErrorCode string

// pqStyleError mirrors github.com/lib/pq.Error, which exposes its SQLSTATE as a
// struct field of a named string type and has no SQLState method.
type pqStyleError struct {
	Code       pqErrorCode
	Message    string
	Column     string
	Constraint string
	Table      string
}

func (e *pqStyleError) Error() string { return "pq: " + e.Message }

func TestPostgresErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain error", errors.New("boom"), ""},
		{"pgx shape", &pgxStyleError{Code: "23505"}, "23505"},
		{"pq shape", &pqStyleError{Code: "23503"}, "23503"},
		{"wrapped once", fmt.Errorf("insert: %w", &pgxStyleError{Code: "23502"}), "23502"},
		{"wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", &pqStyleError{Code: "40001"})), "40001"},
		{"sql.ErrNoRows carries none", sql.ErrNoRows, ""},
		// A Code field that is not a SQLSTATE must not be mistaken for one, or an
		// unrelated library's error would be translated into a constraint failure.
		{"short code", &pqStyleError{Code: "235"}, ""},
		{"long code", &pqStyleError{Code: "235055"}, ""},
		{"lowercase code", &pqStyleError{Code: "23p01"}, ""},
		{"punctuation code", &pqStyleError{Code: "23-05"}, ""},
		{"letters are fine", &pqStyleError{Code: "23P01"}, "23P01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PostgresErrorCode(c.err); got != c.want {
				t.Errorf("PostgresErrorCode = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPostgresErrorCodeIgnoresNonErrorShapes(t *testing.T) {
	// A nil typed pointer and a non-struct error must not panic the reflection path.
	var nilTyped *pqStyleError
	if got := PostgresErrorCode(error(nilTyped)); got != "" {
		t.Errorf("PostgresErrorCode(nil *pqStyleError) = %q", got)
	}
	if got := PostgresErrorCode(errStringKind("x")); got != "" {
		t.Errorf("PostgresErrorCode(string-kinded error) = %q", got)
	}
}

// errStringKind is an error whose underlying kind is a string, not a struct.
type errStringKind string

func (e errStringKind) Error() string { return string(e) }

func TestPostgresTranslateErrorUniqueViolation(t *testing.T) {
	for _, driverErr := range []error{
		&pgxStyleError{Code: PostgresUniqueViolation, Message: "duplicate key", ConstraintName: "users_email_key", ColumnName: "email"},
		&pqStyleError{Code: pqErrorCode(PostgresUniqueViolation), Message: "duplicate key", Constraint: "users_email_key", Column: "email"},
	} {
		got := Postgres.TranslateError(driverErr)
		if !errors.Is(got, ErrUniqueConstraint) {
			t.Fatalf("TranslateError(%T) = %v, want ErrUniqueConstraint", driverErr, got)
		}
		var unique *UniqueConstraintError
		if !errors.As(got, &unique) {
			t.Fatalf("TranslateError(%T) = %T, want *UniqueConstraintError", driverErr, got)
		}
		if len(unique.Fields) != 1 || unique.Fields[0] != "email" {
			t.Errorf("Fields = %v, want [email]", unique.Fields)
		}
		if unique.Underlying() != driverErr {
			t.Errorf("Underlying() = %v, want the driver error", unique.Underlying())
		}
	}
}

func TestPostgresTranslateErrorFallsBackToConstraintName(t *testing.T) {
	// PostgreSQL names the constraint but not the column for a unique index
	// violation, which is the common case.
	got := Postgres.TranslateError(&pqStyleError{Code: pqErrorCode(PostgresUniqueViolation), Constraint: "users_email_key"})
	var unique *UniqueConstraintError
	if !errors.As(got, &unique) {
		t.Fatalf("TranslateError = %T, want *UniqueConstraintError", got)
	}
	if len(unique.Fields) != 1 || unique.Fields[0] != "users_email_key" {
		t.Errorf("Fields = %v, want [users_email_key]", unique.Fields)
	}
	// With neither, there is nothing honest to report.
	got = Postgres.TranslateError(&pqStyleError{Code: pqErrorCode(PostgresUniqueViolation)})
	if !errors.As(got, &unique) {
		t.Fatalf("TranslateError = %T, want *UniqueConstraintError", got)
	}
	if unique.Fields != nil {
		t.Errorf("Fields = %v, want nil", unique.Fields)
	}
}

func TestPostgresTranslateErrorForeignKeyViolation(t *testing.T) {
	driverErr := &pgxStyleError{Code: PostgresForeignKeyViolation, ConstraintName: "posts_user_id_fkey"}
	got := Postgres.TranslateError(driverErr)
	if !errors.Is(got, ErrForeignKeyConstraint) {
		t.Fatalf("TranslateError = %v, want ErrForeignKeyConstraint", got)
	}
	var fk *ForeignKeyError
	if !errors.As(got, &fk) {
		t.Fatalf("TranslateError = %T, want *ForeignKeyError", got)
	}
	if len(fk.Fields) != 1 || fk.Fields[0] != "posts_user_id_fkey" {
		t.Errorf("Fields = %v", fk.Fields)
	}
	if fk.Underlying() != driverErr {
		t.Error("Underlying() lost the driver error")
	}
}

func TestPostgresTranslateErrorNotNullViolation(t *testing.T) {
	got := Postgres.TranslateError(&pgxStyleError{Code: PostgresNotNullViolation, ColumnName: "name"})
	if !errors.Is(got, ErrValidation) {
		t.Fatalf("TranslateError = %v, want ErrValidation", got)
	}
	var ve *ValidationError
	if !errors.As(got, &ve) {
		t.Fatalf("TranslateError = %T, want *ValidationError", got)
	}
	if want := []string{"name"}; len(ve.Attributes()) != 1 || ve.Attributes()[0] != want[0] {
		t.Errorf("Attributes() = %v, want %v", ve.Attributes(), want)
	}
	if !strings.Contains(ve.Error(), "must not be null") {
		t.Errorf("Error() = %q, want it to mention the cause", ve.Error())
	}
	// With no column named there is nothing to attach the failure to, so the
	// driver error passes through rather than becoming a lie.
	bare := &pgxStyleError{Code: PostgresNotNullViolation}
	if got := Postgres.TranslateError(bare); got != error(bare) {
		t.Errorf("TranslateError(bare 23502) = %v, want it unchanged", got)
	}
}

func TestPostgresTranslateErrorPassesThroughUnrecognised(t *testing.T) {
	if got := Postgres.TranslateError(nil); got != nil {
		t.Errorf("TranslateError(nil) = %v, want nil", got)
	}
	for _, err := range []error{
		errors.New("connection refused"),
		sql.ErrNoRows,
		// Class 23 has more members than the three that map onto a typed error;
		// a check-constraint failure is not a unique violation.
		&pgxStyleError{Code: "23514", ConstraintName: "age_positive"},
		&pgxStyleError{Code: "42P01", Message: "relation does not exist"},
		&pqStyleError{Code: "40P01", Message: "deadlock detected"},
	} {
		if got := Postgres.TranslateError(err); got != err {
			t.Errorf("TranslateError(%v) = %v, want it unchanged", err, got)
		}
	}
}

// --- SQL generation through the real builders -------------------------------
//
// These drive the package's own builders with the Postgres dialect installed, so
// they cover the seam as well as the dialect: placeholder numbering, quoting,
// LIMIT/OFFSET and DDL. They never execute the SQL, because there is no server.

func TestPostgresSelectUsesPositionalPlaceholders(t *testing.T) {
	gen := newPostgresGen(t)
	m := gen.Define("User", Attributes{
		"name": {Type: STRING(20)},
		"age":  {Type: INTEGER()},
	}, Timestamps(false))
	got, args, _, err := m.buildSelect(Query{
		Where: And(Op.Eq("name", "ada"), Op.Gt("age", 30)),
		Limit: Int(10),
	})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	want := `SELECT "id", "age", "name" FROM "users" WHERE ("name" = $1 AND "age" > $2) LIMIT $3`
	if got != want {
		t.Errorf("buildSelect =\n%s\nwant\n%s", got, want)
	}
	if len(args) != 3 {
		t.Errorf("args = %v, want three", args)
	}
}

func TestPostgresLimitOffset(t *testing.T) {
	gen := newPostgresGen(t)
	m := gen.Define("Row", Attributes{"name": {Type: STRING(5)}}, Timestamps(false))
	got, args, _, err := m.buildSelect(Query{Limit: Int(5), Offset: Int(10)})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.HasSuffix(got, "LIMIT $1 OFFSET $2") {
		t.Errorf("buildSelect = %s, want it to end LIMIT $1 OFFSET $2", got)
	}
	if len(args) != 2 || args[0] != int64(5) || args[1] != int64(10) {
		t.Errorf("args = %v, want [5 10]", args)
	}
	// Offset alone still needs a LIMIT, and the stand-in must survive BindValue.
	got, args, _, err = m.buildSelect(Query{Offset: Int(3)})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.HasSuffix(got, "LIMIT $1 OFFSET $2") {
		t.Errorf("buildSelect = %s", got)
	}
	if len(args) != 2 || args[0] != int64(math.MaxInt64) || args[1] != int64(3) {
		t.Errorf("args = %v, want the unlimited stand-in then the offset", args)
	}
}

func TestPostgresRendersILike(t *testing.T) {
	gen := newPostgresGen(t)
	m := gen.Define("User", Attributes{"name": {Type: STRING(20)}}, Timestamps(false))
	got, _, _, err := m.buildSelect(Query{Where: Op.ILike("name", "ad%")})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.Contains(got, `"name" ILIKE $1`) {
		t.Errorf("buildSelect = %s, want an ILIKE comparison", got)
	}
}

func TestPostgresInsertPlaceholdersAreNumberedAcrossRows(t *testing.T) {
	gen := newPostgresGen(t)
	m := gen.Define("Item", Attributes{
		"name": {Type: STRING(10)},
		"qty":  {Type: INTEGER()},
	}, Timestamps(false))
	got, args, err := m.buildInsert([]string{"name", "qty"}, []Values{
		{"name": "a", "qty": 1},
		{"name": "b", "qty": 2},
	})
	if err != nil {
		t.Fatalf("buildInsert: %v", err)
	}
	want := `INSERT INTO "items" ("name", "qty") VALUES ($1, $2), ($3, $4)`
	if got != want {
		t.Errorf("buildInsert =\n%s\nwant\n%s", got, want)
	}
	if len(args) != 4 {
		t.Errorf("args = %v, want four", args)
	}
}

func TestPostgresUpdateThenWhereKeepPlaceholderOrder(t *testing.T) {
	gen := newPostgresGen(t)
	m := gen.Define("Item", Attributes{"name": {Type: STRING(10)}, "qty": {Type: INTEGER()}}, Timestamps(false))
	got, args, err := m.buildUpdate(Values{"qty": 9}, Query{Where: Op.Eq("name", "a")})
	if err != nil {
		t.Fatalf("buildUpdate: %v", err)
	}
	want := `UPDATE "items" SET "qty" = $1 WHERE "name" = $2`
	if got != want {
		t.Errorf("buildUpdate =\n%s\nwant\n%s", got, want)
	}
	if len(args) != 2 {
		t.Errorf("args = %v", args)
	}
}

func TestPostgresCreateTableSQL(t *testing.T) {
	gen := newPostgresGen(t)
	m := gen.Define("Widget", Attributes{
		"name":     {Type: STRING(20), AllowNull: NotNull()},
		"sku":      {Type: STRING(10), Unique: true},
		"price":    {Type: DECIMAL(10, 2)},
		"active":   {Type: BOOLEAN()},
		"payload":  {Type: JSON()},
		"ref":      {Type: UUID()},
		"blobby":   {Type: BLOB()},
		"note":     {Type: TEXT()},
		"seenAt":   {Type: DATE()},
		"bornOn":   {Type: DATEONLY()},
		"bigCount": {Type: BIGINT()},
	}, Timestamps(false))
	got, err := m.CreateTableSQL()
	if err != nil {
		t.Fatalf("CreateTableSQL: %v", err)
	}
	for _, want := range []string{
		`"id" SERIAL PRIMARY KEY`,
		`"name" VARCHAR(20) NOT NULL`,
		`"sku" VARCHAR(10) UNIQUE`,
		`"price" NUMERIC(10,2)`,
		`"active" BOOLEAN`,
		`"payload" JSONB`,
		`"ref" UUID`,
		`"blobby" BYTEA`,
		`"note" TEXT`,
		`"seen_at" TIMESTAMPTZ`,
		`"born_on" DATE`,
		`"big_count" BIGINT`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CreateTableSQL = %s\nwant it to contain %s", got, want)
		}
	}
	if strings.Contains(got, "AUTOINCREMENT") {
		t.Errorf("CreateTableSQL leaked SQLite's AUTOINCREMENT: %s", got)
	}
}

func TestPostgresCreateTableSQLBigSerial(t *testing.T) {
	gen := newPostgresGen(t)
	m := gen.Define("Big", Attributes{
		"id":   {Type: BIGINT(), PrimaryKey: true, AutoIncrement: true},
		"name": {Type: STRING(5)},
	}, Timestamps(false))
	got, err := m.CreateTableSQL()
	if err != nil {
		t.Fatalf("CreateTableSQL: %v", err)
	}
	if !strings.Contains(got, `"id" BIGSERIAL PRIMARY KEY`) {
		t.Errorf("CreateTableSQL = %s, want BIGSERIAL", got)
	}
}
