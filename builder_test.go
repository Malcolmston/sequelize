package sequelize

import (
	"errors"
	"strings"
	"testing"
)

// builderModel defines a plain model with timestamps off, so the generated SQL
// in these tests is only what the test asked for.
func builderModel(t *testing.T) *Model {
	t.Helper()
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{
		"name":  {Type: STRING(20), AllowNull: NotNull()},
		"price": {Type: FLOAT()},
		"tag":   {Type: STRING(10)},
	}, Timestamps(false))
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	return m
}

func TestIntReturnsAPointer(t *testing.T) {
	p := Int(5)
	if p == nil || *p != 5 {
		t.Fatalf("Int(5) = %v", p)
	}
	if Int(1) == Int(1) {
		t.Error("Int must return a fresh pointer each time")
	}
}

func TestAscDesc(t *testing.T) {
	if got := Asc("a"); got.Attribute != "a" || got.Descending {
		t.Errorf("Asc = %+v", got)
	}
	if got := Desc("a"); got.Attribute != "a" || !got.Descending {
		t.Errorf("Desc = %+v", got)
	}
}

func TestFirstQuery(t *testing.T) {
	if q, err := firstQuery(nil); err != nil || q.Where != nil {
		t.Errorf("firstQuery(nil) = %+v, %v", q, err)
	}
	want := Query{Distinct: true}
	if q, err := firstQuery([]Query{want}); err != nil || !q.Distinct {
		t.Errorf("firstQuery(one) = %+v, %v", q, err)
	}
	if _, err := firstQuery([]Query{{}, {}}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("firstQuery(two) = %v, want ErrInvalidQuery", err)
	}
}

func TestBuildSelectAllColumns(t *testing.T) {
	m := builderModel(t)
	sql, args, cols, err := m.buildSelect(Query{})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	want := `SELECT "id", "name", "price", "tag" FROM "widgets"`
	if sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	if strings.Join(cols, ",") != "id,name,price,tag" {
		t.Errorf("cols = %v", cols)
	}
}

func TestBuildSelectWithEverything(t *testing.T) {
	m := builderModel(t)
	sql, args, cols, err := m.buildSelect(Query{
		Attrs:    []string{"name", "price"},
		Distinct: true,
		Where:    And(Op.Gte("price", 10.0), Op.In("tag", []string{"a", "b"})),
		Group:    []string{"tag"},
		Having:   Op.Gt("price", 1.0),
		Order:    []OrderTerm{Desc("price"), Asc("name")},
		Limit:    Int(5),
		Offset:   Int(10),
	})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	want := `SELECT DISTINCT "name", "price" FROM "widgets" WHERE ("price" >= ? AND "tag" IN (?, ?)) ` +
		`GROUP BY "tag" HAVING "price" > ? ORDER BY "price" DESC, "name" ASC LIMIT ? OFFSET ?`
	if sql != want {
		t.Errorf("sql =\n%q\nwant\n%q", sql, want)
	}
	if len(args) != 6 {
		t.Errorf("args = %v, want 6 of them", args)
	}
	if strings.Join(cols, ",") != "name,price" {
		t.Errorf("cols = %v", cols)
	}
}

// TestBuildSelectEmptyInIsValidSQL is the parity case: an empty IN set must
// render as a false constant, not as "IN ()".
func TestBuildSelectEmptyInIsValidSQL(t *testing.T) {
	m := builderModel(t)
	sql, args, _, err := m.buildSelect(Query{Where: Op.In("tag", []string{})})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.HasSuffix(sql, "WHERE 1 = 0") {
		t.Errorf("sql = %q, want a WHERE that matches nothing", sql)
	}
	if strings.Contains(sql, "IN ()") {
		t.Errorf("sql = %q contains invalid SQL", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

func TestBuildSelectClauseForms(t *testing.T) {
	m := builderModel(t)
	cases := []struct {
		name   string
		clause *Clause
		want   string
	}{
		{"is null", Op.IsNull("tag"), `"tag" IS NULL`},
		{"is not null", Op.NotNull("tag"), `"tag" IS NOT NULL`},
		{"between", Op.Between("price", 1.0, 2.0), `"price" BETWEEN ? AND ?`},
		{"not between", Op.NotBetween("price", 1.0, 2.0), `"price" NOT BETWEEN ? AND ?`},
		{"not in", Op.NotIn("tag", []string{"x"}), `"tag" NOT IN (?)`},
		{"or", Or(Op.Eq("tag", "a"), Op.Eq("tag", "b")), `("tag" = ? OR "tag" = ?)`},
		{"not", Not(Op.Eq("tag", "a")), `NOT ("tag" = ?)`},
		{"literal", Literal(`"tag" GLOB ?`, "a*"), `("tag" GLOB ?)`},
		{"true constant", And(), `1 = 1`},
		{"like", Op.Like("tag", "%a%"), `"tag" LIKE ?`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, _, _, err := m.buildSelect(Query{Attrs: []string{"id"}, Where: c.clause})
			if err != nil {
				t.Fatalf("buildSelect: %v", err)
			}
			if want := `SELECT "id" FROM "widgets" WHERE ` + c.want; sql != want {
				t.Errorf("sql = %q, want %q", sql, want)
			}
		})
	}
}

func TestBuildSelectRejectsUnknownAttributes(t *testing.T) {
	m := builderModel(t)
	cases := map[string]Query{
		"in Attrs":  {Attrs: []string{"nope"}},
		"in Where":  {Where: Op.Eq("nope", 1)},
		"in Order":  {Order: []OrderTerm{Asc("nope")}},
		"in Group":  {Group: []string{"nope"}},
		"in Having": {Group: []string{"tag"}, Having: Op.Gt("nope", 1)},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := m.buildSelect(q); !errors.Is(err, ErrUnknownAttribute) {
				t.Errorf("buildSelect = %v, want ErrUnknownAttribute", err)
			}
		})
	}
}

