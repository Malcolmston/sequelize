package sequelize

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newTestDB opens a connection against a throwaway SQLite file. A file rather
// than ":memory:" because database/sql pools connections and each pooled
// connection to an in-memory SQLite gets its own empty database.
func newTestDB(t *testing.T, opts ...Option) *Sequelize {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db") + "?_pragma=foreign_keys(1)"
	db, err := New("sqlite", dsn, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

// fixedClock returns a clock that reports the same instant every time, and a
// function to move it.
func fixedClock(start time.Time) (func() time.Time, func(d time.Duration)) {
	now := start
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

func TestNewUnknownDialect(t *testing.T) {
	if _, err := New("oracle", "dsn"); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("New(oracle) = %v, want ErrUnknownDialect", err)
	}
}

func TestNewUnknownDriverFailsOnUse(t *testing.T) {
	db, err := New("sqlite", "file:whatever", WithDriver("no_such_driver"))
	if err == nil {
		// sql.Open may defer the failure; either way it must not succeed in use.
		if pingErr := db.Ping(context.Background()); pingErr == nil {
			t.Error("a bogus driver name produced a usable connection")
		}
		_ = db.Close()
		return
	}
}

func TestOpenAdoptsHandleAndDoesNotCloseIt(t *testing.T) {
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "adopted.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	db, err := Open("sqlite", raw)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db.DB() != raw {
		t.Error("DB() did not return the adopted handle")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The handle is still the caller's, so it must still work.
	if err := raw.PingContext(context.Background()); err != nil {
		t.Errorf("Close closed a handle it did not open: %v", err)
	}
}

func TestOpenRejectsNilHandleAndBadDialect(t *testing.T) {
	if _, err := Open("sqlite", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Open(nil) = %v, want ErrClosed", err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := Open("oracle", raw); !errors.Is(err, ErrUnknownDialect) {
		t.Errorf("Open(oracle) = %v, want ErrUnknownDialect", err)
	}
}

func TestDialectAccessor(t *testing.T) {
	db := newTestDB(t)
	if db.Dialect().Name() != "sqlite" {
		t.Errorf("Dialect().Name() = %q", db.Dialect().Name())
	}
}

func TestDefineAndLookup(t *testing.T) {
	db := newTestDB(t)
	user := db.Define("User", Attributes{"name": {Type: STRING(10)}})
	if err := user.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	got, ok := db.Model("User")
	if !ok || got != user {
		t.Fatalf("Model(User) = %v, %v", got, ok)
	}
	if _, ok := db.Model("Nope"); ok {
		t.Error("Model(Nope) reported a model that was never defined")
	}
	if _, err := db.MustModel("Nope"); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("MustModel(Nope) = %v, want ErrUnknownModel", err)
	}
	if m, err := db.MustModel("User"); err != nil || m != user {
		t.Errorf("MustModel(User) = %v, %v", m, err)
	}
	db.Define("Post", Attributes{"title": {Type: STRING(10)}})
	if got := db.ModelNames(); len(got) != 2 || got[0] != "Post" || got[1] != "User" {
		t.Errorf("ModelNames() = %v, want [Post User]", got)
	}
}

func TestDefineRejectsRedefinition(t *testing.T) {
	db := newTestDB(t)
	db.Define("User", Attributes{"name": {Type: STRING(10)}})
	again := db.Define("User", Attributes{"name": {Type: STRING(10)}})
	if !errors.Is(again.Err(), ErrModelRedefined) {
		t.Fatalf("Err() = %v, want ErrModelRedefined", again.Err())
	}
	if _, err := db.DefineModel("User", Attributes{"name": {Type: STRING(10)}}); !errors.Is(err, ErrModelRedefined) {
		t.Errorf("DefineModel = %v, want ErrModelRedefined", err)
	}
	// The original definition survives.
	first, _ := db.Model("User")
	if first.Err() != nil {
		t.Errorf("the first definition was damaged: %v", first.Err())
	}
}

func TestDefineModelReturnsTheError(t *testing.T) {
	db := newTestDB(t)
	m, err := db.DefineModel("Bad", Attributes{"x": {}})
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("DefineModel = %v, want ErrInvalidQuery", err)
	}
	if m == nil {
		t.Fatal("DefineModel returned no model to inspect")
	}
	if !errors.Is(m.Err(), ErrInvalidQuery) {
		t.Errorf("Err() = %v", m.Err())
	}
	if names := db.ModelNames(); len(names) != 0 {
		t.Errorf("a rejected definition was registered: %v", names)
	}
}

func TestDefineNilOptionIsIgnored(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Nil", Attributes{"a": {Type: INTEGER()}}, nil)
	if err := m.Err(); err != nil {
		t.Fatalf("a nil ModelOption was not ignored: %v", err)
	}
}

func TestCloseIsIdempotentAndBlocksUse(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "closed.db")
	db, err := New("sqlite", dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := db.Define("User", Attributes{"name": {Type: STRING(10)}})
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if err := db.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Ping after Close = %v, want ErrClosed", err)
	}
	if _, err := m.FindAll(context.Background(), Query{}); !errors.Is(err, ErrClosed) {
		t.Errorf("FindAll after Close = %v, want ErrClosed", err)
	}
	if _, err := db.Query(context.Background(), "SELECT 1"); !errors.Is(err, ErrClosed) {
		t.Errorf("Query after Close = %v, want ErrClosed", err)
	}
}

func TestPingReachesTheDatabase(t *testing.T) {
	db := newTestDB(t)
	if err := db.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestWithLoggerSeesEveryStatement(t *testing.T) {
	var statements []string
	db := newTestDB(t, WithLogger(func(_ context.Context, query string, _ []any) {
		statements = append(statements, query)
	}))
	m := db.Define("Logged", Attributes{"name": {Type: STRING(10)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.Create(ctx, Values{"name": "x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(statements) < 2 {
		t.Fatalf("logger saw %d statements, want the DDL and the insert", len(statements))
	}
	if statements[0][:6] != "CREATE" {
		t.Errorf("first logged statement = %q", statements[0])
	}
}

func TestWithClockDrivesTimestamps(t *testing.T) {
	clock, _ := fixedClock(time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC))
	db := newTestDB(t, WithClock(clock))
	if !db.now().Equal(clock()) {
		t.Errorf("now() = %v, want the injected clock's %v", db.now(), clock())
	}
}
