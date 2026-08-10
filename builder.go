package sequelize

import (
	"fmt"
	"math"
	"strings"
)

// OrderTerm is one entry of an ORDER BY list. Build terms with [Asc] and [Desc].
type OrderTerm struct {
	// Attribute is the model attribute to order by.
	Attribute string
	// Descending selects DESC instead of ASC.
	Descending bool
}

// Asc orders by attribute ascending.
func Asc(attribute string) OrderTerm { return OrderTerm{Attribute: attribute} }

// Desc orders by attribute descending.
func Desc(attribute string) OrderTerm { return OrderTerm{Attribute: attribute, Descending: true} }

// Include requests eager loading of an association declared with
// [Model.HasOne], [Model.HasMany], [Model.BelongsTo] or [Model.BelongsToMany].
//
// The loaded rows are placed on each parent row under the association's name (or
// [Include.As]): a Values for a to-one association, nil when it matched nothing,
// and a []Values for a to-many one. Those keys are not model attributes, so a row
// that has been eager-loaded cannot be handed back to a write unchanged.
type Include struct {
	// Association is the name of the association to load.
	Association string
	// As overrides the key the loaded rows are placed under.
	As string
	// Where filters the associated rows. For a joined association it becomes an
	// extra ON condition, so it narrows the association without dropping parents.
	Where *Clause
	// Attrs restricts which of the associated model's attributes are selected. The
	// target's primary key, and any key needed to stitch the rows together, are
	// selected regardless.
	Attrs []string
	// Required makes a joined association an INNER JOIN, dropping parents with no
	// match. It has no effect on the batched to-many kinds, which cannot express it
	// without a subquery.
	Required bool
	// Include nests further eager loading, to any depth.
	Include []Include
}

// Query describes one read or write. Every finder and mutation is built from it,
// and the zero value is a valid "everything, unfiltered" query.
//
// Attribute names, not column names, are used throughout; unknown names are an
// error wrapping [ErrUnknownAttribute] rather than a silently ignored filter.
type Query struct {
	// Where filters rows. Nil means no filter.
	Where *Clause
	// Order lists ORDER BY terms, in order.
	Order []OrderTerm
	// Limit caps the number of rows. Nil means no cap; use [Int] to set it.
	Limit *int
	// Offset skips rows. Nil means no skip; use [Int] to set it.
	Offset *int
	// Attrs restricts the selected attributes. Empty selects them all.
	Attrs []string
	// Group lists GROUP BY attributes.
	Group []string
	// Having filters groups.
	Having *Clause
	// Include requests eager loading. See [Include].
	Include []Include
	// Distinct adds DISTINCT to a select, and makes Count count distinct primary
	// keys.
	Distinct bool
	// Paranoid overrides the model's soft-delete filter for this query. Nil keeps
	// the model's behaviour; false includes soft-deleted rows.
	Paranoid *bool
	// Tx runs the statement inside a transaction.
	Tx *Tx

	// scoped records that the model's scopes have already been merged in, so that
	// a query passing through more than one builder is scoped exactly once. It is
	// unexported, so a caller-built Query always starts out unscoped. See
	// scopes.go.
	scoped bool
}

// Int returns a pointer to n, for setting [Query.Limit] and [Query.Offset]
// inline.
func Int(n int) *int { return &n }

// firstQuery collapses a variadic Query argument, so that operations which need
// at most one can be called with none.
func firstQuery(qs []Query) (Query, error) {
	switch len(qs) {
	case 0:
		return Query{}, nil
	case 1:
		return qs[0], nil
	default:
		return Query{}, fmt.Errorf("%w: at most one Query may be supplied, got %d", ErrInvalidQuery, len(qs))
	}
}

// unlimitedLimit is bound as the LIMIT when a query sets only an Offset. SQL has
// no portable "skip n, take everything", so an effectively infinite limit stands
// in for it.
const unlimitedLimit = math.MaxInt64

// builder accumulates SQL text and its bind arguments for one statement. Values
// only ever enter through bind, so no caller value can reach the SQL text.
type builder struct {
	m *Model
	d Dialect
	// alias qualifies every column this builder writes with a table alias. It is
	// empty for a single-table statement and set by the eager-loading builders in
	// associations.go, where an unqualified column could be ambiguous.
	alias string
	sb    strings.Builder
	args  []any
}

func newBuilder(m *Model) *builder {
	return &builder{m: m, d: m.seq.dialect}
}

func (b *builder) write(parts ...string) {
	for _, p := range parts {
		b.sb.WriteString(p)
	}
}

