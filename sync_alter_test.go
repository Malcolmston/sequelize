package sequelize

import (
	"context"
	"errors"
	"testing"
)

// alterFixture defines a "users" table, syncs it, and returns the connection
// together with the model. Every alter test starts from a real table.
func alterFixture(t *testing.T) (*Sequelize, *Model) {
	t.Helper()
	db := newTestDB(t)
	m := db.Define("User", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER()},
	}, Timestamps(false), WithIndexes(Index{Fields: []string{"name"}}))
	if err := m.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return db, m
}

// changed redefines the same physical table with a different attribute set, so
// that a test can express "the model changed" without redefining a model name.
func changed(t *testing.T, db *Sequelize, name string, attrs Attributes, opts ...ModelOption) *Model {
	t.Helper()
	opts = append([]ModelOption{TableName("users"), Timestamps(false)}, opts...)
	m := db.Define(name, attrs, opts...)
	if err := m.Err(); err != nil {
		t.Fatalf("Define %s: %v", name, err)
	}
	return m
}

func TestInspectTableReadsTheLiveSchema(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)

	info, err := m.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if !info.Exists {
		t.Fatal("InspectTable reported the table does not exist")
	}
	if info.Name != "users" || info.SQL == "" {
		t.Errorf("Name=%q SQL=%q, want users and a CREATE TABLE", info.Name, info.SQL)
	}
	if got, want := len(info.Columns), 4; got != want {
		t.Errorf("got %d columns, want %d: %+v", got, want, info.Columns)
	}
	id, ok := info.Column("id")
	if !ok {
		t.Fatal("no id column")
	}
	if id.PrimaryKeyPos != 1 {
		t.Errorf("id.PrimaryKeyPos = %d, want 1", id.PrimaryKeyPos)
	}
	name, ok := info.Column("name")
	if !ok {
		t.Fatal("no name column")
	}
	if name.Type != "VARCHAR(64)" || !name.NotNull {
		t.Errorf("name = %+v, want VARCHAR(64) NOT NULL", name)
	}
	if got := info.PrimaryKey(); len(got) != 1 || got[0] != "id" {
		t.Errorf("PrimaryKey() = %v, want [id]", got)
	}
	// The declared index, plus the implicit index behind email's UNIQUE.
	if _, ok := info.Index("idx_users_name"); !ok {
		t.Errorf("declared index missing from %+v", info.Indexes)
	}
	var sawUnique bool
	for _, idx := range info.Indexes {
		if idx.Origin == "u" && idx.Unique && len(idx.Columns) == 1 && idx.Columns[0] == "email" {
			sawUnique = true
		}
	}
	if !sawUnique {
		t.Errorf("the UNIQUE constraint on email was not introspected: %+v", info.Indexes)
	}

	// Foreign keys come back too.
	child := db.Define("Post", Attributes{
		"title":  {Type: STRING(64)},
		"userID": {Type: INTEGER(), References: &Reference{Model: m, OnDelete: "CASCADE"}},
	}, Timestamps(false))
	if err := child.Sync(ctx); err != nil {
		t.Fatalf("Sync child: %v", err)
	}
	childInfo, err := child.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable child: %v", err)
	}
	if len(childInfo.ForeignKeys) != 1 {
		t.Fatalf("got %d foreign keys, want 1: %+v", len(childInfo.ForeignKeys), childInfo.ForeignKeys)
	}
	fk := childInfo.ForeignKeys[0]
	if fk.Column != "user_id" || fk.Table != "users" || fk.Key != "id" || fk.OnDelete != "CASCADE" {
		t.Errorf("foreign key = %+v, want user_id -> users.id ON DELETE CASCADE", fk)
	}
}