// A comparison operator the dialect cannot render must fail at build time with a
// clear ErrUnsupported, not emit SQL the driver rejects at execution. On SQLite,
// ILIKE has no spelling.
func TestBuildSelectRejectsUnsupportedOperator(t *testing.T) {
	m := builderModel(t)
	for _, c := range []*Clause{Op.ILike("tag", "%a%"), Op.NotILike("tag", "%a%")} {
		if _, _, _, err := m.buildSelect(Query{Where: c}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("buildSelect with %s = %v, want ErrUnsupported", c.op, err)
		}
	}
	// A portable operator on the same dialect still renders.
	if _, _, _, err := m.buildSelect(Query{Where: Op.Like("tag", "%a%")}); err != nil {
		t.Errorf("Op.Like should render on SQLite: %v", err)
	}
}

func TestBuildSelectRejectsNegativeLimits(t *testing.T) {
	m := builderModel(t)
	if _, _, _, err := m.buildSelect(Query{Limit: Int(-1)}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("negative Limit = %v, want ErrInvalidQuery", err)
	}
	if _, _, _, err := m.buildSelect(Query{Offset: Int(-1)}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("negative Offset = %v, want ErrInvalidQuery", err)
	}
}

func TestBuildSelectOffsetWithoutLimit(t *testing.T) {
	m := builderModel(t)
	sql, args, _, err := m.buildSelect(Query{Offset: Int(3)})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.HasSuffix(sql, "LIMIT ? OFFSET ?") {
		t.Errorf("sql = %q, want a stand-in LIMIT so OFFSET is legal", sql)
	}
	if args[0] != int64(unlimitedLimit) {
		t.Errorf("limit arg = %v, want the unlimited stand-in", args[0])
	}
}

func TestBuildSelectOnBrokenModel(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Broken", Attributes{"x": {}})
	if _, _, _, err := m.buildSelect(Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("buildSelect on a rejected model = %v", err)
	}
}

func TestParanoidFilterIsInjectedIntoEveryRead(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Soft", Attributes{"name": {Type: STRING(10)}}, Paranoid(true))
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}

	sql, _, _, err := m.buildSelect(Query{Attrs: []string{"id"}})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.Contains(sql, `WHERE "deleted_at" IS NULL`) {
		t.Errorf("sql = %q, want the soft-delete filter", sql)
	}

	sql, _, _, err = m.buildSelect(Query{Attrs: []string{"id"}, Where: Op.Eq("name", "x")})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.Contains(sql, `("name" = ? AND "deleted_at" IS NULL)`) {
		t.Errorf("sql = %q, want the filter ANDed onto the caller's", sql)
	}

	sql, _, _, err = m.buildSelect(Query{Attrs: []string{"id"}, Paranoid: Bool(false)})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if strings.Contains(sql, "deleted_at") {
		t.Errorf("sql = %q, want no filter when Paranoid is false", sql)
	}
}

