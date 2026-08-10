package sequelize

import (
	"errors"
	"strings"
	"testing"
)

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"id":          "id",
		"createdAt":   "created_at",
		"UserProfile": "user_profile",
		"HTTPServer":  "http_server",
		"userID":      "user_id",
		"a1B":         "a1_b",
		"already_ok":  "already_ok",
		"":            "",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPluralize(t *testing.T) {
	cases := map[string]string{
		"user":         "users",
		"user_profile": "user_profiles",
		"address":      "addresses",
		"box":          "boxes",
		"branch":       "branches",
		"dish":         "dishes",
		"category":     "categories",
		"day":          "days",
		"leaf":         "leaves",
		"knife":        "knives",
		"":             "",
	}
	for in, want := range cases {
		if got := pluralize(in); got != want {
			t.Errorf("pluralize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAllowNullDefaultsToNullable pins the semantics upstream fixes: a column is
// nullable unless told otherwise, so the Go zero value must mean nullable.
func TestAllowNullDefaultsToNullable(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Thing", Attributes{
		"loose": {Type: STRING(10)},
		"tight": {Type: STRING(10), AllowNull: NotNull()},
	})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	loose, _ := m.Attribute("loose")
	if !loose.Nullable() {
		t.Fatal("an attribute with AllowNull unset must be nullable")
	}
	ddl, err := m.CreateTableSQL()
	if err != nil {
		t.Fatalf("CreateTableSQL: %v", err)
	}
	if strings.Contains(ddl, `"loose" VARCHAR(10) NOT NULL`) {
		t.Errorf("an unset AllowNull produced NOT NULL: %s", ddl)
	}
	if !strings.Contains(ddl, `"tight" VARCHAR(10) NOT NULL`) {
		t.Errorf("AllowNull: NotNull() did not produce NOT NULL: %s", ddl)
	}
}

func TestModelAddsImplicitPrimaryKey(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("User", Attributes{"name": {Type: STRING(10)}})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	if got := m.PrimaryKeys(); len(got) != 1 || got[0] != "id" {
		t.Fatalf("PrimaryKeys() = %v, want [id]", got)
	}
	id, ok := m.Attribute("id")
	if !ok || !id.AutoIncrement || !id.PrimaryKey || id.Nullable() {
		t.Errorf("implicit id = %+v", id)
	}
	if got := m.AttributeNames()[0]; got != "id" {
		t.Errorf("AttributeNames()[0] = %q, want the primary key first", got)
	}
}

func TestModelHonoursDeclaredPrimaryKey(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Doc", Attributes{
		"slug": {Type: STRING(10), PrimaryKey: true, AllowNull: NotNull()},
		"body": {Type: TEXT()},
	})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	if got := m.PrimaryKeys(); len(got) != 1 || got[0] != "slug" {
		t.Errorf("PrimaryKeys() = %v, want [slug]", got)
	}
	if _, ok := m.Attribute("id"); ok {
		t.Error("an implicit id was added despite a declared primary key")
	}
}

func TestModelCompositePrimaryKey(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Membership", Attributes{
		"userID":  {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
		"groupID": {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
		"role":    {Type: STRING(10)},
	})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	if got := m.PrimaryKeys(); len(got) != 2 || got[0] != "groupID" || got[1] != "userID" {
		t.Errorf("PrimaryKeys() = %v, want [groupID userID]", got)
	}
}

func TestModelTableAndColumnNaming(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("UserProfile", Attributes{
		"displayName": {Type: STRING(10)},
		"legacy":      {Type: STRING(10), Field: "legacy_col"},
	})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	if got := m.TableName(); got != "user_profiles" {
		t.Errorf("TableName() = %q, want user_profiles", got)
	}
	if col, err := m.Column("displayName"); err != nil || col != "display_name" {
		t.Errorf("Column(displayName) = %q, %v; want display_name", col, err)
	}
	if col, err := m.Column("legacy"); err != nil || col != "legacy_col" {
		t.Errorf("Column(legacy) = %q, %v; want legacy_col", col, err)
	}
	if _, err := m.Column("nope"); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("Column(nope) = %v, want ErrUnknownAttribute", err)
	}
}

