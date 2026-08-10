package sequelize

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Create inserts one row and returns it as written, including applied defaults,
// timestamps, and the generated primary key when the database generated one.
//
// Attribute defaults are applied for keys the caller omitted; createdAt and
// updatedAt are stamped unless the model was defined with Timestamps(false). At
// most one [Query] may be supplied, and only its Tx is used.
//
// The lifecycle runs in upstream's order: beforeValidate, the attribute
// validators, afterValidate, beforeCreate, the INSERT, afterCreate. A failed
// validator returns a *[ValidationError] naming every attribute that failed, and
// a hook returning an error aborts the write and rolls back its transaction.
func (m *Model) Create(ctx context.Context, values Values, opts ...Query) (Values, error) {
	q, err := firstQuery(opts)
	if err != nil {
		return nil, err
	}
	if err := m.Err(); err != nil {
		return nil, err
	}
	row, err := m.prepareInsert(values, m.now())
	if err != nil {
		return nil, err
	}
	if err := m.beforeWrite(ctx, q, BeforeCreate, row, false); err != nil {
		return nil, err
	}
	attrs, err := m.insertAttributes(row)
	if err != nil {
		return nil, err
	}
	query, args, err := m.buildInsert(attrs, []Values{row})
	if err != nil {
		return nil, err
	}
	res, err := m.exec(ctx, q, query, args)
	if err != nil {
		return nil, err
	}
	m.adoptGeneratedKey(row, res)
	if err := m.afterWrite(ctx, q, AfterCreate, row, 1); err != nil {
		return nil, err
	}
	return row, nil
}

// BulkCreate inserts many rows and returns them as written.
//
// When the model has a single auto-increment primary key and any row omits it,
// the rows are inserted one statement each inside a transaction so that every
// returned row carries its generated key. Otherwise they go in a single
// multi-row INSERT. Either way the insert is all-or-nothing.
//
// Because one statement covers every row, the column list is the union of the
// rows' attributes: a row that omits an attribute another row sets is written as
// NULL for it rather than falling back to a database-side default.
//
// beforeBulkCreate fires once with every prepared row, then every row is
// validated — a failure reports the failed attributes of *all* the rows in one
// *[ValidationError] — and afterBulkCreate fires once on success. Per-row create
// hooks deliberately do not fire, as upstream's bulkCreate does not run them.
func (m *Model) BulkCreate(ctx context.Context, rows []Values, opts ...Query) ([]Values, error) {
	q, err := firstQuery(opts)
	if err != nil {
		return nil, err
	}
	if err := m.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNoValues
	}
	now := m.now()
	prepared := make([]Values, 0, len(rows))
	for _, raw := range rows {
		row, err := m.prepareInsert(raw, now)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, row)
	}
	// Hooks and validation run before the rows are inspected for their shape,
	// because a hook may still fill in the key that decides how they are written.
	if err := m.beforeBulkWrite(ctx, q, prepared); err != nil {
		return nil, err
	}
	needsPerRow := false
	for _, row := range prepared {
		if m.generatesKey(row) {
			needsPerRow = true
		}
	}

	if needsPerRow {
		if err := m.bulkCreateIndividually(ctx, q, prepared); err != nil {
			return nil, err
		}
		if err := m.afterBulkWrite(ctx, q, prepared); err != nil {
			return nil, err
		}
		return prepared, nil
	}

	union := map[string]bool{}
	for _, row := range prepared {
		for k := range row {
			union[k] = true
		}
	}
	attrs := make([]string, 0, len(union))
	for k := range union {
		attrs = append(attrs, k)
	}
	sort.Strings(attrs)
	if err := m.checkAttributes(attrs); err != nil {
		return nil, err
	}
	query, args, err := m.buildInsert(attrs, prepared)
	if err != nil {
		return nil, err
	}
	if _, err := m.exec(ctx, q, query, args); err != nil {
		return nil, err
	}
	if err := m.afterBulkWrite(ctx, q, prepared); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (m *Model) bulkCreateIndividually(ctx context.Context, q Query, prepared []Values) error {
	insert := func(tx *Tx) error {
		for _, row := range prepared {
			attrs, err := m.insertAttributes(row)
			if err != nil {
				return err
			}
			query, args, err := m.buildInsert(attrs, []Values{row})
			if err != nil {
				return err
			}
			res, err := m.exec(ctx, Query{Tx: tx}, query, args)
			if err != nil {
				return err
			}
			m.adoptGeneratedKey(row, res)
		}
		return nil
	}
	if q.Tx != nil {
		return insert(q.Tx)
	}
	return m.seq.Transaction(ctx, insert)
}