func TestInspectTableReportsAMissingTableWithoutError(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Ghost", Attributes{"name": {Type: TEXT()}})
	info, err := m.InspectTable(context.Background())
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if info.Exists {
		t.Error("InspectTable invented a table")
	}
	if _, err := m.SchemaDiff(context.Background()); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("SchemaDiff on a missing table = %v, want ErrRecordNotFound", err)
	}
}

func TestSchemaDiffIsEmptyForAnUnchangedModel(t *testing.T) {
	_, m := alterFixture(t)
	changes, err := m.SchemaDiff(context.Background())
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("a table Sync just created differs from its model: %v", changes)
	}
}

// TestSchemaDiffIsEmptyForEveryDataType guards the type comparison against the
// dialect's own rendering: whatever ColumnType emits must read back equivalent.
func TestSchemaDiffIsEmptyForEveryDataType(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Wide", Attributes{
		"anInt":     {Type: INTEGER()},
		"aBig":      {Type: BIGINT()},
		"aFloat":    {Type: FLOAT()},
		"aDecimal":  {Type: DECIMAL(10, 2)},
		"aString":   {Type: STRING(32)},
		"aText":     {Type: TEXT()},
		"aBool":     {Type: BOOLEAN()},
		"aDate":     {Type: DATE()},
		"aDateOnly": {Type: DATEONLY()},
		"aJSON":     {Type: JSON()},
		"aBlob":     {Type: BLOB()},
		"aUUID":     {Type: UUID()},
		"anEnum":    {Type: ENUM("a", "b")},
	})
	ctx := context.Background()
	if err := m.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	changes, err := m.SchemaDiff(ctx)
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("round-tripping the dialect's own types produced a diff: %v", changes)
	}
}

// TestAlterAddsColumnsAndIndexesAndKeepsData is the round trip the whole feature
// exists for: an existing table with rows, a model that grew a column and an
// index, and a Sync that migrates rather than drops.
func TestAlterAddsColumnsAndIndexesAndKeepsData(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)
	if _, err := m.Create(ctx, Values{"name": "ada", "email": "ada@example.com", "age": 36}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	grown := changed(t, db, "UserV2", Attributes{
		"name":     {Type: STRING(64), AllowNull: NotNull()},
		"email":    {Type: STRING(255), Unique: true},
		"age":      {Type: INTEGER()},
		"nickname": {Type: STRING(32), DefaultValue: "anon"},
	}, WithIndexes(
		Index{Fields: []string{"name"}},
		Index{Fields: []string{"age"}},
	))

	changes, err := grown.SchemaDiff(ctx)
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want add column + add index: %v", len(changes), changes)
	}
	for _, c := range changes {
		if c.Destructive() || c.NeedsRebuild() {
			t.Errorf("%v was classified destructive=%v rebuild=%v", c, c.Destructive(), c.NeedsRebuild())
		}
	}

	if err := db.Sync(ctx, Alter(true)); err != nil {
		t.Fatalf("Sync(Alter): %v", err)
	}

	info, err := grown.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	col, ok := info.Column("nickname")
	if !ok {
		t.Fatalf("nickname was not added: %+v", info.Columns)
	}
	if col.Type != "VARCHAR(32)" || col.Default != "'anon'" {
		t.Errorf("nickname = %+v, want VARCHAR(32) DEFAULT 'anon'", col)
	}
	for _, want := range []string{"idx_users_name", "idx_users_age"} {
		if _, ok := info.Index(want); !ok {
			t.Errorf("index %s missing after alter: %+v", want, info.Indexes)
		}
	}

	rows, err := grown.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after alter, want the 1 that was there before", len(rows))
	}
	if rows[0]["name"] != "ada" || rows[0]["age"] != int64(36) {
		t.Errorf("row did not survive the alter: %v", rows[0])
	}
	// SQLite's ADD COLUMN fills existing rows with the column's DEFAULT, so the
	// pre-existing row picks the default up rather than reading NULL.
	if rows[0]["nickname"] != "anon" {
		t.Errorf("nickname on the pre-existing row = %v, want the column default", rows[0]["nickname"])
	}

	// Second run is a no-op.
	applied, err := grown.AlterTable(ctx)
	if err != nil {
		t.Fatalf("AlterTable again: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("a second alter wanted to change %v", applied)
	}
}

