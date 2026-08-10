package sequelize

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SyncOptions carries the settings a [SyncOption] sets.
type SyncOptions struct {
	// Force drops each table before recreating it, losing its data. It is
	// upstream's `force: true`.
	Force bool
	// Alter migrates an existing table towards the model instead of leaving it
	// alone. See [Alter] for exactly which differences are applied.
	Alter bool
	// AlterDrop permits the destructive half of an alter: dropping a column the
	// model no longer declares. See [AlterDrop].
	AlterDrop bool
	// AlterRebuild permits the copy-and-rename table rebuild that a retype, a
	// nullability change or a default change needs on SQLite. See [AlterRebuild].
	AlterRebuild bool
	// Tx runs the DDL inside a transaction. Not every backend makes DDL
	// transactional; SQLite does.
	Tx *Tx
}

// SyncOption configures [Sequelize.Sync], [Model.Sync] and the drop helpers.
type SyncOption func(*SyncOptions)

// Force makes Sync drop each table before creating it. Everything in the table
// is lost; it exists for tests and scratch databases.
func Force(force bool) SyncOption {
	return func(o *SyncOptions) { o.Force = force }
}

// SyncTx runs the DDL inside tx.
func SyncTx(tx *Tx) SyncOption {
	return func(o *SyncOptions) { o.Tx = tx }
}