// prepareInsert applies attribute defaults and the creation timestamps, and
// rejects attributes the model does not declare.
func (m *Model) prepareInsert(values Values, now time.Time) (Values, error) {
	if values == nil {
		return nil, ErrNoValues
	}
	row := m.applyDefaults(values)
	if m.Timestamps() {
		if _, ok := row[m.CreatedAtName()]; !ok {
			row[m.CreatedAtName()] = now
		}
		if _, ok := row[m.UpdatedAtName()]; !ok {
			row[m.UpdatedAtName()] = now
		}
	}
	names := row.Keys()
	if err := m.checkAttributes(names); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, ErrNoValues
	}
	return row, nil
}

func (m *Model) checkAttributes(names []string) error {
	for _, name := range names {
		if _, ok := m.attrs[name]; !ok {
			return fmt.Errorf("%w: %s.%s", ErrUnknownAttribute, m.name, name)
		}
	}
	return nil
}

// insertAttributes lists the attributes to write for one row, dropping a
// database-generated primary key so the database can assign it.
func (m *Model) insertAttributes(row Values) ([]string, error) {
	attrs := make([]string, 0, len(row))
	for _, name := range row.Keys() {
		if m.isGeneratedKey(name) && row[name] == nil {
			continue
		}
		attrs = append(attrs, name)
	}
	if len(attrs) == 0 {
		return nil, ErrNoValues
	}
	return attrs, m.checkAttributes(attrs)
}

// isGeneratedKey reports whether name is the model's single auto-increment
// primary key.
func (m *Model) isGeneratedKey(name string) bool {
	if len(m.pk) != 1 || m.pk[0] != name {
		return false
	}
	return m.attrs[name].AutoIncrement
}

// generatesKey reports whether the row leaves the generated key to the database.
func (m *Model) generatesKey(row Values) bool {
	if len(m.pk) != 1 {
		return false
	}
	name := m.pk[0]
	if !m.attrs[name].AutoIncrement {
		return false
	}
	v, ok := row[name]
	return !ok || v == nil
}

// adoptGeneratedKey copies a driver-reported LastInsertId onto the row. Drivers
// that do not report one leave the row untouched, which is not an error.
func (m *Model) adoptGeneratedKey(row Values, res sql.Result) {
	if res == nil || !m.generatesKey(row) {
		return
	}
	id, err := res.LastInsertId()
	if err != nil || id == 0 {
		return
	}
	row[m.pk[0]] = id
}

// Update writes values to every row matching q and returns the number of rows
// changed. updatedAt is stamped unless the model was defined with
// Timestamps(false) or the caller set it explicitly; createdAt is never touched.
//
// An empty q.Where updates every row, as upstream does; there is no accidental
// safety net.
//
// The lifecycle is beforeValidate, the validators, afterValidate, beforeUpdate,
// the UPDATE, afterUpdate. Validation covers only the attributes values supplies,
// because an update names a subset of the row.
func (m *Model) Update(ctx context.Context, values Values, q Query) (int64, error) {
	if err := m.Err(); err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, ErrNoValues
	}
	set := values.Clone()
	delete(set, m.CreatedAtName())
	if len(set) == 0 {
		return 0, ErrNoValues
	}
	if m.Timestamps() {
		if _, ok := set[m.UpdatedAtName()]; !ok {
			set[m.UpdatedAtName()] = m.now()
		}
	}
	if err := m.checkAttributes(set.Keys()); err != nil {
		return 0, err
	}
	if err := m.beforeWrite(ctx, q, BeforeUpdate, set, true); err != nil {
		return 0, err
	}
	query, args, err := m.buildUpdate(set, q)
	if err != nil {
		return 0, err
	}
	affected, err := m.execAffected(ctx, q, query, args)
	if err != nil {
		return 0, err
	}
	if err := m.afterWrite(ctx, q, AfterUpdate, set, affected); err != nil {
		return 0, err
	}
	return affected, nil
}

