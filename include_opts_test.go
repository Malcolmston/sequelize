package sequelize

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// eagerCounter records every statement the connection runs. The include options
// are only worth having if they keep the batching guarantee, and a count of
// statements is the only thing that proves they do: a loader that returns exactly
// the right rows with one query per parent is still the bug this package exists to
// avoid.
type eagerCounter struct {
	mu         sync.Mutex
	statements []string
}

func (c *eagerCounter) log(_ context.Context, statement string, _ []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = append(c.statements, statement)
}

func (c *eagerCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = nil
}

func (c *eagerCounter) selects() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.statements {
		if strings.HasPrefix(s, "SELECT") {
			out = append(out, s)
		}
	}
	return out
}

func (c *eagerCounter) want(t *testing.T, n int, what string) {
	t.Helper()
	got := c.selects()
	if len(got) != n {
		t.Errorf("%s ran %d SELECTs, want %d:\n%s", what, len(got), n, strings.Join(got, "\n"))
	}
}

// eagerSchema is the fixture: User has many Post, Post has many Comment, Post
// belongs to many Tag through PostTag. Timestamps are off so the generated SQL in
// these tests is only what the test asked for.
type eagerSchema struct {
	db      *Sequelize
	counter *eagerCounter
	user    *Model
	post    *Model
	comment *Model
	tag     *Model
	postTag *Model
	ids     map[string]int64
}

func newEagerSchema(t *testing.T) *eagerSchema {
	t.Helper()
	counter := &eagerCounter{}
	db := newTestDB(t, WithLogger(counter.log))
	s := &eagerSchema{db: db, counter: counter, ids: map[string]int64{}}

	s.user = db.Define("User", Attributes{
		"name": {Type: STRING(32), AllowNull: NotNull()},
	}, Timestamps(false))
	s.post = db.Define("Post", Attributes{
		"title":  {Type: STRING(32), AllowNull: NotNull()},
		"userId": {Type: INTEGER()},
	}, Timestamps(false))
	s.comment = db.Define("Comment", Attributes{
		"body":   {Type: STRING(32), AllowNull: NotNull()},
		"postId": {Type: INTEGER()},
	}, Timestamps(false))
	s.tag = db.Define("Tag", Attributes{
		"label": {Type: STRING(32), AllowNull: NotNull()},
	}, Timestamps(false))
	s.postTag = db.Define("PostTag", Attributes{
		"postId": {Type: INTEGER()},
		"tagId":  {Type: INTEGER()},
	}, Timestamps(false))
	for _, m := range []*Model{s.user, s.post, s.comment, s.tag, s.postTag} {
		if err := m.Err(); err != nil {
			t.Fatalf("Define %s: %v", m.Name(), err)
		}
	}
	for _, a := range []*Association{
		s.user.HasMany(s.post),
		s.post.BelongsTo(s.user),
		s.post.HasMany(s.comment),
		s.comment.BelongsTo(s.post),
		s.post.BelongsToMany(s.tag, s.postTag),
	} {
		if err := a.Err(); err != nil {
			t.Fatalf("association %v: %v", a, err)
		}
	}

	ctx := context.Background()
	if err := s.db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	create := func(m *Model, key string, vals Values) {
		t.Helper()
		row, err := m.Create(ctx, vals)
		if err != nil {
			t.Fatalf("Create %s %s: %v", m.Name(), key, err)
		}
		id, ok := row["id"].(int64)
		if !ok {
			t.Fatalf("Create %s %s returned id %T", m.Name(), key, row["id"])
		}
		s.ids[key] = id
	}
	create(s.user, "alice", Values{"name": "alice"})
	create(s.user, "bob", Values{"name": "bob"})
	create(s.user, "carol", Values{"name": "carol"}) // no posts at all
	create(s.post, "a1", Values{"title": "a1", "userId": s.ids["alice"]})
	create(s.post, "a2", Values{"title": "a2", "userId": s.ids["alice"]})
	create(s.post, "a3", Values{"title": "a3", "userId": s.ids["alice"]})
	create(s.post, "b1", Values{"title": "b1", "userId": s.ids["bob"]})
	create(s.comment, "c1", Values{"body": "c1", "postId": s.ids["a1"]})
	create(s.comment, "c2", Values{"body": "c2", "postId": s.ids["a1"]})
	create(s.tag, "t1", Values{"label": "t1"})
	create(s.tag, "t2", Values{"label": "t2"})
	create(s.tag, "t3", Values{"label": "t3"})
	create(s.postTag, "a1t1", Values{"postId": s.ids["a1"], "tagId": s.ids["t1"]})
	create(s.postTag, "a1t2", Values{"postId": s.ids["a1"], "tagId": s.ids["t2"]})
	create(s.postTag, "a1t3", Values{"postId": s.ids["a1"], "tagId": s.ids["t3"]})
	create(s.postTag, "b1t1", Values{"postId": s.ids["b1"], "tagId": s.ids["t1"]})
	s.counter.reset()
	return s
}

