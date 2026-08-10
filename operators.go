package sequelize

import (
	"reflect"
	"sort"
)

// Operator is the SQL comparison an [Op] method builds. The constants exist so
// that a rendered clause can be inspected in tests; build clauses through [Op]
// rather than naming operators directly.
type Operator string

// The comparison operators the package renders.
const (
	// OpEq is "=".
	OpEq Operator = "="
	// OpNe is "<>".
	OpNe Operator = "<>"
	// OpGt is ">".
	OpGt Operator = ">"
	// OpGte is ">=".
	OpGte Operator = ">="
	// OpLt is "<".
	OpLt Operator = "<"
	// OpLte is "<=".
	OpLte Operator = "<="
	// OpLike is "LIKE".
	OpLike Operator = "LIKE"
	// OpNotLike is "NOT LIKE".
	OpNotLike Operator = "NOT LIKE"
	// OpILike is "ILIKE", which only some backends provide.
	OpILike Operator = "ILIKE"
	// OpNotILike is "NOT ILIKE", which only some backends provide.
	OpNotILike Operator = "NOT ILIKE"
)

// clauseKind discriminates the [Clause] tree's node types.
type clauseKind int

const (
	clauseCompare clauseKind = iota
	clauseIn
	clauseNotIn
	clauseNull
	clauseNotNull
	clauseBetween
	clauseNotBetween
	clauseLogical
	clauseNot
	clauseRaw
	clauseConstant
)

// Clause is a node in a WHERE (or HAVING) tree: a comparison, a grouping of
// other clauses, or a literal SQL fragment. Build clauses with [Op], [Where],
// [And], [Or], [Not] and [Literal]; the zero value is not usable.
//
// A Clause carries attribute *names*; it is bound to physical columns and
// placeholders only when a model renders it, which is why the same clause can be
// reused across queries.
type Clause struct {
	kind      clauseKind
	attribute string
	op        Operator
	value     any
	values    []any
	logic     string
	children  []*Clause
	sql       string
	args      []any
	constant  bool
}

// opNamespace hosts the operator vocabulary. Use the package-level [Op].
type opNamespace struct{}

// Op is the operator vocabulary, mirroring Sequelize's Op: Op.Eq, Op.In,
// Op.Between and so on. Every method takes the attribute name first and returns
// a [Clause] ready to place in [Query.Where].
var Op = opNamespace{}

func compare(attribute string, op Operator, value any) *Clause {
	return &Clause{kind: clauseCompare, attribute: attribute, op: op, value: value}
}

// Eq matches rows where attribute equals value. A nil value becomes IS NULL, as
// upstream does, because "= NULL" matches nothing.
func (opNamespace) Eq(attribute string, value any) *Clause {
	if value == nil {
		return Op.IsNull(attribute)
	}
	return compare(attribute, OpEq, value)
}

// Ne matches rows where attribute differs from value. A nil value becomes
// IS NOT NULL.
func (opNamespace) Ne(attribute string, value any) *Clause {
	if value == nil {
		return Op.NotNull(attribute)
	}
	return compare(attribute, OpNe, value)
}

// Gt matches rows where attribute is greater than value.
func (opNamespace) Gt(attribute string, value any) *Clause {
	return compare(attribute, OpGt, value)
}

// Gte matches rows where attribute is greater than or equal to value.
func (opNamespace) Gte(attribute string, value any) *Clause {
	return compare(attribute, OpGte, value)
}

// Lt matches rows where attribute is less than value.
func (opNamespace) Lt(attribute string, value any) *Clause {
	return compare(attribute, OpLt, value)
}

// Lte matches rows where attribute is less than or equal to value.
func (opNamespace) Lte(attribute string, value any) *Clause {
	return compare(attribute, OpLte, value)
}

// Like matches attribute against a SQL LIKE pattern. The pattern is a bind
// argument, never interpolated, so "%" and "_" in it are pattern syntax and
// nothing else is.
func (opNamespace) Like(attribute string, pattern string) *Clause {
	return compare(attribute, OpLike, pattern)
}

// NotLike is the negation of [opNamespace.Like].
func (opNamespace) NotLike(attribute string, pattern string) *Clause {
	return compare(attribute, OpNotLike, pattern)
}

// ILike matches attribute case-insensitively. It renders ILIKE, which only some
// backends (notably PostgreSQL) provide; SQLite's LIKE is already
// case-insensitive for ASCII, so use [opNamespace.Like] there.
func (opNamespace) ILike(attribute string, pattern string) *Clause {
	return compare(attribute, OpILike, pattern)
}

// NotILike is the negation of [opNamespace.ILike], with the same portability
// caveat.
func (opNamespace) NotILike(attribute string, pattern string) *Clause {
	return compare(attribute, OpNotILike, pattern)
}

// In matches rows whose attribute is one of values. values may be any slice or
// array, or a single scalar.
//
// An empty set matches nothing: the clause renders as a false constant rather
// than the invalid "IN ()". This mirrors upstream and is the behaviour that
// makes it safe to feed In the result of a filter that came back empty.
func (opNamespace) In(attribute string, values any) *Clause {
	list := expandValues(values)
	if len(list) == 0 {
		return falseClause()
	}
	return &Clause{kind: clauseIn, attribute: attribute, values: list}
}

// NotIn matches rows whose attribute is none of values. An empty set matches
// everything, which is the complement of [opNamespace.In]'s empty case.
func (opNamespace) NotIn(attribute string, values any) *Clause {
	list := expandValues(values)
	if len(list) == 0 {
		return trueClause()
	}
	return &Clause{kind: clauseNotIn, attribute: attribute, values: list}
}