func TestTableNameOptionOverrides(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Person", Attributes{"name": {Type: STRING(10)}}, TableName("people"))
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	if got := m.TableName(); got != "people" {
		t.Errorf("TableName() = %q, want people", got)
	}
}

func TestTimestampsOnByDefaultAndOptional(t *testing.T) {
	db := newTestDB(t)
	on := db.Define("WithStamps", Attributes{"name": {Type: STRING(10)}})
	if !on.Timestamps() {
		t.Error("Timestamps() = false by default, want true")
	}
	for _, name := range []string{DefaultCreatedAt, DefaultUpdatedAt} {
		if _, ok := on.Attribute(name); !ok {
			t.Errorf("attribute %q was not added", name)
		}
	}
	if col, _ := on.Column(DefaultCreatedAt); col != "created_at" {
		t.Errorf("createdAt column = %q, want created_at", col)
	}

	off := db.Define("NoStamps", Attributes{"name": {Type: STRING(10)}}, Timestamps(false))
	if off.Timestamps() {
		t.Error("Timestamps(false) did not turn timestamps off")
	}
	if _, ok := off.Attribute(DefaultCreatedAt); ok {
		t.Error("Timestamps(false) still added createdAt")
	}
}

func TestParanoidAddsDeletedAtAndImpliesTimestamps(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Soft", Attributes{"name": {Type: STRING(10)}}, Paranoid(true), Timestamps(false))
	if !m.IsParanoid() {
		t.Error("IsParanoid() = false")
	}
	if !m.Timestamps() {
		t.Error("Paranoid(true) must imply timestamps")
	}
	deletedAt, ok := m.Attribute(m.DeletedAtName())
	if !ok {
		t.Fatal("deletedAt was not added")
	}
	if !deletedAt.Nullable() {
		t.Error("deletedAt must be nullable, since a live row has no deletion time")
	}
}

func TestTimestampFieldRenaming(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Renamed", Attributes{"name": {Type: STRING(10)}},
		CreatedAtField("bornAt"), UpdatedAtField("touchedAt"), DeletedAtField("goneAt"), Paranoid(true))
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	for _, name := range []string{"bornAt", "touchedAt", "goneAt"} {
		if _, ok := m.Attribute(name); !ok {
			t.Errorf("renamed timestamp %q missing", name)
		}
	}
	if m.CreatedAtName() != "bornAt" || m.UpdatedAtName() != "touchedAt" || m.DeletedAtName() != "goneAt" {
		t.Error("timestamp name accessors did not follow the options")
	}
}

// TestIdentifierValidationRejectsBadNames is the injection guard: a hostile
// column, table or model name must fail rather than be quoted through.
func TestIdentifierValidationRejectsBadNames(t *testing.T) {
	db := newTestDB(t)
	cases := []struct {
		name  string
		model *Model
	}{
		{"bad attribute name", db.Define("BadAttr", Attributes{`x"; DROP TABLE users; --`: {Type: STRING(4)}})},
		{"bad Field", db.Define("BadField", Attributes{"x": {Type: STRING(4), Field: `y" --`}})},
		{"bad table name", db.Define("BadTable", Attributes{"x": {Type: STRING(4)}}, TableName("users; DROP TABLE t"))},
		{"bad model name", db.Define("Bad Model", Attributes{"x": {Type: STRING(4)}})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !errors.Is(c.model.Err(), ErrInvalidIdentifier) {
				t.Fatalf("Err() = %v, want ErrInvalidIdentifier", c.model.Err())
			}
			if _, err := c.model.CreateTableSQL(); !errors.Is(err, ErrInvalidIdentifier) {
				t.Errorf("CreateTableSQL() = %v, want ErrInvalidIdentifier", err)
			}
		})
	}
}

func TestDefineRejectsMissingType(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Untyped", Attributes{"x": {}})
	if !errors.Is(m.Err(), ErrInvalidQuery) {
		t.Errorf("Err() = %v, want ErrInvalidQuery", m.Err())
	}
}

