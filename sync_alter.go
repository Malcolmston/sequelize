package sequelize

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Alter makes Sync migrate an existing table towards the model instead of
// leaving it alone. It is upstream's `alter: true`, and it is deliberately
// conservative about anything that can lose data:
//
//   - Applied: a column the model declares and the table lacks (ALTER TABLE ...
//     ADD COLUMN), an index the model declares and the table lacks, and an index
//     the table has that the model no longer declares.
//   - Applied only with [AlterDrop]: dropping a column the model no longer
//     declares, which destroys that column's data, and dropping a UNIQUE
//     constraint the model no longer declares.
//   - Applied only with [AlterRebuild]: a column whose type, nullability,
//     default or foreign key changed, a change of primary key, a UNIQUE
//     constraint added or removed, and a new column SQLite cannot add in place
//     (a UNIQUE or primary-key column). SQLite has no ALTER COLUMN, so these
//     need the table rebuilt — see [AlterRebuild].
//   - Never applied, and never guessed at: everything else. A difference Sync
//     will not apply is reported as an *[AlterRefusedError] listing it, rather
//     than skipped quietly.
//
// Use [Model.SchemaDiff] to see what an alter would do before running it.
func Alter(alter bool) SyncOption {
	return func(o *SyncOptions) { o.Alter = alter }
}

// AlterDrop permits the destructive half of [Alter]: dropping a column the model
// no longer declares, and with it every value stored in that column. It is off
// by default and an alter that needs it fails with an *[AlterRefusedError]
// instead, because no amount of convenience is worth deleting a column of
// production data on the strength of an edited struct literal.
//
// It also permits dropping a UNIQUE constraint the live table has and the model
// does not declare. That loses no rows, but it is a guarantee that cannot be put
// back once duplicates exist, so it is the caller's decision too.
func AlterDrop(drop bool) SyncOption {
	return func(o *SyncOptions) { o.AlterDrop = drop }
}

// AlterRebuild permits the table rebuild that a retype, a nullability change, a
// default change or a foreign-key change needs on SQLite, which has no ALTER
// COLUMN. The rebuild is upstream's strategy and SQLite's documented one:
// inside a transaction, create a new table with the target schema, copy every
// row across, drop the original, rename the new table into its place, and
// recreate the indexes.
//
// It is off by default and an alter that needs it fails with an
// *[AlterRefusedError] instead. The reason is that the copy goes through
// SQLite's type conversion rules: rebuilding a VARCHAR column as INTEGER
// succeeds and silently rewrites every value it can coerce. That is a data
// migration, not a schema migration, and it should be a decision rather than a
// side effect of passing [Alter].
//
// A rebuild is all-or-nothing: it either commits with every row present or rolls
// back leaving the original table untouched. It refuses to start if it would
// lose an index it cannot rebuild from introspection (a partial or expression
// index) or, without [AlterDrop], a UNIQUE constraint the model does not declare.
func AlterRebuild(rebuild bool) SyncOption {
	return func(o *SyncOptions) { o.AlterRebuild = rebuild }
}

// ColumnInfo is one column of a live table as the database reports it. It is
// what the model's declared [Attribute] is compared against.
type ColumnInfo struct {
	// Name is the physical column name.
	Name string
	// Type is the column's declared type, verbatim as the schema spells it —
	// "VARCHAR(255)", "INTEGER", "" for an untyped SQLite column.
	Type string
	// NotNull reports whether the column carries a NOT NULL constraint.
	//
	// SQLite reports 0 here for an INTEGER PRIMARY KEY, which is an alias for the
	// rowid and cannot hold NULL; the diff therefore never compares nullability
	// on a primary-key column.
	NotNull bool
	// Default is the column's DEFAULT as a SQL literal ("1", "'x'"), or the empty
	// string when it has none.
	Default string
	// PrimaryKeyPos is the column's 1-based position in the primary key, or 0
	// when it is not part of it.
	PrimaryKeyPos int
}

// IndexInfo is one index of a live table, or one index a model declares.
type IndexInfo struct {
	// Name is the index name.
	Name string
	// Unique reports whether the index is UNIQUE.
	Unique bool
	// Columns lists the indexed column names in index order. It is empty for an
	// index over an expression rather than plain columns.
	Columns []string
	// Origin says where the index came from, using SQLite's vocabulary: "c" for
	// a CREATE INDEX statement, "u" for a UNIQUE constraint in the table
	// definition, "pk" for the primary key. It is empty for a model's declared
	// index, which has not been created yet.
	Origin string
	// Partial reports whether the index has a WHERE clause. A partial index
	// cannot be reconstructed from introspection alone.
	Partial bool
}

// Expression reports whether the index covers an expression rather than a plain
// list of columns, in which case it cannot be reconstructed from introspection.
func (i IndexInfo) Expression() bool { return len(i.Columns) == 0 }

// ForeignKeyInfo is one foreign key of a live table.
type ForeignKeyInfo struct {
	// Column is the referencing column in this table.
	Column string
	// Table is the referenced table.
	Table string
	// Key is the referenced column. It is empty when the schema referenced the
	// parent's primary key without naming it.
	Key string
	// OnUpdate and OnDelete are the referential actions, as the schema spells
	// them ("NO ACTION" when unspecified).
	OnUpdate string
	OnDelete string
}

