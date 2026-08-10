package sequelize

import "testing"

func TestOperatorConstants(t *testing.T) {
	cases := map[Operator]string{
		OpEq: "=", OpNe: "<>", OpGt: ">", OpGte: ">=", OpLt: "<", OpLte: "<=",
		OpLike: "LIKE", OpNotLike: "NOT LIKE", OpILike: "ILIKE", OpNotILike: "NOT ILIKE",
	}
	for op, want := range cases {
		if string(op) != want {
			t.Errorf("operator = %q, want %q", op, want)
		}
	}
}

func TestOpComparisonsCarryTheirOperator(t *testing.T) {
	cases := []struct {
		clause *Clause
		op     Operator
	}{
		{Op.Eq("a", 1), OpEq},
		{Op.Ne("a", 1), OpNe},
		{Op.Gt("a", 1), OpGt},
		{Op.Gte("a", 1), OpGte},
		{Op.Lt("a", 1), OpLt},
		{Op.Lte("a", 1), OpLte},
		{Op.Like("a", "%x%"), OpLike},
		{Op.NotLike("a", "%x%"), OpNotLike},
		{Op.ILike("a", "%x%"), OpILike},
		{Op.NotILike("a", "%x%"), OpNotILike},
	}
	for _, c := range cases {
		if c.clause.kind != clauseCompare {
			t.Errorf("%s: kind = %v, want a comparison", c.op, c.clause.kind)
		}
		if c.clause.op != c.op {
			t.Errorf("clause operator = %q, want %q", c.clause.op, c.op)
		}
		if c.clause.attribute != "a" {
			t.Errorf("clause attribute = %q, want a", c.clause.attribute)
		}
	}
}

func TestOpEqAndNeTreatNilAsNull(t *testing.T) {
	if got := Op.Eq("a", nil); got.kind != clauseNull {
		t.Errorf("Op.Eq(a, nil).kind = %v, want IS NULL", got.kind)
	}
	if got := Op.Ne("a", nil); got.kind != clauseNotNull {
		t.Errorf("Op.Ne(a, nil).kind = %v, want IS NOT NULL", got.kind)
	}
	if got := Op.Is("a", nil); got.kind != clauseNull {
		t.Errorf("Op.Is(a, nil).kind = %v, want IS NULL", got.kind)
	}
	if got := Op.Is("a", 1); got.kind != clauseCompare {
		t.Errorf("Op.Is(a, 1).kind = %v, want a comparison", got.kind)
	}
}

func TestOpInExpandsEveryKindOfSet(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  int
	}{
		{"typed slice", []int{1, 2, 3}, 3},
		{"any slice", []any{1, "x"}, 2},
		{"array", [2]string{"a", "b"}, 2},
		{"scalar", 7, 1},
		{"string is scalar", "abc", 1},
		{"bytes are scalar", []byte("ab"), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Op.In("a", c.input)
			if got.kind != clauseIn {
				t.Fatalf("kind = %v, want IN", got.kind)
			}
			if len(got.values) != c.want {
				t.Errorf("Op.In(%v) has %d values, want %d", c.input, len(got.values), c.want)
			}
		})
	}
}

// TestOpInEmptyMatchesNothing pins the semantics ARCHITECTURE.md calls out: an
// empty set must be a clause matching nothing, never "IN ()".
func TestOpInEmptyMatchesNothing(t *testing.T) {
	for _, empty := range []any{[]int{}, []any{}, []string(nil), nil} {
		got := Op.In("a", empty)
		if got.kind != clauseConstant || got.constant {
			t.Errorf("Op.In(a, %v) = %+v, want the false constant", empty, got)
		}
	}
}

func TestOpNotInEmptyMatchesEverything(t *testing.T) {
	got := Op.NotIn("a", []int{})
	if got.kind != clauseConstant || !got.constant {
		t.Errorf("Op.NotIn(a, []) = %+v, want the true constant", got)
	}
	if got := Op.NotIn("a", []int{1}); got.kind != clauseNotIn {
		t.Errorf("Op.NotIn(a, [1]).kind = %v, want NOT IN", got.kind)
	}
}

func TestOpBetween(t *testing.T) {
	got := Op.Between("a", 1, 2)
	if got.kind != clauseBetween || len(got.values) != 2 {
		t.Errorf("Op.Between = %+v", got)
	}
	if got := Op.NotBetween("a", 1, 2); got.kind != clauseNotBetween {
		t.Errorf("Op.NotBetween.kind = %v", got.kind)
	}
}