// bind appends value as a bind argument, encoded for attribute's type when the
// attribute is known, and writes its placeholder.
func (b *builder) bind(attribute string, value any) error {
	t := DataType{}
	if attribute != "" {
		attr, ok := b.m.attrs[attribute]
		if !ok {
			return fmt.Errorf("%w: %s.%s", ErrUnknownAttribute, b.m.name, attribute)
		}
		t = attr.Type
	}
	encoded, err := encodeValue(t, value)
	if err != nil {
		return err
	}
	bound, err := b.d.BindValue(t, encoded)
	if err != nil {
		return err
	}
	b.args = append(b.args, bound)
	b.sb.WriteString(b.d.Placeholder(len(b.args)))
	return nil
}

// bindRaw appends a value with no attribute type to guide encoding, for literal
// clauses and LIMIT/OFFSET.
func (b *builder) bindRaw(value any) error {
	bound, err := b.d.BindValue(DataType{}, value)
	if err != nil {
		return err
	}
	b.args = append(b.args, bound)
	b.sb.WriteString(b.d.Placeholder(len(b.args)))
	return nil
}

// column writes the quoted physical column for an attribute, qualified by the
// builder's table alias when it has one.
func (b *builder) column(attribute string) error {
	col, err := b.m.Column(attribute)
	if err != nil {
		return err
	}
	quoted, err := b.d.Quote(col)
	if err != nil {
		return err
	}
	if b.alias != "" {
		quotedAlias, err := b.d.Quote(b.alias)
		if err != nil {
			return err
		}
		b.sb.WriteString(quotedAlias)
		b.sb.WriteString(".")
	}
	b.sb.WriteString(quoted)
	return nil
}

func (b *builder) table() error {
	quoted, err := b.d.Quote(b.m.table)
	if err != nil {
		return err
	}
	b.sb.WriteString(quoted)
	return nil
}

// clause renders a clause tree. Constants render as "1 = 1" / "1 = 0" so that an
// empty IN set produces valid SQL that matches nothing.
func (b *builder) clause(c *Clause) error {
	if c == nil {
		b.write("1 = 1")
		return nil
	}
	switch c.kind {
	case clauseConstant:
		if c.constant {
			b.write("1 = 1")
		} else {
			b.write("1 = 0")
		}
		return nil
	case clauseCompare:
		if err := b.column(c.attribute); err != nil {
			return err
		}
		b.write(" ", string(c.op), " ")
		return b.bind(c.attribute, c.value)
	case clauseIn, clauseNotIn:
		if err := b.column(c.attribute); err != nil {
			return err
		}
		if c.kind == clauseIn {
			b.write(" IN (")
		} else {
			b.write(" NOT IN (")
		}
		for i, v := range c.values {
			if i > 0 {
				b.write(", ")
			}
			if err := b.bind(c.attribute, v); err != nil {
				return err
			}
		}
		b.write(")")
		return nil
	case clauseNull, clauseNotNull:
		if err := b.column(c.attribute); err != nil {
			return err
		}
		if c.kind == clauseNull {
			b.write(" IS NULL")
		} else {
			b.write(" IS NOT NULL")
		}
		return nil
	case clauseBetween, clauseNotBetween:
		if err := b.column(c.attribute); err != nil {
			return err
		}
		if c.kind == clauseBetween {
			b.write(" BETWEEN ")
		} else {
			b.write(" NOT BETWEEN ")
		}
		if err := b.bind(c.attribute, c.values[0]); err != nil {
			return err
		}
		b.write(" AND ")
		return b.bind(c.attribute, c.values[1])
	case clauseLogical:
		b.write("(")
		for i, child := range c.children {
			if i > 0 {
				b.write(" ", c.logic, " ")
			}
			if err := b.clause(child); err != nil {
				return err
			}
		}
		b.write(")")
		return nil
	case clauseNot:
		b.write("NOT (")
		if err := b.clause(c.children[0]); err != nil {
			return err
		}
		b.write(")")
		return nil
	case clauseRaw:
		b.write("(", c.sql, ")")
		for _, arg := range c.args {
			bound, err := b.d.BindValue(DataType{}, arg)
			if err != nil {
				return err
			}
			b.args = append(b.args, bound)
		}
		return nil
	default:
		return fmt.Errorf("%w: unrecognised clause", ErrInvalidQuery)
	}
}

// effectiveWhere combines a query's own filter with the model's soft-delete
// filter. Paranoid models hide stamped rows unless the query sets Paranoid to
// false.
func (m *Model) effectiveWhere(q Query) *Clause {
	if !m.IsParanoid() {
		return q.Where
	}
	if q.Paranoid != nil && !*q.Paranoid {
		return q.Where
	}
	notDeleted := Op.IsNull(m.DeletedAtName())
	if q.Where == nil {
		return notDeleted
	}
	return And(q.Where, notDeleted)
}

