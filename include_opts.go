package sequelize

import (
	"context"
	"fmt"
	"strings"
)

// This file implements the [Include] options that need more than a JOIN:
//
//   - Include.Required for the batched to-many kinds, which becomes a subquery
//     filter on the parent select. That works on the ordinary [Model.FindAll]
//     path, because it only changes the WHERE the parent select is built with.
//   - Include.Order, Include.Limit and Include.Offset, which need the children
//     fetched as a result set of their own with a per-parent window. That needs a
//     loader that knows about them, which is [Model.FindAllEager].
//
// Nothing here ever issues a query per parent row. The per-parent limit is a
// window function where the dialect has one, and a single batched query trimmed in
// Go where it does not; both cost one statement for the whole batch.

// WindowFunctioner is an optional extension to [Dialect]: a dialect implements it
// to declare whether it can render an OVER (PARTITION BY ... ORDER BY ...) window
// clause, which is what a per-parent [Include.Limit] is built from.
//
// It is an optional interface rather than a method on [Dialect] for the reason
// given there: [Dialect] is the seam every third-party backend is written
// against. Prefer [DialectSupportsWindowFunctions] to type-asserting.
type WindowFunctioner interface {
	// SupportsWindowFunctions reports whether the backend has window functions.
	SupportsWindowFunctions() bool
}

// windowFunctionDialects names the dialects assumed to have window functions when
// they do not implement [WindowFunctioner]. The list is deliberately conservative:
// MySQL is absent because 5.7 has no window functions and 8.0 does, and the
// dialect — not this table — is the only thing that can tell them apart. A
// dialect left off the list still gets a correct per-parent limit, from one
// batched query trimmed in Go.
var windowFunctionDialects = map[string]bool{
	"sqlite":     true,
	"postgres":   true,
	"postgresql": true,
}

// DialectSupportsWindowFunctions reports whether d can render a window function.
//
// A dialect implementing [WindowFunctioner] answers for itself; otherwise the
// answer comes from a conservative allow-list of the dialects known to have them.
// A false answer is not a loss of correctness, only of efficiency: the per-parent
// limit is then applied to a single batched result set in Go instead of in SQL.
func DialectSupportsWindowFunctions(d Dialect) bool {
	if d == nil {
		return false
	}
	if w, ok := d.(WindowFunctioner); ok {
		return w.SupportsWindowFunctions()
	}
	return windowFunctionDialects[strings.ToLower(strings.TrimSpace(d.Name()))]
}

// checkIncludes rejects an include tree the statement being built cannot honour,
// so that an ignored request is an error instead of a surprise.
//
// It is called from writeLimit, which every SELECT the package emits passes
// through. Two things are checked: that no include asks for an option this path
// cannot deliver, and that the subquery a Required include expands to can actually
// be rendered — effectiveWhere builds that subquery with nowhere to put an error,
// so this is where the error comes from.
func (m *Model) checkIncludes(q Query) error {
	if len(q.Include) == 0 {
		return nil
	}
	if err := m.checkIncludeOptions(q.Include); err != nil {
		return err
	}
	_, err := m.includeRequiredFilters(q, q.Include)
	return err
}

// checkIncludeOptions rejects the include options this path cannot honour.
//
// [Model.FindAllEager] strips the child options out of the include tree it hands
// to the join builder, because it honours them itself.
func (m *Model) checkIncludeOptions(incs []Include) error {
	for _, inc := range incs {
		assoc, ok := m.Association(inc.Association)
		if !ok {
			return fmt.Errorf("%w: %s declares no association %q",
				ErrInvalidQuery, m.name, inc.Association)
		}
		if err := assoc.Err(); err != nil {
			return err
		}
		if inc.hasChildOptions() {
			if !assoc.kind.Multiple() {
				return fmt.Errorf("%w: Include.Order/Limit/Offset on %s.%s, which is a %s and yields at most one row per parent",
					ErrUnsupported, m.name, inc.Association, assoc.kind)
			}
			return fmt.Errorf("%w: Include.Order/Limit/Offset on %s.%s needs the children in a query of their own; use Model.FindAllEager",
				ErrUnsupported, m.name, inc.Association)
		}
		if err := assoc.target.checkIncludeOptions(inc.Include); err != nil {
			return err
		}
	}
	return nil
}