// addUsers creates n extra parents, so a test can prove the statement count does
// not follow the number of parents.
func (s *eagerSchema) addUsers(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("extra%d", i)
		row, err := s.user.Create(ctx, Values{"name": name})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		id := row["id"].(int64)
		if _, err := s.post.Create(ctx, Values{"title": name + "-p1", "userId": id}); err != nil {
			t.Fatalf("Create post for %s: %v", name, err)
		}
		if _, err := s.post.Create(ctx, Values{"title": name + "-p2", "userId": id}); err != nil {
			t.Fatalf("Create post for %s: %v", name, err)
		}
	}
	s.counter.reset()
}

// eagerNames lists an attribute of every row, for compact assertions.
func eagerNames(rows []Values, attribute string) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmt.Sprint(row[attribute]))
	}
	return out
}

// eagerRow finds the single row whose attribute equals want.
func eagerRow(t *testing.T, rows []Values, attribute string, want any) Values {
	t.Helper()
	for _, row := range rows {
		if fmt.Sprint(row[attribute]) == fmt.Sprint(want) {
			return row
		}
	}
	t.Fatalf("no row with %s = %v in %v", attribute, want, eagerNames(rows, attribute))
	return nil
}

// eagerChildren reads an eager-loaded to-many slice.
func eagerChildren(t *testing.T, row Values, key string) []Values {
	t.Helper()
	value, present := row[key]
	if !present {
		t.Fatalf("row carries no %q: %v", key, row)
	}
	children, ok := value.([]Values)
	if !ok {
		t.Fatalf("row[%q] is %T, want []Values", key, value)
	}
	return children
}

// --------------------------------------------------------------------------
// Include.Required for the batched to-many kinds.
// --------------------------------------------------------------------------

