package sequelize

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FindAll returns every row matching q, as [Values] keyed by attribute name.
// An empty result is an empty slice and a nil error, never an error.
//
// A paranoid model's soft-deleted rows are excluded unless q.Paranoid is set to
// false.
//
// When q.Include names associations, the rows carry their eager-loaded relations
// nested under the association names; see [Include] and [Model.HasMany].
func (m *Model) FindAll(ctx context.Context, q Query) ([]Values, error) {
	// Scoping happens here rather than in the builder because a default scope may
	// itself contribute includes, and the eager-loading path must see them.
	if q = m.scoped(q); len(q.Include) > 0 {
		return m.findAllIncluded(ctx, q)
	}
	query, args, cols, err := m.buildSelect(q)
	if err != nil {
		return nil, err
	}
	querier, err := m.querierFor(q)
	if err != nil {
		return nil, err
	}
	m.seq.log(ctx, query, args)
	rows, err := querier.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, m.seq.dialect.TranslateError(err)
	}
	defer rows.Close()
	return m.scanRows(rows, cols)
}

// FindOne returns the first row matching q.
//
// Unlike upstream, which returns null, a miss is a *[RecordNotFoundError]
// wrapping [ErrRecordNotFound], so that a missing row cannot be mistaken for an
// empty record. Test for it with errors.Is(err, sequelize.ErrRecordNotFound).
func (m *Model) FindOne(ctx context.Context, q Query) (Values, error) {
	q.Limit = Int(1)
	rows, err := m.FindAll(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, &RecordNotFoundError{Model: m.name}
	}
	return rows[0], nil
}

// FindByPk returns the row with the given primary key.
//
// For a single-column primary key, pass the key value. For a composite primary
// key, pass a [Values] holding every key attribute. At most one [Query] may be
// supplied, to carry Attrs, Tx or Paranoid; its Where is ANDed with the key
// filter.
func (m *Model) FindByPk(ctx context.Context, pk any, opts ...Query) (Values, error) {
	q, err := firstQuery(opts)
	if err != nil {
		return nil, err
	}
	keyClause, err := m.primaryKeyClause(pk)
	if err != nil {
		return nil, err
	}
	q.Where = And(q.Where, keyClause)
	return m.FindOne(ctx, q)
}

// primaryKeyClause turns a primary key value into a filter.
func (m *Model) primaryKeyClause(pk any) (*Clause, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}
	if len(m.pk) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoPrimaryKey, m.name)
	}
	if vals, ok := pk.(Values); ok {
		clauses := make([]*Clause, 0, len(m.pk))
		for _, name := range m.pk {
			v, present := vals[name]
			if !present {
				return nil, fmt.Errorf("%w: primary key %s.%s missing", ErrNoValues, m.name, name)
			}
			clauses = append(clauses, Op.Eq(name, v))
		}
		return And(clauses...), nil
	}
	if len(m.pk) != 1 {
		return nil, fmt.Errorf("%w: %s has a composite primary key; pass a Values", ErrInvalidQuery, m.name)
	}
	return Op.Eq(m.pk[0], pk), nil
}