// validateEagerIncludes checks the include options [Model.FindAllEager] is about
// to honour: that an Order term names an attribute of the association's target,
// and that a Limit or Offset is not negative.
func (m *Model) validateEagerIncludes(incs []Include) error {
	for _, inc := range incs {
		assoc, ok := m.Association(inc.Association)
		if !ok {
			return fmt.Errorf("%w: %s declares no association %q",
				ErrInvalidQuery, m.name, inc.Association)
		}
		if err := assoc.Err(); err != nil {
			return err
		}
		if inc.hasChildOptions() && !assoc.kind.Multiple() {
			return fmt.Errorf("%w: Include.Order/Limit/Offset on %s.%s, which is a %s and yields at most one row per parent",
				ErrUnsupported, m.name, inc.Association, assoc.kind)
		}
		for _, term := range inc.Order {
			if _, ok := assoc.target.attrs[term.Attribute]; !ok {
				return fmt.Errorf("%w: %s.%s (Include.Order for %s)",
					ErrUnknownAttribute, assoc.target.name, term.Attribute, inc.Association)
			}
		}
		if inc.Limit != nil && *inc.Limit < 0 {
			return fmt.Errorf("%w: negative Include.Limit %d on %s", ErrInvalidQuery, *inc.Limit, inc.Association)
		}
		if inc.Offset != nil && *inc.Offset < 0 {
			return fmt.Errorf("%w: negative Include.Offset %d on %s", ErrInvalidQuery, *inc.Offset, inc.Association)
		}
		if err := assoc.target.validateEagerIncludes(inc.Include); err != nil {
			return err
		}
	}
	return nil
}

// stripIncludeChildOptions deep-copies an include tree with Order, Limit and
// Offset removed, which is the tree the joined parent select is built from:
// [Model.FindAllEager] honours those itself, one batched query later.
func stripIncludeChildOptions(incs []Include) []Include {
	if len(incs) == 0 {
		return nil
	}
	out := make([]Include, 0, len(incs))
	for _, inc := range incs {
		inc.Order = nil
		inc.Limit = nil
		inc.Offset = nil
		inc.Include = stripIncludeChildOptions(inc.Include)
		out = append(out, inc)
	}
	return out
}

// ---------------------------------------------------------------------------
// Include.Required for the batched to-many kinds.
// ---------------------------------------------------------------------------

// includeFilter is a pending "<attribute> IN (<select>)" restriction on some
// model, where select is already-rendered SQL carrying substitution markers (see
// rawSubDelim) in place of its bind parameters. Keeping it in this half-rendered
// form is what lets a filter discovered several levels down the include tree be
// lifted, one association at a time, until it is expressed against the model the
// statement is actually selecting from — at which point it needs no correlation to
// an outer table alias, and so works identically in a plain select, in an aliased
// join and in an aggregate.
type includeFilter struct {
	attribute string
	sql       string
	args      []any
}

// includeRequiredClause returns the extra filter a query's Required includes
// impose on the rows the statement selects, or nil when they impose none.
//
// A joined (to-one) include needs nothing here: it is an INNER JOIN. A batched
// to-many include cannot be an INNER JOIN, because its rows arrive in a later
// query, so Required becomes `parentKey IN (SELECT foreignKey FROM children ...)`.
// A Required include nested below a to-one include, or below another to-many one,
// is lifted into a nested subquery so that it still filters the parents — which
// mirrors upstream, where `required` propagates up the include chain.
//
// Errors are not returned because this is called from effectiveWhere, which has
// nowhere to put one; every error reachable here is also produced by
// checkIncludeOptions, which writeLimit calls on the same include tree for every
// select the package emits.
func (m *Model) includeRequiredClause(q Query) *Clause {
	filters, err := m.includeRequiredFilters(q, q.Include)
	if err != nil || len(filters) == 0 {
		return nil
	}
	clauses := make([]*Clause, 0, len(filters))
	for _, f := range filters {
		clauses = append(clauses, &Clause{
			kind: clauseRaw,
			sql:  rawColumnMarker(f.attribute) + " IN (" + f.sql + ")",
			args: f.args,
		})
	}
	return And(clauses...)
}