func TestIncludeRequiredHasManyDropsChildlessParents(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()

	all, err := s.user.FindAll(ctx, Query{Include: []Include{{Association: "posts"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("without Required got %d users, want 3", len(all))
	}
	s.counter.reset()

	required, err := s.user.FindAll(ctx, Query{Include: []Include{{Association: "posts", Required: true}}})
	if err != nil {
		t.Fatalf("FindAll(Required): %v", err)
	}
	if got := eagerNames(required, "name"); len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("Required posts returned %v, want [alice bob]", got)
	}
	// The subquery filter must not cost a query of its own.
	s.counter.want(t, 2, "a Required HasMany include")
}

func TestIncludeRequiredHasManyRespectsTheIncludeWhere(t *testing.T) {
	s := newEagerSchema(t)
	rows, err := s.user.FindAll(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Where:       Op.Like("title", "b%"),
		Required:    true,
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := eagerNames(rows, "name"); len(got) != 1 || got[0] != "bob" {
		t.Errorf("Required + Where returned %v, want [bob]", got)
	}
	s.counter.want(t, 2, "a filtered Required HasMany include")
}

func TestIncludeRequiredHasManyStaysBatchedAsParentsGrow(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	q := Query{Include: []Include{{Association: "posts", Required: true}}}

	rows, err := s.user.FindAll(ctx, q)
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d users, want 2", len(rows))
	}
	s.counter.want(t, 2, "a Required include over 2 matching users")

	s.addUsers(t, 10)
	rows, err = s.user.FindAll(ctx, q)
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 12 {
		t.Fatalf("got %d users, want 12", len(rows))
	}
	s.counter.want(t, 2, "a Required include over 12 matching users")
}

func TestIncludeRequiredBelongsToManyDropsUnlinkedParents(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	if _, err := s.post.Create(ctx, Values{"title": "untagged", "userId": s.ids["alice"]}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.counter.reset()

	rows, err := s.post.FindAll(ctx, Query{
		Order:   []OrderTerm{Asc("title")},
		Include: []Include{{Association: "tags", Required: true}},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := eagerNames(rows, "title"); len(got) != 2 || got[0] != "a1" || got[1] != "b1" {
		t.Errorf("Required tags returned %v, want [a1 b1]", got)
	}
	// The parents, the join table, then the tags: the subquery is free.
	s.counter.want(t, 3, "a Required BelongsToMany include")
}

func TestIncludeRequiredPropagatesUpFromANestedInclude(t *testing.T) {
	s := newEagerSchema(t)
	// bob's only post has no comments, so bob has no post that qualifies and is
	// dropped along with it — which is what upstream's required does.
	rows, err := s.user.FindAll(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Include:     []Include{{Association: "comments", Required: true}},
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := eagerNames(rows, "name"); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("nested Required returned %v, want [alice]", got)
	}
	posts := eagerChildren(t, rows[0], "posts")
	if got := eagerNames(posts, "title"); len(got) != 1 || got[0] != "a1" {
		t.Errorf("alice's posts = %v, want only the commented a1", got)
	}
	s.counter.want(t, 3, "a two-level include with a nested Required")
}

func TestIncludeRequiredUnderAJoinedIncludeFiltersTheRoot(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	// b1 gets a comment of its own, so that the Required filter below the join has
	// something to exclude.
	if _, err := s.comment.Create(ctx, Values{"body": "c3", "postId": s.ids["b1"]}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.counter.reset()

	// Comment belongs to Post (a join), and only a post carrying a "c1" comment
	// qualifies: the restriction is two associations away from the rows being
	// selected and still has to narrow them.
	rows, err := s.comment.FindAll(ctx, Query{
		Order: []OrderTerm{Asc("body")},
		Include: []Include{{
			Association: "post",
			Include: []Include{{
				Association: "comments",
				Where:       Op.Eq("body", "c1"),
				Required:    true,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := eagerNames(rows, "body"); strings.Join(got, ",") != "c1,c2" {
		t.Errorf("got %v, want the comments of the only qualifying post", got)
	}
	// The joined comments, then the nested to-many batch: the lifted subquery costs
	// no statement of its own.
	s.counter.want(t, 2, "a Required include below a joined include")
}

func TestCountWithIncludeAndDistinctStillCountsParents(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()

	total, err := s.user.Count(ctx, Query{Include: []Include{{Association: "posts"}}, Distinct: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 3 {
		t.Errorf("Count(Include, Distinct) = %d, want the 3 distinct users", total)
	}
	withChildren, err := s.user.Count(ctx, Query{
		Include:  []Include{{Association: "posts", Required: true}},
		Distinct: true,
	})
	if err != nil {
		t.Fatalf("Count(Required): %v", err)
	}
	if withChildren != 2 {
		t.Errorf("Count(Required, Distinct) = %d, want 2", withChildren)
	}
}

func TestIncludeRequiredIsIgnoredForAnEmptyIncludeTree(t *testing.T) {
	// The subquery machinery must not fire when there is nothing to include, or
	// every plain select would pay for it.
	m := builderModel(t)
	if got := m.includeRequiredClause(Query{}); got != nil {
		t.Errorf("includeRequiredClause(no includes) = %v, want nil", got)
	}
}

// --------------------------------------------------------------------------
// Include.Order, Include.Limit and Include.Offset.
// --------------------------------------------------------------------------

func TestIncludeOrderSortsChildren(t *testing.T) {
	s := newEagerSchema(t)
	rows, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Desc("title")},
	}}})
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	alice := eagerRow(t, rows, "name", "alice")
	if got := eagerNames(eagerChildren(t, alice, "posts"), "title"); strings.Join(got, ",") != "a3,a2,a1" {
		t.Errorf("alice's posts = %v, want [a3 a2 a1]", got)
	}
	s.counter.want(t, 2, "an ordered HasMany include")
}

func TestIncludeLimitIsPerParentNotPerBatch(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	q := Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Desc("title")},
		Limit:       Int(1),
	}}}

	rows, err := s.user.FindAllEager(ctx, q)
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	// A plain LIMIT 1 on the batched query would give alice one post and bob none.
	alice := eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts")
	if got := eagerNames(alice, "title"); len(got) != 1 || got[0] != "a3" {
		t.Errorf("alice's posts = %v, want [a3]", got)
	}
	bob := eagerChildren(t, eagerRow(t, rows, "name", "bob"), "posts")
	if got := eagerNames(bob, "title"); len(got) != 1 || got[0] != "b1" {
		t.Errorf("bob's posts = %v, want [b1]", got)
	}
	if got := eagerChildren(t, eagerRow(t, rows, "name", "carol"), "posts"); len(got) != 0 {
		t.Errorf("carol's posts = %v, want empty", got)
	}
	s.counter.want(t, 2, "a per-parent limited HasMany include over 3 users")

	// The window function is what keeps this at two statements; a per-parent query
	// would grow with the parents.
	s.addUsers(t, 10)
	rows, err = s.user.FindAllEager(ctx, q)
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	if len(rows) != 13 {
		t.Fatalf("got %d users, want 13", len(rows))
	}
	s.counter.want(t, 2, "a per-parent limited HasMany include over 13 users")
	for _, row := range rows {
		if got := eagerChildren(t, row, "posts"); len(got) > 1 {
			t.Errorf("%v kept %d posts, want at most 1", row["name"], len(got))
		}
	}
}

func TestIncludeOffsetSkipsPerParent(t *testing.T) {
	s := newEagerSchema(t)
	rows, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Asc("title")},
		Offset:      Int(1),
		Limit:       Int(1),
	}}})
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	if got := eagerNames(eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts"), "title"); len(got) != 1 || got[0] != "a2" {
		t.Errorf("alice's posts = %v, want [a2]", got)
	}
	// bob has one post, so an offset of one leaves him none.
	if got := eagerChildren(t, eagerRow(t, rows, "name", "bob"), "posts"); len(got) != 0 {
		t.Errorf("bob's posts = %v, want empty", got)
	}
	s.counter.want(t, 2, "a per-parent offset HasMany include")
}