func TestAndOrNotCollapse(t *testing.T) {
	one := Op.Eq("a", 1)
	if got := And(one); got != one {
		t.Error("And with a single clause should return it unwrapped")
	}
	if got := Or(one); got != one {
		t.Error("Or with a single clause should return it unwrapped")
	}
	if got := And(nil, one, nil); got != one {
		t.Error("And should drop nil clauses")
	}
	if got := And(); got.kind != clauseConstant || !got.constant {
		t.Errorf("And() = %+v, want the true constant", got)
	}
	if got := Or(); got.kind != clauseConstant || got.constant {
		t.Errorf("Or() = %+v, want the false constant", got)
	}
	if got := Not(nil); got.kind != clauseConstant || got.constant {
		t.Errorf("Not(nil) = %+v, want the false constant", got)
	}
	both := And(one, Op.Eq("b", 2))
	if both.kind != clauseLogical || both.logic != "AND" || len(both.children) != 2 {
		t.Errorf("And(two) = %+v", both)
	}
	if got := Or(one, Op.Eq("b", 2)); got.logic != "OR" {
		t.Errorf("Or(two).logic = %q", got.logic)
	}
	if got := Not(one); got.kind != clauseNot || got.children[0] != one {
		t.Errorf("Not = %+v", got)
	}
}

func TestOpNamespaceDelegates(t *testing.T) {
	one := Op.Eq("a", 1)
	if got := Op.And(one); got != one {
		t.Error("Op.And does not delegate to And")
	}
	if got := Op.Or(one); got != one {
		t.Error("Op.Or does not delegate to Or")
	}
	if got := Op.Not(one); got.kind != clauseNot {
		t.Error("Op.Not does not delegate to Not")
	}
	if got := Op.NotNull("a"); got.kind != clauseNotNull {
		t.Error("Op.NotNull")
	}
	if got := Op.IsNull("a"); got.kind != clauseNull {
		t.Error("Op.IsNull")
	}
}

func TestWhereShorthand(t *testing.T) {
	got := Where(Values{"b": 2, "a": 1})
	if got.kind != clauseLogical || len(got.children) != 2 {
		t.Fatalf("Where(two keys) = %+v", got)
	}
	// Sorted for deterministic SQL.
	if got.children[0].attribute != "a" || got.children[1].attribute != "b" {
		t.Errorf("Where did not visit keys in sorted order: %v, %v",
			got.children[0].attribute, got.children[1].attribute)
	}
	if got := Where(Values{"a": nil}); got.kind != clauseNull {
		t.Errorf("Where with a nil value = %+v, want IS NULL", got)
	}
	if got := Where(Values{"a": []int{1, 2}}); got.kind != clauseIn {
		t.Errorf("Where with a slice = %+v, want IN", got)
	}
	inner := Op.Gt("a", 5)
	if got := Where(Values{"a": inner}); got != inner {
		t.Error("Where should use a *Clause value verbatim")
	}
	if got := Where(nil); got.kind != clauseConstant || !got.constant {
		t.Errorf("Where(nil) = %+v, want the true constant", got)
	}
}

func TestLiteralCopiesArgs(t *testing.T) {
	args := []any{1, 2}
	c := Literal("a = ? AND b = ?", args...)
	args[0] = 99
	if c.args[0] != 1 {
		t.Error("Literal kept a reference to the caller's args slice")
	}
	if c.kind != clauseRaw || c.sql != "a = ? AND b = ?" {
		t.Errorf("Literal = %+v", c)
	}
}

func TestClauseIsEmpty(t *testing.T) {
	var nilClause *Clause
	if !nilClause.IsEmpty() {
		t.Error("a nil clause should be empty")
	}
	if !And().IsEmpty() {
		t.Error("And() should be empty")
	}
	if Op.Eq("a", 1).IsEmpty() {
		t.Error("a comparison should not be empty")
	}
	if Or().IsEmpty() {
		t.Error("Or() matches nothing, which is a constraint, so it is not empty")
	}
}

func TestIsMultiValue(t *testing.T) {
	if isMultiValue(nil) || isMultiValue("s") || isMultiValue([]byte("b")) || isMultiValue(1) {
		t.Error("scalars reported as sets")
	}
	if !isMultiValue([]int{1}) || !isMultiValue([1]int{1}) {
		t.Error("sets reported as scalars")
	}
}