// selectColumns resolves the attribute list a select should project.
func (m *Model) selectColumns(attrs []string) ([]string, error) {
	if len(attrs) == 0 {
		return m.AttributeNames(), nil
	}
	for _, a := range attrs {
		if _, ok := m.attrs[a]; !ok {
			return nil, fmt.Errorf("%w: %s.%s", ErrUnknownAttribute, m.name, a)
		}
	}
	return append([]string(nil), attrs...), nil
}

// writeWhere appends a WHERE clause when there is anything to filter on.
func (b *builder) writeWhere(c *Clause) error {
	if c == nil {
		return nil
	}
	b.write(" WHERE ")
	return b.clause(c)
}

// writeTail appends GROUP BY, HAVING, ORDER BY, LIMIT and OFFSET.
func (b *builder) writeTail(q Query, withOrder bool) error {
	if len(q.Group) > 0 {
		b.write(" GROUP BY ")
		for i, attr := range q.Group {
			if i > 0 {
				b.write(", ")
			}
			if err := b.column(attr); err != nil {
				return err
			}
		}
	}
	if q.Having != nil {
		b.write(" HAVING ")
		if err := b.clause(q.Having); err != nil {
			return err
		}
	}
	if withOrder && len(q.Order) > 0 {
		b.write(" ORDER BY ")
		for i, term := range q.Order {
			if i > 0 {
				b.write(", ")
			}
			if err := b.column(term.Attribute); err != nil {
				return err
			}
			if term.Descending {
				b.write(" DESC")
			} else {
				b.write(" ASC")
			}
		}
	}
	return b.writeLimit(q)
}

func (b *builder) writeLimit(q Query) error {
	if q.Limit == nil && q.Offset == nil {
		return nil
	}
	if q.Limit != nil && *q.Limit < 0 {
		return fmt.Errorf("%w: negative Limit %d", ErrInvalidQuery, *q.Limit)
	}
	if q.Offset != nil && *q.Offset < 0 {
		return fmt.Errorf("%w: negative Offset %d", ErrInvalidQuery, *q.Offset)
	}
	b.write(" LIMIT ")
	limit := any(unlimitedLimit)
	if q.Limit != nil {
		limit = int64(*q.Limit)
	}
	if err := b.bindRaw(limit); err != nil {
		return err
	}
	if q.Offset != nil {
		b.write(" OFFSET ")
		if err := b.bindRaw(int64(*q.Offset)); err != nil {
			return err
		}
	}
	return nil
}

// buildSelect renders a SELECT for q and returns it with its bind arguments and
// the attribute list the projection is in, which is what the row scanner needs.
func (m *Model) buildSelect(q Query) (string, []any, []string, error) {
	if err := m.Err(); err != nil {
		return "", nil, nil, err
	}
	q = m.scoped(q)
	cols, err := m.selectColumns(q.Attrs)
	if err != nil {
		return "", nil, nil, err
	}
	b := newBuilder(m)
	b.write("SELECT ")
	if q.Distinct {
		b.write("DISTINCT ")
	}
	for i, attr := range cols {
		if i > 0 {
			b.write(", ")
		}
		if err := b.column(attr); err != nil {
			return "", nil, nil, err
		}
	}
	b.write(" FROM ")
	if err := b.table(); err != nil {
		return "", nil, nil, err
	}
	if err := b.writeWhere(m.effectiveWhere(q)); err != nil {
		return "", nil, nil, err
	}
	if err := b.writeTail(q, true); err != nil {
		return "", nil, nil, err
	}
	return b.sb.String(), b.args, cols, nil
}