func TestIncludeLimitZeroKeepsNothing(t *testing.T) {
	s := newEagerSchema(t)
	rows, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Limit:       Int(0),
	}}})
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	for _, row := range rows {
		if got := eagerChildren(t, row, "posts"); len(got) != 0 {
			t.Errorf("%v kept %v with Limit 0", row["name"], got)
		}
	}
	s.counter.want(t, 2, "a HasMany include limited to nothing")
}

func TestIncludeLimitWithANestedIncludeStaysBatched(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	// A nested include rules the window function out, because the children have to
	// come back through the loader that can attach their own children. The fallback
	// trims one batched result set rather than querying per parent.
	q := Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Asc("title")},
		Limit:       Int(1),
		Include:     []Include{{Association: "comments"}},
	}}}
	rows, err := s.user.FindAllEager(ctx, q)
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	alice := eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts")
	if got := eagerNames(alice, "title"); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("alice's posts = %v, want [a1]", got)
	}
	if got := eagerNames(eagerChildren(t, alice[0], "comments"), "body"); len(got) != 2 {
		t.Errorf("a1's comments = %v, want two", got)
	}
	s.counter.want(t, 3, "a limited include with a nested include over 3 users")

	s.addUsers(t, 10)
	if _, err := s.user.FindAllEager(ctx, q); err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	s.counter.want(t, 3, "a limited include with a nested include over 13 users")
}

func TestIncludeLimitOnBelongsToManyIsPerParent(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	q := Query{
		Order: []OrderTerm{Asc("title")},
		Include: []Include{{
			Association: "tags",
			Order:       []OrderTerm{Desc("label")},
			Limit:       Int(2),
		}},
	}
	rows, err := s.post.FindAllEager(ctx, q)
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	a1 := eagerChildren(t, eagerRow(t, rows, "title", "a1"), "tags")
	if got := eagerNames(a1, "label"); strings.Join(got, ",") != "t3,t2" {
		t.Errorf("a1's tags = %v, want [t3 t2]", got)
	}
	b1 := eagerChildren(t, eagerRow(t, rows, "title", "b1"), "tags")
	if got := eagerNames(b1, "label"); strings.Join(got, ",") != "t1" {
		t.Errorf("b1's tags = %v, want [t1]", got)
	}
	// The posts, the join table, the tags. A per-parent limit adds nothing.
	s.counter.want(t, 3, "a per-parent limited BelongsToMany include")
}

