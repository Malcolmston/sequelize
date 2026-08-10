package sequelize

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateReturnsTheStoredRow(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	row, err := user.Create(ctx, Values{"name": "hopper", "email": "h@example.com", "age": 85})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, ok := row["id"].(int64)
	if !ok || id == 0 {
		t.Fatalf("Create did not report the generated key: %#v", row["id"])
	}
	if row["admin"] != false {
		t.Errorf("admin = %#v, want the attribute default", row["admin"])
	}
	if _, ok := row[DefaultCreatedAt].(time.Time); !ok {
		t.Errorf("createdAt = %#v, want a stamped time", row[DefaultCreatedAt])
	}

	stored, err := user.FindByPk(ctx, id)
	if err != nil {
		t.Fatalf("FindByPk: %v", err)
	}
	if stored["name"] != "hopper" || stored["age"] != int64(85) {
		t.Errorf("stored row = %v", stored)
	}
}

func TestCreateStoresEveryDataType(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Everything", Attributes{
		"i": {Type: INTEGER()},
		"b": {Type: BIGINT()},
		"f": {Type: FLOAT()},
		"d": {Type: DECIMAL(10, 2)},
		"s": {Type: STRING(20)},
		"t": {Type: TEXT()},
		"o": {Type: BOOLEAN()},
		"w": {Type: DATE()},
		"y": {Type: DATEONLY()},
		"j": {Type: JSON()},
		"l": {Type: BLOB()},
		"u": {Type: UUID()},
		"e": {Type: ENUM("red", "green")},
	}, Timestamps(false))
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	when := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	in := Values{
		"i": 1, "b": int64(2), "f": 3.5, "d": "4.25", "s": "five", "t": "six",
		"o": true, "w": when, "y": when, "j": map[string]any{"k": "v"},
		"l": []byte{1, 2, 3}, "u": "8b1c0e6e-0000-4000-8000-000000000000", "e": "green",
	}
	created, err := m.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row, err := m.FindByPk(ctx, created["id"])
	if err != nil {
		t.Fatalf("FindByPk: %v", err)
	}
	if row["i"] != int64(1) || row["b"] != int64(2) || row["f"] != 3.5 || row["d"] != "4.25" {
		t.Errorf("numeric round trip: %v", row)
	}
	if row["s"] != "five" || row["t"] != "six" || row["u"] != in["u"] || row["e"] != "green" {
		t.Errorf("string round trip: %v", row)
	}
	if row["o"] != true {
		t.Errorf("bool round trip = %#v", row["o"])
	}
	if !row["w"].(time.Time).Equal(when) {
		t.Errorf("DATE round trip = %v, want %v", row["w"], when)
	}
	if got := row["y"].(time.Time); got.Year() != 2024 || got.Hour() != 0 {
		t.Errorf("DATEONLY round trip = %v", got)
	}
	if got := row["j"].(map[string]any); got["k"] != "v" {
		t.Errorf("JSON round trip = %v", got)
	}
	if got := row["l"].([]byte); len(got) != 3 || got[0] != 1 {
		t.Errorf("BLOB round trip = %v", got)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	if _, err := user.Create(ctx, nil); !errors.Is(err, ErrNoValues) {
		t.Errorf("Create(nil) = %v, want ErrNoValues", err)
	}
	// An empty Values on a model with timestamps and defaults is not empty by the
	// time it reaches the database, so the ErrNoValues case needs a bare model.
	bare := user.seq.Define("Bare", Attributes{"name": {Type: STRING(4)}}, Timestamps(false))
	if _, err := bare.Create(ctx, Values{}); !errors.Is(err, ErrNoValues) {
		t.Errorf("Create(empty) = %v, want ErrNoValues", err)
	}
	if _, err := user.Create(ctx, Values{"nope": 1}); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("Create with an unknown attribute = %v, want ErrUnknownAttribute", err)
	}
	if _, err := user.Create(ctx, Values{"name": "x"}, Query{}, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Create with two queries = %v, want ErrInvalidQuery", err)
	}
}

func TestCreateReportsAUniqueConstraintAsATypedError(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	_, err := user.Create(ctx, Values{"name": "clone", "email": "ada@example.com"})
	if !errors.Is(err, ErrUniqueConstraint) {
		t.Fatalf("Create with a duplicate email = %v, want ErrUniqueConstraint", err)
	}
	var uce *UniqueConstraintError
	if !errors.As(err, &uce) {
		t.Fatalf("error = %#v, want a *UniqueConstraintError", err)
	}
	if uce.Model != "User" {
		t.Errorf("Model = %q, want User", uce.Model)
	}
	if len(uce.Fields) != 1 || uce.Fields[0] != "email" {
		t.Errorf("Fields = %v, want [email]", uce.Fields)
	}
}