func TestBuildAggregate(t *testing.T) {
	m := builderModel(t)
	sql, args, err := m.buildAggregate("COUNT", "", Query{Where: Op.Eq("tag", "a")})
	if err != nil {
		t.Fatalf("buildAggregate: %v", err)
	}
	if want := `SELECT COUNT(*) FROM "widgets" WHERE "tag" = ?`; sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v", args)
	}

	sql, _, err = m.buildAggregate("SUM", "price", Query{})
	if err != nil {
		t.Fatalf("buildAggregate: %v", err)
	}
	if want := `SELECT SUM("price") FROM "widgets"`; sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}

	sql, _, err = m.buildAggregate("COUNT", "id", Query{Distinct: true})
	if err != nil {
		t.Fatalf("buildAggregate: %v", err)
	}
	if want := `SELECT COUNT(DISTINCT "id") FROM "widgets"`; sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
}

func TestBuildAggregateOverGroupsUsesASubquery(t *testing.T) {
	m := builderModel(t)
	sql, _, err := m.buildAggregate("COUNT", "", Query{Group: []string{"tag"}})
	if err != nil {
		t.Fatalf("buildAggregate: %v", err)
	}
	want := `SELECT COUNT(*) FROM (SELECT 1 AS "one" FROM "widgets" GROUP BY "tag") AS "grouped"`
	if sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
}

func TestBuildAggregateRejectsBadInput(t *testing.T) {
	m := builderModel(t)
	if _, _, err := m.buildAggregate("COUNT(*) FROM x --", "", Query{}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("aggregate function name is not validated: %v", err)
	}
	if _, _, err := m.buildAggregate("COUNT", "", Query{Having: Op.Gt("price", 1.0)}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("HAVING without GROUP BY = %v, want ErrInvalidQuery", err)
	}
}

func TestBuildInsert(t *testing.T) {
	m := builderModel(t)
	sql, args, err := m.buildInsert([]string{"name", "price"}, []Values{
		{"name": "a", "price": 1.0},
		{"name": "b", "price": 2.0},
	})
	if err != nil {
		t.Fatalf("buildInsert: %v", err)
	}
	want := `INSERT INTO "widgets" ("name", "price") VALUES (?, ?), (?, ?)`
	if sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if len(args) != 4 || args[0] != "a" || args[3] != 2.0 {
		t.Errorf("args = %v", args)
	}
}

func TestBuildInsertRejectsEmptyInput(t *testing.T) {
	m := builderModel(t)
	if _, _, err := m.buildInsert(nil, []Values{{"name": "a"}}); !errors.Is(err, ErrNoValues) {
		t.Errorf("no columns = %v, want ErrNoValues", err)
	}
	if _, _, err := m.buildInsert([]string{"name"}, nil); !errors.Is(err, ErrNoValues) {
		t.Errorf("no rows = %v, want ErrNoValues", err)
	}
}

func TestBuildUpdate(t *testing.T) {
	m := builderModel(t)
	sql, args, err := m.buildUpdate(Values{"price": 3.0, "name": "z"}, Query{Where: Op.Eq("id", 1)})
	if err != nil {
		t.Fatalf("buildUpdate: %v", err)
	}
	want := `UPDATE "widgets" SET "name" = ?, "price" = ? WHERE "id" = ?`
	if sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if len(args) != 3 {
		t.Errorf("args = %v", args)
	}
	if _, _, err := m.buildUpdate(Values{}, Query{}); !errors.Is(err, ErrNoValues) {
		t.Errorf("empty SET = %v, want ErrNoValues", err)
	}
}

func TestBuildIncrement(t *testing.T) {
	m := builderModel(t)
	sql, args, err := m.buildIncrement(Values{"price": 1.5}, Values{"tag": "hot"}, Query{Where: Op.Eq("id", 2)})
	if err != nil {
		t.Fatalf("buildIncrement: %v", err)
	}
	want := `UPDATE "widgets" SET "price" = "price" + ?, "tag" = ? WHERE "id" = ?`
	if sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if len(args) != 3 {
		t.Errorf("args = %v", args)
	}
	if _, _, err := m.buildIncrement(Values{}, nil, Query{}); !errors.Is(err, ErrNoValues) {
		t.Errorf("no deltas = %v, want ErrNoValues", err)
	}
}

func TestBuildDelete(t *testing.T) {
	m := builderModel(t)
	sql, args, err := m.buildDelete(Query{Where: Op.Eq("tag", "x")})
	if err != nil {
		t.Fatalf("buildDelete: %v", err)
	}
	if want := `DELETE FROM "widgets" WHERE "tag" = ?`; sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v", args)
	}
}

// TestValuesNeverReachTheSQLText is the other half of the injection guard: a
// hostile *value* becomes an argument, and the SQL text never grows.
func TestValuesNeverReachTheSQLText(t *testing.T) {
	m := builderModel(t)
	hostile := `x'; DROP TABLE widgets; --`
	sql, args, _, err := m.buildSelect(Query{Attrs: []string{"id"}, Where: Op.Eq("name", hostile)})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if strings.Contains(sql, "DROP") {
		t.Fatalf("a value reached the SQL text: %q", sql)
	}
	if len(args) != 1 || args[0] != hostile {
		t.Errorf("args = %v, want the value bound verbatim", args)
	}
}