func TestIncludeAttrsRestrictChildColumns(t *testing.T) {
	s := newEagerSchema(t)
	rows, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Attrs:       []string{"title"},
		Order:       []OrderTerm{Asc("title")},
		Limit:       Int(2),
	}}})
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	posts := eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts")
	if len(posts) != 2 {
		t.Fatalf("alice kept %d posts, want 2", len(posts))
	}
	if _, present := posts[0]["title"]; !present {
		t.Errorf("the requested attribute is missing: %v", posts[0])
	}
	// The stitching key is selected regardless of the restriction, and nothing else
	// is.
	if _, present := posts[0]["userId"]; !present {
		t.Errorf("the foreign key was dropped, so the rows could not be grouped: %v", posts[0])
	}
	if _, present := posts[0]["id"]; present {
		t.Errorf("Attrs did not restrict the projection: %v", posts[0])
	}
}

func TestIncludeOrderAndRequiredTogether(t *testing.T) {
	s := newEagerSchema(t)
	rows, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Required:    true,
		Order:       []OrderTerm{Desc("title")},
		Limit:       Int(2),
	}}})
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	if got := eagerNames(rows, "name"); len(got) != 2 {
		t.Fatalf("Required + Limit returned %v, want two users", got)
	}
	if got := eagerNames(eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts"), "title"); strings.Join(got, ",") != "a3,a2" {
		t.Errorf("alice's posts = %v, want [a3 a2]", got)
	}
	s.counter.want(t, 2, "a Required, ordered and limited include")
}

func TestFindAllEagerMatchesFindAllWithoutChildOptions(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	q := Query{Include: []Include{{Association: "posts", Include: []Include{{Association: "comments"}}}}}
	viaFindAll, err := s.user.FindAll(ctx, q)
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	s.counter.reset()
	viaEager, err := s.user.FindAllEager(ctx, q)
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	if fmt.Sprint(viaEager) != fmt.Sprint(viaFindAll) {
		t.Errorf("FindAllEager diverged from FindAll:\n%v\n%v", viaEager, viaFindAll)
	}
	s.counter.want(t, 3, "FindAllEager with no child options")
}

func TestFindOneEagerAppliesIncludeOptions(t *testing.T) {
	s := newEagerSchema(t)
	row, err := s.user.FindOneEager(context.Background(), Query{
		Where: Op.Eq("name", "alice"),
		Include: []Include{{
			Association: "posts",
			Order:       []OrderTerm{Desc("title")},
			Limit:       Int(1),
		}},
	})
	if err != nil {
		t.Fatalf("FindOneEager: %v", err)
	}
	if got := eagerNames(eagerChildren(t, row, "posts"), "title"); len(got) != 1 || got[0] != "a3" {
		t.Errorf("alice's posts = %v, want [a3]", got)
	}
}

func TestFindOneEagerReportsAMiss(t *testing.T) {
	s := newEagerSchema(t)
	_, err := s.user.FindOneEager(context.Background(), Query{
		Where:   Op.Eq("name", "nobody"),
		Include: []Include{{Association: "posts", Limit: Int(1)}},
	})
	if !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("FindOneEager(miss) = %v, want ErrRecordNotFound", err)
	}
}