func TestBulkCreateAssignsEveryGeneratedKey(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Item", Attributes{"name": {Type: STRING(10)}}, Timestamps(false))
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	rows, err := m.BulkCreate(ctx, []Values{{"name": "a"}, {"name": "b"}, {"name": "c"}})
	if err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	seen := map[int64]bool{}
	for _, row := range rows {
		id, ok := row["id"].(int64)
		if !ok || id == 0 {
			t.Fatalf("row %v has no generated key", row)
		}
		if seen[id] {
			t.Fatalf("duplicate generated key %d", id)
		}
		seen[id] = true
	}
	if n, err := m.Count(ctx, Query{}); err != nil || n != 3 {
		t.Errorf("Count = %d, %v; want 3", n, err)
	}
}

func TestBulkCreateWithSuppliedKeysUsesOneStatement(t *testing.T) {
	db := newTestDB(t)
	var statements []string
	db2 := newTestDB(t, WithLogger(func(_ context.Context, q string, _ []any) {
		statements = append(statements, q)
	}))
	_ = db
	m := db2.Define("Fixed", Attributes{
		"id":   {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
		"name": {Type: STRING(10)},
	}, Timestamps(false))
	ctx := context.Background()
	if err := db2.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	before := len(statements)
	if _, err := m.BulkCreate(ctx, []Values{{"id": 1, "name": "a"}, {"id": 2, "name": "b"}}); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	if got := len(statements) - before; got != 1 {
		t.Errorf("BulkCreate ran %d statements, want a single multi-row INSERT", got)
	}
	if n, err := m.Count(ctx, Query{}); err != nil || n != 2 {
		t.Errorf("Count = %d, %v; want 2", n, err)
	}
}

func TestBulkCreateRejectsNoRows(t *testing.T) {
	_, user := usersFixture(t)
	if _, err := user.BulkCreate(context.Background(), nil); !errors.Is(err, ErrNoValues) {
		t.Errorf("BulkCreate(nil) = %v, want ErrNoValues", err)
	}
}

// TestUpdateMovesUpdatedAtButNotCreatedAt is the parity case for timestamps.
func TestUpdateMovesUpdatedAtButNotCreatedAt(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock, advance := fixedClock(start)
	db := newTestDB(t, WithClock(clock))
	m := db.Define("Note", Attributes{"body": {Type: STRING(20)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	created, err := m.Create(ctx, Values{"body": "first"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created[DefaultCreatedAt].(time.Time).Equal(start) {
		t.Fatalf("createdAt = %v, want %v", created[DefaultCreatedAt], start)
	}

	advance(time.Hour)
	n, err := m.Update(ctx, Values{"body": "second"}, Query{Where: Op.Eq("id", created["id"])})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n != 1 {
		t.Fatalf("Update affected %d rows, want 1", n)
	}
	row, err := m.FindByPk(ctx, created["id"])
	if err != nil {
		t.Fatalf("FindByPk: %v", err)
	}
	if !row[DefaultCreatedAt].(time.Time).Equal(start) {
		t.Errorf("createdAt moved to %v, want it fixed at %v", row[DefaultCreatedAt], start)
	}
	if got := row[DefaultUpdatedAt].(time.Time); !got.Equal(start.Add(time.Hour)) {
		t.Errorf("updatedAt = %v, want %v", got, start.Add(time.Hour))
	}
	if row["body"] != "second" {
		t.Errorf("body = %v", row["body"])
	}
}

func TestUpdateIgnoresAnAttemptToSetCreatedAt(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	row, err := user.FindOne(ctx, Query{Where: Op.Eq("name", "ada")})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	before := row[DefaultCreatedAt].(time.Time)
	if _, err := user.Update(ctx,
		Values{"age": 37, DefaultCreatedAt: time.Unix(0, 0).UTC()},
		Query{Where: Op.Eq("id", row["id"])}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := user.FindByPk(ctx, row["id"])
	if err != nil {
		t.Fatalf("FindByPk: %v", err)
	}
	if !after[DefaultCreatedAt].(time.Time).Equal(before) {
		t.Errorf("createdAt = %v, want it unchanged at %v", after[DefaultCreatedAt], before)
	}
	if after["age"] != int64(37) {
		t.Errorf("age = %v, want the rest of the update applied", after["age"])
	}
}

func TestUpdateRejectsBadInput(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	if _, err := user.Update(ctx, Values{}, Query{}); !errors.Is(err, ErrNoValues) {
		t.Errorf("Update(empty) = %v, want ErrNoValues", err)
	}
	if _, err := user.Update(ctx, Values{DefaultCreatedAt: time.Now()}, Query{}); !errors.Is(err, ErrNoValues) {
		t.Errorf("Update(createdAt only) = %v, want ErrNoValues", err)
	}
	if _, err := user.Update(ctx, Values{"nope": 1}, Query{}); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("Update(unknown) = %v, want ErrUnknownAttribute", err)
	}
}

func TestDestroyRemovesRowsOnAPlainModel(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	n, err := user.Destroy(ctx, Query{Where: Op.Eq("name", "ada")})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if n != 1 {
		t.Fatalf("Destroy affected %d rows, want 1", n)
	}
	if total, err := user.Count(ctx, Query{}); err != nil || total != 2 {
		t.Errorf("Count = %d, %v; want 2", total, err)
	}
}

// paranoidFixture defines and syncs a paranoid model with two rows.
func paranoidFixture(t *testing.T) (*Sequelize, *Model) {
	t.Helper()
	db := newTestDB(t)
	m := db.Define("Task", Attributes{
		"title": {Type: STRING(20), AllowNull: NotNull()},
	}, Paranoid(true))
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.BulkCreate(ctx, []Values{{"title": "keep"}, {"title": "drop"}}); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	return db, m
}

// TestParanoidDeleteHidesAndRestoreBringsBack is the parity case for soft deletes.
func TestParanoidDeleteHidesAndRestoreBringsBack(t *testing.T) {
	_, task := paranoidFixture(t)
	ctx := context.Background()

	n, err := task.Destroy(ctx, Query{Where: Op.Eq("title", "drop")})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if n != 1 {
		t.Fatalf("Destroy affected %d rows, want 1", n)
	}

	// Hidden from the finders...
	rows, err := task.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "keep" {
		t.Errorf("FindAll = %v, want only the surviving row", rows)
	}
	if _, err := task.FindOne(ctx, Query{Where: Op.Eq("title", "drop")}); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("FindOne of a soft-deleted row = %v, want ErrRecordNotFound", err)
	}
	if n, err := task.Count(ctx, Query{}); err != nil || n != 1 {
		t.Errorf("Count = %d, %v; want 1", n, err)
	}

	// ...but still in the table.
	all, err := task.FindAll(ctx, Query{Paranoid: Bool(false)})
	if err != nil {
		t.Fatalf("FindAll unscoped: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("FindAll with Paranoid false = %d rows, want 2", len(all))
	}
	var stamped Values
	for _, row := range all {
		if row["title"] == "drop" {
			stamped = row
		}
	}
	if _, ok := stamped[DefaultDeletedAt].(time.Time); !ok {
		t.Errorf("deletedAt = %#v, want a stamped time", stamped[DefaultDeletedAt])
	}

	restored, err := task.Restore(ctx, Query{Where: Op.Eq("title", "drop")})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored != 1 {
		t.Fatalf("Restore affected %d rows, want 1", restored)
	}
	back, err := task.FindOne(ctx, Query{Where: Op.Eq("title", "drop")})
	if err != nil {
		t.Fatalf("FindOne after Restore: %v", err)
	}
	if back[DefaultDeletedAt] != nil {
		t.Errorf("deletedAt = %v after Restore, want nil", back[DefaultDeletedAt])
	}
}

func TestParanoidForceDeleteReallyDeletes(t *testing.T) {
	_, task := paranoidFixture(t)
	ctx := context.Background()
	if _, err := task.Destroy(ctx, Query{Where: Op.Eq("title", "drop"), Paranoid: Bool(false)}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	all, err := task.FindAll(ctx, Query{Paranoid: Bool(false)})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("a forced Destroy left %d rows, want 1", len(all))
	}
}

func TestRestoreOnAPlainModelIsAnError(t *testing.T) {
	_, user := usersFixture(t)
	if _, err := user.Restore(context.Background(), Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Restore on a plain model = %v, want ErrInvalidQuery", err)
	}
}

// TestUpsertReportsInsertion is the parity case: the bool distinguishes an insert
// from an update.
func TestUpsertReportsInsertion(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()

	row, inserted, err := user.Upsert(ctx, Values{"name": "new", "email": "new@example.com", "age": 1})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !inserted {
		t.Fatal("Upsert of an absent row reported inserted=false")
	}
	if row["id"] == nil {
		t.Errorf("inserted row has no key: %v", row)
	}

	updated, inserted, err := user.Upsert(ctx, Values{"email": "new@example.com", "name": "new", "age": 2})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if inserted {
		t.Fatal("Upsert of an existing row reported inserted=true")
	}
	if updated["age"] != 2 {
		t.Errorf("age = %v, want the new value", updated["age"])
	}
	stored, err := user.FindOne(ctx, Query{Where: Op.Eq("email", "new@example.com")})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if stored["age"] != int64(2) {
		t.Errorf("stored age = %v, want 2", stored["age"])
	}
	if n, err := user.Count(ctx, Query{}); err != nil || n != 4 {
		t.Errorf("Count = %d, %v; want exactly one new row", n, err)
	}
}

func TestUpsertOnThePrimaryKey(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	first, err := user.FindOne(ctx, Query{Where: Op.Eq("name", "ada")})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	_, inserted, err := user.Upsert(ctx, Values{"id": first["id"], "name": "ada2"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if inserted {
		t.Error("Upsert on an existing primary key reported an insert")
	}
	row, err := user.FindByPk(ctx, first["id"])
	if err != nil {
		t.Fatalf("FindByPk: %v", err)
	}
	if row["name"] != "ada2" {
		t.Errorf("name = %v, want the upserted value", row["name"])
	}
}

func TestUpsertWithNothingToConflictOn(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Loose", Attributes{"name": {Type: STRING(10)}}, Timestamps(false))
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, _, err := m.Upsert(ctx, Values{"name": "x"}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Upsert = %v, want ErrInvalidQuery", err)
	}
	if _, _, err := m.Upsert(ctx, Values{}); !errors.Is(err, ErrNoValues) {
		t.Errorf("Upsert(empty) = %v, want ErrNoValues", err)
	}
}

func TestIncrementAndDecrement(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	where := Query{Where: Op.Eq("name", "ada")}

	if _, err := user.Increment(ctx, Values{"age": 4}, where); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	row, err := user.FindOne(ctx, where)
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if row["age"] != int64(40) {
		t.Errorf("age = %v, want 40", row["age"])
	}

	if _, err := user.Decrement(ctx, Values{"age": 10}, where); err != nil {
		t.Fatalf("Decrement: %v", err)
	}
	row, err = user.FindOne(ctx, where)
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if row["age"] != int64(30) {
		t.Errorf("age = %v, want 30", row["age"])
	}
}

func TestIncrementRejectsBadInput(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	if _, err := user.Increment(ctx, Values{}, Query{}); !errors.Is(err, ErrNoValues) {
		t.Errorf("Increment(empty) = %v, want ErrNoValues", err)
	}
	if _, err := user.Increment(ctx, Values{"nope": 1}, Query{}); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("Increment(unknown) = %v, want ErrUnknownAttribute", err)
	}
	if _, err := user.Decrement(ctx, Values{"age": "lots"}, Query{}); !errors.Is(err, ErrInvalidType) {
		t.Errorf("Decrement by a string = %v, want ErrInvalidType", err)
	}
}

func TestMutationsOnABrokenModel(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Broken", Attributes{"x": {}})
	ctx := context.Background()
	if _, err := m.Create(ctx, Values{"x": 1}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Create = %v", err)
	}
	if _, err := m.BulkCreate(ctx, []Values{{"x": 1}}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("BulkCreate = %v", err)
	}
	if _, err := m.Update(ctx, Values{"x": 1}, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Update = %v", err)
	}
	if _, err := m.Destroy(ctx, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Destroy = %v", err)
	}
	if _, _, err := m.Upsert(ctx, Values{"x": 1}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Upsert = %v", err)
	}
	if _, err := m.Increment(ctx, Values{"x": 1}, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Increment = %v", err)
	}
	if _, err := m.Restore(ctx, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Restore = %v", err)
	}
}

func TestNegate(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{1, -1}, {int8(1), int8(-1)}, {int16(1), int16(-1)}, {int32(1), int32(-1)},
		{int64(1), int64(-1)}, {float32(1.5), float32(-1.5)}, {1.5, -1.5},
	}
	for _, c := range cases {
		got, err := negate(c.in)
		if err != nil {
			t.Fatalf("negate(%v): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("negate(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := negate("x"); !errors.Is(err, ErrInvalidType) {
		t.Errorf("negate(string) = %v, want ErrInvalidType", err)
	}
}