// FindAndCountAll returns the rows matching q together with the total number of
// rows that match ignoring Limit and Offset, which is what a paginated listing
// needs.
func (m *Model) FindAndCountAll(ctx context.Context, q Query) ([]Values, int64, error) {
	rows, err := m.FindAll(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	countQuery := q
	countQuery.Limit = nil
	countQuery.Offset = nil
	countQuery.Order = nil
	total, err := m.Count(ctx, countQuery)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// FindOrCreate returns the first row matching q, creating it from q's equality
// filters merged with defaults when nothing matches. The bool reports whether a
// row was created.
//
// The whole operation is atomic: when q carries no transaction, one is opened for
// the duration, so two concurrent callers cannot both create. Values in defaults
// win over values derived from the filter.
func (m *Model) FindOrCreate(ctx context.Context, q Query, defaults Values) (Values, bool, error) {
	if err := m.Err(); err != nil {
		return nil, false, err
	}
	if q.Tx != nil {
		return m.findOrCreate(ctx, q, defaults)
	}
	var (
		row     Values
		created bool
	)
	err := m.seq.Transaction(ctx, func(tx *Tx) error {
		inner := q
		inner.Tx = tx
		var err error
		row, created, err = m.findOrCreate(ctx, inner, defaults)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return row, created, nil
}

func (m *Model) findOrCreate(ctx context.Context, q Query, defaults Values) (Values, bool, error) {
	found, err := m.FindOne(ctx, q)
	if err == nil {
		return found, false, nil
	}
	if !errors.Is(err, ErrRecordNotFound) {
		return nil, false, err
	}
	vals := Values{}
	for k, v := range equalityValues(q.Where) {
		if _, ok := m.attrs[k]; ok {
			vals[k] = v
		}
	}
	for k, v := range defaults {
		vals[k] = v
	}
	created, err := m.Create(ctx, vals, Query{Tx: q.Tx})
	if err != nil {
		return nil, false, err
	}
	return created, true, nil
}

// equalityValues collects the attribute/value pairs a clause tree asserts
// unconditionally, which is what FindOrCreate seeds a new row from. Only
// top-level ANDed equality comparisons qualify; anything under an OR or a NOT is
// not implied by a match and is skipped.
func equalityValues(c *Clause) Values {
	out := Values{}
	collectEqualities(c, out)
	return out
}

func collectEqualities(c *Clause, into Values) {
	if c == nil {
		return
	}
	switch c.kind {
	case clauseCompare:
		if c.op == OpEq {
			into[c.attribute] = c.value
		}
	case clauseLogical:
		if c.logic != "AND" {
			return
		}
		for _, child := range c.children {
			collectEqualities(child, into)
		}
	}
}

// Count returns the number of rows matching q.
//
// With q.Distinct the count is over distinct primary keys, so that a query which
// would otherwise multiply rows counts parents rather than joined rows. With
// q.Group it counts groups.
func (m *Model) Count(ctx context.Context, q Query) (int64, error) {
	attribute := ""
	if q.Distinct {
		if err := m.Err(); err != nil {
			return 0, err
		}
		if len(m.pk) != 1 {
			return 0, fmt.Errorf("%w: distinct Count needs a single-column primary key on %s", ErrNoPrimaryKey, m.name)
		}
		attribute = m.pk[0]
	}
	countQuery := q
	countQuery.Order = nil
	raw, err := m.aggregate(ctx, "COUNT", attribute, countQuery)
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return 0, nil
	}
	n, err := decodeInt(BIGINT(), raw)
	if err != nil {
		return 0, err
	}
	return n.(int64), nil
}

// Sum returns the sum of attribute over the rows matching q, or nil when no rows
// match. The result carries the attribute's own type: an integer column sums to
// an int64, a float column to a float64.
func (m *Model) Sum(ctx context.Context, attribute string, q Query) (any, error) {
	return m.typedAggregate(ctx, "SUM", attribute, q)
}

// Min returns the smallest value of attribute over the rows matching q, or nil
// when no rows match.
func (m *Model) Min(ctx context.Context, attribute string, q Query) (any, error) {
	return m.typedAggregate(ctx, "MIN", attribute, q)
}

// Max returns the largest value of attribute over the rows matching q, or nil
// when no rows match.
func (m *Model) Max(ctx context.Context, attribute string, q Query) (any, error) {
	return m.typedAggregate(ctx, "MAX", attribute, q)
}

func (m *Model) typedAggregate(ctx context.Context, fn, attribute string, q Query) (any, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}
	attr, ok := m.attrs[attribute]
	if !ok {
		return nil, fmt.Errorf("%w: %s.%s", ErrUnknownAttribute, m.name, attribute)
	}
	q.Order = nil
	raw, err := m.aggregate(ctx, fn, attribute, q)
	if err != nil || raw == nil {
		return nil, err
	}
	return decodeValue(attr.Type, raw)
}

// aggregate runs a single-value aggregate and returns the driver's raw value.
func (m *Model) aggregate(ctx context.Context, fn, attribute string, q Query) (any, error) {
	query, args, err := m.buildAggregate(fn, attribute, q)
	if err != nil {
		return nil, err
	}
	querier, err := m.querierFor(q)
	if err != nil {
		return nil, err
	}
	m.seq.log(ctx, query, args)
	rows, err := querier.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, m.seq.dialect.TranslateError(err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var raw any
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	return raw, rows.Err()
}

// scanRows decodes a result set into Values, one per row, decoding each column
// according to its attribute's declared type. NULL becomes a nil entry, which is
// present in the map so a caller can distinguish NULL from "not selected".
func (m *Model) scanRows(rows *sql.Rows, attrs []string) ([]Values, error) {
	types := make([]DataType, len(attrs))
	for i, name := range attrs {
		attr, ok := m.attrs[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s.%s", ErrUnknownAttribute, m.name, name)
		}
		types[i] = attr.Type
	}
	out := []Values{}
	for rows.Next() {
		raw := make([]any, len(attrs))
		dest := make([]any, len(attrs))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(Values, len(attrs))
		for i, name := range attrs {
			decoded, err := decodeValue(types[i], raw[i])
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", m.name, name, err)
			}
			row[name] = decoded
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
