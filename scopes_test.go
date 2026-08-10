package sequelize

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// scopeFixture defines a model with a default scope hiding drafts, plus two
// named scopes, and fills it with rows covering every combination.
func scopeFixture(t *testing.T) (*Sequelize, *Model) {
	t.Helper()
	db := newTestDB(t)
	m := db.Define("Article", Attributes{
		"title":     {Type: STRING(32), AllowNull: NotNull()},
		"published": {Type: BOOLEAN()},
		"views":     {Type: INTEGER()},
	})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	m.SetDefaultScope(Query{Where: Op.Eq("published", true)})
	if err := m.AddScope("popular", Query{Where: Op.Gte("views", 100)}); err != nil {
		t.Fatalf("AddScope: %v", err)
	}
	if err := m.AddScope("newestFirst", Query{Order: []OrderTerm{Desc("views")}, Limit: Int(2)}); err != nil {
		t.Fatalf("AddScope: %v", err)
	}
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	rows := []Values{
		{"title": "hit", "published": true, "views": 500},
		{"title": "ok", "published": true, "views": 100},
		{"title": "quiet", "published": true, "views": 5},
		{"title": "draft", "published": false, "views": 900},
	}
	// The default scope must not interfere with writing the fixture, so the rows go
	// in through the unscoped view.
	if _, err := m.Unscoped().BulkCreate(ctx, rows); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	return db, m
}

func titles(rows []Values) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprint(r["title"]))
	}
	return out
}

func sortedTitles(rows []Values) string {
	got := titles(rows)
	sort.Strings(got)
	return strings.Join(got, ",")
}

// TestDefaultScopeAppliesToEveryRead is the baseline: nothing has to be asked for.
func TestDefaultScopeAppliesToEveryRead(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()

	rows, err := m.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "hit,ok,quiet" {
		t.Errorf("FindAll = %v, want the published rows", got)
	}
	if count, err := m.Count(ctx, Query{}); err != nil || count != 3 {
		t.Errorf("Count = %d, %v; want 3", count, err)
	}
	if sum, err := m.Sum(ctx, "views", Query{}); err != nil || sum != int64(605) {
		t.Errorf("Sum = %v, %v; want 605", sum, err)
	}
	if max, err := m.Max(ctx, "views", Query{}); err != nil || max != int64(500) {
		t.Errorf("Max = %v, %v; want 500 rather than the draft's 900", max, err)
	}
	if _, err := m.FindOne(ctx, Query{Where: Op.Eq("title", "draft")}); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("FindOne for an out-of-scope row = %v, want ErrRecordNotFound", err)
	}
}

// TestDefaultScopeComposesWithAnExplicitWhere is the composition case: a scope
// that a caller's Where could overwrite would be no protection at all.
func TestDefaultScopeComposesWithAnExplicitWhere(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()

	rows, err := m.FindAll(ctx, Query{Where: Op.Gte("views", 100)})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "hit,ok" {
		t.Errorf("FindAll = %v, want the published rows over 100 views", got)
	}

	// An explicit filter naming the very attribute the scope filters on is ANDed,
	// not substituted: asking for drafts inside the published scope finds nothing.
	rows, err = m.FindAll(ctx, Query{Where: Op.Eq("published", false)})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("an explicit Where escaped the default scope: %v", titles(rows))
	}

	// An OR of the caller's own alternatives stays intact inside the scope.
	rows, err = m.FindAll(ctx, Query{Where: Or(Op.Eq("title", "hit"), Op.Eq("title", "draft"))})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "hit" {
		t.Errorf("FindAll = %v, want only the published alternative", got)
	}
}