func TestIncludeEagerWorksInsideATransaction(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	err := s.db.Transaction(ctx, func(tx *Tx) error {
		rows, err := s.user.FindAllEager(ctx, Query{
			Tx: tx,
			Include: []Include{{
				Association: "posts",
				Order:       []OrderTerm{Asc("title")},
				Limit:       Int(1),
			}},
		})
		if err != nil {
			return err
		}
		got := eagerNames(eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts"), "title")
		if len(got) != 1 || got[0] != "a1" {
			t.Errorf("alice's posts = %v, want [a1]", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
}

// --------------------------------------------------------------------------
// The options that are refused rather than ignored.
// --------------------------------------------------------------------------

func TestIncludeLimitOnFindAllIsRefusedNotIgnored(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	for _, inc := range []Include{
		{Association: "posts", Limit: Int(1)},
		{Association: "posts", Offset: Int(1)},
		{Association: "posts", Order: []OrderTerm{Asc("title")}},
	} {
		if _, err := s.user.FindAll(ctx, Query{Include: []Include{inc}}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("FindAll with %+v = %v, want ErrUnsupported", inc, err)
		}
	}
	// A nested one is caught too.
	_, err := s.user.FindAll(ctx, Query{Include: []Include{{
		Association: "posts",
		Include:     []Include{{Association: "comments", Limit: Int(1)}},
	}}})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("FindAll with a nested Include.Limit = %v, want ErrUnsupported", err)
	}
}

func TestIncludeChildOptionsOnAToOneAssociationAreRefused(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	q := Query{Include: []Include{{Association: "user", Limit: Int(1)}}}
	if _, err := s.post.FindAllEager(ctx, q); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FindAllEager = %v, want ErrUnsupported", err)
	}
	if _, err := s.post.FindAll(ctx, q); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FindAll = %v, want ErrUnsupported", err)
	}
}

func TestIncludeOrderRejectsAnUnknownAttribute(t *testing.T) {
	s := newEagerSchema(t)
	_, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Asc("nope")},
	}}})
	if !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("Include.Order on an unknown attribute = %v, want ErrUnknownAttribute", err)
	}
}

func TestIncludeRejectsNegativeLimitAndOffset(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	if _, err := s.user.FindAllEager(ctx, Query{Include: []Include{{Association: "posts", Limit: Int(-1)}}}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("negative Include.Limit = %v, want ErrInvalidQuery", err)
	}
	if _, err := s.user.FindAllEager(ctx, Query{Include: []Include{{Association: "posts", Offset: Int(-2)}}}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("negative Include.Offset = %v, want ErrInvalidQuery", err)
	}
}

func TestFindAllEagerRejectsAnUnknownAssociation(t *testing.T) {
	s := newEagerSchema(t)
	_, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{Association: "nope", Limit: Int(1)}}})
	if !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("FindAllEager with an unknown association = %v, want ErrInvalidQuery", err)
	}
}

// --------------------------------------------------------------------------
// The generated SQL and the dialect capability.
// --------------------------------------------------------------------------

func TestBuildPartitionedSelectRanksWithinEachParent(t *testing.T) {
	s := newEagerSchema(t)
	cols := []string{"id", "title", "userId"}
	statement, args, err := s.post.buildPartitionedSelect(cols, "userId",
		Query{Where: Op.In("userId", []any{int64(1), int64(2)})},
		Include{Order: []OrderTerm{Desc("title")}, Limit: Int(2), Offset: Int(1)})
	if err != nil {
		t.Fatalf("buildPartitionedSelect: %v", err)
	}
	for _, want := range []string{
		`ROW_NUMBER() OVER (PARTITION BY "user_id" ORDER BY "title" DESC)`,
		`AS "sequelize_row_number"`,
		`) AS "sequelize_partitioned" WHERE`,
		`"sequelize_partitioned"."sequelize_row_number" > ?`,
		`"sequelize_partitioned"."sequelize_row_number" <= ?`,
	} {
		if !strings.Contains(statement, want) {
			t.Errorf("statement is missing %q:\n%s", want, statement)
		}
	}
	// Two keys, then the window bounds, all bound rather than interpolated.
	if len(args) != 4 {
		t.Fatalf("args = %v, want the two keys and the two bounds", args)
	}
	if args[2] != int64(1) || args[3] != int64(3) {
		t.Errorf("window bounds = %v, %v, want 1 and 3 for offset 1 limit 2", args[2], args[3])
	}
	if strings.Contains(statement, rawSubDelim) {
		t.Error("a substitution marker survived into the statement")
	}
}

func TestBuildPartitionedSelectDefaultsToARankingOrder(t *testing.T) {
	s := newEagerSchema(t)
	statement, _, err := s.post.buildPartitionedSelect([]string{"id", "userId"}, "userId", Query{}, Include{Limit: Int(1)})
	if err != nil {
		t.Fatalf("buildPartitionedSelect: %v", err)
	}
	if !strings.Contains(statement, `ORDER BY "id" ASC)`) {
		t.Errorf("an unordered include was not ranked by the primary key:\n%s", statement)
	}
}

