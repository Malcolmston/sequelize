package sequelize

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Values is an attribute-name-keyed record: the port's stand-in for a Sequelize
// model instance. Keys are *attribute* names as declared in [Attributes], not
// physical column names; the mapping to columns is the model's job.
//
// Sequelize is dynamically typed, so the port is map-based rather than
// struct-tag based. This is its single biggest deliberate deviation from Go ORM
// convention and is recorded in API-DEVIATIONS.md.
type Values map[string]any

// Clone returns a shallow copy of v. Nested values are shared.
func (v Values) Clone() Values {
	out := make(Values, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out
}

// Keys returns v's attribute names, sorted, so callers get deterministic output.
func (v Values) Keys() []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Validator checks one attribute value and returns a message-bearing error when
// it is unacceptable. Validators are declared on [Attribute.Validate] and are
// run before every write by [Model.Create], [Model.BulkCreate] and
// [Model.Update]; see validation.go for the built-in set ([NotEmpty], [Len],
// [MinValue], [MaxValue], [Matches], [IsIn], [IsEmail], [Custom], ...).
//
// A validator is never called with a nil value: nullability is
// [Attribute.AllowNull]'s business.
type Validator func(value any) error

// Reference declares a foreign key target for an attribute. Sync renders it as a
// REFERENCES clause.
type Reference struct {
	// Table is the referenced physical table name. When Model is set, Table is
	// derived from it and may be left empty.
	Table string
	// Model is the referenced model, if it is defined on the same connection.
	Model *Model
	// Key is the referenced column name; it defaults to the target's single
	// primary key column.
	Key string
	// OnDelete is the referential action, for example "CASCADE" or "SET NULL".
	// It must be a bare SQL word sequence; anything else is rejected.
	OnDelete string
	// OnUpdate is the referential action for updates, spelled as OnDelete.
	OnUpdate string
}

// Attribute describes one column of a model.
//
// The zero value is nullable, which mirrors upstream: a Sequelize column is
// nullable unless told otherwise. Only [Attribute.Type] is required.
type Attribute struct {
	// Type is the abstract column type; it is required.
	Type DataType
	// AllowNull mirrors upstream: a column is nullable unless told otherwise, so
	// the zero value — nil — is nullable.
	//
	// It is a *bool rather than a bool because a Go bool cannot default to true:
	// with a plain bool, "unset" and "NOT NULL" would be the same value and one of
	// the two meanings would be lost. Write [NotNull] to require a value and
	// [Null] to say nullable explicitly; ask [Attribute.Nullable] rather than
	// dereferencing it.
	AllowNull *bool
	// PrimaryKey marks the attribute as part of the primary key.
	PrimaryKey bool
	// AutoIncrement asks the database to generate the value. It is honoured only
	// for a single-column integer primary key.
	AutoIncrement bool
	// Unique adds a UNIQUE constraint on the column.
	Unique bool
	// DefaultValue is applied in Go when a write omits the attribute, and is also
	// rendered as a DDL DEFAULT when it is a scalar.
	DefaultValue any
	// Field is the physical column name. It defaults to the snake_cased
	// attribute key.
	Field string
	// References declares a foreign key target.
	References *Reference
	// Validate lists per-attribute validators, run before every write. See
	// [Validator].
	Validate []Validator
	// Comment is a human-readable note. Dialects that cannot express column
	// comments ignore it.
	Comment string
}

// Nullable reports whether the attribute permits NULL, which it does unless
// [Attribute.AllowNull] was set to [NotNull].
func (a Attribute) Nullable() bool { return a.AllowNull == nil || *a.AllowNull }

// Bool returns a pointer to v, for setting [Attribute.AllowNull] and other
// tri-state fields inline.
func Bool(v bool) *bool { return &v }

// NotNull is the [Attribute.AllowNull] value that requires a value: it renders
// NOT NULL. It reads as `AllowNull: sequelize.NotNull()`, which is this port's
// spelling of upstream's `allowNull: false`.
func NotNull() *bool { return Bool(false) }

// Null is the [Attribute.AllowNull] value that permits NULL. It is the default,
// so it only ever needs writing for emphasis.
func Null() *bool { return Bool(true) }

// Attributes maps attribute names to their definitions.
type Attributes map[string]Attribute

// Index describes a secondary index to create during [Sequelize.Sync].
type Index struct {
	// Name is the index name. When empty it is derived as
	// idx_<table>_<fields joined by _>.
	Name string
	// Fields lists the attribute names to index, in order.
	Fields []string
	// Unique makes the index a UNIQUE index.
	Unique bool
}

// ModelOptions carries the model-level settings a [ModelOption] sets. Construct
// it through the options rather than by literal.
type ModelOptions struct {
	// TableName overrides the derived physical table name.
	TableName string
	// Timestamps controls the createdAt/updatedAt attributes. Nil means the
	// upstream default, which is on.
	Timestamps *bool
	// Paranoid enables soft deletes via the deletedAt attribute.
	Paranoid bool
	// CreatedAtField, UpdatedAtField and DeletedAtField rename the timestamp
	// attributes. Empty means the defaults createdAt, updatedAt and deletedAt.
	CreatedAtField string
	UpdatedAtField string
	DeletedAtField string
	// Indexes lists secondary indexes to create on Sync.
	Indexes []Index
}

// ModelOption configures a model at [Sequelize.Define] time.
type ModelOption func(*ModelOptions)

// TableName overrides the physical table name, which otherwise is the model
// name snake_cased and pluralised.
func TableName(name string) ModelOption {
	return func(o *ModelOptions) { o.TableName = name }
}

// Timestamps turns the createdAt/updatedAt attributes on or off. They are on by
// default, as upstream.
func Timestamps(enabled bool) ModelOption {
	return func(o *ModelOptions) { o.Timestamps = &enabled }
}

// Paranoid enables soft deletes: [Model.Destroy] stamps deletedAt instead of
// removing the row, every finder hides stamped rows, and [Model.Restore] clears
// the stamp. Enabling it implies timestamps.
func Paranoid(enabled bool) ModelOption {
	return func(o *ModelOptions) { o.Paranoid = enabled }
}

// CreatedAtField renames the creation timestamp attribute.
func CreatedAtField(name string) ModelOption {
	return func(o *ModelOptions) { o.CreatedAtField = name }
}

// UpdatedAtField renames the update timestamp attribute.
func UpdatedAtField(name string) ModelOption {
	return func(o *ModelOptions) { o.UpdatedAtField = name }
}

// DeletedAtField renames the soft-delete timestamp attribute.
func DeletedAtField(name string) ModelOption {
	return func(o *ModelOptions) { o.DeletedAtField = name }
}

// WithIndexes appends secondary indexes to create during [Sequelize.Sync].
func WithIndexes(indexes ...Index) ModelOption {
	return func(o *ModelOptions) { o.Indexes = append(o.Indexes, indexes...) }
}

// Default attribute names for the timestamp columns.
const (
	// DefaultCreatedAt is the attribute name used for the creation timestamp.
	DefaultCreatedAt = "createdAt"
	// DefaultUpdatedAt is the attribute name used for the update timestamp.
	DefaultUpdatedAt = "updatedAt"
	// DefaultDeletedAt is the attribute name used for the soft-delete timestamp.
	DefaultDeletedAt = "deletedAt"
)

// Model is a defined model: a name, a table, resolved attributes, and the
// options that govern timestamps and soft deletes. Obtain one from
// [Sequelize.Define] or [Sequelize.Model]; the zero value is not usable.
type Model struct {
	seq   *Sequelize
	name  string
	table string
	attrs map[string]Attribute
	names []string
	pk    []string
	opts  ModelOptions
	err   error

	// The three concerns that hang off a model without belonging to its shape.
	// assoc and hooks are pointers, so every scoped view of a model shares them;
	// scope is a value, so the view [Model.Scope] returns has its own active set
	// while sharing the definitions. See associations.go, hooks.go and scopes.go.
	assoc *associationSet
	hooks *hookRegistry
	scope scopeState
}

// newModel resolves a definition into a Model. Resolution failures are stored on
// the model so that Define can keep upstream's error-free signature; every
// operation checks Err first.
func newModel(seq *Sequelize, name string, attrs Attributes, opts ...ModelOption) *Model {
	var o ModelOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	m := &Model{
		seq:   seq,
		name:  name,
		opts:  o,
		attrs: map[string]Attribute{},
		assoc: newAssociationSet(),
		hooks: newHookRegistry(),
		scope: newScopeState(),
	}
	m.err = m.resolve(attrs)
	return m
}

// resolve fills in physical column names, injects the implicit primary key and
// the timestamp attributes, and validates every identifier the model will emit.
func (m *Model) resolve(attrs Attributes) error {
	if err := ValidateIdentifier(m.name); err != nil {
		return fmt.Errorf("model name: %w", err)
	}

	m.table = m.opts.TableName
	if m.table == "" {
		m.table = pluralize(snakeCase(m.name))
	}
	if err := ValidateIdentifier(m.table); err != nil {
		return fmt.Errorf("table name for model %s: %w", m.name, err)
	}

	declared := make([]string, 0, len(attrs))
	for attrName, attr := range attrs {
		if err := ValidateIdentifier(attrName); err != nil {
			return fmt.Errorf("attribute name on model %s: %w", m.name, err)
		}
		if attr.Type.IsZero() {
			return fmt.Errorf("%w: attribute %s.%s has no Type", ErrInvalidQuery, m.name, attrName)
		}
		if attr.Field == "" {
			attr.Field = snakeCase(attrName)
		}
		if err := ValidateIdentifier(attr.Field); err != nil {
			return fmt.Errorf("column for %s.%s: %w", m.name, attrName, err)
		}
		if attr.References != nil {
			if err := validateReference(attr.References); err != nil {
				return fmt.Errorf("reference on %s.%s: %w", m.name, attrName, err)
			}
		}
		m.attrs[attrName] = attr
		declared = append(declared, attrName)
	}

	// Primary keys, in declared-name order for determinism.
	sort.Strings(declared)
	for _, attrName := range declared {
		if m.attrs[attrName].PrimaryKey {
			m.pk = append(m.pk, attrName)
		}
	}

	// Upstream adds an auto-increment "id" when no primary key is declared.
	if len(m.pk) == 0 {
		const implicit = "id"
		if _, taken := m.attrs[implicit]; taken {
			return fmt.Errorf("%w: model %s declares %q but no primary key", ErrNoPrimaryKey, m.name, implicit)
		}
		m.attrs[implicit] = Attribute{
			Type:          INTEGER(),
			PrimaryKey:    true,
			AutoIncrement: true,
			AllowNull:     NotNull(),
			Field:         implicit,
		}
		m.pk = []string{implicit}
		declared = append([]string{implicit}, declared...)
	}

	// Column order: primary keys first, then the rest, then timestamps.
	ordered := make([]string, 0, len(declared)+3)
	seen := map[string]bool{}
	for _, pk := range m.pk {
		ordered = append(ordered, pk)
		seen[pk] = true
	}
	for _, attrName := range declared {
		if !seen[attrName] {
			ordered = append(ordered, attrName)
			seen[attrName] = true
		}
	}

	if m.Timestamps() {
		for _, attrName := range []string{m.CreatedAtName(), m.UpdatedAtName()} {
			if _, ok := m.attrs[attrName]; ok {
				continue
			}
			if err := ValidateIdentifier(attrName); err != nil {
				return fmt.Errorf("timestamp attribute on %s: %w", m.name, err)
			}
			m.attrs[attrName] = Attribute{Type: DATE(), AllowNull: NotNull(), Field: snakeCase(attrName)}
			ordered = append(ordered, attrName)
			seen[attrName] = true
		}
	}
	if m.opts.Paranoid {
		attrName := m.DeletedAtName()
		if _, ok := m.attrs[attrName]; !ok {
			if err := ValidateIdentifier(attrName); err != nil {
				return fmt.Errorf("deletedAt attribute on %s: %w", m.name, err)
			}
			m.attrs[attrName] = Attribute{Type: DATE(), AllowNull: Null(), Field: snakeCase(attrName)}
			ordered = append(ordered, attrName)
		}
	}

	m.names = ordered
	for _, idx := range m.opts.Indexes {
		if len(idx.Fields) == 0 {
			return fmt.Errorf("%w: index on %s has no fields", ErrInvalidQuery, m.name)
		}
		for _, f := range idx.Fields {
			if _, ok := m.attrs[f]; !ok {
				return fmt.Errorf("%w: index on %s names %q", ErrUnknownAttribute, m.name, f)
			}
		}
		if idx.Name != "" {
			if err := ValidateIdentifier(idx.Name); err != nil {
				return fmt.Errorf("index on %s: %w", m.name, err)
			}
		}
	}
	return nil
}

func validateReference(ref *Reference) error {
	if ref.Table == "" && ref.Model == nil {
		return fmt.Errorf("%w: reference names neither Table nor Model", ErrInvalidQuery)
	}
	if ref.Table != "" {
		if err := ValidateIdentifier(ref.Table); err != nil {
			return err
		}
	}
	if ref.Key != "" {
		if err := ValidateIdentifier(ref.Key); err != nil {
			return err
		}
	}
	for _, action := range []string{ref.OnDelete, ref.OnUpdate} {
		if action == "" {
			continue
		}
		for _, word := range strings.Fields(action) {
			if err := ValidateIdentifier(word); err != nil {
				return err
			}
		}
	}
	return nil
}

// Err reports why the model definition was rejected, or nil when it is usable.
// Because [Sequelize.Define] mirrors upstream's error-free signature, a bad
// definition surfaces here and from every operation on the model.
func (m *Model) Err() error { return m.err }

// Name returns the model name as passed to [Sequelize.Define].
func (m *Model) Name() string { return m.name }

// TableName returns the physical table name.
func (m *Model) TableName() string { return m.table }

// Sequelize returns the connection the model was defined on.
func (m *Model) Sequelize() *Sequelize { return m.seq }

// Options returns a copy of the model's resolved options.
func (m *Model) Options() ModelOptions {
	o := m.opts
	o.Indexes = append([]Index(nil), m.opts.Indexes...)
	return o
}

// AttributeNames returns every attribute name in column order: primary keys
// first, then the remaining declared attributes in name order, then the
// timestamp attributes.
func (m *Model) AttributeNames() []string {
	return append([]string(nil), m.names...)
}

// Attribute returns the resolved definition of one attribute.
func (m *Model) Attribute(name string) (Attribute, bool) {
	attr, ok := m.attrs[name]
	return attr, ok
}

// Attributes returns a copy of every resolved attribute, including the implicit
// primary key and timestamps.
func (m *Model) Attributes() Attributes {
	out := make(Attributes, len(m.attrs))
	for k, v := range m.attrs {
		out[k] = v
	}
	return out
}

// PrimaryKeys returns the attribute names making up the primary key, in order.
func (m *Model) PrimaryKeys() []string { return append([]string(nil), m.pk...) }

// Column returns the physical column name for an attribute, or an error
// wrapping [ErrUnknownAttribute].
func (m *Model) Column(attribute string) (string, error) {
	attr, ok := m.attrs[attribute]
	if !ok {
		return "", fmt.Errorf("%w: %s.%s", ErrUnknownAttribute, m.name, attribute)
	}
	return attr.Field, nil
}

// Timestamps reports whether the model manages createdAt and updatedAt. It is
// true unless [Timestamps](false) was passed, and always true when the model is
// paranoid.
func (m *Model) Timestamps() bool {
	if m.opts.Timestamps != nil {
		return *m.opts.Timestamps || m.opts.Paranoid
	}
	return true
}

// IsParanoid reports whether the model uses soft deletes.
func (m *Model) IsParanoid() bool { return m.opts.Paranoid }

// CreatedAtName returns the creation timestamp attribute name.
func (m *Model) CreatedAtName() string {
	if m.opts.CreatedAtField != "" {
		return m.opts.CreatedAtField
	}
	return DefaultCreatedAt
}

// UpdatedAtName returns the update timestamp attribute name.
func (m *Model) UpdatedAtName() string {
	if m.opts.UpdatedAtField != "" {
		return m.opts.UpdatedAtField
	}
	return DefaultUpdatedAt
}

// DeletedAtName returns the soft-delete timestamp attribute name.
func (m *Model) DeletedAtName() string {
	if m.opts.DeletedAtField != "" {
		return m.opts.DeletedAtField
	}
	return DefaultDeletedAt
}

// now returns the model's clock reading, truncated to microseconds so that a
// value survives a round trip through backends with microsecond resolution.
func (m *Model) now() time.Time {
	return m.seq.now().UTC().Truncate(time.Microsecond)
}

// applyDefaults fills omitted attributes from Attribute.DefaultValue. It never
// overwrites a key the caller supplied, not even an explicit nil.
func (m *Model) applyDefaults(vals Values) Values {
	out := vals.Clone()
	for name, attr := range m.attrs {
		if attr.DefaultValue == nil {
			continue
		}
		if _, ok := out[name]; !ok {
			out[name] = attr.DefaultValue
		}
	}
	return out
}

// snakeCase converts a camelCase or PascalCase identifier to snake_case,
// keeping runs of capitals together: "createdAt" becomes "created_at" and
// "HTTPServer" becomes "http_server".
func snakeCase(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(runes) + 4)
	for i, r := range runes {
		if r == '_' || r == '-' || r == ' ' {
			b.WriteByte('_')
			continue
		}
		if unicode.IsUpper(r) && i > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// pluralize applies the port's table-naming rule to an already snake_cased
// word. It is intentionally a small English heuristic, not a linguistics
// library: -s/-x/-z/-ch/-sh take "es", a consonant followed by -y becomes
// "-ies", -f/-fe become "-ves", and everything else takes "s". Already-plural
// words and irregulars are not detected; use [TableName] for those.
func pluralize(word string) string {
	if word == "" {
		return word
	}
	lower := strings.ToLower(word)
	switch {
	case strings.HasSuffix(lower, "s"), strings.HasSuffix(lower, "x"),
		strings.HasSuffix(lower, "z"), strings.HasSuffix(lower, "ch"),
		strings.HasSuffix(lower, "sh"):
		return word + "es"
	case strings.HasSuffix(lower, "y") && len(lower) > 1 && !isVowel(lower[len(lower)-2]):
		return word[:len(word)-1] + "ies"
	case strings.HasSuffix(lower, "fe"):
		return word[:len(word)-2] + "ves"
	case strings.HasSuffix(lower, "f"):
		return word[:len(word)-1] + "ves"
	default:
		return word + "s"
	}
}

func isVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}