// Destroy removes the rows matching q and returns how many it affected.
//
// On a paranoid model it is a soft delete: deletedAt is stamped and the row stays
// in the table, hidden from every finder. Pass Paranoid set to false on the query
// to force a hard DELETE, which is upstream's `force: true`.
//
// beforeDestroy fires before the statement and afterDestroy after it, carrying
// the number of rows affected. Validation does not run for a destroy, so no
// validate hooks fire around it.
func (m *Model) Destroy(ctx context.Context, q Query) (int64, error) {
	if err := m.Err(); err != nil {
		return 0, err
	}
	if err := m.runHooks(ctx, newHookContext(BeforeDestroy, q, nil, nil)); err != nil {
		return 0, err
	}
	force := q.Paranoid != nil && !*q.Paranoid
	var (
		query string
		args  []any
		err   error
	)
	if m.IsParanoid() && !force {
		query, args, err = m.buildUpdate(Values{m.DeletedAtName(): m.now()}, q)
	} else {
		query, args, err = m.buildDelete(q)
	}
	if err != nil {
		return 0, err
	}
	affected, err := m.execAffected(ctx, q, query, args)
	if err != nil {
		return 0, err
	}
	if err := m.afterWrite(ctx, q, AfterDestroy, nil, affected); err != nil {
		return 0, err
	}
	return affected, nil
}

// Restore clears deletedAt on the soft-deleted rows matching q and returns how
// many it restored. It is only meaningful on a paranoid model; on any other it
// returns an error wrapping [ErrInvalidQuery].
//
// The soft-delete filter is deliberately inverted here: Restore looks only at
// rows a finder would hide.
func (m *Model) Restore(ctx context.Context, q Query) (int64, error) {
	if err := m.Err(); err != nil {
		return 0, err
	}
	if !m.IsParanoid() {
		return 0, fmt.Errorf("%w: %s is not paranoid, nothing to restore", ErrInvalidQuery, m.name)
	}
	restoreQuery := q
	restoreQuery.Paranoid = new(bool) // false: do not hide the rows we are here for
	restoreQuery.Where = And(q.Where, Op.NotNull(m.DeletedAtName()))
	set := Values{m.DeletedAtName(): nil}
	query, args, err := m.buildUpdate(set, restoreQuery)
	if err != nil {
		return 0, err
	}
	return m.execAffected(ctx, restoreQuery, query, args)
}

// Upsert inserts values, or updates the existing row when one already matches on
// the primary key or on a unique attribute. The bool reports whether a row was
// inserted.
//
// The conflict target is the primary key when values supplies all of it, and
// otherwise the unique attributes that values supplies. With neither, there is
// nothing to conflict on and Upsert returns an error wrapping [ErrInvalidQuery].
//
// The lookup and the write share a transaction — the caller's when q carries one,
// a fresh one otherwise — so the decision cannot be raced.
func (m *Model) Upsert(ctx context.Context, values Values, opts ...Query) (Values, bool, error) {
	q, err := firstQuery(opts)
	if err != nil {
		return nil, false, err
	}
	if err := m.Err(); err != nil {
		return nil, false, err
	}
	if len(values) == 0 {
		return nil, false, ErrNoValues
	}
	if q.Tx != nil {
		return m.upsert(ctx, q.Tx, values)
	}
	var (
		row      Values
		inserted bool
	)
	err = m.seq.Transaction(ctx, func(tx *Tx) error {
		var innerErr error
		row, inserted, innerErr = m.upsert(ctx, tx, values)
		return innerErr
	})
	if err != nil {
		return nil, false, err
	}
	return row, inserted, nil
}

func (m *Model) upsert(ctx context.Context, tx *Tx, values Values) (Values, bool, error) {
	if err := m.checkAttributes(values.Keys()); err != nil {
		return nil, false, err
	}
	conflict, err := m.conflictClause(values)
	if err != nil {
		return nil, false, err
	}
	existing, err := m.FindOne(ctx, Query{Where: conflict, Tx: tx, Paranoid: new(bool)})
	switch {
	case err == nil:
		if _, err := m.Update(ctx, values, Query{Where: conflict, Tx: tx, Paranoid: new(bool)}); err != nil {
			return nil, false, err
		}
		merged := existing.Clone()
		for k, v := range values {
			merged[k] = v
		}
		if m.Timestamps() {
			merged[m.UpdatedAtName()] = m.now()
		}
		return merged, false, nil
	case errors.Is(err, ErrRecordNotFound):
		created, err := m.Create(ctx, values, Query{Tx: tx})
		if err != nil {
			return nil, false, err
		}
		return created, true, nil
	default:
		return nil, false, err
	}
}

