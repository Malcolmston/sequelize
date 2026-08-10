package sequelize

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCreateTableSQL(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{
		"name":  {Type: STRING(20), AllowNull: NotNull()},
		"sku":   {Type: STRING(10), Unique: true},
		"price": {Type: FLOAT(), DefaultValue: 1.5},
		"note":  {Type: TEXT()},
	}, Timestamps(false))
	got, err := m.CreateTableSQL()
	if err != nil {
		t.Fatalf("CreateTableSQL: %v", err)
	}
	want := `CREATE TABLE IF NOT EXISTS "widgets" (` +
		`"id" INTEGER PRIMARY KEY AUTOINCREMENT, ` +
		`"name" VARCHAR(20) NOT NULL, ` +
		`"note" TEXT, ` +
		`"price" REAL DEFAULT 1.5, ` +
		`"sku" VARCHAR(10) UNIQUE)`
	if got != want {
		t.Errorf("CreateTableSQL =\n%s\nwant\n%s", got, want)
	}
}

func TestCreateTableSQLTimestampsAndParanoid(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Task", Attributes{"title": {Type: STRING(10)}}, Paranoid(true))
	got, err := m.CreateTableSQL()
	if err != nil {
		t.Fatalf("CreateTableSQL: %v", err)
	}
	for _, want := range []string{
		`"created_at" DATETIME NOT NULL`,
		`"updated_at" DATETIME NOT NULL`,
		`"deleted_at" DATETIME`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CreateTableSQL = %s, want it to contain %s", got, want)
		}
	}
	if strings.Contains(got, `"deleted_at" DATETIME NOT NULL`) {
		t.Errorf("deletedAt must be nullable: %s", got)
	}
}

func TestCreateTableSQLCompositePrimaryKey(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Membership", Attributes{
		"userID":  {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
		"groupID": {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
	}, Timestamps(false))
	got, err := m.CreateTableSQL()
	if err != nil {
		t.Fatalf("CreateTableSQL: %v", err)
	}
	if !strings.Contains(got, `PRIMARY KEY ("group_id", "user_id")`) {
		t.Errorf("CreateTableSQL = %s, want a table-level PRIMARY KEY", got)
	}
	if strings.Contains(got, "AUTOINCREMENT") {
		t.Errorf("a composite key must not auto-increment: %s", got)
	}
}

func TestCreateTableSQLReferences(t *testing.T) {
	db := newTestDB(t)
	user := db.Define("User", Attributes{"name": {Type: STRING(10)}}, Timestamps(false))
	post := db.Define("Post", Attributes{
		"userID": {Type: INTEGER(), References: &Reference{Model: user, OnDelete: "cascade"}},
		"other":  {Type: INTEGER(), References: &Reference{Table: "users", Key: "id", OnUpdate: "SET NULL"}},
	}, Timestamps(false))
	got, err := post.CreateTableSQL()
	if err != nil {
		t.Fatalf("CreateTableSQL: %v", err)
	}
	if !strings.Contains(got, `"user_id" INTEGER REFERENCES "users" ("id") ON DELETE CASCADE`) {
		t.Errorf("CreateTableSQL = %s, want a resolved model reference", got)
	}
	if !strings.Contains(got, `"other" INTEGER REFERENCES "users" ("id") ON UPDATE SET NULL`) {
		t.Errorf("CreateTableSQL = %s, want an explicit table reference", got)
	}
}