func TestDefineRejectsIDAttributeWithoutPrimaryKey(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Clash", Attributes{"id": {Type: STRING(10)}})
	if !errors.Is(m.Err(), ErrNoPrimaryKey) {
		t.Errorf("Err() = %v, want ErrNoPrimaryKey", m.Err())
	}
}

func TestDefineRejectsBadReference(t *testing.T) {
	db := newTestDB(t)
	cases := map[string]Attribute{
		"no target":  {Type: INTEGER(), References: &Reference{}},
		"bad table":  {Type: INTEGER(), References: &Reference{Table: "a b"}},
		"bad key":    {Type: INTEGER(), References: &Reference{Table: "t", Key: "a b"}},
		"bad action": {Type: INTEGER(), References: &Reference{Table: "t", Key: "id", OnDelete: "CASCADE; DROP"}},
	}
	i := 0
	for name, attr := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			m := db.Define("Ref"+strings.Repeat("X", i), Attributes{"other": attr})
			if m.Err() == nil {
				t.Fatal("Err() = nil, want a rejected reference")
			}
		})
	}
}

func TestDefineRejectsBadIndexes(t *testing.T) {
	db := newTestDB(t)
	if m := db.Define("NoFields", Attributes{"a": {Type: INTEGER()}},
		WithIndexes(Index{})); !errors.Is(m.Err(), ErrInvalidQuery) {
		t.Errorf("index with no fields: Err() = %v, want ErrInvalidQuery", m.Err())
	}
	if m := db.Define("BadField2", Attributes{"a": {Type: INTEGER()}},
		WithIndexes(Index{Fields: []string{"nope"}})); !errors.Is(m.Err(), ErrUnknownAttribute) {
		t.Errorf("index on unknown attribute: Err() = %v, want ErrUnknownAttribute", m.Err())
	}
	if m := db.Define("BadIdxName", Attributes{"a": {Type: INTEGER()}},
		WithIndexes(Index{Name: "bad name", Fields: []string{"a"}})); !errors.Is(m.Err(), ErrInvalidIdentifier) {
		t.Errorf("index with a bad name: Err() = %v, want ErrInvalidIdentifier", m.Err())
	}
}

func TestModelAccessorsCopyTheirState(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Copy", Attributes{"a": {Type: INTEGER()}}, WithIndexes(Index{Fields: []string{"a"}}))
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	names := m.AttributeNames()
	names[0] = "clobbered"
	if m.AttributeNames()[0] == "clobbered" {
		t.Error("AttributeNames() exposed the model's own slice")
	}
	pks := m.PrimaryKeys()
	pks[0] = "clobbered"
	if m.PrimaryKeys()[0] == "clobbered" {
		t.Error("PrimaryKeys() exposed the model's own slice")
	}
	attrs := m.Attributes()
	delete(attrs, "a")
	if _, ok := m.Attribute("a"); !ok {
		t.Error("Attributes() exposed the model's own map")
	}
	opts := m.Options()
	opts.Indexes[0].Name = "clobbered"
	if m.Options().Indexes[0].Name == "clobbered" {
		t.Error("Options() exposed the model's own indexes")
	}
	if m.Sequelize() != db {
		t.Error("Sequelize() did not return the defining connection")
	}
	if m.Name() != "Copy" {
		t.Errorf("Name() = %q", m.Name())
	}
}

func TestApplyDefaultsDoesNotOverwrite(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Defaulted", Attributes{
		"a": {Type: INTEGER(), DefaultValue: 5},
		"b": {Type: INTEGER(), DefaultValue: 7},
		"c": {Type: INTEGER()},
	})
	got := m.applyDefaults(Values{"a": 1, "c": nil})
	if got["a"] != 1 {
		t.Errorf("applyDefaults overwrote a supplied value: %v", got["a"])
	}
	if got["b"] != 7 {
		t.Errorf("applyDefaults did not apply b: %v", got["b"])
	}
	if v, ok := got["c"]; !ok || v != nil {
		t.Errorf("applyDefaults lost an explicit nil: %v, %v", v, ok)
	}
}