func TestAlterDropsAnIndexTheModelNoLongerDeclares(t *testing.T) {
	ctx := context.Background()
	db, _ := alterFixture(t)
	slimmed := changed(t, db, "UserNoIndex", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER()},
	})
	applied, err := slimmed.AlterTable(ctx)
	if err != nil {
		t.Fatalf("AlterTable: %v", err)
	}
	if len(applied) != 1 || applied[0].Kind != ChangeDropIndex || applied[0].Index != "idx_users_name" {
		t.Fatalf("applied = %v, want a single drop of idx_users_name", applied)
	}
	info, err := slimmed.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if _, ok := info.Index("idx_users_name"); ok {
		t.Error("idx_users_name survived the alter")
	}
	// The UNIQUE constraint's implicit index is not the model's to drop.
	var sawUnique bool
	for _, idx := range info.Indexes {
		if idx.Origin == "u" {
			sawUnique = true
		}
	}
	if !sawUnique {
		t.Errorf("alter dropped an implicit constraint index: %+v", info.Indexes)
	}
}

func TestAlterRefusesToDropAColumnWithoutTheFlag(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)
	if _, err := m.Create(ctx, Values{"name": "grace", "age": 45}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	shrunk := changed(t, db, "UserNoAge", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	_, err := shrunk.AlterTable(ctx)
	if err == nil {
		t.Fatal("AlterTable dropped a column without being asked to")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want it to wrap ErrUnsupported", err)
	}
	var refused *AlterRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %T, want *AlterRefusedError", err)
	}
	if len(refused.Changes) != 1 || refused.Changes[0].Kind != ChangeDropColumn || refused.Changes[0].Column != "age" {
		t.Errorf("refused.Changes = %v, want the drop of age", refused.Changes)
	}
	if refused.Table != "users" {
		t.Errorf("refused.Table = %q, want users", refused.Table)
	}

	// Nothing was applied: the column and its data are still there.
	info, err := shrunk.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if _, ok := info.Column("age"); !ok {
		t.Fatal("the refused alter dropped age anyway")
	}
	rows, err := m.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["age"] != int64(45) {
		t.Errorf("rows = %v, want the original row intact", rows)
	}

	// With the flag it goes through, and the surviving column keeps its data.
	applied, err := shrunk.AlterTable(ctx, AlterDrop(true))
	if err != nil {
		t.Fatalf("AlterTable(AlterDrop): %v", err)
	}
	if len(applied) != 1 || applied[0].Kind != ChangeDropColumn {
		t.Errorf("applied = %v, want the drop of age", applied)
	}
	info, err = shrunk.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if _, ok := info.Column("age"); ok {
		t.Error("age is still there after AlterDrop(true)")
	}
	rows, err = shrunk.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "grace" {
		t.Errorf("rows = %v, want the row minus the dropped column", rows)
	}
}

func TestAlterRefusesARetypeWithoutRebuild(t *testing.T) {
	ctx := context.Background()
	db, _ := alterFixture(t)
	retyped := changed(t, db, "UserWideAge", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: TEXT()},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	changes, err := retyped.SchemaDiff(ctx)
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != ChangeColumnType || !changes[0].NeedsRebuild() {
		t.Fatalf("changes = %v, want one retype needing a rebuild", changes)
	}
	if changes[0].From != "INTEGER" || changes[0].To != "TEXT" {
		t.Errorf("change = %v, want INTEGER -> TEXT", changes[0])
	}

	_, err = retyped.AlterTable(ctx)
	var refused *AlterRefusedError
	if !errors.As(err, &refused) || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("AlterTable without AlterRebuild = %v, want an *AlterRefusedError", err)
	}

	info, err := retyped.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if col, _ := info.Column("age"); col.Type != "INTEGER" {
		t.Errorf("age.Type = %q, want the refused alter to have changed nothing", col.Type)
	}
}