// builderAssocModels defines a parent with a to-many association, for the include
// cases the single-table builder does have to answer for.
func builderAssocModels(t *testing.T) (*Model, *Model) {
	t.Helper()
	db := newTestDB(t)
	parent := db.Define("Owner", Attributes{"name": {Type: STRING(20)}}, Timestamps(false))
	child := db.Define("Item", Attributes{
		"label":   {Type: STRING(20)},
		"ownerId": {Type: INTEGER()},
	}, Timestamps(false))
	if a := parent.HasMany(child); a.Err() != nil {
		t.Fatalf("HasMany: %v", a.Err())
	}
	return parent, child
}

func TestBuildSelectLeavesJoinsToTheEagerLoader(t *testing.T) {
	// Eager loading is planned by the include loader, which builds its own aliased,
	// joined statement; the single-table builder deliberately renders the same SQL
	// whether or not an Include is present, because an include that only asks for
	// extra rows does not change which parents match.
	parent, _ := builderAssocModels(t)
	with, _, _, err := parent.buildSelect(Query{Include: []Include{{Association: "items"}}})
	if err != nil {
		t.Fatalf("buildSelect with Include: %v", err)
	}
	without, _, _, err := parent.buildSelect(Query{})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if with != without {
		t.Errorf("the single-table builder acted on an Include:\n%q\n%q", with, without)
	}
}

func TestBuildSelectAppliesRequiredIncludeAsASubquery(t *testing.T) {
	// A Required to-many include cannot be a join — the children come back in a
	// query of their own — so it has to narrow the parent select itself.
	parent, _ := builderAssocModels(t)
	sql, args, _, err := parent.buildSelect(Query{Include: []Include{{Association: "items", Required: true}}})
	if err != nil {
		t.Fatalf("buildSelect: %v", err)
	}
	if !strings.Contains(sql, `"id" IN (SELECT "owner_id" FROM "items"`) {
		t.Errorf("Required include did not become a subquery filter:\n%s", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none for an unfiltered Required include", args)
	}
}

func TestBuildSelectRejectsAnUnknownIncludedAssociation(t *testing.T) {
	parent, _ := builderAssocModels(t)
	_, _, _, err := parent.buildSelect(Query{Include: []Include{{Association: "Nothing"}}})
	if !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("buildSelect with an unknown association = %v, want ErrInvalidQuery", err)
	}
}

func TestRawSubstitutionsAreRenderedInOrder(t *testing.T) {
	// The substitution vocabulary is what lets a generated subquery be spliced into
	// a statement whose bind positions are not known when the subquery is built.
	m := builderModel(t)
	b := newBuilder(m)
	c := &Clause{
		kind: clauseRaw,
		sql:  rawColumnMarker("name") + " = " + rawBindMarker + " AND " + rawColumnMarker("price") + " > " + rawBindMarker,
		args: []any{"x", int64(3)},
	}
	if err := b.clause(c); err != nil {
		t.Fatalf("clause: %v", err)
	}
	if got, want := b.sb.String(), `("name" = ? AND "price" > ?)`; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
	if len(b.args) != 2 || b.args[0] != "x" || b.args[1] != int64(3) {
		t.Errorf("args = %v, want [x 3] in marker order", b.args)
	}
}

func TestRawSubstitutionsRejectAMismatch(t *testing.T) {
	m := builderModel(t)
	b := newBuilder(m)
	tooFew := &Clause{kind: clauseRaw, sql: rawBindMarker + " " + rawBindMarker, args: []any{1}}
	if err := b.clause(tooFew); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("too few arguments = %v, want ErrInvalidQuery", err)
	}
	b = newBuilder(m)
	unknown := &Clause{kind: clauseRaw, sql: rawSubDelim + "z:nope" + rawSubDelim}
	if err := b.clause(unknown); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("unknown substitution = %v, want ErrInvalidQuery", err)
	}
}

func TestMarkerModeRefusesACallerLiteral(t *testing.T) {
	// A caller's Literal carries the caller's own placeholders, which cannot be
	// renumbered; rendering it into a generated fragment would misplace its args.
	m := builderModel(t)
	b := newBuilder(m)
	b.markers = true
	if err := b.clause(Literal("name = ?", "x")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Literal in marker mode = %v, want ErrUnsupported", err)
	}
}