// TestDefaultScopeComposesWithParanoidFiltering checks the two filters stack
// rather than one replacing the other.
func TestDefaultScopeComposesWithParanoidFiltering(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	m := db.Define("Task", Attributes{
		"title": {Type: STRING(32), AllowNull: NotNull()},
		"done":  {Type: BOOLEAN()},
	}, Paranoid(true))
	m.SetDefaultScope(Query{Where: Op.Eq("done", false)})
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.Unscoped().BulkCreate(ctx, []Values{
		{"title": "open", "done": false},
		{"title": "closed", "done": true},
		{"title": "gone", "done": false},
	}); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	if _, err := m.Unscoped().Destroy(ctx, Query{Where: Op.Eq("title", "gone")}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	rows, err := m.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "open" {
		t.Errorf("FindAll = %v, want only the row that is both in scope and not deleted", got)
	}

	// Asking to see deleted rows keeps the default scope.
	rows, err = m.FindAll(ctx, Query{Paranoid: Bool(false)})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "gone,open" {
		t.Errorf("FindAll(Paranoid false) = %v, want the undone rows including the deleted one", got)
	}

	// Unscoped keeps the soft-delete filter: it is not a scope.
	rows, err = m.Unscoped().FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "closed,open" {
		t.Errorf("Unscoped().FindAll = %v, want every live row", got)
	}

	// Both dropped at once.
	rows, err = m.Unscoped().FindAll(ctx, Query{Paranoid: Bool(false)})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "closed,gone,open" {
		t.Errorf("Unscoped().FindAll(Paranoid false) = %v, want every row", got)
	}
}

func TestNamedScopesCompose(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()

	rows, err := m.Scope("popular").FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "hit,ok" {
		t.Errorf("Scope(popular) = %v, want the published rows over 100 views", got)
	}

	// A named scope stacks on the default scope and on an explicit Where.
	rows, err = m.Scope("popular").FindAll(ctx, Query{Where: Op.Eq("title", "hit")})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "hit" {
		t.Errorf("Scope(popular) with a Where = %v", got)
	}

	// Two named scopes at once, in either spelling.
	both := m.Scope("popular", "newestFirst")
	rows, err = both.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := strings.Join(titles(rows), ","); got != "hit,ok" {
		t.Errorf("two scopes = %v, want the ordered popular rows", got)
	}
	if got := m.Scope("popular").Scope("newestFirst").ActiveScopes(); strings.Join(got, ",") != "popular,newestFirst" {
		t.Errorf("chained Scope() = %v", got)
	}
	if count, err := m.Scope("popular").Count(ctx, Query{}); err != nil || count != 2 {
		t.Errorf("Count under a scope = %d, %v; want 2", count, err)
	}
}

func TestScopeSuppliesOrderAndLimitOnlyWhenUnset(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()

	rows, err := m.Scope("newestFirst").FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := strings.Join(titles(rows), ","); got != "hit,ok" {
		t.Errorf("Scope(newestFirst) = %v, want the two most-viewed published rows", got)
	}

	// The caller's own Order and Limit win.
	rows, err = m.Scope("newestFirst").FindAll(ctx, Query{
		Order: []OrderTerm{Asc("views")},
		Limit: Int(1),
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := strings.Join(titles(rows), ","); got != "quiet" {
		t.Errorf("explicit Order/Limit = %v, want the scope's overridden", got)
	}
}

func TestUnscopedDropsTheDefaultScope(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()

	rows, err := m.Unscoped().FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "draft,hit,ok,quiet" {
		t.Errorf("Unscoped() = %v, want every row", got)
	}
	if !m.Unscoped().IsUnscoped() {
		t.Error("IsUnscoped() did not report the unscoped view")
	}
	if m.IsUnscoped() {
		t.Error("Unscoped() mutated the model it was called on")
	}
	if got := m.Scope("popular").Unscoped().ActiveScopes(); len(got) != 0 {
		t.Errorf("Unscoped() kept the active scopes: %v", got)
	}
	// The original model is untouched.
	rows, err = m.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("the base model saw %d rows after Unscoped(), want 3", len(rows))
	}
}

// TestScopesApplyToWrites checks a scope narrows Update, Destroy and Increment
// too, as upstream's scoped model does.
func TestScopesApplyToWrites(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()

	n, err := m.Update(ctx, Values{"views": 1}, Query{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n != 3 {
		t.Errorf("Update touched %d rows, want the 3 in scope", n)
	}
	draft, err := m.Unscoped().FindOne(ctx, Query{Where: Op.Eq("title", "draft")})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if draft["views"] != int64(900) {
		t.Errorf("the out-of-scope row was updated: %v", draft)
	}

	if n, err = m.Increment(ctx, Values{"views": 1}, Query{}); err != nil || n != 3 {
		t.Errorf("Increment = %d, %v; want 3", n, err)
	}
	if n, err = m.Destroy(ctx, Query{}); err != nil || n != 3 {
		t.Errorf("Destroy = %d, %v; want 3", n, err)
	}
	left, err := m.Unscoped().Count(ctx, Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if left != 1 {
		t.Errorf("%d rows left, want the out-of-scope draft", left)
	}
}

func TestScopeIsAppliedExactlyOnce(t *testing.T) {
	counter := &queryCounter{}
	db := newTestDB(t, WithLogger(counter.log))
	ctx := context.Background()
	m := db.Define("Article", Attributes{
		"title":     {Type: STRING(32)},
		"published": {Type: BOOLEAN()},
	})
	m.SetDefaultScope(Query{Where: Op.Eq("published", true)})
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	counter.reset()
	if _, err := m.FindAll(ctx, Query{}); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	stmts := counter.selects()
	if len(stmts) != 1 {
		t.Fatalf("FindAll ran %d SELECTs", len(stmts))
	}
	where := stmts[0][strings.Index(stmts[0], "WHERE"):]
	if got := strings.Count(where, "published"); got != 1 {
		t.Errorf("the scope was merged %d times into %q", got, where)
	}
}

func TestScopeWithIncludeIsEagerLoaded(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()
	s.user.SetDefaultScope(Query{Include: []Include{{Association: "posts"}}})

	s.counter.reset()
	rows, err := s.user.FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	s.counter.wantSelects(t, 2, "a default scope carrying an include")
	if got := childRows(t, rowByAttr(t, rows, "name", "alice"), "posts"); len(got) != 2 {
		t.Errorf(`scoped alice["posts"] = %v, want 2`, got)
	}

	// A caller's own includes are added to the scope's rather than replacing them.
	s.counter.reset()
	rows, err = s.user.FindAll(ctx, Query{Include: []Include{{Association: "profile"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	s.counter.wantSelects(t, 2, "a scope include plus a caller include")
	alice := rowByAttr(t, rows, "name", "alice")
	if _, ok := alice["profile"].(Values); !ok {
		t.Errorf(`alice["profile"] = %#v`, alice["profile"])
	}
	if got := childRows(t, alice, "posts"); len(got) != 2 {
		t.Errorf(`alice["posts"] = %v`, got)
	}
}

func TestScopeFuncIsEvaluatedPerStatement(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()
	threshold := 100
	m.SetDefaultScopeFunc(func() Query { return Query{Where: Op.Gte("views", threshold)} })

	if count, err := m.Count(ctx, Query{}); err != nil || count != 3 {
		t.Errorf("Count = %d, %v; want the 3 rows over 100 views", count, err)
	}
	threshold = 600
	if count, err := m.Count(ctx, Query{}); err != nil || count != 1 {
		t.Errorf("Count after moving the threshold = %d, %v; want 1", count, err)
	}

	if err := m.AddScopeFunc("dynamic", func() Query { return Query{Where: Op.Eq("title", "draft")} }); err != nil {
		t.Fatalf("AddScopeFunc: %v", err)
	}
	rows, err := m.Scope("dynamic").FindAll(ctx, Query{})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := sortedTitles(rows); got != "draft" {
		t.Errorf("Scope(dynamic) = %v", got)
	}

	// A nil default scope removes it.
	m.SetDefaultScopeFunc(nil)
	if count, err := m.Count(ctx, Query{}); err != nil || count != 4 {
		t.Errorf("Count with no default scope = %d, %v; want 4", count, err)
	}
}

func TestScopeRejectsUnknownAndDuplicateNames(t *testing.T) {
	_, m := scopeFixture(t)
	ctx := context.Background()

	view := m.Scope("nope")
	if !errors.Is(view.Err(), ErrInvalidQuery) {
		t.Errorf("Scope(nope).Err() = %v, want ErrInvalidQuery", view.Err())
	}
	if _, err := view.FindAll(ctx, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("FindAll on an unknown scope = %v", err)
	}
	if _, err := view.Count(ctx, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Count on an unknown scope = %v", err)
	}
	if m.Err() != nil {
		t.Errorf("the base model was damaged: %v", m.Err())
	}

	if err := m.AddScope("popular", Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("duplicate AddScope = %v, want ErrInvalidQuery", err)
	}
	if err := m.AddScope("", Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("empty AddScope = %v, want ErrInvalidQuery", err)
	}
	if err := m.AddScopeFunc("nilFn", nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("nil AddScopeFunc = %v, want ErrInvalidQuery", err)
	}
	if got := m.ScopeNames(); strings.Join(got, ",") != "newestFirst,popular" {
		t.Errorf("ScopeNames = %v", got)
	}
}

func TestScopesOnAModelWithNoDefinitions(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Plain", Attributes{"a": {Type: INTEGER()}})
	if got := m.ScopeNames(); len(got) != 0 {
		t.Errorf("ScopeNames = %v, want none", got)
	}
	if got := m.ActiveScopes(); len(got) != 0 {
		t.Errorf("ActiveScopes = %v, want none", got)
	}
	// A model built outside Define has no scope table at all.
	bare := &Model{name: "Bare"}
	if got := bare.ScopeNames(); got != nil {
		t.Errorf("ScopeNames on a bare model = %v", got)
	}
	if got := bare.Scope("x"); !errors.Is(got.Err(), ErrInvalidQuery) {
		t.Errorf("Scope on a bare model = %v", got.Err())
	}
	if q := bare.scoped(Query{}); !q.scoped {
		t.Error("scoped() did not mark the query")
	}
}

func TestMergeScopeTakesUnsetFieldsOnly(t *testing.T) {
	scope := Query{
		Attrs:    []string{"a"},
		Group:    []string{"a"},
		Having:   Op.Gt("a", 1),
		Offset:   Int(5),
		Paranoid: Bool(false),
		Distinct: true,
	}
	empty := mergeScope(scope, Query{})
	if len(empty.Attrs) != 1 || len(empty.Group) != 1 || empty.Having == nil ||
		empty.Offset == nil || empty.Paranoid == nil || !empty.Distinct {
		t.Errorf("mergeScope into an empty query dropped fields: %+v", empty)
	}

	caller := Query{
		Attrs:    []string{"b"},
		Group:    []string{"b"},
		Having:   Op.Gt("b", 1),
		Offset:   Int(9),
		Paranoid: Bool(true),
	}
	merged := mergeScope(scope, caller)
	if merged.Attrs[0] != "b" || merged.Group[0] != "b" || *merged.Offset != 9 || !*merged.Paranoid {
		t.Errorf("mergeScope overwrote the caller's own fields: %+v", merged)
	}
}

// ExampleModel_Scope composes a named scope over a default one.
func ExampleModel_Scope() {
	ctx := context.Background()
	db, _ := New("sqlite", "file:example_scope?mode=memory&cache=shared")
	defer db.Close()

	article := db.Define("Article", Attributes{
		"title":     {Type: STRING(32)},
		"published": {Type: BOOLEAN()},
		"views":     {Type: INTEGER()},
	})
	article.SetDefaultScope(Query{Where: Op.Eq("published", true)})
	_ = article.AddScope("popular", Query{Where: Op.Gte("views", 100)})
	_ = db.Sync(ctx)
	_, _ = article.Unscoped().BulkCreate(ctx, []Values{
		{"title": "hit", "published": true, "views": 500},
		{"title": "quiet", "published": true, "views": 1},
		{"title": "draft", "published": false, "views": 900},
	})

	rows, _ := article.Scope("popular").FindAll(ctx, Query{})
	fmt.Println(titles(rows))
	// Output: [hit]
}