// TableInfo is a live table's shape, read back from the database. It is the
// foundation the schema diff is computed from: without it a migration tool can
// only ever create or drop.
type TableInfo struct {
	// Name is the table name that was inspected.
	Name string
	// Exists reports whether the table is there at all. When it is false every
	// other field is zero.
	Exists bool
	// Columns lists the columns in schema order.
	Columns []ColumnInfo
	// Indexes lists every index on the table, primary-key and UNIQUE-constraint
	// indexes included.
	Indexes []IndexInfo
	// ForeignKeys lists the table's foreign keys.
	ForeignKeys []ForeignKeyInfo
	// SQL is the CREATE TABLE statement the database stores for the table, which
	// is the only fully faithful record of it.
	SQL string
}

// Column returns the named column, matched case-insensitively as SQL does.
func (t TableInfo) Column(name string) (ColumnInfo, bool) {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return ColumnInfo{}, false
}

// Index returns the named index, matched case-insensitively.
func (t TableInfo) Index(name string) (IndexInfo, bool) {
	for _, i := range t.Indexes {
		if strings.EqualFold(i.Name, name) {
			return i, true
		}
	}
	return IndexInfo{}, false
}

// PrimaryKey returns the primary key's column names in key order.
func (t TableInfo) PrimaryKey() []string {
	cols := make([]ColumnInfo, 0, len(t.Columns))
	for _, c := range t.Columns {
		if c.PrimaryKeyPos > 0 {
			cols = append(cols, c)
		}
	}
	sort.SliceStable(cols, func(i, j int) bool { return cols[i].PrimaryKeyPos < cols[j].PrimaryKeyPos })
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out
}

// SchemaIntrospector is the optional [Dialect] extension that reads a live
// table's shape. A dialect that implements it is used in preference to the
// package's built-in reader, which understands SQLite only; a dialect that does
// not gets an [ErrUnsupported] from [Model.InspectTable].
type SchemaIntrospector interface {
	// InspectTable reads table's columns, indexes and foreign keys through q,
	// returning a TableInfo whose Exists is false when there is no such table.
	InspectTable(ctx context.Context, q Querier, table string) (TableInfo, error)
}

// ChangeKind names one kind of difference between a live table and a model.
type ChangeKind int

// The kinds of schema difference the diff reports.
const (
	// ChangeAddColumn is a column the model declares and the table lacks.
	ChangeAddColumn ChangeKind = iota
	// ChangeDropColumn is a column the table has and the model no longer
	// declares. Applying it destroys that column's data.
	ChangeDropColumn
	// ChangeColumnType is a column whose declared type differs.
	ChangeColumnType
	// ChangeColumnNull is a column whose nullability differs.
	ChangeColumnNull
	// ChangeColumnDefault is a column whose DEFAULT differs.
	ChangeColumnDefault
	// ChangeAddIndex is an index the model declares and the table lacks.
	ChangeAddIndex
	// ChangeDropIndex is an index the table has and the model no longer declares.
	ChangeDropIndex
	// ChangePrimaryKey is a difference in the primary key's columns or order.
	ChangePrimaryKey
	// ChangeForeignKey is a foreign key the model declares and the table lacks,
	// or the reverse. Referential actions are not compared; see [Model.SchemaDiff].
	ChangeForeignKey
)

// String returns the kind's name.
func (k ChangeKind) String() string {
	switch k {
	case ChangeAddColumn:
		return "add column"
	case ChangeDropColumn:
		return "drop column"
	case ChangeColumnType:
		return "change column type"
	case ChangeColumnNull:
		return "change column nullability"
	case ChangeColumnDefault:
		return "change column default"
	case ChangeAddIndex:
		return "add index"
	case ChangeDropIndex:
		return "drop index"
	case ChangePrimaryKey:
		return "change primary key"
	case ChangeForeignKey:
		return "change foreign key"
	default:
		return fmt.Sprintf("ChangeKind(%d)", int(k))
	}
}

// SchemaChange is one difference between a live table and a model's declared
// attributes, as reported by [Model.SchemaDiff].
type SchemaChange struct {
	// Kind says what sort of difference this is.
	Kind ChangeKind
	// Attribute is the model attribute involved, empty when the change concerns
	// something the model does not declare (a dropped column or index).
	Attribute string
	// Column is the physical column involved, empty for an index-level change.
	Column string
	// Index is the index name involved, empty for a column-level change.
	Index string
	// From is the live schema's value: the old type, "NULL"/"NOT NULL", the old
	// default, the old key. It is empty where there was nothing.
	From string
	// To is the model's value, in the same spelling as From.
	To string

	// destructive and rebuild are set by the planner rather than the caller.
	destructive bool
	rebuild     bool
}

// Destructive reports whether applying the change loses data or a constraint,
// which requires [AlterDrop].
func (c SchemaChange) Destructive() bool { return c.destructive }

// NeedsRebuild reports whether applying the change needs the table rebuilt
// because the dialect cannot alter it in place, which requires [AlterRebuild].
func (c SchemaChange) NeedsRebuild() bool { return c.rebuild }

// String renders the change for a log line or an error message.
func (c SchemaChange) String() string {
	target := c.Column
	if target == "" {
		target = c.Index
	}
	out := c.Kind.String()
	if target != "" {
		out += " " + target
	}
	switch {
	case c.From != "" && c.To != "":
		out += " (" + c.From + " -> " + c.To + ")"
	case c.To != "":
		out += " (" + c.To + ")"
	case c.From != "":
		out += " (" + c.From + ")"
	}
	return out
}