// TestAlterRebuildRetypesAndKeepsEverything proves the copy-and-rename strategy:
// after it, the column has its new type, every row is still there with its
// values, and the indexes are back.
func TestAlterRebuildRetypesAndKeepsEverything(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)
	for _, row := range []Values{
		{"name": "ada", "email": "ada@example.com", "age": 36},
		{"name": "grace", "email": "grace@example.com", "age": 45},
	} {
		if _, err := m.Create(ctx, row); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// A hand-made index the model does not declare. It is not silently kept and
	// not silently lost: the diff reports its drop, and the rebuild honours that.
	if _, err := db.DB().ExecContext(ctx, `CREATE INDEX handmade_users_email ON users (email)`); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	retyped := changed(t, db, "UserBigAge", Attributes{
		"name":  {Type: STRING(128), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER(), DefaultValue: 0},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	applied, err := retyped.AlterTable(ctx, AlterRebuild(true))
	if err != nil {
		t.Fatalf("AlterTable(AlterRebuild): %v", err)
	}
	var sawRetype, sawHandmadeDrop bool
	for _, c := range applied {
		if c.Kind == ChangeColumnType && c.Column == "name" {
			sawRetype = true
		}
		if c.Kind == ChangeDropIndex && c.Index == "handmade_users_email" {
			sawHandmadeDrop = true
		}
	}
	if !sawRetype || !sawHandmadeDrop {
		t.Fatalf("applied = %v, want the retype and the drop of the undeclared index", applied)
	}

	info, err := retyped.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	age, ok := info.Column("age")
	if !ok {
		t.Fatal("age went missing in the rebuild")
	}
	if age.Default != "0" {
		t.Errorf("age.Default = %q, want 0", age.Default)
	}
	if name, _ := info.Column("name"); name.Type != "VARCHAR(128)" || !name.NotNull {
		t.Errorf("name = %+v, want VARCHAR(128) NOT NULL after the rebuild", name)
	}
	if got := info.PrimaryKey(); len(got) != 1 || got[0] != "id" {
		t.Errorf("PrimaryKey() = %v, want [id] after the rebuild", got)
	}
	if _, ok := info.Index("idx_users_name"); !ok {
		t.Errorf("the declared index did not survive the rebuild: %+v", info.Indexes)
	}
	if _, ok := info.Index("handmade_users_email"); ok {
		t.Errorf("the undeclared index the diff reported dropping is still there: %+v", info.Indexes)
	}
	if !uniqueIndexCovers(info, []string{"email"}) {
		t.Errorf("the UNIQUE constraint on email did not survive the rebuild: %+v", info.Indexes)
	}
	if _, ok := info.Column("users_sequelize_rebuild"); ok {
		t.Error("the scratch table leaked into the schema")
	}
	scratch, err := retyped.inspectNamed(ctx, SyncOptions{}, "users_sequelize_rebuild")
	if err != nil {
		t.Fatalf("inspectNamed: %v", err)
	}
	if scratch.Exists {
		t.Error("the rebuild left its scratch table behind")
	}

	rows, err := retyped.FindAll(ctx, Query{Order: []OrderTerm{{Attribute: "name"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows after the rebuild, want 2", len(rows))
	}
	if rows[0]["name"] != "ada" || rows[0]["age"] != int64(36) || rows[0]["email"] != "ada@example.com" {
		t.Errorf("row 0 = %v, want ada/36 unchanged", rows[0])
	}
	if rows[1]["name"] != "grace" || rows[1]["age"] != int64(45) {
		t.Errorf("row 1 = %v, want grace/45 unchanged", rows[1])
	}
	// The primary key is still a working auto-increment after the rebuild.
	created, err := retyped.Create(ctx, Values{"name": "alan", "email": "alan@example.com"})
	if err != nil {
		t.Fatalf("Create after rebuild: %v", err)
	}
	if created["id"] == nil {
		t.Error("the rebuilt table lost its auto-increment primary key")
	}
	if diff, err := retyped.SchemaDiff(ctx); err != nil || len(diff) != 0 {
		t.Errorf("SchemaDiff after the rebuild = %v, %v; want empty", diff, err)
	}
}

func TestAlterRebuildTightensNullability(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)
	if _, err := m.Create(ctx, Values{"name": "ada", "age": 36}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tightened := changed(t, db, "UserStrictAge", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER(), AllowNull: NotNull()},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	changes, err := tightened.SchemaDiff(ctx)
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != ChangeColumnNull {
		t.Fatalf("changes = %v, want one nullability change", changes)
	}
	if changes[0].From != "NULL" || changes[0].To != "NOT NULL" {
		t.Errorf("change = %v, want NULL -> NOT NULL", changes[0])
	}
	if _, err := tightened.AlterTable(ctx, AlterRebuild(true)); err != nil {
		t.Fatalf("AlterTable(AlterRebuild): %v", err)
	}
	info, err := tightened.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if col, _ := info.Column("age"); !col.NotNull {
		t.Errorf("age = %+v, want NOT NULL", col)
	}
	rows, err := tightened.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["age"] != int64(36) {
		t.Errorf("rows = %v, want the row intact", rows)
	}
	// And the constraint is live.
	if _, err := tightened.Create(ctx, Values{"name": "nobody"}); err == nil {
		t.Error("the rebuilt table did not enforce NOT NULL on age")
	}
}

// TestAlterRebuildRollsBackOnFailure checks the transaction: a nullability
// tightening that existing rows violate must leave the table exactly as it was.
func TestAlterRebuildRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)
	if _, err := m.Create(ctx, Values{"name": "ada"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tightened := changed(t, db, "UserStrictAge2", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER(), AllowNull: NotNull()},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	if _, err := tightened.AlterTable(ctx, AlterRebuild(true)); err == nil {
		t.Fatal("the rebuild claimed to make a NULL column NOT NULL with a NULL in it")
	}
	info, err := tightened.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if !info.Exists {
		t.Fatal("the failed rebuild destroyed the table")
	}
	if col, _ := info.Column("age"); col.NotNull {
		t.Error("the failed rebuild committed half its work")
	}
	if _, ok := info.Index("idx_users_name"); !ok {
		t.Errorf("the failed rebuild lost an index: %+v", info.Indexes)
	}
	rows, err := m.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "ada" {
		t.Errorf("rows = %v, want the original row", rows)
	}
}

func TestAlterRefusesANotNullColumnWithNoDefaultOnANonEmptyTable(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)
	if _, err := m.Create(ctx, Values{"name": "ada"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	grown := changed(t, db, "UserRequiredCity", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER()},
		"city":  {Type: STRING(32), AllowNull: NotNull()},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	_, err := grown.AlterTable(ctx, AlterRebuild(true))
	var refused *AlterRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("AlterTable = %v, want an *AlterRefusedError", err)
	}
	if len(refused.Changes) != 1 || refused.Changes[0].Column != "city" {
		t.Errorf("refused.Changes = %v, want the addition of city", refused.Changes)
	}
	info, err := grown.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if _, ok := info.Column("city"); ok {
		t.Error("the refused alter added the column anyway")
	}

	// The same change is fine while the table is still empty.
	if err := m.Truncate(ctx); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := grown.AlterTable(ctx, AlterRebuild(true)); err != nil {
		t.Fatalf("AlterTable on an empty table: %v", err)
	}
	info, err = grown.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	col, ok := info.Column("city")
	if !ok || !col.NotNull {
		t.Errorf("city = %+v, %v; want a NOT NULL column", col, ok)
	}
}

func TestAlterRefusesToLoseAUniqueConstraintTheModelDropped(t *testing.T) {
	ctx := context.Background()
	db, _ := alterFixture(t)
	// email loses its UNIQUE, and age is retyped so that a rebuild is needed.
	loosened := changed(t, db, "UserNoUnique", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255)},
		"age":   {Type: BIGINT(), DefaultValue: 1},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	_, err := loosened.AlterTable(ctx, AlterRebuild(true))
	var refused *AlterRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("AlterTable = %v, want an *AlterRefusedError about the UNIQUE constraint", err)
	}
	if _, err := loosened.AlterTable(ctx, AlterRebuild(true), AlterDrop(true)); err != nil {
		t.Fatalf("AlterTable(AlterRebuild, AlterDrop): %v", err)
	}
	info, err := loosened.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	for _, idx := range info.Indexes {
		if idx.Origin == "u" {
			t.Errorf("the UNIQUE constraint survived: %+v", idx)
		}
	}
}

func TestAlterDetectsDefaultAndForeignKeyChanges(t *testing.T) {
	ctx := context.Background()
	db, users := alterFixture(t)
	posts := db.Define("Post", Attributes{
		"title":  {Type: STRING(64)},
		"userID": {Type: INTEGER()},
	}, Timestamps(false))
	if err := posts.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	linked := db.Define("PostLinked", Attributes{
		"title":  {Type: STRING(64), DefaultValue: "untitled"},
		"userID": {Type: INTEGER(), References: &Reference{Model: users}},
	}, TableName("posts"), Timestamps(false))

	changes, err := linked.SchemaDiff(ctx)
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	var sawDefault, sawFK bool
	for _, c := range changes {
		switch c.Kind {
		case ChangeColumnDefault:
			sawDefault = c.Column == "title" && c.To == "'untitled'" && c.NeedsRebuild()
		case ChangeForeignKey:
			sawFK = c.Column == "user_id" && c.To == "users.id" && c.NeedsRebuild()
		}
	}
	if !sawDefault || !sawFK {
		t.Fatalf("changes = %v, want a default change and a foreign-key change", changes)
	}
	if _, err := linked.AlterTable(ctx, AlterRebuild(true)); err != nil {
		t.Fatalf("AlterTable(AlterRebuild): %v", err)
	}
	info, err := linked.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if col, _ := info.Column("title"); col.Default != "'untitled'" {
		t.Errorf("title.Default = %q, want 'untitled'", col.Default)
	}
	if len(info.ForeignKeys) != 1 || info.ForeignKeys[0].Table != "users" {
		t.Errorf("ForeignKeys = %+v, want one pointing at users", info.ForeignKeys)
	}
	if diff, err := linked.SchemaDiff(ctx); err != nil || len(diff) != 0 {
		t.Errorf("SchemaDiff after the rebuild = %v, %v; want empty", diff, err)
	}
}

func TestSyncAlterCreatesAMissingTable(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	m := db.Define("Fresh", Attributes{"name": {Type: STRING(16)}},
		WithIndexes(Index{Fields: []string{"name"}}))
	if err := db.Sync(ctx, Alter(true)); err != nil {
		t.Fatalf("Sync(Alter) on a missing table: %v", err)
	}
	info, err := m.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if !info.Exists {
		t.Fatal("Sync(Alter) did not create the table")
	}
	if _, ok := info.Index("idx_freshes_name"); !ok {
		t.Errorf("indexes = %+v, want the declared one", info.Indexes)
	}
	if diff, err := m.SchemaDiff(ctx); err != nil || len(diff) != 0 {
		t.Errorf("SchemaDiff = %v, %v; want empty", diff, err)
	}
}

func TestAlterInsideACallerTransaction(t *testing.T) {
	ctx := context.Background()
	db, m := alterFixture(t)
	if _, err := m.Create(ctx, Values{"name": "ada", "age": 36}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	grown := changed(t, db, "UserTx", Attributes{
		"name":  {Type: STRING(64), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER()},
		"city":  {Type: STRING(32)},
	}, WithIndexes(Index{Fields: []string{"name"}}))

	// A transaction the caller rolls back must leave the schema alone.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := grown.AlterTable(ctx, SyncTx(tx)); err != nil {
		t.Fatalf("AlterTable in tx: %v", err)
	}
	info, err := grown.InspectTable(ctx, SyncTx(tx))
	if err != nil {
		t.Fatalf("InspectTable in tx: %v", err)
	}
	if _, ok := info.Column("city"); !ok {
		t.Error("the alter did not take effect inside the transaction")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	info, err = grown.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if _, ok := info.Column("city"); ok {
		t.Error("a rolled-back alter changed the schema anyway")
	}

	// Committed, it sticks, and the data is still there.
	if err := db.Transaction(ctx, func(tx *Tx) error {
		_, err := grown.AlterTable(ctx, SyncTx(tx))
		return err
	}); err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	info, err = grown.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if _, ok := info.Column("city"); !ok {
		t.Error("the committed alter did not stick")
	}
	rows, err := grown.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "ada" {
		t.Errorf("rows = %v, want the original row", rows)
	}
}

func TestAlterRefusesABrokenModel(t *testing.T) {
	broken := newTestDB(t).Define("Broken", Attributes{"nope": {}})
	if _, err := broken.AlterTable(context.Background()); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("AlterTable on a broken model = %v, want ErrInvalidQuery", err)
	}
	if _, err := broken.InspectTable(context.Background()); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("InspectTable on a broken model = %v, want ErrInvalidQuery", err)
	}
}

func TestTypeEquivalent(t *testing.T) {
	equal := [][2]string{
		{"INTEGER", "INTEGER"},
		{"integer", "INTEGER"},
		{"INT", "INTEGER"},
		{"BIGINT", "INTEGER"},
		{"varchar(255)", "VARCHAR(255)"},
		{"NUMERIC(10, 2)", "NUMERIC(10,2)"},
		{"CHARACTER(36)", "VARCHAR(36)"},
		{"", "BLOB"},
	}
	for _, pair := range equal {
		if !typeEquivalent(pair[0], pair[1]) {
			t.Errorf("typeEquivalent(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"VARCHAR(64)", "VARCHAR(255)"},
		{"VARCHAR(255)", "TEXT"},
		{"INTEGER", "TEXT"},
		{"REAL", "INTEGER"},
		{"NUMERIC", "REAL"},
	}
	for _, pair := range different {
		if typeEquivalent(pair[0], pair[1]) {
			t.Errorf("typeEquivalent(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

func TestChangeKindAndSchemaChangeStrings(t *testing.T) {
	if got := ChangeAddColumn.String(); got != "add column" {
		t.Errorf("ChangeAddColumn.String() = %q", got)
	}
	if got := ChangeKind(99).String(); got != "ChangeKind(99)" {
		t.Errorf("ChangeKind(99).String() = %q", got)
	}
	c := SchemaChange{Kind: ChangeColumnType, Column: "age", From: "INTEGER", To: "TEXT"}
	if got, want := c.String(), "change column type age (INTEGER -> TEXT)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	c = SchemaChange{Kind: ChangeAddIndex, Index: "idx", To: "(a)"}
	if got, want := c.String(), "add index idx ((a))"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	c = SchemaChange{Kind: ChangeDropIndex, Index: "idx", From: "(a)"}
	if got, want := c.String(), "drop index idx ((a))"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	err := &AlterRefusedError{Model: "User", Table: "users", Reason: "because", Changes: []SchemaChange{c}}
	if got, want := err.Error(), "sequelize: refusing to alter users (User): because: drop index idx ((a))"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Error("AlterRefusedError does not wrap ErrUnsupported")
	}
}

func TestIndexHelpers(t *testing.T) {
	if !(IndexInfo{}).Expression() {
		t.Error("an index with no columns should report Expression()")
	}
	if (IndexInfo{Columns: []string{"a"}}).Expression() {
		t.Error("an index over a column should not report Expression()")
	}
	if got := indexSignature(IndexInfo{Unique: true, Columns: []string{"A", "b"}, Partial: true}); got != "UNIQUE (a, b (partial))" {
		t.Errorf("indexSignature = %q", got)
	}
}

func TestAlterRebuildChangesThePrimaryKey(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	old := db.Define("Thing", Attributes{"label": {Type: TEXT()}}, Timestamps(false))
	if err := old.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := old.Create(ctx, Values{"label": "widget"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rekeyed := db.Define("ThingByLabel", Attributes{
		"label": {Type: STRING(32), PrimaryKey: true, AllowNull: NotNull()},
	}, TableName("things"), Timestamps(false))

	changes, err := rekeyed.SchemaDiff(ctx)
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	var sawPK bool
	for _, c := range changes {
		if c.Kind == ChangePrimaryKey && c.From == "id" && c.To == "label" && c.NeedsRebuild() {
			sawPK = true
		}
	}
	if !sawPK {
		t.Fatalf("changes = %v, want a primary-key change needing a rebuild", changes)
	}
	if _, err := rekeyed.AlterTable(ctx, AlterRebuild(true), AlterDrop(true)); err != nil {
		t.Fatalf("AlterTable: %v", err)
	}
	info, err := rekeyed.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if got := info.PrimaryKey(); len(got) != 1 || got[0] != "label" {
		t.Errorf("PrimaryKey() = %v, want [label]", got)
	}
	if _, ok := info.Column("id"); ok {
		t.Error("the old primary-key column is still there")
	}
	rows, err := rekeyed.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["label"] != "widget" {
		t.Errorf("rows = %v, want the row keyed by its label", rows)
	}
}

func TestAlterRebuildRemovesAForeignKey(t *testing.T) {
	ctx := context.Background()
	db, users := alterFixture(t)
	linked := db.Define("Post", Attributes{
		"title":  {Type: STRING(64)},
		"userID": {Type: INTEGER(), References: &Reference{Model: users}},
	}, Timestamps(false))
	if err := linked.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	unlinked := db.Define("PostFree", Attributes{
		"title":  {Type: STRING(64)},
		"userID": {Type: INTEGER()},
	}, TableName("posts"), Timestamps(false))

	changes, err := unlinked.SchemaDiff(ctx)
	if err != nil {
		t.Fatalf("SchemaDiff: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != ChangeForeignKey || changes[0].From != "users.id" || changes[0].To != "" {
		t.Fatalf("changes = %v, want the removal of one foreign key", changes)
	}
	if _, err := unlinked.AlterTable(ctx, AlterRebuild(true)); err != nil {
		t.Fatalf("AlterTable: %v", err)
	}
	info, err := unlinked.InspectTable(ctx)
	if err != nil {
		t.Fatalf("InspectTable: %v", err)
	}
	if len(info.ForeignKeys) != 0 {
		t.Errorf("ForeignKeys = %+v, want none", info.ForeignKeys)
	}
}

func TestChangeKindStringsAreAllNamed(t *testing.T) {
	kinds := []ChangeKind{
		ChangeAddColumn, ChangeDropColumn, ChangeColumnType, ChangeColumnNull,
		ChangeColumnDefault, ChangeAddIndex, ChangeDropIndex, ChangePrimaryKey,
		ChangeForeignKey,
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		name := k.String()
		if name == "" || seen[name] {
			t.Errorf("ChangeKind(%d).String() = %q, want a distinct name", int(k), name)
		}
		seen[name] = true
	}
}