func TestBuildPartitionedSelectNeedsColumns(t *testing.T) {
	s := newEagerSchema(t)
	if _, _, err := s.post.buildPartitionedSelect(nil, "userId", Query{}, Include{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("no columns = %v, want ErrInvalidQuery", err)
	}
}

func TestRowNumberColumnAvoidsARealColumn(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Ranked", Attributes{
		"sequelizeRowNumber": {Type: INTEGER(), Field: rowNumberAlias},
	}, Timestamps(false))
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	got, err := m.rowNumberColumn()
	if err != nil {
		t.Fatalf("rowNumberColumn: %v", err)
	}
	if got == rowNumberAlias {
		t.Errorf("rowNumberColumn() = %q, which collides with a real column", got)
	}
}

// noWindows is a Dialect that declares it has no window functions.
type noWindows struct{ Dialect }

func (noWindows) SupportsWindowFunctions() bool { return false }

func TestDialectSupportsWindowFunctions(t *testing.T) {
	sqlite, err := DialectByName("sqlite")
	if err != nil {
		t.Fatalf("DialectByName: %v", err)
	}
	if !DialectSupportsWindowFunctions(sqlite) {
		t.Error("sqlite is on the allow-list and must report window support")
	}
	if DialectSupportsWindowFunctions(noWindows{sqlite}) {
		t.Error("a dialect that answers for itself was overruled by the allow-list")
	}
	if DialectSupportsWindowFunctions(nil) {
		t.Error("a nil dialect must not claim window support")
	}
}

func TestStripIncludeChildOptions(t *testing.T) {
	in := []Include{{
		Association: "posts",
		Required:    true,
		Attrs:       []string{"title"},
		Limit:       Int(1),
		Offset:      Int(1),
		Order:       []OrderTerm{Asc("title")},
		Include:     []Include{{Association: "comments", Limit: Int(2)}},
	}}
	out := stripIncludeChildOptions(in)
	if includesHaveChildOptions(out) {
		t.Errorf("child options survived stripping: %+v", out)
	}
	if !out[0].Required || len(out[0].Attrs) != 1 {
		t.Errorf("stripping dropped an option it should keep: %+v", out[0])
	}
	if !includesHaveChildOptions(in) {
		t.Error("stripping mutated the caller's include tree")
	}
	if stripIncludeChildOptions(nil) != nil {
		t.Error("stripping nothing must yield nothing")
	}
}

func TestTrimGroups(t *testing.T) {
	three := []Values{{"n": 1}, {"n": 2}, {"n": 3}}
	grouped := map[string][]Values{"a": three, "b": {{"n": 9}}}
	trimGroups(grouped, Int(2), Int(1))
	if got := len(grouped["a"]); got != 2 {
		t.Errorf("a kept %d, want 2", got)
	}
	if grouped["a"][0]["n"] != 2 {
		t.Errorf("a starts at %v, want the offset applied", grouped["a"][0]["n"])
	}
	if got := len(grouped["b"]); got != 0 {
		t.Errorf("b kept %d, want none: the offset is past its end", got)
	}
	// Nothing to do is a no-op rather than a rewrite.
	untouched := map[string][]Values{"a": three}
	trimGroups(untouched, nil, nil)
	if len(untouched["a"]) != 3 {
		t.Errorf("a kept %d with no window, want 3", len(untouched["a"]))
	}
}

func TestIncludeLimitUsesAWindowFunctionOnSQLite(t *testing.T) {
	// The window function is the whole point: it is what makes one statement
	// produce a per-parent slice. Assert it reached the database.
	s := newEagerSchema(t)
	if _, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Asc("title")},
		Limit:       Int(2),
	}}}); err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	statements := s.counter.selects()
	if len(statements) != 2 {
		t.Fatalf("ran %d SELECTs, want 2:\n%s", len(statements), strings.Join(statements, "\n"))
	}
	if !strings.Contains(statements[1], "ROW_NUMBER() OVER (PARTITION BY") {
		t.Errorf("the children were not fetched with a window function:\n%s", statements[1])
	}
}