// AlterRefusedError reports that an alter was not performed because some of the
// differences it found need an option the caller did not pass, or cannot be
// performed at all. It wraps [ErrUnsupported].
//
// Nothing was applied: the alter is planned in full and refused before any DDL
// runs, so a refusal never leaves a half-migrated table.
type AlterRefusedError struct {
	// Model is the model being synced.
	Model string
	// Table is its table.
	Table string
	// Changes are the differences that were refused.
	Changes []SchemaChange
	// Reason says which option would have permitted them, or why nothing would.
	Reason string
}

// Error implements the error interface.
func (e *AlterRefusedError) Error() string {
	parts := make([]string, 0, len(e.Changes))
	for _, c := range e.Changes {
		parts = append(parts, c.String())
	}
	return fmt.Sprintf("sequelize: refusing to alter %s (%s): %s: %s",
		e.Table, e.Model, e.Reason, strings.Join(parts, "; "))
}

// Unwrap makes the error match [ErrUnsupported] with [errors.Is].
func (e *AlterRefusedError) Unwrap() error { return ErrUnsupported }

// InspectTable reads the model's table back from the database: its columns with
// their declared types, nullability, defaults and primary-key positions, its
// indexes, its foreign keys, and the CREATE TABLE statement the database stores
// for it. TableInfo.Exists is false, with no error, when the table is not there.
//
// It accepts [SyncTx] so that it can read inside a transaction that has already
// changed the schema.
func (m *Model) InspectTable(ctx context.Context, opts ...SyncOption) (TableInfo, error) {
	return m.inspectTable(ctx, resolveSyncOptions(opts))
}

func (m *Model) inspectTable(ctx context.Context, o SyncOptions) (TableInfo, error) {
	if err := m.Err(); err != nil {
		return TableInfo{}, err
	}
	return m.inspectNamed(ctx, o, m.table)
}

// inspectNamed introspects an arbitrary table on the model's connection, which
// the rebuild needs so that it can check its scratch table name is free.
func (m *Model) inspectNamed(ctx context.Context, o SyncOptions, table string) (TableInfo, error) {
	q, err := m.querierFor(Query{Tx: o.Tx})
	if err != nil {
		return TableInfo{}, err
	}
	if custom, ok := m.seq.dialect.(SchemaIntrospector); ok {
		return custom.InspectTable(ctx, q, table)
	}
	switch m.seq.dialect.Name() {
	case "sqlite", "sqlite3":
		return sqliteInspectTable(ctx, q, table)
	default:
		return TableInfo{}, fmt.Errorf("%w: %s cannot introspect a table; implement SchemaIntrospector",
			ErrUnsupported, m.seq.dialect.Name())
	}
}

// sqliteInspectTable reads a SQLite table's shape.
//
// Every statement here is parameterised: SQLite exposes its introspection
// pragmas as table-valued functions (pragma_table_info and friends) which take
// the table name as a bind argument, so no identifier is interpolated into SQL
// text at any point.
func sqliteInspectTable(ctx context.Context, q Querier, table string) (TableInfo, error) {
	if err := ValidateIdentifier(table); err != nil {
		return TableInfo{}, err
	}
	info := TableInfo{Name: table}

	err := scanRows(ctx, q, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`,
		[]any{table}, func(rows *sql.Rows) error {
			var ddl sql.NullString
			if err := rows.Scan(&ddl); err != nil {
				return err
			}
			info.Exists = true
			info.SQL = ddl.String
			return nil
		})
	if err != nil {
		return TableInfo{}, err
	}
	if !info.Exists {
		return info, nil
	}

	err = scanRows(ctx, q, `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`,
		[]any{table}, func(rows *sql.Rows) error {
			var (
				col     ColumnInfo
				notNull int
				dflt    sql.NullString
				pk      int
			)
			if err := rows.Scan(&col.Name, &col.Type, &notNull, &dflt, &pk); err != nil {
				return err
			}
			col.NotNull = notNull != 0
			col.Default = dflt.String
			col.PrimaryKeyPos = pk
			info.Columns = append(info.Columns, col)
			return nil
		})
	if err != nil {
		return TableInfo{}, err
	}

	err = scanRows(ctx, q, `SELECT name, "unique", origin, partial FROM pragma_index_list(?)`,
		[]any{table}, func(rows *sql.Rows) error {
			var (
				idx     IndexInfo
				unique  int
				partial int
			)
			if err := rows.Scan(&idx.Name, &unique, &idx.Origin, &partial); err != nil {
				return err
			}
			idx.Unique = unique != 0
			idx.Partial = partial != 0
			info.Indexes = append(info.Indexes, idx)
			return nil
		})
	if err != nil {
		return TableInfo{}, err
	}
	for i := range info.Indexes {
		idx := &info.Indexes[i]
		expression := false
		err := scanRows(ctx, q, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`,
			[]any{idx.Name}, func(rows *sql.Rows) error {
				var col sql.NullString
				if err := rows.Scan(&col); err != nil {
					return err
				}
				if !col.Valid {
					// A NULL name is an indexed expression or the rowid.
					expression = true
					return nil
				}
				idx.Columns = append(idx.Columns, col.String)
				return nil
			})
		if err != nil {
			return TableInfo{}, err
		}
		if expression {
			idx.Columns = nil
		}
	}

	err = scanRows(ctx, q, `SELECT "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list(?)`,
		[]any{table}, func(rows *sql.Rows) error {
			var (
				fk  ForeignKeyInfo
				to  sql.NullString
				col sql.NullString
			)
			if err := rows.Scan(&fk.Table, &col, &to, &fk.OnUpdate, &fk.OnDelete); err != nil {
				return err
			}
			fk.Column = col.String
			fk.Key = to.String
			info.ForeignKeys = append(info.ForeignKeys, fk)
			return nil
		})
	if err != nil {
		return TableInfo{}, err
	}
	sort.SliceStable(info.Indexes, func(i, j int) bool { return info.Indexes[i].Name < info.Indexes[j].Name })
	return info, nil
}