func TestCreateTableSQLRejectsAReferenceToACompositeKey(t *testing.T) {
	db := newTestDB(t)
	target := db.Define("Membership", Attributes{
		"userID":  {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
		"groupID": {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
	}, Timestamps(false))
	m := db.Define("Ref", Attributes{
		"target": {Type: INTEGER(), References: &Reference{Model: target}},
	}, Timestamps(false))
	if _, err := m.CreateTableSQL(); !errors.Is(err, ErrNoPrimaryKey) {
		t.Errorf("CreateTableSQL = %v, want ErrNoPrimaryKey", err)
	}
}

func TestCreateIndexSQL(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{
		"name": {Type: STRING(10)},
		"tag":  {Type: STRING(10)},
	}, Timestamps(false), WithIndexes(
		Index{Fields: []string{"name", "tag"}},
		Index{Name: "uniq_tag", Fields: []string{"tag"}, Unique: true},
	))
	got, err := m.CreateIndexSQL()
	if err != nil {
		t.Fatalf("CreateIndexSQL: %v", err)
	}
	want := []string{
		`CREATE INDEX IF NOT EXISTS "idx_widgets_name_tag" ON "widgets" ("name", "tag")`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "uniq_tag" ON "widgets" ("tag")`,
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("CreateIndexSQL =\n%v\nwant\n%v", got, want)
	}
}

func TestDropTableSQL(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(10)}})
	got, err := m.DropTableSQL()
	if err != nil {
		t.Fatalf("DropTableSQL: %v", err)
	}
	if want := `DROP TABLE IF EXISTS "widgets"`; got != want {
		t.Errorf("DropTableSQL = %q, want %q", got, want)
	}
}

func TestSyncCreatesAWorkingTable(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(10)}},
		WithIndexes(Index{Fields: []string{"name"}}))
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Sync is idempotent thanks to IF NOT EXISTS.
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if _, err := m.Create(ctx, Values{"name": "x"}); err != nil {
		t.Fatalf("Create against the synced table: %v", err)
	}
	res, err := db.Query(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`,
		WithArgs("idx_widgets_name"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("the declared index was not created: %v", res.Rows)
	}
}

func TestSyncForceRecreatesTheTable(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(10)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.Create(ctx, Values{"name": "x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Sync(ctx, Force(true)); err != nil {
		t.Fatalf("Sync(Force): %v", err)
	}
	if n, err := m.Count(ctx, Query{}); err != nil || n != 0 {
		t.Errorf("Count = %d, %v; want an empty recreated table", n, err)
	}
}

func TestSyncInsideATransaction(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(10)}})
	ctx := context.Background()
	err := db.Transaction(ctx, func(tx *Tx) error {
		if err := db.Sync(ctx, SyncTx(tx)); err != nil {
			return err
		}
		_, err := m.Create(ctx, Values{"name": "x"}, Query{Tx: tx})
		return err
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if n, err := m.Count(ctx, Query{}); err != nil || n != 1 {
		t.Errorf("Count = %d, %v; want the committed row", n, err)
	}
}

func TestDropAndTruncate(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(10)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.BulkCreate(ctx, []Values{{"name": "a"}, {"name": "b"}}); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	if err := m.Truncate(ctx); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if n, err := m.Count(ctx, Query{}); err != nil || n != 0 {
		t.Errorf("Count after Truncate = %d, %v; want 0", n, err)
	}
	if err := db.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, err := m.Count(ctx, Query{}); err == nil {
		t.Error("Count against a dropped table succeeded")
	}
	// Drop is idempotent.
	if err := m.Drop(ctx); err != nil {
		t.Errorf("second Drop: %v", err)
	}
}

func TestSyncRefusesABrokenModel(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Broken", Attributes{"x": {}})
	ctx := context.Background()
	if err := m.Sync(ctx); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Sync = %v, want ErrInvalidQuery", err)
	}
	if err := m.Truncate(ctx); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Truncate = %v, want ErrInvalidQuery", err)
	}
	if _, err := m.DropTableSQL(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("DropTableSQL = %v, want ErrInvalidQuery", err)
	}
	if _, err := m.CreateIndexSQL(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("CreateIndexSQL = %v, want ErrInvalidQuery", err)
	}
}

func TestDefaultLiteral(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{true, "1"},
		{false, "0"},
		{3, "3"},
		{int64(-4), "-4"},
		{uint8(5), "5"},
		{1.5, "1.5"},
		{float32(2.5), "2.5"},
		{"plain", "'plain'"},
	}
	for _, c := range cases {
		got, err := defaultLiteral(c.in)
		if err != nil {
			t.Fatalf("defaultLiteral(%v): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("defaultLiteral(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDefaultLiteralRefusesAnythingItCannotProveSafe guards the one place the
// package writes a value into SQL text.
func TestDefaultLiteralRefusesAnythingItCannotProveSafe(t *testing.T) {
	bad := []any{
		"it's",
		`say "hi"`,
		`back\slash`,
		"new\nline",
		strings.Repeat("x", safeDefaultLength+1),
		[]int{1},
		struct{}{},
		map[string]any{},
	}
	for _, v := range bad {
		if got, err := defaultLiteral(v); !errors.Is(err, ErrUnsupported) {
			t.Errorf("defaultLiteral(%v) = %q, %v; want ErrUnsupported", v, got, err)
		}
	}
}

func TestCreateTableSQLRefusesAnUnrenderableDefault(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Bad", Attributes{"note": {Type: STRING(10), DefaultValue: "it's"}}, Timestamps(false))
	if _, err := m.CreateTableSQL(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CreateTableSQL = %v, want ErrUnsupported", err)
	}
}

func TestReferentialAction(t *testing.T) {
	got, err := referentialAction("set null")
	if err != nil {
		t.Fatalf("referentialAction: %v", err)
	}
	if got != "SET NULL" {
		t.Errorf("referentialAction = %q, want SET NULL", got)
	}
	if _, err := referentialAction("   "); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("referentialAction(blank) = %v, want ErrInvalidQuery", err)
	}
	if _, err := referentialAction("cascade; drop table t"); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("referentialAction(injection) = %v, want ErrInvalidIdentifier", err)
	}
}

func TestResolveSyncOptionsIgnoresNil(t *testing.T) {
	o := resolveSyncOptions([]SyncOption{nil, Force(true)})
	if !o.Force {
		t.Error("Force(true) was not applied")
	}
}