func TestIncludeOffsetWithoutALimitKeepsTheRest(t *testing.T) {
	s := newEagerSchema(t)
	rows, err := s.user.FindAllEager(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Asc("title")},
		Offset:      Int(1),
	}}})
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	if got := eagerNames(eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts"), "title"); strings.Join(got, ",") != "a2,a3" {
		t.Errorf("alice's posts = %v, want [a2 a3]", got)
	}
	s.counter.want(t, 2, "an offset-only HasMany include")
}

func TestIncludeLimitFallsBackWhenAScopeGetsInTheWay(t *testing.T) {
	// A default scope on the target contributing an order is something the window
	// statement cannot merge, so the loader falls back to one batched select trimmed
	// per parent — still one statement, never one per parent.
	s := newEagerSchema(t)
	s.post.SetDefaultScope(Query{Order: []OrderTerm{Desc("title")}})
	ctx := context.Background()
	q := Query{Include: []Include{{Association: "posts", Limit: Int(1)}}}

	rows, err := s.user.FindAllEager(ctx, q)
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	if got := eagerNames(eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts"), "title"); len(got) != 1 || got[0] != "a3" {
		t.Errorf("alice's posts = %v, want the scope's ordering to pick [a3]", got)
	}
	statements := s.counter.selects()
	if len(statements) != 2 {
		t.Fatalf("ran %d SELECTs, want 2:\n%s", len(statements), strings.Join(statements, "\n"))
	}
	if strings.Contains(statements[1], "ROW_NUMBER") {
		t.Errorf("the scope should have ruled the window statement out:\n%s", statements[1])
	}

	s.addUsers(t, 10)
	if _, err := s.user.FindAllEager(ctx, q); err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	s.counter.want(t, 2, "the trimmed fallback over 13 users")
}

func TestRequiredIncludeWithALiteralWhereIsRefused(t *testing.T) {
	// The subquery a Required include expands to has to renumber its bind
	// parameters, which a caller's Literal makes impossible. Refusing is the only
	// safe answer: silently dropping the Required would return the wrong parents.
	s := newEagerSchema(t)
	ctx := context.Background()
	q := Query{Include: []Include{{
		Association: "posts",
		Where:       Literal(`"title" = ?`, "a1"),
		Required:    true,
	}}}
	if _, err := s.user.FindAll(ctx, q); !errors.Is(err, ErrUnsupported) {
		t.Errorf("FindAll = %v, want ErrUnsupported", err)
	}
	if _, err := s.user.Count(ctx, q); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Count = %v, want ErrUnsupported", err)
	}
	// Without Required there is no subquery, so the Literal is fine.
	plain := Query{Include: []Include{{Association: "posts", Where: Literal(`"title" = ?`, "a1")}}}
	rows, err := s.user.FindAll(ctx, plain)
	if err != nil {
		t.Fatalf("FindAll without Required: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d users, want 3", len(rows))
	}
}

func TestIncludeLimitsAtTwoLevelsStayBatched(t *testing.T) {
	s := newEagerSchema(t)
	ctx := context.Background()
	q := Query{Include: []Include{{
		Association: "posts",
		Order:       []OrderTerm{Asc("title")},
		Limit:       Int(2),
		Include: []Include{{
			Association: "comments",
			Order:       []OrderTerm{Desc("body")},
			Limit:       Int(1),
		}},
	}}}
	rows, err := s.user.FindAllEager(ctx, q)
	if err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	posts := eagerChildren(t, eagerRow(t, rows, "name", "alice"), "posts")
	if got := eagerNames(posts, "title"); strings.Join(got, ",") != "a1,a2" {
		t.Fatalf("alice's posts = %v, want [a1 a2]", got)
	}
	if got := eagerNames(eagerChildren(t, posts[0], "comments"), "body"); strings.Join(got, ",") != "c2" {
		t.Errorf("a1's comments = %v, want the single highest [c2]", got)
	}
	if got := eagerChildren(t, posts[1], "comments"); len(got) != 0 {
		t.Errorf("a2's comments = %v, want empty", got)
	}
	s.counter.want(t, 3, "per-parent limits at two levels over 3 users")

	s.addUsers(t, 10)
	if _, err := s.user.FindAllEager(ctx, q); err != nil {
		t.Fatalf("FindAllEager: %v", err)
	}
	s.counter.want(t, 3, "per-parent limits at two levels over 13 users")
}