// scanRows runs a parameterised query and hands each row to fn.
func scanRows(ctx context.Context, q Querier, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// declaredIndexes returns the indexes the model declares with [WithIndexes],
// resolved to physical column names and with derived names filled in. It is the
// model side of the index diff, and [Model.CreateIndexSQL] renders exactly these.
func (m *Model) declaredIndexes() ([]IndexInfo, error) {
	out := make([]IndexInfo, 0, len(m.opts.Indexes))
	for _, idx := range m.opts.Indexes {
		cols := make([]string, 0, len(idx.Fields))
		for _, field := range idx.Fields {
			attr, ok := m.attrs[field]
			if !ok {
				return nil, fmt.Errorf("%w: index on %s names %q", ErrUnknownAttribute, m.name, field)
			}
			cols = append(cols, attr.Field)
		}
		name := idx.Name
		if name == "" {
			name = "idx_" + m.table + "_" + strings.Join(cols, "_")
		}
		if err := ValidateIdentifier(name); err != nil {
			return nil, fmt.Errorf("index on %s: %w", m.name, err)
		}
		out = append(out, IndexInfo{Name: name, Unique: idx.Unique, Columns: cols})
	}
	return out, nil
}

// createIndexSQL renders CREATE INDEX for one index over plain columns. Every
// identifier goes through the dialect's quoting, so an index reconstructed from
// introspection is as safely rendered as one the model declared.
func (m *Model) createIndexSQL(idx IndexInfo) (string, error) {
	d := m.seq.dialect
	if len(idx.Columns) == 0 {
		return "", fmt.Errorf("%w: index %q covers no plain columns", ErrUnsupported, idx.Name)
	}
	table, err := d.Quote(m.table)
	if err != nil {
		return "", err
	}
	name, err := d.Quote(idx.Name)
	if err != nil {
		return "", err
	}
	cols := make([]string, 0, len(idx.Columns))
	for _, col := range idx.Columns {
		quoted, err := d.Quote(col)
		if err != nil {
			return "", err
		}
		cols = append(cols, quoted)
	}
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	return "CREATE " + unique + "INDEX IF NOT EXISTS " + name + " ON " + table + " (" + strings.Join(cols, ", ") + ")", nil
}

// dropIndexSQL renders DROP INDEX for an index name.
func (m *Model) dropIndexSQL(name string) (string, error) {
	quoted, err := m.seq.dialect.Quote(name)
	if err != nil {
		return "", err
	}
	return "DROP INDEX IF EXISTS " + quoted, nil
}

// SchemaDiff reports every difference between the model's table as it exists in
// the database and the attributes the model declares, in a stable order:
// primary key, then columns in model order, then dropped columns, then indexes,
// then foreign keys. An empty result means the table already matches.
//
// It returns an error wrapping [ErrRecordNotFound] when the table does not
// exist; use [Model.InspectTable] if you want to ask without that being an
// error.
//
// What is compared, and what is not:
//
//   - Types are compared as text against the dialect's rendering of the
//     declared [DataType], leniently: differences in letter case and in SQLite
//     type affinity aliases ("INT" against "INTEGER") are not differences, a
//     change of length or precision is.
//   - Nullability is not compared on a primary-key column, which is NOT NULL by
//     definition however the schema spells it.
//   - Defaults are compared as SQL literal text. A default the package cannot
//     render as a literal at all — see [Attribute.DefaultValue] — is applied in
//     Go rather than by the database, and so is not compared.
//   - Foreign keys are compared by (column, referenced table, referenced
//     column) only. Referential actions — ON DELETE, ON UPDATE — are not
//     compared, because a change to one still needs hand-written DDL.
//   - [Attribute.Comment] and the value list of an [ENUM] are not compared,
//     because the SQLite dialect renders neither: there is nothing in the schema
//     to compare them against.
//   - [Attribute.AutoIncrement] is not compared. SQLite reports a rowid alias's
//     type as plain INTEGER either way, so telling AUTOINCREMENT apart would
//     mean parsing the stored CREATE TABLE.
//   - CHECK constraints, collations, generated columns and table options such as
//     WITHOUT ROWID are neither introspected nor compared. A model that needs one
//     changed needs hand-written DDL.
func (m *Model) SchemaDiff(ctx context.Context, opts ...SyncOption) ([]SchemaChange, error) {
	o := resolveSyncOptions(opts)
	live, err := m.inspectTable(ctx, o)
	if err != nil {
		return nil, err
	}
	if !live.Exists {
		return nil, fmt.Errorf("%w: table %s for model %s", ErrRecordNotFound, m.table, m.name)
	}
	return m.diffAgainst(live)
}

// diffAgainst computes the diff between a live table and the model.
func (m *Model) diffAgainst(live TableInfo) ([]SchemaChange, error) {
	d := m.seq.dialect
	var changes []SchemaChange

	// Primary key.
	wantPK := make([]string, 0, len(m.pk))
	for _, name := range m.pk {
		wantPK = append(wantPK, m.attrs[name].Field)
	}
	if livePK := live.PrimaryKey(); !equalFoldSlice(livePK, wantPK) {
		changes = append(changes, SchemaChange{
			Kind:    ChangePrimaryKey,
			From:    strings.Join(livePK, ", "),
			To:      strings.Join(wantPK, ", "),
			rebuild: true,
		})
	}

	// Columns the model declares.
	declared := make(map[string]bool, len(m.names))
	for _, name := range m.names {
		attr := m.attrs[name]
		declared[strings.ToLower(attr.Field)] = true
		wantType, err := d.ColumnType(attr.Type)
		if err != nil {
			return nil, fmt.Errorf("type for %s.%s: %w", m.name, name, err)
		}
		col, ok := live.Column(attr.Field)
		if !ok {
			change := SchemaChange{
				Kind:      ChangeAddColumn,
				Attribute: name,
				Column:    attr.Field,
				To:        wantType,
			}
			// SQLite's ADD COLUMN cannot add a PRIMARY KEY or UNIQUE column, and
			// cannot add a NOT NULL column without a default. Those need the
			// table rebuilt.
			if attr.PrimaryKey || attr.Unique || (!attr.Nullable() && attr.DefaultValue == nil) {
				change.rebuild = true
			}
			changes = append(changes, change)
			continue
		}
		if !typeEquivalent(col.Type, wantType) {
			changes = append(changes, SchemaChange{
				Kind:      ChangeColumnType,
				Attribute: name,
				Column:    attr.Field,
				From:      col.Type,
				To:        wantType,
				rebuild:   true,
			})
		}
		if col.PrimaryKeyPos == 0 && col.NotNull == attr.Nullable() {
			changes = append(changes, SchemaChange{
				Kind:      ChangeColumnNull,
				Attribute: name,
				Column:    attr.Field,
				From:      nullabilityWord(!col.NotNull),
				To:        nullabilityWord(attr.Nullable()),
				rebuild:   true,
			})
		}
		if want, comparable := renderableDefault(attr); comparable && !strings.EqualFold(strings.TrimSpace(col.Default), want) {
			changes = append(changes, SchemaChange{
				Kind:      ChangeColumnDefault,
				Attribute: name,
				Column:    attr.Field,
				From:      col.Default,
				To:        want,
				rebuild:   true,
			})
		}
	}

	// Columns the table has and the model does not.
	for _, col := range live.Columns {
		if declared[strings.ToLower(col.Name)] {
			continue
		}
		changes = append(changes, SchemaChange{
			Kind:        ChangeDropColumn,
			Column:      col.Name,
			From:        col.Type,
			destructive: true,
			// SQLite refuses DROP COLUMN on a primary-key, UNIQUE or indexed
			// column, so those have to go through a rebuild.
			rebuild: col.PrimaryKeyPos > 0 || columnIndexed(live, col.Name),
		})
	}

	// Indexes. Only indexes created by CREATE INDEX are compared: an index
	// backing a UNIQUE constraint or the primary key belongs to the table
	// definition, not to WithIndexes.
	wantIndexes, err := m.declaredIndexes()
	if err != nil {
		return nil, err
	}
	wantByName := make(map[string]IndexInfo, len(wantIndexes))
	for _, idx := range wantIndexes {
		wantByName[strings.ToLower(idx.Name)] = idx
		existing, ok := live.Index(idx.Name)
		if !ok {
			changes = append(changes, SchemaChange{
				Kind:  ChangeAddIndex,
				Index: idx.Name,
				To:    indexSignature(idx),
			})
			continue
		}
		if indexSignature(existing) != indexSignature(idx) {
			// Recreating an index loses nothing, so a changed index is a drop
			// and an add rather than a rebuild.
			changes = append(changes,
				SchemaChange{Kind: ChangeDropIndex, Index: existing.Name, From: indexSignature(existing)},
				SchemaChange{Kind: ChangeAddIndex, Index: idx.Name, To: indexSignature(idx)},
			)
		}
	}
	for _, idx := range live.Indexes {
		if idx.Origin != "c" || strings.HasPrefix(strings.ToLower(idx.Name), "sqlite_") {
			continue
		}
		if _, ok := wantByName[strings.ToLower(idx.Name)]; ok {
			continue
		}
		changes = append(changes, SchemaChange{Kind: ChangeDropIndex, Index: idx.Name, From: indexSignature(idx)})
	}

	// UNIQUE constraints in the table definition rather than in a CREATE INDEX.
	// They are not indexes anyone declared, so they are compared as constraints:
	// SQLite can neither add nor remove one in place.
	for _, name := range m.names {
		attr := m.attrs[name]
		if !attr.Unique || uniqueIndexCovers(live, []string{attr.Field}) {
			continue
		}
		changes = append(changes, SchemaChange{
			Kind: ChangeAddIndex, Attribute: name, Column: attr.Field,
			To: "UNIQUE (" + strings.ToLower(attr.Field) + ")", rebuild: true,
		})
	}
	for _, idx := range live.Indexes {
		if idx.Origin != "u" || m.uniquenessCovers(idx.Columns) {
			continue
		}
		changes = append(changes, SchemaChange{
			Kind: ChangeDropIndex, Index: idx.Name, From: indexSignature(idx),
			// Giving up a uniqueness guarantee is not data loss, but it is a
			// guarantee the caller may not have meant to drop, and it cannot be
			// put back once duplicate rows exist.
			destructive: true, rebuild: true,
		})
	}

	// Foreign keys, by (column, table, key) only.
	fkChanges, err := m.diffForeignKeys(live)
	if err != nil {
		return nil, err
	}
	changes = append(changes, fkChanges...)
	return changes, nil
}

// diffForeignKeys compares the model's References against the live table's
// foreign keys, ignoring referential actions.
func (m *Model) diffForeignKeys(live TableInfo) ([]SchemaChange, error) {
	liveByColumn := make(map[string]ForeignKeyInfo, len(live.ForeignKeys))
	for _, fk := range live.ForeignKeys {
		liveByColumn[strings.ToLower(fk.Column)] = fk
	}
	var changes []SchemaChange
	seen := make(map[string]bool, len(liveByColumn))
	for _, name := range m.names {
		attr := m.attrs[name]
		if attr.References == nil {
			continue
		}
		table, key, err := resolveReference(attr.References)
		if err != nil {
			return nil, fmt.Errorf("reference on %s.%s: %w", m.name, name, err)
		}
		want := table + "." + key
		seen[strings.ToLower(attr.Field)] = true
		fk, ok := liveByColumn[strings.ToLower(attr.Field)]
		if !ok {
			changes = append(changes, SchemaChange{
				Kind: ChangeForeignKey, Attribute: name, Column: attr.Field,
				To: want, rebuild: true,
			})
			continue
		}
		// A NULL "to" means the parent's primary key was not named; take the
		// model's word for which column that is.
		got := fk.Table + "." + fk.Key
		if fk.Key == "" {
			got = fk.Table + "." + key
		}
		if !strings.EqualFold(got, want) {
			changes = append(changes, SchemaChange{
				Kind: ChangeForeignKey, Attribute: name, Column: attr.Field,
				From: got, To: want, rebuild: true,
			})
		}
	}
	for _, fk := range live.ForeignKeys {
		if seen[strings.ToLower(fk.Column)] {
			continue
		}
		if _, declared := m.columnAttribute(fk.Column); !declared {
			// The whole column is going away; the drop covers its foreign key.
			continue
		}
		changes = append(changes, SchemaChange{
			Kind: ChangeForeignKey, Column: fk.Column,
			From: fk.Table + "." + fk.Key, rebuild: true,
		})
	}
	return changes, nil
}

// columnAttribute finds the attribute whose physical column is name.
func (m *Model) columnAttribute(name string) (string, bool) {
	for _, attrName := range m.names {
		if strings.EqualFold(m.attrs[attrName].Field, name) {
			return attrName, true
		}
	}
	return "", false
}

// AlterTable migrates the model's table towards the model's declared
// attributes, creating it first if it does not exist, and returns the changes it
// applied. It is what [Sync] with [Alter] runs, exposed so that a caller can
// migrate one model without syncing every other.
//
// It applies nothing at all unless it can apply everything: the whole diff is
// planned first, and any part of it needing an option that was not passed
// produces an *[AlterRefusedError] before a single statement runs. See [Alter],
// [AlterDrop] and [AlterRebuild] for which differences need what.
func (m *Model) AlterTable(ctx context.Context, opts ...SyncOption) ([]SchemaChange, error) {
	o := resolveSyncOptions(opts)
	o.Alter = true
	return m.alterTableWith(ctx, o)
}

func (m *Model) alterTableWith(ctx context.Context, o SyncOptions) ([]SchemaChange, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}
	live, err := m.inspectTable(ctx, o)
	if err != nil {
		return nil, err
	}
	if !live.Exists {
		// Nothing to migrate: create the table and its indexes.
		create := o
		create.Alter = false
		if err := m.syncWith(ctx, create); err != nil {
			return nil, err
		}
		return nil, nil
	}

	changes, err := m.diffAgainst(live)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, nil
	}

	// Refuse first, in full, so that a refusal never leaves half a migration.
	var destructive, needRebuild []SchemaChange
	for _, c := range changes {
		if c.destructive {
			destructive = append(destructive, c)
		}
		if c.rebuild {
			needRebuild = append(needRebuild, c)
		}
	}
	if len(destructive) > 0 && !o.AlterDrop {
		return nil, &AlterRefusedError{
			Model: m.name, Table: m.table, Changes: destructive,
			Reason: "would lose data; pass AlterDrop(true) to allow it",
		}
	}
	if len(needRebuild) > 0 && !o.AlterRebuild {
		return nil, &AlterRefusedError{
			Model: m.name, Table: m.table, Changes: needRebuild,
			Reason: m.seq.dialect.Name() + " cannot change this in place; pass AlterRebuild(true) to rebuild the table",
		}
	}

	if len(needRebuild) > 0 {
		if err := m.rebuildTable(ctx, o, live, changes); err != nil {
			return nil, err
		}
		return changes, nil
	}
	if err := m.applyInPlace(ctx, o, changes); err != nil {
		return nil, err
	}
	return changes, nil
}