// buildAggregate renders SELECT <fn>(<attribute>) for the aggregate finders. A
// star aggregate (COUNT with no attribute) is spelled with an empty attribute.
func (m *Model) buildAggregate(fn string, attribute string, q Query) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	q = m.scoped(q)
	// An aggregate over an eager-loaded query needs the joins, so that a Distinct
	// count collapses the multiplied rows back to distinct parents.
	if len(q.Include) > 0 {
		return m.buildIncludedAggregate(fn, attribute, q)
	}
	if err := ValidateIdentifier(fn); err != nil {
		return "", nil, err
	}
	b := newBuilder(m)
	// A GROUP BY changes what one row means, so the aggregate is taken over the
	// grouped rows via a subquery: COUNT(*) of the groups, not of the rows.
	grouped := len(q.Group) > 0
	if grouped {
		b.write("SELECT ", fn, "(*) FROM (SELECT 1 AS ")
		one, err := b.d.Quote("one")
		if err != nil {
			return "", nil, err
		}
		b.write(one, " FROM ")
	} else {
		b.write("SELECT ", fn, "(")
		if q.Distinct {
			b.write("DISTINCT ")
		}
		if attribute == "" {
			b.write("*")
		} else if err := b.column(attribute); err != nil {
			return "", nil, err
		}
		b.write(") FROM ")
	}
	if err := b.table(); err != nil {
		return "", nil, err
	}
	if err := b.writeWhere(m.effectiveWhere(q)); err != nil {
		return "", nil, err
	}
	if grouped {
		if err := b.writeTail(Query{Group: q.Group, Having: q.Having}, false); err != nil {
			return "", nil, err
		}
		b.write(") AS ")
		alias, err := b.d.Quote("grouped")
		if err != nil {
			return "", nil, err
		}
		b.write(alias)
		return b.sb.String(), b.args, nil
	}
	if q.Having != nil {
		return "", nil, fmt.Errorf("%w: Having requires Group", ErrInvalidQuery)
	}
	if err := b.writeLimit(q); err != nil {
		return "", nil, err
	}
	return b.sb.String(), b.args, nil
}

// buildInsert renders a multi-row INSERT over the given attributes.
func (m *Model) buildInsert(attrs []string, rows []Values) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	if len(attrs) == 0 || len(rows) == 0 {
		return "", nil, ErrNoValues
	}
	b := newBuilder(m)
	b.write("INSERT INTO ")
	if err := b.table(); err != nil {
		return "", nil, err
	}
	b.write(" (")
	for i, attr := range attrs {
		if i > 0 {
			b.write(", ")
		}
		if err := b.column(attr); err != nil {
			return "", nil, err
		}
	}
	b.write(") VALUES ")
	for r, row := range rows {
		if r > 0 {
			b.write(", ")
		}
		b.write("(")
		for i, attr := range attrs {
			if i > 0 {
				b.write(", ")
			}
			if err := b.bind(attr, row[attr]); err != nil {
				return "", nil, err
			}
		}
		b.write(")")
	}
	return b.sb.String(), b.args, nil
}

// buildUpdate renders an UPDATE. set is visited in sorted attribute order for
// deterministic SQL.
func (m *Model) buildUpdate(set Values, q Query) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	q = m.scoped(q)
	if len(set) == 0 {
		return "", nil, ErrNoValues
	}
	b := newBuilder(m)
	b.write("UPDATE ")
	if err := b.table(); err != nil {
		return "", nil, err
	}
	b.write(" SET ")
	for i, attr := range set.Keys() {
		if i > 0 {
			b.write(", ")
		}
		if err := b.column(attr); err != nil {
			return "", nil, err
		}
		b.write(" = ")
		if err := b.bind(attr, set[attr]); err != nil {
			return "", nil, err
		}
	}
	if err := b.writeWhere(m.effectiveWhere(q)); err != nil {
		return "", nil, err
	}
	return b.sb.String(), b.args, nil
}

// buildIncrement renders an UPDATE that adjusts columns relative to their
// current value: SET col = col + ?.
func (m *Model) buildIncrement(deltas Values, extra Values, q Query) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	q = m.scoped(q)
	if len(deltas) == 0 {
		return "", nil, ErrNoValues
	}
	b := newBuilder(m)
	b.write("UPDATE ")
	if err := b.table(); err != nil {
		return "", nil, err
	}
	b.write(" SET ")
	for i, attr := range deltas.Keys() {
		if i > 0 {
			b.write(", ")
		}
		if err := b.column(attr); err != nil {
			return "", nil, err
		}
		b.write(" = ")
		if err := b.column(attr); err != nil {
			return "", nil, err
		}
		b.write(" + ")
		if err := b.bind(attr, deltas[attr]); err != nil {
			return "", nil, err
		}
	}
	for _, attr := range extra.Keys() {
		b.write(", ")
		if err := b.column(attr); err != nil {
			return "", nil, err
		}
		b.write(" = ")
		if err := b.bind(attr, extra[attr]); err != nil {
			return "", nil, err
		}
	}
	if err := b.writeWhere(m.effectiveWhere(q)); err != nil {
		return "", nil, err
	}
	return b.sb.String(), b.args, nil
}

// buildDelete renders a hard DELETE, ignoring the soft-delete filter's inverse:
// a paranoid model still only deletes rows the query can see.
func (m *Model) buildDelete(q Query) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	q = m.scoped(q)
	b := newBuilder(m)
	b.write("DELETE FROM ")
	if err := b.table(); err != nil {
		return "", nil, err
	}
	if err := b.writeWhere(m.effectiveWhere(q)); err != nil {
		return "", nil, err
	}
	return b.sb.String(), b.args, nil
}
