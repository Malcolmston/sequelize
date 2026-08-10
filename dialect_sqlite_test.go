package sequelize

import (
	"errors"
	"fmt"
	"testing"
)

func TestSQLiteQuote(t *testing.T) {
	got, err := SQLite.Quote("user_name")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if want := `"user_name"`; got != want {
		t.Errorf("Quote = %q, want %q", got, want)
	}
}

func TestSQLiteQuoteRefusesToEscape(t *testing.T) {
	// The point of the rule: a hostile name is an error, not a quoted string.
	for _, name := range []string{`a" ; DROP TABLE users --`, "a b", ""} {
		got, err := SQLite.Quote(name)
		if !errors.Is(err, ErrInvalidIdentifier) {
			t.Errorf("Quote(%q) = %q, %v; want ErrInvalidIdentifier", name, got, err)
		}
		if got != "" {
			t.Errorf("Quote(%q) returned %q alongside an error", name, got)
		}
	}
}

func TestSQLitePlaceholderIsPositionless(t *testing.T) {
	for i := 1; i < 4; i++ {
		if got := SQLite.Placeholder(i); got != "?" {
			t.Errorf("Placeholder(%d) = %q, want ?", i, got)
		}
	}
}

func TestSQLiteColumnTypes(t *testing.T) {
	cases := []struct {
		dt   DataType
		want string
	}{
		{INTEGER(), "INTEGER"},
		{BIGINT(), "INTEGER"},
		{FLOAT(), "REAL"},
		{DECIMAL(), "NUMERIC"},
		{DECIMAL(10, 2), "NUMERIC(10,2)"},
		{STRING(64), "VARCHAR(64)"},
		{STRING(0), "VARCHAR(255)"},
		{TEXT(), "TEXT"},
		{JSON(), "TEXT"},
		{BOOLEAN(), "BOOLEAN"},
		{DATE(), "DATETIME"},
		{DATEONLY(), "DATE"},
		{BLOB(), "BLOB"},
		{UUID(), "VARCHAR(36)"},
		{ENUM("a"), "TEXT"},
	}
	for _, c := range cases {
		got, err := SQLite.ColumnType(c.dt)
		if err != nil {
			t.Fatalf("ColumnType(%s): %v", c.dt, err)
		}
		if got != c.want {
			t.Errorf("ColumnType(%s) = %q, want %q", c.dt, got, c.want)
		}
	}
}

func TestSQLiteColumnTypeRejectsUnmapped(t *testing.T) {
	if _, err := SQLite.ColumnType(ENUM()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ColumnType(empty ENUM) = %v, want ErrUnsupported", err)
	}
	if _, err := SQLite.ColumnType(DataType{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ColumnType(zero) = %v, want ErrUnsupported", err)
	}
}

func TestSQLiteAutoIncrementColumn(t *testing.T) {
	got, err := SQLite.AutoIncrementColumn(`"id"`, INTEGER())
	if err != nil {
		t.Fatalf("AutoIncrementColumn: %v", err)
	}
	if want := `"id" INTEGER PRIMARY KEY AUTOINCREMENT`; got != want {
		t.Errorf("AutoIncrementColumn = %q, want %q", got, want)
	}
	if _, err := SQLite.AutoIncrementColumn(`"id"`, STRING(10)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("AutoIncrementColumn(STRING) = %v, want ErrUnsupported", err)
	}
}

func TestSQLiteBindValue(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{nil, nil},
		{true, int64(1)},
		{false, int64(0)},
		{int(3), int64(3)},
		{int32(3), int64(3)},
		{uint16(3), int64(3)},
		{float32(1.5), float64(1.5)},
		{"x", "x"},
	}
	for _, c := range cases {
		got, err := SQLite.BindValue(DataType{}, c.in)
		if err != nil {
			t.Fatalf("BindValue(%v): %v", c.in, err)
		}
		if fmt.Sprintf("%T %v", got, got) != fmt.Sprintf("%T %v", c.want, c.want) {
			t.Errorf("BindValue(%v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestSQLiteTranslateError(t *testing.T) {
	unique := SQLite.TranslateError(errors.New("constraint failed: UNIQUE constraint failed: users.email (2067)"))
	if !errors.Is(unique, ErrUniqueConstraint) {
		t.Fatalf("TranslateError(unique) = %v", unique)
	}
	var uce *UniqueConstraintError
	if !errors.As(unique, &uce) {
		t.Fatal("TranslateError did not produce a *UniqueConstraintError")
	}
	if len(uce.Fields) != 1 || uce.Fields[0] != "email" {
		t.Errorf("Fields = %v, want [email]", uce.Fields)
	}

	pk := SQLite.TranslateError(errors.New("PRIMARY KEY constraint failed: users.id"))
	if !errors.Is(pk, ErrUniqueConstraint) {
		t.Errorf("TranslateError(primary key) = %v, want ErrUniqueConstraint", pk)
	}

	fk := SQLite.TranslateError(errors.New("FOREIGN KEY constraint failed"))
	if !errors.Is(fk, ErrForeignKeyConstraint) {
		t.Errorf("TranslateError(foreign key) = %v, want ErrForeignKeyConstraint", fk)
	}

	other := errors.New("disk I/O error")
	if got := SQLite.TranslateError(other); got != other {
		t.Errorf("TranslateError(unrecognised) = %v, want it unchanged", got)
	}
	if got := SQLite.TranslateError(nil); got != nil {
		t.Errorf("TranslateError(nil) = %v, want nil", got)
	}
}

func TestSQLiteConstraintFieldsMultiColumn(t *testing.T) {
	got := sqliteConstraintFields("UNIQUE constraint failed: t.a, t.b (2067)", "UNIQUE constraint failed:")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("sqliteConstraintFields = %v, want [a b]", got)
	}
	if got := sqliteConstraintFields("no marker here", "UNIQUE constraint failed:"); got != nil {
		t.Errorf("sqliteConstraintFields with no marker = %v, want nil", got)
	}
}

func TestSQLiteSupportsReturning(t *testing.T) {
	if !SQLite.SupportsReturning() {
		t.Error("SupportsReturning() = false")
	}
}