// applyInPlace applies a diff that needs no rebuild: DROP INDEX first (an index
// can block a DROP COLUMN), then ADD COLUMN, then DROP COLUMN, then CREATE INDEX.
func (m *Model) applyInPlace(ctx context.Context, o SyncOptions, changes []SchemaChange) error {
	table, err := m.seq.dialect.Quote(m.table)
	if err != nil {
		return err
	}
	var stmts []string
	for _, c := range changes {
		if c.Kind != ChangeDropIndex {
			continue
		}
		stmt, err := m.dropIndexSQL(c.Index)
		if err != nil {
			return err
		}
		stmts = append(stmts, stmt)
	}
	for _, c := range changes {
		if c.Kind != ChangeAddColumn {
			continue
		}
		def, err := m.columnDefinition(c.Attribute)
		if err != nil {
			return err
		}
		stmts = append(stmts, "ALTER TABLE "+table+" ADD COLUMN "+def)
	}
	for _, c := range changes {
		if c.Kind != ChangeDropColumn {
			continue
		}
		col, err := m.seq.dialect.Quote(c.Column)
		if err != nil {
			return err
		}
		stmts = append(stmts, "ALTER TABLE "+table+" DROP COLUMN "+col)
	}
	for _, c := range changes {
		if c.Kind != ChangeAddIndex || c.Index == "" {
			// An unnamed add-index change is a column-level UNIQUE constraint,
			// which only the rebuild path can apply.
			continue
		}
		idx, err := m.declaredIndex(c.Index)
		if err != nil {
			return err
		}
		stmt, err := m.createIndexSQL(idx)
		if err != nil {
			return err
		}
		stmts = append(stmts, stmt)
	}
	return m.runDDL(ctx, o, stmts)
}

