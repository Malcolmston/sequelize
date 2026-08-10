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