// Between matches rows where attribute lies within the inclusive range
// [low, high].
func (opNamespace) Between(attribute string, low, high any) *Clause {
	return &Clause{kind: clauseBetween, attribute: attribute, values: []any{low, high}}
}

// NotBetween is the negation of [opNamespace.Between].
func (opNamespace) NotBetween(attribute string, low, high any) *Clause {
	return &Clause{kind: clauseNotBetween, attribute: attribute, values: []any{low, high}}
}

// IsNull matches rows where attribute is NULL.
func (opNamespace) IsNull(attribute string) *Clause {
	return &Clause{kind: clauseNull, attribute: attribute}
}

// NotNull matches rows where attribute is not NULL.
func (opNamespace) NotNull(attribute string) *Clause {
	return &Clause{kind: clauseNotNull, attribute: attribute}
}

// Is matches NULL when value is nil and equality otherwise, which is upstream's
// Op.is.
func (opNamespace) Is(attribute string, value any) *Clause {
	return Op.Eq(attribute, value)
}

// And groups clauses with AND. Nil clauses are skipped; an empty group matches
// everything.
func (opNamespace) And(clauses ...*Clause) *Clause { return And(clauses...) }

// Or groups clauses with OR. Nil clauses are skipped; an empty group matches
// nothing, because an OR of no alternatives is unsatisfiable.
func (opNamespace) Or(clauses ...*Clause) *Clause { return Or(clauses...) }

// Not negates a clause.
func (opNamespace) Not(clause *Clause) *Clause { return Not(clause) }

// And groups clauses with AND. Nil entries are ignored, a single clause is
// returned unwrapped, and an empty group matches everything.
func And(clauses ...*Clause) *Clause {
	kept := compact(clauses)
	switch len(kept) {
	case 0:
		return trueClause()
	case 1:
		return kept[0]
	default:
		return &Clause{kind: clauseLogical, logic: "AND", children: kept}
	}
}

// Or groups clauses with OR. Nil entries are ignored, a single clause is
// returned unwrapped, and an empty group matches nothing.
func Or(clauses ...*Clause) *Clause {
	kept := compact(clauses)
	switch len(kept) {
	case 0:
		return falseClause()
	case 1:
		return kept[0]
	default:
		return &Clause{kind: clauseLogical, logic: "OR", children: kept}
	}
}

// Not negates a clause. Not(nil) matches nothing, since the negation of "match
// everything" is "match nothing".
func Not(clause *Clause) *Clause {
	if clause == nil {
		return falseClause()
	}
	return &Clause{kind: clauseNot, children: []*Clause{clause}}
}

// Where builds an AND of equality comparisons from a [Values] map, the shorthand
// upstream spells as `where: { id: 1, name: 'x' }`. Keys are visited in sorted
// order so the rendered SQL is deterministic.
//
// A nil value becomes IS NULL. A slice or array value becomes IN, so
// Where(Values{"id": []int{1,2}}) matches either. A *[Clause] value is used
// verbatim, which lets a map mix shorthand with operator clauses; note that such
// a clause carries its own attribute name, so the map key is then only
// documentation.
func Where(values Values) *Clause {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	clauses := make([]*Clause, 0, len(keys))
	for _, k := range keys {
		switch v := values[k].(type) {
		case nil:
			clauses = append(clauses, Op.IsNull(k))
		case *Clause:
			clauses = append(clauses, v)
		default:
			if isMultiValue(values[k]) {
				clauses = append(clauses, Op.In(k, values[k]))
			} else {
				clauses = append(clauses, Op.Eq(k, values[k]))
			}
		}
	}
	return And(clauses...)
}

// Literal wraps a raw SQL fragment and its bind arguments as a clause, for the
// conditions the operator vocabulary cannot express.
//
// The fragment is emitted verbatim: it is the one place in the package where the
// caller, not the port, is responsible for the SQL's safety. Keep every value in
// args and every identifier a constant in your own source.
func Literal(sql string, args ...any) *Clause {
	return &Clause{kind: clauseRaw, sql: sql, args: append([]any(nil), args...)}
}

// IsEmpty reports whether the clause constrains nothing, which is true for a nil
// clause and for an empty [And].
func (c *Clause) IsEmpty() bool {
	return c == nil || (c.kind == clauseConstant && c.constant)
}

func trueClause() *Clause  { return &Clause{kind: clauseConstant, constant: true} }
func falseClause() *Clause { return &Clause{kind: clauseConstant, constant: false} }

func compact(clauses []*Clause) []*Clause {
	kept := make([]*Clause, 0, len(clauses))
	for _, c := range clauses {
		if c != nil {
			kept = append(kept, c)
		}
	}
	return kept
}

// isMultiValue reports whether v should be treated as a set for [Where]. Strings
// and byte slices are scalars, everything else sliceable is a set.
func isMultiValue(v any) bool {
	switch v.(type) {
	case nil, string, []byte:
		return false
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.Slice, reflect.Array:
		return true
	default:
		return false
	}
}

// expandValues flattens a slice, array or scalar into a []any for IN rendering.
func expandValues(values any) []any {
	if values == nil {
		return nil
	}
	if list, ok := values.([]any); ok {
		return append([]any(nil), list...)
	}
	if !isMultiValue(values) {
		return []any{values}
	}
	rv := reflect.ValueOf(values)
	out := make([]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out = append(out, rv.Index(i).Interface())
	}
	return out
}