// declaredIndex looks up one of the model's declared indexes by resolved name.
func (m *Model) declaredIndex(name string) (IndexInfo, error) {
	indexes, err := m.declaredIndexes()
	if err != nil {
		return IndexInfo{}, err
	}
	for _, idx := range indexes {
		if strings.EqualFold(idx.Name, name) {
			return idx, nil
		}
	}
	return IndexInfo{}, fmt.Errorf("%w: model %s declares no index %q", ErrUnknownAttribute, m.name, name)
}

// runDDL executes statements in order, in the caller's transaction when there is
// one and in a transaction of its own otherwise, so that a failure part-way
// leaves the schema as it was.
func (m *Model) runDDL(ctx context.Context, o SyncOptions, stmts []string) error {
	if len(stmts) == 0 {
		return nil
	}
	if o.Tx != nil {
		for _, stmt := range stmts {
			if _, err := m.exec(ctx, Query{Tx: o.Tx}, stmt, nil); err != nil {
				return err
			}
		}
		return nil
	}
	return m.seq.Transaction(ctx, func(tx *Tx) error {
		for _, stmt := range stmts {
			if _, err := m.exec(ctx, Query{Tx: tx}, stmt, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// rebuildSuffix names the scratch table a rebuild copies into.
const rebuildSuffix = "_sequelize_rebuild"

// rebuildTable applies a whole diff by rebuilding the table: create the target
// schema under a scratch name, copy every row that both schemas share a column
// for, drop the original, rename the scratch table into its place, and recreate
// the indexes. It is SQLite's documented answer to having no ALTER COLUMN, and
// it runs as one transaction.
func (m *Model) rebuildTable(ctx context.Context, o SyncOptions, live TableInfo, changes []SchemaChange) error {
	d := m.seq.dialect
	tmpName := m.table + rebuildSuffix
	if err := ValidateIdentifier(tmpName); err != nil {
		return err
	}

	// The rebuilt table carries the model's indexes. It cannot carry any other,
	// because every index the model does not declare is in the diff as a
	// ChangeDropIndex — that is what makes dropping them a reported change
	// rather than a side effect of the rebuild.
	declaredIdx, err := m.declaredIndexes()
	if err != nil {
		return err
	}

	// A NOT NULL column with no default cannot be filled for existing rows.
	if len(live.Columns) > 0 {
		var unfillable []SchemaChange
		for _, c := range changes {
			if c.Kind != ChangeAddColumn {
				continue
			}
			attr := m.attrs[c.Attribute]
			if attr.Nullable() || attr.DefaultValue != nil || attr.AutoIncrement {
				continue
			}
			unfillable = append(unfillable, c)
		}
		if len(unfillable) > 0 {
			empty, err := m.tableIsEmpty(ctx, o)
			if err != nil {
				return err
			}
			if !empty {
				return &AlterRefusedError{
					Model: m.name, Table: m.table, Changes: unfillable,
					Reason: "cannot add a NOT NULL column with no default to a table that already has rows; give it a DefaultValue or migrate by hand",
				}
			}
		}
	}

	// Columns to copy: every column both schemas have.
	var copyCols []string
	for _, col := range live.Columns {
		if attrName, ok := m.columnAttribute(col.Name); ok {
			quoted, err := d.Quote(m.attrs[attrName].Field)
			if err != nil {
				return err
			}
			copyCols = append(copyCols, quoted)
		}
	}

	createTmp, err := m.createTableSQL(tmpName)
	if err != nil {
		return err
	}
	quotedTmp, err := d.Quote(tmpName)
	if err != nil {
		return err
	}
	quotedTable, err := d.Quote(m.table)
	if err != nil {
		return err
	}

	stmts := []string{createTmp}
	if len(copyCols) > 0 {
		cols := strings.Join(copyCols, ", ")
		stmts = append(stmts, "INSERT INTO "+quotedTmp+" ("+cols+") SELECT "+cols+" FROM "+quotedTable)
	}
	stmts = append(stmts,
		"DROP TABLE "+quotedTable,
		"ALTER TABLE "+quotedTmp+" RENAME TO "+quotedTable,
	)
	for _, idx := range declaredIdx {
		stmt, err := m.createIndexSQL(idx)
		if err != nil {
			return err
		}
		stmts = append(stmts, stmt)
	}

	// The scratch name has to be free: a leftover from an interrupted rebuild is
	// a sign something went wrong, not something to overwrite.
	existing, err := m.inspectNamed(ctx, o, tmpName)
	if err != nil {
		return err
	}
	if existing.Exists {
		return fmt.Errorf("%w: %s already exists; a previous rebuild of %s did not finish",
			ErrInvalidQuery, tmpName, m.table)
	}
	return m.runDDL(ctx, o, stmts)
}

// tableIsEmpty reports whether the model's table has no rows.
func (m *Model) tableIsEmpty(ctx context.Context, o SyncOptions) (bool, error) {
	quoted, err := m.seq.dialect.Quote(m.table)
	if err != nil {
		return false, err
	}
	q, err := m.querierFor(Query{Tx: o.Tx})
	if err != nil {
		return false, err
	}
	empty := true
	err = scanRows(ctx, q, "SELECT 1 FROM "+quoted+" LIMIT 1", nil, func(*sql.Rows) error {
		empty = false
		return nil
	})
	return empty, err
}

// uniquenessCovers reports whether the model already guarantees uniqueness over
// exactly cols, through a unique attribute, a declared unique index, or the
// primary key.
func (m *Model) uniquenessCovers(cols []string) bool {
	if len(cols) == 0 {
		return false
	}
	candidates := make([][]string, 0, 4)
	pk := make([]string, 0, len(m.pk))
	for _, name := range m.pk {
		pk = append(pk, m.attrs[name].Field)
	}
	candidates = append(candidates, pk)
	for _, name := range m.names {
		if m.attrs[name].Unique {
			candidates = append(candidates, []string{m.attrs[name].Field})
		}
	}
	declared, err := m.declaredIndexes()
	if err == nil {
		for _, idx := range declared {
			if idx.Unique {
				candidates = append(candidates, idx.Columns)
			}
		}
	}
	for _, candidate := range candidates {
		if equalFoldSet(candidate, cols) {
			return true
		}
	}
	return false
}

// uniqueIndexCovers reports whether the live table has a unique index over
// exactly cols, whichever way that uniqueness was declared.
func uniqueIndexCovers(live TableInfo, cols []string) bool {
	if pk := live.PrimaryKey(); equalFoldSet(pk, cols) {
		return true
	}
	for _, idx := range live.Indexes {
		if idx.Unique && !idx.Partial && equalFoldSet(idx.Columns, cols) {
			return true
		}
	}
	return false
}

// columnIndexed reports whether any index on the table covers the column.
func columnIndexed(live TableInfo, column string) bool {
	for _, idx := range live.Indexes {
		for _, col := range idx.Columns {
			if strings.EqualFold(col, column) {
				return true
			}
		}
	}
	return false
}

// indexSignature renders an index's shape for comparison and for diff output.
func indexSignature(idx IndexInfo) string {
	prefix := ""
	if idx.Unique {
		prefix = "UNIQUE "
	}
	cols := strings.ToLower(strings.Join(idx.Columns, ", "))
	if idx.Partial {
		cols += " (partial)"
	}
	return prefix + "(" + cols + ")"
}

// nullabilityWord spells a nullability for a diff message.
func nullabilityWord(nullable bool) string {
	if nullable {
		return "NULL"
	}
	return "NOT NULL"
}

// renderableDefault returns the DDL literal the model wants for an attribute's
// default and whether it can be compared at all. A default the package cannot
// render — a function, a time, a long or quote-bearing string — is applied in Go
// on every write and never reaches the schema, so there is nothing to compare.
func renderableDefault(attr Attribute) (literal string, comparable bool) {
	if attr.DefaultValue == nil {
		// No default declared: an existing DEFAULT in the schema is a difference.
		return "", true
	}
	rendered, err := defaultLiteral(attr.DefaultValue)
	if err != nil {
		return "", false
	}
	return rendered, true
}

// typeEquivalent compares a live column's declared type against the dialect's
// rendering of the model's type. It is deliberately lenient about spelling and
// strict about parameters: "int" matches "INTEGER" because SQLite gives them the
// same affinity, but "VARCHAR(64)" does not match "VARCHAR(255)".
func typeEquivalent(live, want string) bool {
	liveNorm := normaliseType(live)
	wantNorm := normaliseType(want)
	if liveNorm == wantNorm {
		return true
	}
	liveBase, liveArgs := splitType(liveNorm)
	wantBase, wantArgs := splitType(wantNorm)
	if liveArgs != wantArgs {
		return false
	}
	return typeAffinity(liveBase) == typeAffinity(wantBase)
}

// normaliseType upper-cases a declared type and strips its whitespace.
func normaliseType(t string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(t) {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// splitType separates "VARCHAR(255)" into "VARCHAR" and "(255)".
func splitType(t string) (base, args string) {
	if i := strings.IndexByte(t, '('); i >= 0 {
		return t[:i], t[i:]
	}
	return t, ""
}

// typeAffinity applies SQLite's five affinity rules to a declared type name, so
// that aliases of the same storage class compare equal.
func typeAffinity(base string) string {
	switch {
	case strings.Contains(base, "INT"):
		return "INTEGER"
	case strings.Contains(base, "CHAR"), strings.Contains(base, "CLOB"), strings.Contains(base, "TEXT"):
		return "TEXT"
	case base == "", strings.Contains(base, "BLOB"):
		return "BLOB"
	case strings.Contains(base, "REAL"), strings.Contains(base, "FLOA"), strings.Contains(base, "DOUB"):
		return "REAL"
	default:
		return "NUMERIC"
	}
}

// equalFoldSlice compares two ordered identifier lists case-insensitively.
func equalFoldSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// equalFoldSet compares two identifier lists as sets, case-insensitively.
func equalFoldSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	lower := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strings.ToLower(s)
		}
		sort.Strings(out)
		return out
	}
	la, lb := lower(a), lower(b)
	for i := range la {
		if la[i] != lb[i] {
			return false
		}
	}
	return true
}