func resolveSyncOptions(opts []SyncOption) SyncOptions {
	var o SyncOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// Sync creates the table for every defined model, in definition order, plus each
// model's indexes.
//
// An existing table is left alone unless [Force] or [Alter] is passed: Force
// drops and recreates it, losing its data, while Alter migrates it towards the
// model. Without either, a model whose attributes changed needs a migration
// written by hand.
func (s *Sequelize) Sync(ctx context.Context, opts ...SyncOption) error {
	o := resolveSyncOptions(opts)
	for _, m := range s.definedModels() {
		if err := m.syncWith(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// Drop drops every defined model's table, in reverse definition order so that a
// table referenced by a foreign key goes after the table referencing it.
func (s *Sequelize) Drop(ctx context.Context, opts ...SyncOption) error {
	o := resolveSyncOptions(opts)
	models := s.definedModels()
	for i := len(models) - 1; i >= 0; i-- {
		if err := models[i].dropWith(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// Sync creates the model's table if it does not exist, then its indexes. With
// [Alter] an existing table is migrated towards the model instead of being left
// alone; see [Model.AlterTable].
func (m *Model) Sync(ctx context.Context, opts ...SyncOption) error {
	return m.syncWith(ctx, resolveSyncOptions(opts))
}

func (m *Model) syncWith(ctx context.Context, o SyncOptions) error {
	if err := m.Err(); err != nil {
		return err
	}
	if o.Force {
		if err := m.dropWith(ctx, o); err != nil {
			return err
		}
	} else if o.Alter {
		// AlterTable creates the table when it does not exist yet, so the
		// create path below has nothing left to do either way.
		if _, err := m.alterTableWith(ctx, o); err != nil {
			return err
		}
		return nil
	}
	ddl, err := m.CreateTableSQL()
	if err != nil {
		return err
	}
	if _, err := m.exec(ctx, Query{Tx: o.Tx}, ddl, nil); err != nil {
		return err
	}
	indexes, err := m.CreateIndexSQL()
	if err != nil {
		return err
	}
	for _, stmt := range indexes {
		if _, err := m.exec(ctx, Query{Tx: o.Tx}, stmt, nil); err != nil {
			return err
		}
	}
	return nil
}

// Drop drops the model's table if it exists.
func (m *Model) Drop(ctx context.Context, opts ...SyncOption) error {
	return m.dropWith(ctx, resolveSyncOptions(opts))
}

func (m *Model) dropWith(ctx context.Context, o SyncOptions) error {
	stmt, err := m.DropTableSQL()
	if err != nil {
		return err
	}
	_, err = m.exec(ctx, Query{Tx: o.Tx}, stmt, nil)
	return err
}

// Truncate removes every row from the model's table, soft-deleted rows included.
//
// It is rendered as an unqualified DELETE rather than TRUNCATE, which SQLite does
// not have and which several backends spell differently; the effect on the rows
// is the same, the effect on auto-increment counters is not.
func (m *Model) Truncate(ctx context.Context, opts ...SyncOption) error {
	if err := m.Err(); err != nil {
		return err
	}
	o := resolveSyncOptions(opts)
	quoted, err := m.seq.dialect.Quote(m.table)
	if err != nil {
		return err
	}
	_, err = m.exec(ctx, Query{Tx: o.Tx}, "DELETE FROM "+quoted, nil)
	return err
}

// DropTableSQL returns the DROP TABLE statement for the model.
func (m *Model) DropTableSQL() (string, error) {
	if err := m.Err(); err != nil {
		return "", err
	}
	quoted, err := m.seq.dialect.Quote(m.table)
	if err != nil {
		return "", err
	}
	return "DROP TABLE IF EXISTS " + quoted, nil
}

// CreateTableSQL returns the CREATE TABLE IF NOT EXISTS statement for the model,
// with columns in attribute order: primary keys, then the declared attributes in
// name order, then the timestamps.
//
// The statement carries no bind arguments, because DDL cannot take them. Every
// identifier in it has been validated, and the only literals it can contain are
// the scalar defaults described on [Attribute.DefaultValue].
func (m *Model) CreateTableSQL() (string, error) {
	return m.createTableSQL(m.table)
}

// createTableSQL renders the model's CREATE TABLE against an arbitrary table
// name. Everything but the name comes from the model, which is what the SQLite
// table rebuild in sync_alter.go needs: it builds the target schema under a
// temporary name, copies the rows across, and renames.
func (m *Model) createTableSQL(tableName string) (string, error) {
	if err := m.Err(); err != nil {
		return "", err
	}
	d := m.seq.dialect
	table, err := d.Quote(tableName)
	if err != nil {
		return "", err
	}
	singlePK := len(m.pk) == 1
	defs := make([]string, 0, len(m.names)+1)
	for _, name := range m.names {
		def, err := m.columnDefinition(name)
		if err != nil {
			return "", err
		}
		defs = append(defs, def)
	}
	if !singlePK && len(m.pk) > 0 {
		cols := make([]string, 0, len(m.pk))
		for _, name := range m.pk {
			col, err := d.Quote(m.attrs[name].Field)
			if err != nil {
				return "", err
			}
			cols = append(cols, col)
		}
		defs = append(defs, "PRIMARY KEY ("+strings.Join(cols, ", ")+")")
	}
	return "CREATE TABLE IF NOT EXISTS " + table + " (" + strings.Join(defs, ", ") + ")", nil
}

// columnDefinition renders one attribute's column definition exactly as
// [Model.CreateTableSQL] writes it, inline PRIMARY KEY and all. It is shared
// with the ALTER TABLE ... ADD COLUMN path in sync_alter.go, which only ever
// asks for non-primary-key columns, so the two renderings cannot drift.
func (m *Model) columnDefinition(name string) (string, error) {
	attr, ok := m.attrs[name]
	if !ok {
		return "", fmt.Errorf("%w: %s.%s", ErrUnknownAttribute, m.name, name)
	}
	d := m.seq.dialect
	col, err := d.Quote(attr.Field)
	if err != nil {
		return "", err
	}
	singlePK := len(m.pk) == 1 && m.pk[0] == name
	if singlePK && attr.AutoIncrement {
		return d.AutoIncrementColumn(col, attr.Type)
	}
	var def strings.Builder
	sqlType, err := d.ColumnType(attr.Type)
	if err != nil {
		return "", err
	}
	def.WriteString(col)
	def.WriteString(" ")
	def.WriteString(sqlType)
	if singlePK {
		def.WriteString(" PRIMARY KEY")
	}
	if !attr.Nullable() {
		def.WriteString(" NOT NULL")
	}
	if attr.Unique {
		def.WriteString(" UNIQUE")
	}
	if attr.DefaultValue != nil {
		literal, err := defaultLiteral(attr.DefaultValue)
		if err != nil {
			return "", fmt.Errorf("default for %s.%s: %w", m.name, name, err)
		}
		def.WriteString(" DEFAULT ")
		def.WriteString(literal)
	}
	if attr.References != nil {
		ref, err := m.referenceClause(attr.References)
		if err != nil {
			return "", fmt.Errorf("reference on %s.%s: %w", m.name, name, err)
		}
		def.WriteString(ref)
	}
	return def.String(), nil
}

// referenceClause renders " REFERENCES <table> (<column>)" plus any referential
// actions.
func (m *Model) referenceClause(ref *Reference) (string, error) {
	d := m.seq.dialect
	table, key, err := resolveReference(ref)
	if err != nil {
		return "", err
	}
	quotedTable, err := d.Quote(table)
	if err != nil {
		return "", err
	}
	quotedKey, err := d.Quote(key)
	if err != nil {
		return "", err
	}
	out := " REFERENCES " + quotedTable + " (" + quotedKey + ")"
	if ref.OnDelete != "" {
		action, err := referentialAction(ref.OnDelete)
		if err != nil {
			return "", err
		}
		out += " ON DELETE " + action
	}
	if ref.OnUpdate != "" {
		action, err := referentialAction(ref.OnUpdate)
		if err != nil {
			return "", err
		}
		out += " ON UPDATE " + action
	}
	return out, nil
}

// resolveReference resolves a [Reference] to the physical table and column it
// points at, filling either in from Reference.Model when it was left empty. The
// foreign-key diff in sync_alter.go compares against the same pair the DDL
// emits.
func resolveReference(ref *Reference) (table, key string, err error) {
	table = ref.Table
	key = ref.Key
	if ref.Model != nil {
		if err := ref.Model.Err(); err != nil {
			return "", "", err
		}
		if table == "" {
			table = ref.Model.TableName()
		}
		if key == "" {
			if len(ref.Model.pk) != 1 {
				return "", "", fmt.Errorf("%w: referenced model %s has no single-column primary key", ErrNoPrimaryKey, ref.Model.name)
			}
			key = ref.Model.attrs[ref.Model.pk[0]].Field
		}
	}
	if table == "" || key == "" {
		return "", "", fmt.Errorf("%w: reference needs a table and a key", ErrInvalidQuery)
	}
	return table, key, nil
}

// referentialAction validates a referential action as a sequence of bare words,
// so that nothing but SQL keywords can reach the statement.
func referentialAction(action string) (string, error) {
	words := strings.Fields(strings.ToUpper(action))
	if len(words) == 0 {
		return "", fmt.Errorf("%w: empty referential action", ErrInvalidQuery)
	}
	for _, w := range words {
		if err := ValidateIdentifier(w); err != nil {
			return "", err
		}
	}
	return strings.Join(words, " "), nil
}

// CreateIndexSQL returns one CREATE INDEX statement per index declared with
// [WithIndexes]. An index with no name is named idx_<table>_<fields>.
func (m *Model) CreateIndexSQL() ([]string, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}
	// declaredIndexes and createIndexSQL live in sync_alter.go, which needs the
	// same two halves separately: the resolved index shapes to diff against the
	// live schema, and the rendering to recreate one on a rebuilt table.
	indexes, err := m.declaredIndexes()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(indexes))
	for _, idx := range indexes {
		stmt, err := m.createIndexSQL(idx)
		if err != nil {
			return nil, err
		}
		out = append(out, stmt)
	}
	return out, nil
}

// safeDefaultLength caps how long a string default may be before it is refused.
const safeDefaultLength = 255

// defaultLiteral renders a scalar default as SQL text. DDL cannot take bind
// parameters, so this is the one place the package writes a value into a
// statement — and it therefore accepts only what it can render provably safely:
// numbers, booleans, and short strings of printable characters with no quote or
// backslash. Anything else is refused with [ErrUnsupported]; supply the default
// through [Attribute.DefaultValue] alone, which is applied in Go on every write,
// or write the DDL yourself.
func defaultLiteral(v any) (string, error) {
	switch value := v.(type) {
	case bool:
		if value {
			return "1", nil
		}
		return "0", nil
	case int:
		return strconv.Itoa(value), nil
	case int8, int16, int32, int64:
		return fmt.Sprintf("%d", value), nil
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", value), nil
	case float32:
		return strconv.FormatFloat(float64(value), 'g', -1, 32), nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case string:
		if !safeDefaultString(value) {
			return "", fmt.Errorf("%w: string default %q cannot be rendered as a DDL literal", ErrUnsupported, value)
		}
		return "'" + value + "'", nil
	default:
		return "", fmt.Errorf("%w: default of type %T cannot be rendered as a DDL literal", ErrUnsupported, v)
	}
}

func safeDefaultString(s string) bool {
	if len(s) > safeDefaultLength {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e || r == '\'' || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}