// conflictClause derives the uniqueness filter Upsert matches on.
func (m *Model) conflictClause(values Values) (*Clause, error) {
	if len(m.pk) > 0 {
		complete := true
		clauses := make([]*Clause, 0, len(m.pk))
		for _, name := range m.pk {
			v, ok := values[name]
			if !ok || v == nil {
				complete = false
				break
			}
			clauses = append(clauses, Op.Eq(name, v))
		}
		if complete {
			return And(clauses...), nil
		}
	}
	var clauses []*Clause
	for _, name := range values.Keys() {
		if attr, ok := m.attrs[name]; ok && attr.Unique && values[name] != nil {
			clauses = append(clauses, Op.Eq(name, values[name]))
		}
	}
	if len(clauses) == 0 {
		return nil, fmt.Errorf("%w: Upsert on %s needs the primary key or a unique attribute in values", ErrInvalidQuery, m.name)
	}
	return Or(clauses...), nil
}

// Increment adds each delta in deltas to the current value of its attribute for
// every row matching q, and returns how many rows changed. Use a negative delta
// or [Model.Decrement] to go the other way.
//
// The adjustment happens in the database — SET col = col + ? — so it is safe
// under concurrency. updatedAt is stamped as for [Model.Update].
func (m *Model) Increment(ctx context.Context, deltas Values, q Query) (int64, error) {
	if err := m.Err(); err != nil {
		return 0, err
	}
	if len(deltas) == 0 {
		return 0, ErrNoValues
	}
	if err := m.checkAttributes(deltas.Keys()); err != nil {
		return 0, err
	}
	extra := Values{}
	if m.Timestamps() {
		extra[m.UpdatedAtName()] = m.now()
	}
	query, args, err := m.buildIncrement(deltas, extra, q)
	if err != nil {
		return 0, err
	}
	return m.execAffected(ctx, q, query, args)
}

// Decrement subtracts each delta in deltas from its attribute. It is
// [Model.Increment] with the signs flipped, and accepts the same numeric types.
func (m *Model) Decrement(ctx context.Context, deltas Values, q Query) (int64, error) {
	negated := make(Values, len(deltas))
	for k, v := range deltas {
		n, err := negate(v)
		if err != nil {
			return 0, fmt.Errorf("%s.%s: %w", m.name, k, err)
		}
		negated[k] = n
	}
	return m.Increment(ctx, negated, q)
}

func negate(v any) (any, error) {
	switch n := v.(type) {
	case int:
		return -n, nil
	case int8:
		return -n, nil
	case int16:
		return -n, nil
	case int32:
		return -n, nil
	case int64:
		return -n, nil
	case float32:
		return -n, nil
	case float64:
		return -n, nil
	default:
		return nil, fmt.Errorf("%w: cannot decrement by %T", ErrInvalidType, v)
	}
}

// exec runs a statement, translating driver errors into the package's typed ones.
func (m *Model) exec(ctx context.Context, q Query, query string, args []any) (sql.Result, error) {
	querier, err := m.querierFor(q)
	if err != nil {
		return nil, err
	}
	m.seq.log(ctx, query, args)
	res, err := querier.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, m.annotate(m.translateWriteError(m.seq.dialect.TranslateError(err)))
	}
	return res, nil
}

// execAffected runs a statement and reports how many rows it changed.
func (m *Model) execAffected(ctx context.Context, q Query, query string, args []any) (int64, error) {
	res, err := m.exec(ctx, q, query, args)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// A driver that cannot report the count is not a failed statement.
		return 0, nil
	}
	return n, nil
}

// annotate names the model on the constraint errors the dialect produced.
func (m *Model) annotate(err error) error {
	switch typed := err.(type) {
	case *UniqueConstraintError:
		if typed.Model == "" {
			typed.Model = m.name
		}
	case *ForeignKeyError:
		if typed.Model == "" {
			typed.Model = m.name
		}
	}
	return err
}
