package sequelize

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateIdentifierAccepts(t *testing.T) {
	for _, name := range []string{"id", "_id", "userName", "a1", "A_B_9", "created_at"} {
		if err := ValidateIdentifier(name); err != nil {
			t.Errorf("ValidateIdentifier(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateIdentifierRejects(t *testing.T) {
	bad := []string{
		"", " id", "id ", "1id", "user-name", "user.name", `users"`, "drop table",
		"users; DROP TABLE users", "naïve", "a\nb", "*",
	}
	for _, name := range bad {
		err := ValidateIdentifier(name)
		if !errors.Is(err, ErrInvalidIdentifier) {
			t.Errorf("ValidateIdentifier(%q) = %v, want ErrInvalidIdentifier", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
			t.Errorf("ValidateIdentifier(%q) error %q does not quote the offending name", name, err)
		}
	}
}

func TestDialectByName(t *testing.T) {
	for _, name := range []string{"sqlite", "SQLite", " sqlite3 "} {
		d, err := DialectByName(name)
		if err != nil {
			t.Fatalf("DialectByName(%q) = %v", name, err)
		}
		if !strings.HasPrefix(d.Name(), "sqlite") {
			t.Errorf("DialectByName(%q).Name() = %q", name, d.Name())
		}
	}
	if _, err := DialectByName("oracle"); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("DialectByName(oracle) = %v, want ErrUnknownDialect", err)
	}
}

func TestDialectsListsSQLite(t *testing.T) {
	names := Dialects()
	found := 0
	for _, n := range names {
		if n == "sqlite" || n == "sqlite3" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("Dialects() = %v, want it to contain sqlite and sqlite3", names)
	}
}

func TestRegisterRejectsDuplicateAndNil(t *testing.T) {
	if err := Register(sqliteDialect{name: "sqlite"}); !errors.Is(err, ErrDialectRegistered) {
		t.Errorf("Register(duplicate) = %v, want ErrDialectRegistered", err)
	}
	if err := Register(nil); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("Register(nil) = %v, want ErrUnknownDialect", err)
	}
	if err := Register(sqliteDialect{name: "  "}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("Register(empty name) = %v, want ErrInvalidIdentifier", err)
	}
}

// plainDialect implements Dialect and none of the optional capability
// interfaces, so the Dialect* helpers must fall back for it. It stands in for a
// third-party dialect written against the v0.1.0 interface, which is exactly
// what the optional-interface design has to keep working.
type plainDialect struct{ returning bool }

func (d plainDialect) Name() string { return "plain" }
func (d plainDialect) Quote(name string) (string, error) {
	if err := ValidateIdentifier(name); err != nil {
		return "", err
	}
	return "[" + name + "]", nil
}
func (d plainDialect) Placeholder(int) string                   { return "?" }
func (d plainDialect) ColumnType(DataType) (string, error)      { return "TEXT", nil }
func (d plainDialect) BindValue(_ DataType, v any) (any, error) { return v, nil }
func (d plainDialect) TranslateError(err error) error           { return err }
func (d plainDialect) SupportsReturning() bool                  { return d.returning }
func (d plainDialect) AutoIncrementColumn(quoted string, _ DataType) (string, error) {
	return quoted + " INTEGER PRIMARY KEY", nil
}

func TestDialectColumnTypeFallsBackToColumnType(t *testing.T) {
	// SQLite implements no ColumnTyper, so naming a column changes nothing.
	got, err := DialectColumnType(SQLite, "users", "role", ENUM("a"))
	if err != nil {
		t.Fatalf("DialectColumnType: %v", err)
	}
	if got != "TEXT" {
		t.Errorf("DialectColumnType = %q, want TEXT", got)
	}
	// An empty table or column also selects the plain rendering, even on a
	// dialect that does implement ColumnTyper.
	got, err = DialectColumnType(Postgres, "", "", ENUM("a"))
	if err != nil {
		t.Fatalf("DialectColumnType: %v", err)
	}
	if got != "TEXT" {
		t.Errorf("DialectColumnType(no column) = %q, want TEXT", got)
	}
}

func TestDialectReturningGenericSpelling(t *testing.T) {
	// A dialect with no Returner of its own gets the SQL-standard clause, quoted
	// its own way.
	got, err := DialectReturning(plainDialect{returning: true}, []string{"id", "name"})
	if err != nil {
		t.Fatalf("DialectReturning: %v", err)
	}
	if want := " RETURNING [id], [name]"; got != want {
		t.Errorf("DialectReturning = %q, want %q", got, want)
	}
	if _, err := DialectReturning(plainDialect{returning: false}, []string{"id"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DialectReturning(no support) = %v, want ErrUnsupported", err)
	}
	if _, err := DialectReturning(plainDialect{returning: true}, nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("DialectReturning(no columns) = %v, want ErrInvalidQuery", err)
	}
	// SQLite does support RETURNING, and gets the same standard spelling.
	got, err = DialectReturning(SQLite, []string{"id"})
	if err != nil {
		t.Fatalf("DialectReturning(sqlite): %v", err)
	}
	if want := ` RETURNING "id"`; got != want {
		t.Errorf("DialectReturning(sqlite) = %q, want %q", got, want)
	}
}

func TestDialectUpsertNeedsANativeClause(t *testing.T) {
	// There is no portable ON CONFLICT, so a dialect without one says so rather
	// than emitting SQL its backend will reject.
	if _, err := DialectUpsert(SQLite, []string{"id"}, []string{"name"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DialectUpsert(sqlite) = %v, want ErrUnsupported", err)
	}
	if _, err := DialectUpsert(Postgres, nil, nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("DialectUpsert(no conflict columns) = %v, want ErrInvalidQuery", err)
	}
}

func TestDialectSupportsOperatorDefaultsConservatively(t *testing.T) {
	d := plainDialect{}
	for _, op := range []Operator{OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpLike, OpNotLike} {
		if !DialectSupportsOperator(d, op) {
			t.Errorf("DialectSupportsOperator(plain, %s) = false, want true", op)
		}
	}
	for _, op := range []Operator{OpILike, OpNotILike} {
		if DialectSupportsOperator(d, op) {
			t.Errorf("DialectSupportsOperator(plain, %s) = true; ILIKE is not portable", op)
		}
	}
}

func TestDialectTableDDLIsOptional(t *testing.T) {
	cols := []ColumnDef{{Column: "role", Type: ENUM("a")}}
	before, err := DialectBeforeCreateTable(SQLite, "users", cols)
	if err != nil || before != nil {
		t.Errorf("DialectBeforeCreateTable(sqlite) = %v, %v; want nil, nil", before, err)
	}
	after, err := DialectAfterDropTable(SQLite, "users", cols)
	if err != nil || after != nil {
		t.Errorf("DialectAfterDropTable(sqlite) = %v, %v; want nil, nil", after, err)
	}
}

func TestDialectHelpersRejectNilDialect(t *testing.T) {
	if _, err := DialectColumnType(nil, "t", "c", TEXT()); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("DialectColumnType(nil) = %v, want ErrUnknownDialect", err)
	}
	if _, err := DialectReturning(nil, []string{"id"}); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("DialectReturning(nil) = %v, want ErrUnknownDialect", err)
	}
	if _, err := DialectUpsert(nil, []string{"id"}, nil); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("DialectUpsert(nil) = %v, want ErrUnknownDialect", err)
	}
	if _, err := DialectBeforeCreateTable(nil, "t", nil); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("DialectBeforeCreateTable(nil) = %v, want ErrUnknownDialect", err)
	}
	if _, err := DialectAfterDropTable(nil, "t", nil); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("DialectAfterDropTable(nil) = %v, want ErrUnknownDialect", err)
	}
	if DialectSupportsOperator(nil, OpEq) {
		t.Error("DialectSupportsOperator(nil) = true")
	}
}

func TestRegisterAndLookupRoundTrip(t *testing.T) {
	const name = "sqlite_test_clone"
	if err := Register(sqliteDialect{name: name}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() {
		dialectsMu.Lock()
		delete(dialects, name)
		dialectsMu.Unlock()
	})
	d, err := DialectByName(name)
	if err != nil {
		t.Fatalf("DialectByName: %v", err)
	}
	if d.Name() != name {
		t.Errorf("Name() = %q, want %q", d.Name(), name)
	}
}