// includeRequiredFilters collects the restrictions incs impose on rows of m.
func (m *Model) includeRequiredFilters(q Query, incs []Include) ([]includeFilter, error) {
	var out []includeFilter
	for _, inc := range incs {
		assoc, ok := m.Association(inc.Association)
		if !ok {
			return nil, fmt.Errorf("%w: %s declares no association %q",
				ErrInvalidQuery, m.name, inc.Association)
		}
		if err := assoc.Err(); err != nil {
			return nil, err
		}
		// Restrictions from further down are expressed against the target, and are
		// what makes a deeply nested Required propagate up.
		nested, err := assoc.target.includeRequiredFilters(q, inc.Include)
		if err != nil {
			return nil, err
		}
		if !assoc.kind.Multiple() {
			// A to-one include is already an INNER JOIN when it is Required, so it
			// only needs a filter of its own to carry a descendant's.
			if len(nested) == 0 {
				continue
			}
		} else if !inc.Required && len(nested) == 0 {
			continue
		}
		f, err := assoc.requiredFilter(q, inc, nested)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// requiredFilter expresses "the source row has a matching associated row that
// itself satisfies nested" as a restriction on the source model.
func (a *Association) requiredFilter(q Query, inc Include, nested []includeFilter) (includeFilter, error) {
	switch a.kind {
	case AssocBelongsTo:
		// The source holds the key, so the subquery projects the target's key.
		sql, args, err := a.target.filterSelect(q, a.targetKey, inc.Where, nested)
		return includeFilter{attribute: a.foreignKey, sql: sql, args: args}, err
	case AssocHasOne, AssocHasMany:
		sql, args, err := a.target.filterSelect(q, a.foreignKey, inc.Where, nested)
		return includeFilter{attribute: a.sourceKey, sql: sql, args: args}, err
	case AssocBelongsToMany:
		// Two hops: the join table's rows pointing at a qualifying target.
		targetSQL, targetArgs, err := a.target.filterSelect(q, a.targetKey, inc.Where, nested)
		if err != nil {
			return includeFilter{}, err
		}
		viaTarget := includeFilter{attribute: a.otherKey, sql: targetSQL, args: targetArgs}
		sql, args, err := a.through.filterSelect(q, a.foreignKey, nil, []includeFilter{viaTarget})
		return includeFilter{attribute: a.sourceKey, sql: sql, args: args}, err
	default:
		return includeFilter{}, fmt.Errorf("%w: unknown association kind %d", ErrInvalidQuery, int(a.kind))
	}
}

// filterSelect renders `SELECT <projected> FROM <m> WHERE ...` for use as the
// right-hand side of an IN. The statement is uncorrelated — it names exactly one
// table and no alias — so it can be spliced into any enclosing statement.
//
// Bind parameters become substitution markers, because their positions depend on
// what the enclosing statement has already bound.
func (m *Model) filterSelect(q Query, projected string, where *Clause, extra []includeFilter) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	b := newBuilder(m)
	b.markers = true
	b.write("SELECT ")
	if err := b.column(projected); err != nil {
		return "", nil, err
	}
	b.write(" FROM ")
	if err := b.table(); err != nil {
		return "", nil, err
	}
	// Include is deliberately left out of this Query: the nested restrictions have
	// already been resolved into extra, and passing them again would recurse.
	conds := m.effectiveWhere(Query{Where: where, Paranoid: q.Paranoid})
	if conds != nil || len(extra) > 0 {
		b.write(" WHERE ")
		if conds == nil {
			b.write("1 = 1")
		} else if err := b.clause(conds); err != nil {
			return "", nil, err
		}
		for _, f := range extra {
			b.write(" AND ")
			if err := b.column(f.attribute); err != nil {
				return "", nil, err
			}
			b.write(" IN (", f.sql, ")")
			// The arguments must join the list in the order their markers appear.
			b.args = append(b.args, f.args...)
		}
	}
	return b.sb.String(), b.args, nil
}

// ---------------------------------------------------------------------------
// Ordered and per-parent-limited eager loading.
// ---------------------------------------------------------------------------

// FindAllEager is [Model.FindAll] with the full [Include] vocabulary: as well as
// Where, Attrs, Required and nesting, it honours [Include.Order],
// [Include.Limit] and [Include.Offset] on the batched to-many associations.
//
// It exists as a separate entry point because ordering and per-parent limiting
// need the children loaded by a loader that knows about them, and Include.Limit is
// rejected by FindAll rather than silently dropped. Everything else behaves
// exactly as FindAll: scopes, soft deletes, transactions and the shape of the
// returned rows are unchanged.
//
// The statement count is a function of the include tree, never of the number of
// rows. One select covers the model and its to-one includes; each to-many
// association costs one more (two for a BelongsToMany, for the join table and the
// targets), at each level of nesting. A per-parent Limit or Offset does not add
// one: it is folded into the association's own batched select with
// ROW_NUMBER() OVER (PARTITION BY foreignKey ORDER BY ...), and where the dialect
// has no window functions (see [DialectSupportsWindowFunctions]) the same single
// batched select is trimmed per parent in Go instead. There is no per-parent query
// on any path.
func (m *Model) FindAllEager(ctx context.Context, q Query) ([]Values, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}
	q = m.scoped(q)
	if err := m.validateEagerIncludes(q.Include); err != nil {
		return nil, err
	}
	if len(q.Include) == 0 {
		return m.FindAll(ctx, q)
	}
	if !includesHaveChildOptions(q.Include) {
		// Nothing here to add, so use the ordinary loader and stay bug-compatible
		// with it.
		return m.FindAll(ctx, q)
	}

	joined := q
	joined.Include = stripIncludeChildOptions(q.Include)
	plan, err := m.planIncludes(joined, false)
	if err != nil {
		return nil, err
	}
	pairs, err := m.batchedIncludes(q.Include)
	if err != nil {
		return nil, err
	}
	if len(pairs) != len(plan.batched) {
		return nil, fmt.Errorf("%w: eager plan has %d batched associations, the include tree %d",
			ErrInvalidQuery, len(plan.batched), len(pairs))
	}

	statement, args, err := m.buildIncludedSelect(joined, plan)
	if err != nil {
		return nil, err
	}
	querier, err := m.querierFor(q)
	if err != nil {
		return nil, err
	}
	m.seq.log(ctx, statement, args)
	rows, err := querier.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, m.seq.dialect.TranslateError(err)
	}
	result, scanErr := m.scanIncluded(rows, plan)
	closeErr := rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	for i, node := range plan.batched {
		if node.assoc != pairs[i].assoc {
			return nil, fmt.Errorf("%w: eager plan and include tree disagree at %s",
				ErrInvalidQuery, node.as)
		}
		if err := loadEagerBatch(ctx, q, result, node, pairs[i].inc); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// FindOneEager is [Model.FindOne] with the full [Include] vocabulary. As with
// FindOne, a miss is a *[RecordNotFoundError].
func (m *Model) FindOneEager(ctx context.Context, q Query) (Values, error) {
	q.Limit = Int(1)
	rows, err := m.FindAllEager(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, &RecordNotFoundError{Model: m.name}
	}
	return rows[0], nil
}

// includesHaveChildOptions reports whether anywhere in the tree asks for an
// order, a per-parent limit or a per-parent offset.
func includesHaveChildOptions(incs []Include) bool {
	for _, inc := range incs {
		if inc.hasChildOptions() || includesHaveChildOptions(inc.Include) {
			return true
		}
	}
	return false
}

// batchedInclude pairs a to-many include with the association it names.
type batchedInclude struct {
	assoc *Association
	inc   Include
}

// batchedIncludes lists the to-many includes in the order the eager plan batches
// them: pre-order, with a to-many association ending the descent because its
// children are loaded from its own result set. The order must match
// includePlan.batched element for element, and FindAllEager checks that it does
// rather than trusting it.
func (m *Model) batchedIncludes(incs []Include) ([]batchedInclude, error) {
	var out []batchedInclude
	for _, inc := range incs {
		assoc, ok := m.Association(inc.Association)
		if !ok {
			return nil, fmt.Errorf("%w: %s declares no association %q",
				ErrInvalidQuery, m.name, inc.Association)
		}
		if err := assoc.Err(); err != nil {
			return nil, err
		}
		if assoc.kind.Multiple() {
			out = append(out, batchedInclude{assoc: assoc, inc: inc})
			continue
		}
		nested, err := assoc.target.batchedIncludes(inc.Include)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// loadEagerBatch loads one to-many association for every parent at once,
// honouring the include's Order, Limit and Offset.
func loadEagerBatch(ctx context.Context, q Query, rows []Values, n *includeNode, inc Include) error {
	a := n.assoc
	parents := gatherParents(rows, n.parentPath)
	for _, parent := range parents {
		parent[n.as] = []Values{}
	}
	keys := distinctKeyValues(parents, a.sourceKey)
	if len(keys) == 0 {
		return nil
	}
	var (
		grouped map[string][]Values
		err     error
	)
	switch a.kind {
	case AssocHasMany:
		grouped, err = loadHasManyGroups(ctx, q, a, inc, keys)
	case AssocBelongsToMany:
		grouped, err = loadBelongsToManyGroups(ctx, q, a, inc, keys)
	default:
		return fmt.Errorf("%w: %s is not a batched association", ErrInvalidQuery, a.kind)
	}
	if err != nil {
		return err
	}
	assignGroups(parents, a.sourceKey, n.as, grouped)
	return nil
}

// loadHasManyGroups fetches every parent's children in one statement and returns
// them grouped by foreign key, already ordered and cut to the include's per-parent
// window.
func loadHasManyGroups(ctx context.Context, q Query, a *Association, inc Include, keys []any) (map[string][]Values, error) {
	where := And(inc.Where, Op.In(a.foreignKey, keys))
	child := Query{Where: where, Attrs: restrictedAttrs(inc.Attrs, a.foreignKey)}
	if (inc.Limit != nil || inc.Offset != nil) &&
		len(inc.Include) == 0 &&
		DialectSupportsWindowFunctions(a.target.seq.dialect) {
		// A scope on the target may contribute an order, a limit or a projection that
		// the window statement is in no position to merge, so the window is only used
		// when the scoped query is the one this loader built.
		scoped := a.target.scoped(child)
		if plainlyScoped(scoped, child) {
			cols, err := a.target.selectColumns(scoped.Attrs)
			if err != nil {
				return nil, err
			}
			cols = ensureAttrs(cols, a.foreignKey)
			statement, args, err := a.target.buildPartitionedSelect(cols, a.foreignKey, scoped, inc)
			if err != nil {
				return nil, err
			}
			children, err := a.target.runSelect(ctx, q, statement, args, cols)
			if err != nil {
				return nil, err
			}
			return groupByKey(children, a.foreignKey), nil
		}
	}
	// One batched select for the whole set, ordered in SQL; the per-parent window is
	// then a slice of each group. This is the path for a dialect without window
	// functions and for an include that nests further, and it is still one
	// statement for every parent — it over-fetches rows, it does not re-query.
	children, err := a.target.FindAllEager(ctx, Query{
		Where:   where,
		Attrs:   restrictedAttrs(inc.Attrs, a.foreignKey),
		Order:   inc.Order,
		Include: inc.Include,
		Tx:      q.Tx,
	})
	if err != nil {
		return nil, err
	}
	grouped := groupByKey(children, a.foreignKey)
	trimGroups(grouped, inc.Limit, inc.Offset)
	return grouped, nil
}

// plainlyScoped reports whether scoping a child query changed nothing the window
// statement cannot express. Only Where and Paranoid may differ: the window select
// renders both, whereas a scope's Order, Limit, Offset, Group, Having, Distinct or
// extra Include would have to be composed with the include's own window, which the
// batched-then-trimmed path does correctly and this one does not.
func plainlyScoped(scoped, original Query) bool {
	return len(scoped.Order) == 0 &&
		scoped.Limit == nil &&
		scoped.Offset == nil &&
		len(scoped.Group) == 0 &&
		scoped.Having == nil &&
		!scoped.Distinct &&
		len(scoped.Include) == 0 &&
		len(scoped.Attrs) == len(original.Attrs)
}

// loadBelongsToManyGroups fetches the join-table rows and then the target rows,
// two statements for the whole batch however many parents there are.
//
// The per-parent window is applied in Go: the partition key lives in the join
// table rather than in the target, so a window over the target's own select cannot
// see it, and pulling the join into that select would break the nested-include
// path. The targets are ordered in SQL and grouped in that order, so the slice
// each parent keeps is the right one.
func loadBelongsToManyGroups(ctx context.Context, q Query, a *Association, inc Include, keys []any) (map[string][]Values, error) {
	pairs, err := a.through.FindAll(ctx, Query{
		Attrs: ensureAttrs([]string{a.foreignKey}, a.otherKey),
		Where: Op.In(a.foreignKey, keys),
		Tx:    q.Tx,
	})
	if err != nil {
		return nil, err
	}
	targetKeys := distinctKeyValues(pairs, a.otherKey)
	if len(targetKeys) == 0 {
		return nil, nil
	}
	targets, err := a.target.FindAllEager(ctx, Query{
		Where:   And(inc.Where, Op.In(a.targetKey, targetKeys)),
		Attrs:   restrictedAttrs(inc.Attrs, a.targetKey),
		Order:   inc.Order,
		Include: inc.Include,
		Tx:      q.Tx,
	})
	if err != nil {
		return nil, err
	}
	sourcesByTarget := make(map[string][]string, len(targetKeys))
	for _, pair := range pairs {
		source, okSource := associationKey(pair[a.foreignKey])
		target, okTarget := associationKey(pair[a.otherKey])
		if !okSource || !okTarget {
			continue
		}
		sourcesByTarget[target] = append(sourcesByTarget[target], source)
	}
	// Grouping is driven by the ordered targets, so each group comes out in the
	// order the include asked for.
	grouped := map[string][]Values{}
	for _, row := range targets {
		key, ok := associationKey(row[a.targetKey])
		if !ok {
			continue
		}
		for _, source := range sourcesByTarget[key] {
			grouped[source] = append(grouped[source], row)
		}
	}
	trimGroups(grouped, inc.Limit, inc.Offset)
	return grouped, nil
}

// trimGroups cuts every group to the include's per-parent window. A group shorter
// than the offset keeps nothing, and a zero limit keeps nothing, which is what
// LIMIT 0 means.
func trimGroups(grouped map[string][]Values, limit, offset *int) {
	if limit == nil && offset == nil {
		return
	}
	skip := 0
	if offset != nil {
		skip = *offset
	}
	for key, list := range grouped {
		if skip >= len(list) {
			grouped[key] = []Values{}
			continue
		}
		list = list[skip:]
		if limit != nil && *limit < len(list) {
			list = list[:*limit]
		}
		grouped[key] = list
	}
}

// rowNumberAlias is the column the window function's rank is projected as. It is
// suffixed with underscores if the model already has a column of that name, so a
// table cannot shadow it.
const rowNumberAlias = "sequelize_row_number"

// rowNumberColumn returns a column name for the window function's rank that no
// column of m collides with.
func (m *Model) rowNumberColumn() (string, error) {
	taken := map[string]bool{}
	for _, attr := range m.AttributeNames() {
		col, err := m.Column(attr)
		if err != nil {
			return "", err
		}
		taken[col] = true
	}
	name := rowNumberAlias
	for taken[name] {
		name += "_"
	}
	return name, ValidateIdentifier(name)
}

// partitionAlias is the derived table the window select ranks inside.
const partitionAlias = "sequelize_partitioned"

// buildPartitionedSelect renders one statement that returns, for every parent key
// the WHERE matches, only that parent's slice of the association's rows:
//
//	SELECT "p"."id", "p"."title", "p"."user_id" FROM (
//	  SELECT "id", "title", "user_id",
//	         ROW_NUMBER() OVER (PARTITION BY "user_id" ORDER BY "title" ASC) AS "rn"
//	  FROM "posts" WHERE "user_id" IN (?, ?, ?)
//	) AS "p" WHERE "p"."rn" > ? AND "p"."rn" <= ? ORDER BY "p"."rn" ASC
//
// The window is what makes the limit per parent. A plain LIMIT would cut the
// combined result instead, handing every row to the first parent and nothing to
// the rest — which is the bug this exists to avoid.
//
// Ranking needs a deterministic order, so an include with no Order is ranked by
// the target's primary key.
// The query supplies the children's own filter and soft-delete behaviour; its
// Order, Limit and Offset are not read, because the window — not the statement —
// is what bounds each parent's slice.
func (m *Model) buildPartitionedSelect(cols []string, partitionAttr string, child Query, inc Include) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	if len(cols) == 0 {
		return "", nil, fmt.Errorf("%w: a partitioned select needs at least one column", ErrInvalidQuery)
	}
	rank, err := m.rowNumberColumn()
	if err != nil {
		return "", nil, err
	}
	quotedRank, err := m.seq.dialect.Quote(rank)
	if err != nil {
		return "", nil, err
	}
	quotedOuter, err := m.seq.dialect.Quote(partitionAlias)
	if err != nil {
		return "", nil, err
	}

	order := inc.Order
	if len(order) == 0 {
		for _, key := range m.pk {
			order = append(order, Asc(key))
		}
		if len(order) == 0 {
			order = []OrderTerm{Asc(partitionAttr)}
		}
	}

	b := newBuilder(m)
	b.write("SELECT ")
	for i, attr := range cols {
		if i > 0 {
			b.write(", ")
		}
		if err := b.qualified(m, partitionAlias, attr); err != nil {
			return "", nil, err
		}
	}
	b.write(" FROM (SELECT ")
	for i, attr := range cols {
		if i > 0 {
			b.write(", ")
		}
		if err := b.column(attr); err != nil {
			return "", nil, err
		}
	}
	b.write(", ROW_NUMBER() OVER (PARTITION BY ")
	if err := b.column(partitionAttr); err != nil {
		return "", nil, err
	}
	b.write(" ORDER BY ")
	if err := b.writeOrderTerms(order); err != nil {
		return "", nil, err
	}
	b.write(") AS ", quotedRank, " FROM ")
	if err := b.table(); err != nil {
		return "", nil, err
	}
	if err := b.writeWhere(m.effectiveWhere(Query{Where: child.Where, Paranoid: child.Paranoid})); err != nil {
		return "", nil, err
	}
	b.write(") AS ", quotedOuter, " WHERE ")

	skip := int64(0)
	if inc.Offset != nil {
		skip = int64(*inc.Offset)
	}
	b.write(quotedOuter, ".", quotedRank, " > ")
	if err := b.bindRaw(skip); err != nil {
		return "", nil, err
	}
	if inc.Limit != nil {
		b.write(" AND ", quotedOuter, ".", quotedRank, " <= ")
		if err := b.bindRaw(skip + int64(*inc.Limit)); err != nil {
			return "", nil, err
		}
	}
	// Ranking order is the include's order within each partition, so ordering the
	// outer select by the rank is enough for every group to come out sorted.
	b.write(" ORDER BY ", quotedOuter, ".", quotedRank, " ASC")
	return b.sb.String(), b.args, nil
}

// writeOrderTerms appends a comma-separated ORDER BY list, without the keyword.
func (b *builder) writeOrderTerms(terms []OrderTerm) error {
	for i, term := range terms {
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
	return nil
}

// runSelect executes a statement this package generated and decodes it as rows of
// m, projected in the order cols gives.
func (m *Model) runSelect(ctx context.Context, q Query, statement string, args []any, cols []string) ([]Values, error) {
	querier, err := m.querierFor(q)
	if err != nil {
		return nil, err
	}
	m.seq.log(ctx, statement, args)
	rows, err := querier.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, m.seq.dialect.TranslateError(err)
	}
	defer rows.Close()
	return m.scanRows(rows, cols)
}
