package sequelize

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// queryCounter records every statement the connection runs, so a test can assert
// on the *number* of queries. Eager loading that returns the right rows with one
// query per parent is still a bug, and only a count catches it.
type queryCounter struct {
	mu         sync.Mutex
	statements []string
}

func (c *queryCounter) log(_ context.Context, query string, _ []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = append(c.statements, query)
}

func (c *queryCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = nil
}

// selects returns the SELECT statements seen since the last reset.
func (c *queryCounter) selects() []string {
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

func (c *queryCounter) wantSelects(t *testing.T, n int, what string) {
	t.Helper()
	got := c.selects()
	if len(got) != n {
		t.Errorf("%s ran %d SELECTs, want %d:\n%s", what, len(got), n, strings.Join(got, "\n"))
	}
}

// assocSchema is the fixture graph: User has many Post, Post has many Comment,
// User has one Profile, Post belongs to many Tag through PostTag.
type assocSchema struct {
	db      *Sequelize
	counter *queryCounter
	user    *Model
	post    *Model
	comment *Model
	profile *Model
	tag     *Model
	postTag *Model
	ids     map[string]int64
}

func newAssocSchema(t *testing.T) *assocSchema {
	t.Helper()
	counter := &queryCounter{}
	db := newTestDB(t, WithLogger(counter.log))
	s := &assocSchema{db: db, counter: counter, ids: map[string]int64{}}

	s.user = db.Define("User", Attributes{"name": {Type: STRING(32), AllowNull: NotNull()}})
	s.post = db.Define("Post", Attributes{
		"title":  {Type: STRING(32), AllowNull: NotNull()},
		"userId": {Type: INTEGER()},
	})
	s.comment = db.Define("Comment", Attributes{
		"body":   {Type: STRING(32), AllowNull: NotNull()},
		"postId": {Type: INTEGER()},
	})
	s.profile = db.Define("Profile", Attributes{
		"bio":    {Type: STRING(32)},
		"userId": {Type: INTEGER()},
	})
	s.tag = db.Define("Tag", Attributes{"label": {Type: STRING(32), AllowNull: NotNull()}})
	s.postTag = db.Define("PostTag", Attributes{
		"postId": {Type: INTEGER()},
		"tagId":  {Type: INTEGER()},
	})
	for _, m := range []*Model{s.user, s.post, s.comment, s.profile, s.tag, s.postTag} {
		if err := m.Err(); err != nil {
			t.Fatalf("Define %s: %v", m.Name(), err)
		}
	}

	assocs := []*Association{
		s.user.HasMany(s.post),
		s.user.HasOne(s.profile),
		s.post.BelongsTo(s.user),
		s.post.HasMany(s.comment),
		s.comment.BelongsTo(s.post),
		s.post.BelongsToMany(s.tag, s.postTag),
	}
	for _, a := range assocs {
		if err := a.Err(); err != nil {
			t.Fatalf("association %v: %v", a, err)
		}
	}

	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	create := func(m *Model, key string, vals Values) {
		t.Helper()
		row, err := m.Create(ctx, vals)
		if err != nil {
			t.Fatalf("Create %s: %v", m.Name(), err)
		}
		if key != "" {
			id, ok := row["id"].(int64)
			if !ok {
				t.Fatalf("Create %s returned no id: %v", m.Name(), row)
			}
			s.ids[key] = id
		}
	}

	create(s.user, "alice", Values{"name": "alice"})
	create(s.user, "bob", Values{"name": "bob"})
	create(s.user, "carol", Values{"name": "carol"})
	create(s.profile, "aliceProfile", Values{"bio": "hi", "userId": s.ids["alice"]})
	create(s.post, "p1", Values{"title": "p1", "userId": s.ids["alice"]})
	create(s.post, "p2", Values{"title": "p2", "userId": s.ids["alice"]})
	create(s.post, "p3", Values{"title": "p3", "userId": s.ids["bob"]})
	create(s.post, "orphan", Values{"title": "orphan"})
	create(s.comment, "c1", Values{"body": "c1", "postId": s.ids["p1"]})
	create(s.comment, "c2", Values{"body": "c2", "postId": s.ids["p1"]})
	create(s.comment, "c3", Values{"body": "c3", "postId": s.ids["p2"]})
	create(s.tag, "go", Values{"label": "go"})
	create(s.tag, "sql", Values{"label": "sql"})
	create(s.postTag, "", Values{"postId": s.ids["p1"], "tagId": s.ids["go"]})
	create(s.postTag, "", Values{"postId": s.ids["p1"], "tagId": s.ids["sql"]})
	create(s.postTag, "", Values{"postId": s.ids["p2"], "tagId": s.ids["go"]})

	counter.reset()
	return s
}

// byName sorts rows by a string attribute, so assertions do not depend on the
// order the database happened to return batched children in.
func byName(rows []Values, attr string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprint(r[attr]))
	}
	sort.Strings(out)
	return out
}

func childRows(t *testing.T, row Values, key string) []Values {
	t.Helper()
	got, ok := row[key].([]Values)
	if !ok {
		t.Fatalf("row[%q] = %#v, want []Values", key, row[key])
	}
	return got
}

func rowByAttr(t *testing.T, rows []Values, attr, want string) Values {
	t.Helper()
	for _, r := range rows {
		if fmt.Sprint(r[attr]) == want {
			return r
		}
	}
	t.Fatalf("no row with %s = %q in %v", attr, want, rows)
	return nil
}

func TestAssociationInfersKeysAndNames(t *testing.T) {
	s := newAssocSchema(t)

	posts, ok := s.user.Association("posts")
	if !ok {
		t.Fatal("HasMany did not register under the pluralised default name")
	}
	if posts.Kind() != AssocHasMany || !posts.Kind().Multiple() {
		t.Errorf("Kind = %v", posts.Kind())
	}
	if posts.ForeignKey() != "userId" {
		t.Errorf("inferred ForeignKey = %q, want userId", posts.ForeignKey())
	}
	if posts.SourceKey() != "id" || posts.TargetKey() != "id" {
		t.Errorf("keys = %q/%q, want id/id", posts.SourceKey(), posts.TargetKey())
	}
	if posts.Source() != s.user || posts.Target() != s.post || posts.Through() != nil {
		t.Error("Source/Target/Through do not describe the association")
	}

	if a, ok := s.post.Association("user"); !ok || a.Kind() != AssocBelongsTo || a.ForeignKey() != "userId" {
		t.Errorf("BelongsTo inference = %v, %v", a, ok)
	}
	if a, ok := s.user.Association("profile"); !ok || a.Kind() != AssocHasOne || a.ForeignKey() != "userId" {
		t.Errorf("HasOne inference = %v, %v", a, ok)
	}
	tags, ok := s.post.Association("tags")
	if !ok {
		t.Fatal("BelongsToMany did not register")
	}
	if tags.ForeignKey() != "postId" || tags.OtherKey() != "tagId" || tags.Through() != s.postTag {
		t.Errorf("BelongsToMany inference: fk=%q other=%q through=%v",
			tags.ForeignKey(), tags.OtherKey(), tags.Through())
	}

	if names := len(s.post.Associations()); names != 3 {
		t.Errorf("Post has %d associations, want 3", names)
	}
	if summary := s.user.AssociationSummary(); !strings.Contains(summary, "hasMany") {
		t.Errorf("AssociationSummary = %q", summary)
	}
	if got := AssocBelongsToMany.String(); got != "belongsToMany" {
		t.Errorf("AssociationKind.String = %q", got)
	}
	if got := AssociationKind(0).String(); got != "unknown" {
		t.Errorf("AssociationKind(0).String = %q", got)
	}
}

func TestAssociationExplicitOverrides(t *testing.T) {
	db := newTestDB(t)
	author := db.Define("Author", Attributes{"slug": {Type: STRING(16), Unique: true}})
	book := db.Define("Book", Attributes{
		"title":      {Type: STRING(16)},
		"authorSlug": {Type: STRING(16)},
	})

	a := author.HasMany(book, As("works"), ForeignKey("authorSlug"), SourceKey("slug"))
	if err := a.Err(); err != nil {
		t.Fatalf("overrides rejected: %v", err)
	}
	if a.Name() != "works" || a.ForeignKey() != "authorSlug" || a.SourceKey() != "slug" {
		t.Errorf("overrides not honoured: %v", a)
	}
	if _, ok := author.Association("works"); !ok {
		t.Error("As() name was not registered")
	}
	if _, ok := author.Association("books"); ok {
		t.Error("the default name was registered as well as the override")
	}

	b := book.BelongsTo(author, ForeignKey("authorSlug"), TargetKey("slug"))
	if err := b.Err(); err != nil {
		t.Fatalf("BelongsTo overrides rejected: %v", err)
	}
	if b.TargetKey() != "slug" {
		t.Errorf("TargetKey = %q", b.TargetKey())
	}
}

func TestBelongsToManyExplicitKeys(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	left := db.Define("Left", Attributes{"name": {Type: STRING(8)}})
	right := db.Define("Right", Attributes{"name": {Type: STRING(8)}})
	link := db.Define("Link", Attributes{
		"lhs": {Type: INTEGER()},
		"rhs": {Type: INTEGER()},
	})
	a := left.BelongsToMany(right, link, As("rights"), ForeignKey("lhs"), OtherKey("rhs"))
	if err := a.Err(); err != nil {
		t.Fatalf("BelongsToMany with explicit keys: %v", err)
	}
	if a.ForeignKey() != "lhs" || a.OtherKey() != "rhs" {
		t.Fatalf("keys = %q/%q", a.ForeignKey(), a.OtherKey())
	}
	if !strings.Contains(a.String(), "belongsToMany") {
		t.Errorf("String = %q", a.String())
	}
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	l, err := left.Create(ctx, Values{"name": "l"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r, err := right.Create(ctx, Values{"name": "r"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := link.Create(ctx, Values{"lhs": l["id"], "rhs": r["id"]}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	rows, err := left.FindAll(ctx, Query{Include: []Include{{Association: "rights"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := childRows(t, rows[0], "rights"); len(got) != 1 || got[0]["name"] != "r" {
		t.Errorf(`left["rights"] = %v`, got)
	}

	// A rejected declaration renders its error rather than pretending to work.
	bad := left.BelongsToMany(right, link, As("more"), ForeignKey("nope"))
	if !strings.Contains(bad.String(), "invalid") {
		t.Errorf("String of a rejected association = %q", bad.String())
	}
	if got := bad.Name(); got != "more" {
		t.Errorf("Name = %q", got)
	}
}

func TestAssociationRejectsBadDeclarations(t *testing.T) {
	db := newTestDB(t)
	user := db.Define("User", Attributes{"name": {Type: STRING(8)}})
	post := db.Define("Post", Attributes{"title": {Type: STRING(8)}})
	other := newTestDB(t).Define("User", Attributes{"name": {Type: STRING(8)}})

	if err := user.HasMany(post).Err(); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("missing foreign key = %v, want ErrUnknownAttribute", err)
	}
	if err := user.HasMany(nil).Err(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("nil target = %v, want ErrInvalidQuery", err)
	}
	if err := user.HasMany(other).Err(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("cross-connection = %v, want ErrInvalidQuery", err)
	}
	if err := user.BelongsToMany(post, nil).Err(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("nil through = %v, want ErrInvalidQuery", err)
	}
	if err := user.HasOne(post, As("name")).Err(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("alias colliding with an attribute = %v, want ErrInvalidQuery", err)
	}
	if err := user.HasOne(post, As("not an identifier")).Err(); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("bad alias = %v, want ErrInvalidIdentifier", err)
	}
	if err := user.HasOne(post, ForeignKey("title"), SourceKey("nope")).Err(); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("bad SourceKey = %v, want ErrUnknownAttribute", err)
	}

	// A duplicate alias is rejected rather than silently replacing the first.
	ok := user.HasOne(post, As("mine"), ForeignKey("title"))
	if err := ok.Err(); err != nil {
		t.Fatalf("first declaration: %v", err)
	}
	if err := user.HasMany(post, As("mine"), ForeignKey("title")).Err(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("duplicate alias = %v, want ErrInvalidQuery", err)
	}

	// A model with a composite primary key has no obvious key to associate on.
	composite := db.Define("Member", Attributes{
		"userID":  {Type: INTEGER(), PrimaryKey: true},
		"groupID": {Type: INTEGER(), PrimaryKey: true},
	})
	if err := composite.HasMany(post, ForeignKey("title")).Err(); !errors.Is(err, ErrNoPrimaryKey) {
		t.Errorf("composite source key = %v, want ErrNoPrimaryKey", err)
	}

	// An unusable model cannot carry an association.
	broken := db.Define("Broken", Attributes{"x": {}})
	if err := broken.HasMany(post).Err(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("association on a broken model = %v", err)
	}
	if err := user.HasMany(broken).Err(); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("association to a broken model = %v", err)
	}
}

// TestIncludeBelongsToIsASingleJoinedQuery is the parity case for a to-one
// include: a JOIN, not a second query.
func TestIncludeBelongsToIsASingleJoinedQuery(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	rows, err := s.post.FindAll(ctx, Query{
		Order:   []OrderTerm{Asc("title")},
		Include: []Include{{Association: "user"}},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	s.counter.wantSelects(t, 1, "a BelongsTo include")
	if len(rows) != 4 {
		t.Fatalf("got %d posts, want 4", len(rows))
	}
	p1 := rowByAttr(t, rows, "title", "p1")
	user, ok := p1["user"].(Values)
	if !ok {
		t.Fatalf(`p1["user"] = %#v, want Values`, p1["user"])
	}
	if user["name"] != "alice" {
		t.Errorf("joined user = %v, want alice", user)
	}
	if user["id"] != s.ids["alice"] {
		t.Errorf("joined user id = %v, want %d", user["id"], s.ids["alice"])
	}

	// The unmatched outer join is a nil entry, present so it can be told apart
	// from an association that was never requested.
	orphan := rowByAttr(t, rows, "title", "orphan")
	value, present := orphan["user"]
	if !present || value != nil {
		t.Errorf(`orphan["user"] = %#v, present=%v; want a nil entry`, value, present)
	}
}

func TestIncludeHasOneJoins(t *testing.T) {
	s := newAssocSchema(t)
	rows, err := s.user.FindAll(context.Background(), Query{Include: []Include{{Association: "profile"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	s.counter.wantSelects(t, 1, "a HasOne include")
	alice := rowByAttr(t, rows, "name", "alice")
	profile, ok := alice["profile"].(Values)
	if !ok || profile["bio"] != "hi" {
		t.Errorf(`alice["profile"] = %#v`, alice["profile"])
	}
	bob := rowByAttr(t, rows, "name", "bob")
	if bob["profile"] != nil {
		t.Errorf(`bob["profile"] = %#v, want nil`, bob["profile"])
	}
}

// TestIncludeHasManyIsBatchedNotNPlusOne is the parity case that matters most: a
// to-many include must cost one extra query, whatever the number of parents.
func TestIncludeHasManyIsBatchedNotNPlusOne(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	rows, err := s.user.FindAll(ctx, Query{Include: []Include{{Association: "posts"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	// One SELECT for the users, one for every post of every user.
	s.counter.wantSelects(t, 2, "a HasMany include over 3 users")
	if len(rows) != 3 {
		t.Fatalf("got %d users, want 3", len(rows))
	}
	alice := rowByAttr(t, rows, "name", "alice")
	if got := byName(childRows(t, alice, "posts"), "title"); len(got) != 2 || got[0] != "p1" || got[1] != "p2" {
		t.Errorf(`alice["posts"] = %v, want [p1 p2]`, got)
	}
	if got := childRows(t, rowByAttr(t, rows, "name", "bob"), "posts"); len(got) != 1 {
		t.Errorf(`bob["posts"] = %v, want one post`, got)
	}
	// A parent with no children gets an empty slice, not a nil.
	carol := rowByAttr(t, rows, "name", "carol")
	if got := childRows(t, carol, "posts"); len(got) != 0 {
		t.Errorf(`carol["posts"] = %v, want empty`, got)
	}

	// Growing the parent set must not grow the query count. This is the assertion
	// an N+1 fails even though it returns exactly the right rows.
	for i := 0; i < 10; i++ {
		if _, err := s.user.Create(ctx, Values{"name": fmt.Sprintf("extra%d", i)}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	s.counter.reset()
	rows, err = s.user.FindAll(ctx, Query{Include: []Include{{Association: "posts"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 13 {
		t.Fatalf("got %d users, want 13", len(rows))
	}
	s.counter.wantSelects(t, 2, "a HasMany include over 13 users")
}

func TestIncludeNestedTwoLevelsStaysBatched(t *testing.T) {
	s := newAssocSchema(t)

	rows, err := s.user.FindAll(context.Background(), Query{Include: []Include{{
		Association: "posts",
		Include:     []Include{{Association: "comments"}},
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	// users, then all posts, then all comments: three queries for three levels.
	s.counter.wantSelects(t, 3, "a two-level HasMany include")

	alice := rowByAttr(t, rows, "name", "alice")
	posts := childRows(t, alice, "posts")
	p1 := rowByAttr(t, posts, "title", "p1")
	if got := byName(childRows(t, p1, "comments"), "body"); len(got) != 2 || got[0] != "c1" {
		t.Errorf(`p1["comments"] = %v, want [c1 c2]`, got)
	}
	p2 := rowByAttr(t, posts, "title", "p2")
	if got := childRows(t, p2, "comments"); len(got) != 1 {
		t.Errorf(`p2["comments"] = %v, want one comment`, got)
	}
	bobPosts := childRows(t, rowByAttr(t, rows, "name", "bob"), "posts")
	if got := childRows(t, bobPosts[0], "comments"); len(got) != 0 {
		t.Errorf(`p3["comments"] = %v, want empty`, got)
	}
}

func TestIncludeNestedJoinsAcrossTwoLevelsInOneQuery(t *testing.T) {
	s := newAssocSchema(t)

	rows, err := s.comment.FindAll(context.Background(), Query{Include: []Include{{
		Association: "post",
		Include:     []Include{{Association: "user"}},
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	// Both levels are to-one, so both are joins in the one statement.
	s.counter.wantSelects(t, 1, "a two-level to-one include")
	if len(rows) != 3 {
		t.Fatalf("got %d comments, want 3", len(rows))
	}
	c1 := rowByAttr(t, rows, "body", "c1")
	post, ok := c1["post"].(Values)
	if !ok {
		t.Fatalf(`c1["post"] = %#v`, c1["post"])
	}
	user, ok := post["user"].(Values)
	if !ok || user["name"] != "alice" {
		t.Errorf(`c1["post"]["user"] = %#v, want alice`, post["user"])
	}
}

// TestIncludeMixedShapesShareTheRootQuery checks that a to-one and a to-many
// include on the same query cost one query plus one batch, not two of each.
func TestIncludeMixedShapesShareTheRootQuery(t *testing.T) {
	s := newAssocSchema(t)

	rows, err := s.post.FindAll(context.Background(), Query{Include: []Include{
		{Association: "user"},
		{Association: "comments"},
	}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	s.counter.wantSelects(t, 2, "a mixed include")
	p1 := rowByAttr(t, rows, "title", "p1")
	if user, ok := p1["user"].(Values); !ok || user["name"] != "alice" {
		t.Errorf(`p1["user"] = %#v`, p1["user"])
	}
	if got := childRows(t, p1, "comments"); len(got) != 2 {
		t.Errorf(`p1["comments"] = %v, want 2`, got)
	}
}

// TestIncludeBatchedUnderAJoinedParent exercises the path where the rows a batch
// hangs off are themselves nested one level down.
func TestIncludeBatchedUnderAJoinedParent(t *testing.T) {
	s := newAssocSchema(t)

	rows, err := s.comment.FindAll(context.Background(), Query{Include: []Include{{
		Association: "post",
		Include:     []Include{{Association: "tags"}},
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	// comments joined to posts, then the join table, then the tags.
	s.counter.wantSelects(t, 3, "a BelongsToMany under a joined parent")
	c1 := rowByAttr(t, rows, "body", "c1")
	post := c1["post"].(Values)
	if got := byName(childRows(t, post, "tags"), "label"); len(got) != 2 || got[0] != "go" || got[1] != "sql" {
		t.Errorf(`p1["tags"] = %v, want [go sql]`, got)
	}
}

func TestIncludeBelongsToManyIsBatched(t *testing.T) {
	s := newAssocSchema(t)

	rows, err := s.post.FindAll(context.Background(), Query{Include: []Include{{Association: "tags"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	// The posts, the join table, then the tags: three queries for four posts.
	s.counter.wantSelects(t, 3, "a BelongsToMany include")
	if got := byName(childRows(t, rowByAttr(t, rows, "title", "p1"), "tags"), "label"); len(got) != 2 {
		t.Errorf(`p1["tags"] = %v, want two tags`, got)
	}
	if got := childRows(t, rowByAttr(t, rows, "title", "p2"), "tags"); len(got) != 1 {
		t.Errorf(`p2["tags"] = %v, want one tag`, got)
	}
	if got := childRows(t, rowByAttr(t, rows, "title", "p3"), "tags"); len(got) != 0 {
		t.Errorf(`p3["tags"] = %v, want none`, got)
	}
}

func TestIncludeRequiredMakesAnInnerJoin(t *testing.T) {
	s := newAssocSchema(t)
	rows, err := s.post.FindAll(context.Background(), Query{
		Include: []Include{{Association: "user", Required: true}},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("Required include returned %d posts, want the 3 with a user", len(rows))
	}
	for _, r := range rows {
		if r["title"] == "orphan" {
			t.Error("an INNER JOIN kept the row with no association")
		}
	}
}

// TestIncludeWhereNarrowsTheAssociationNotTheParents checks that Include.Where
// lands in the ON clause: filtering an optional association must not drop the
// parent row.
func TestIncludeWhereNarrowsTheAssociationNotTheParents(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	rows, err := s.post.FindAll(ctx, Query{Include: []Include{{
		Association: "user",
		Where:       Op.Eq("name", "alice"),
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d posts, want all 4", len(rows))
	}
	if user, ok := rowByAttr(t, rows, "title", "p1")["user"].(Values); !ok || user["name"] != "alice" {
		t.Errorf("alice's post lost its user: %#v", rowByAttr(t, rows, "title", "p1")["user"])
	}
	if got := rowByAttr(t, rows, "title", "p3")["user"]; got != nil {
		t.Errorf(`p3["user"] = %#v, want nil: the ON filter excluded bob`, got)
	}

	// The same filter on a batched association narrows the children.
	s.counter.reset()
	users, err := s.user.FindAll(ctx, Query{Include: []Include{{
		Association: "posts",
		Where:       Op.Eq("title", "p1"),
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	s.counter.wantSelects(t, 2, "a filtered HasMany include")
	if got := childRows(t, rowByAttr(t, users, "name", "alice"), "posts"); len(got) != 1 {
		t.Errorf(`filtered alice["posts"] = %v, want just p1`, got)
	}
	if got := childRows(t, rowByAttr(t, users, "name", "bob"), "posts"); len(got) != 0 {
		t.Errorf(`filtered bob["posts"] = %v, want empty`, got)
	}
}

func TestIncludeAttrsKeepTheStitchingKeys(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	rows, err := s.user.FindAll(ctx, Query{
		Attrs:   []string{"name"},
		Include: []Include{{Association: "posts", Attrs: []string{"title"}}},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	alice := rowByAttr(t, rows, "name", "alice")
	// The root projection did not ask for the key the batch groups by, but it has
	// to be there for the children to be attached at all.
	if _, ok := alice["id"]; !ok {
		t.Error("the source key was dropped from the root projection")
	}
	posts := childRows(t, alice, "posts")
	if len(posts) != 2 {
		t.Fatalf(`alice["posts"] = %v, want 2`, posts)
	}
	if _, ok := posts[0]["userId"]; !ok {
		t.Error("the foreign key was dropped from the child projection")
	}
	if _, ok := posts[0]["body"]; ok {
		t.Error("Attrs did not restrict the child projection")
	}

	// A restricted to-one projection still carries the target's primary key.
	joined, err := s.post.FindAll(ctx, Query{Include: []Include{{
		Association: "user", Attrs: []string{"name"},
	}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	user := rowByAttr(t, joined, "title", "p1")["user"].(Values)
	if _, ok := user["id"]; !ok {
		t.Error("the joined primary key was dropped, so an unmatched join is undetectable")
	}
}

// TestCountWithIncludeAndDistinctCountsParents is the parity case from
// ARCHITECTURE.md: the joins multiply rows and Distinct must collapse them.
func TestCountWithIncludeAndDistinctCountsParents(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	// alice 2 posts + bob 1 post + carol's unmatched outer join row = 4.
	joined, err := s.user.Count(ctx, Query{Include: []Include{{Association: "posts"}}})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if joined != 4 {
		t.Fatalf("Count over the join = %d, want the 4 joined rows", joined)
	}

	parents, err := s.user.Count(ctx, Query{
		Include:  []Include{{Association: "posts"}},
		Distinct: true,
	})
	if err != nil {
		t.Fatalf("Count distinct: %v", err)
	}
	if parents != 3 {
		t.Errorf("distinct Count = %d, want 3 parents", parents)
	}

	withPosts, err := s.user.Count(ctx, Query{
		Include:  []Include{{Association: "posts", Required: true}},
		Distinct: true,
	})
	if err != nil {
		t.Fatalf("Count distinct required: %v", err)
	}
	if withPosts != 2 {
		t.Errorf("distinct Count with a required include = %d, want 2", withPosts)
	}

	// Filters and a BelongsToMany join count parents too.
	tagged, err := s.post.Count(ctx, Query{
		Include:  []Include{{Association: "tags", Required: true}},
		Distinct: true,
	})
	if err != nil {
		t.Fatalf("Count over a join table: %v", err)
	}
	if tagged != 2 {
		t.Errorf("distinct Count over the join table = %d, want 2", tagged)
	}
}

func TestFindOneAndFindAndCountAllWithInclude(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	row, err := s.post.FindOne(ctx, Query{
		Where:   Op.Eq("title", "p1"),
		Include: []Include{{Association: "user"}, {Association: "comments"}},
	})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if user, ok := row["user"].(Values); !ok || user["name"] != "alice" {
		t.Errorf(`FindOne row["user"] = %#v`, row["user"])
	}
	if got := childRows(t, row, "comments"); len(got) != 2 {
		t.Errorf(`FindOne row["comments"] = %v`, got)
	}

	rows, total, err := s.user.FindAndCountAll(ctx, Query{
		Limit:    Int(2),
		Order:    []OrderTerm{Asc("name")},
		Include:  []Include{{Association: "posts"}},
		Distinct: true,
	})
	if err != nil {
		t.Fatalf("FindAndCountAll: %v", err)
	}
	if len(rows) != 2 || total != 3 {
		t.Errorf("FindAndCountAll = %d rows, total %d; want 2 and 3", len(rows), total)
	}
}

func TestIncludeParanoidTargetHidesSoftDeletedRows(t *testing.T) {
	counter := &queryCounter{}
	db := newTestDB(t, WithLogger(counter.log))
	ctx := context.Background()

	list := db.Define("List", Attributes{"name": {Type: STRING(16)}})
	item := db.Define("Item", Attributes{
		"label":  {Type: STRING(16)},
		"listId": {Type: INTEGER()},
	}, Paranoid(true))
	if err := list.HasMany(item).Err(); err != nil {
		t.Fatalf("HasMany: %v", err)
	}
	if err := item.BelongsTo(list).Err(); err != nil {
		t.Fatalf("BelongsTo: %v", err)
	}
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	parent, err := list.Create(ctx, Values{"name": "l"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, label := range []string{"keep", "drop"} {
		if _, err := item.Create(ctx, Values{"label": label, "listId": parent["id"]}); err != nil {
			t.Fatalf("Create item: %v", err)
		}
	}
	if _, err := item.Destroy(ctx, Query{Where: Op.Eq("label", "drop")}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// The batched path inherits the target's soft-delete filter through FindAll.
	rows, err := list.FindAll(ctx, Query{Include: []Include{{Association: "items"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got := childRows(t, rows[0], "items"); len(got) != 1 || got[0]["label"] != "keep" {
		t.Errorf(`batched items = %v, want only "keep"`, got)
	}

	// The joined path needs the filter in its ON clause instead.
	joined, err := item.FindAll(ctx, Query{
		Paranoid: Bool(false),
		Include:  []Include{{Association: "list"}},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(joined) != 2 {
		t.Errorf("Paranoid(false) returned %d items, want both", len(joined))
	}

	// A paranoid target, joined from a non-paranoid parent.
	if err := item.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	one, err := list.FindAll(ctx, Query{Include: []Include{{Association: "soleItem"}}})
	if err == nil {
		t.Fatalf("an unknown association was accepted: %v", one)
	}
	if !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("unknown association = %v, want ErrInvalidQuery", err)
	}
}

func TestIncludeParanoidJoinIsFilteredInTheOnClause(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	item := db.Define("Item", Attributes{"label": {Type: STRING(16)}}, Paranoid(true))
	holder := db.Define("Holder", Attributes{
		"name":   {Type: STRING(16)},
		"itemId": {Type: INTEGER()},
	})
	if err := holder.BelongsTo(item).Err(); err != nil {
		t.Fatalf("BelongsTo: %v", err)
	}
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	row, err := item.Create(ctx, Values{"label": "gone"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := holder.Create(ctx, Values{"name": "h", "itemId": row["id"]}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := item.Destroy(ctx, Query{Where: Op.Eq("label", "gone")}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	rows, err := holder.FindAll(ctx, Query{Include: []Include{{Association: "item"}}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the parent was dropped along with its soft-deleted association: %v", rows)
	}
	if rows[0]["item"] != nil {
		t.Errorf(`holder["item"] = %#v, want nil for a soft-deleted target`, rows[0]["item"])
	}

	// Asking to see deleted rows brings the association back.
	rows, err = holder.FindAll(ctx, Query{
		Paranoid: Bool(false),
		Include:  []Include{{Association: "item"}},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if got, ok := rows[0]["item"].(Values); !ok || got["label"] != "gone" {
		t.Errorf(`Paranoid(false) holder["item"] = %#v`, rows[0]["item"])
	}
}

func TestIncludeAsOverridesTheKey(t *testing.T) {
	s := newAssocSchema(t)
	rows, err := s.post.FindAll(context.Background(), Query{Include: []Include{
		{Association: "user", As: "author"},
		{Association: "comments", As: "replies"},
	}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	p1 := rowByAttr(t, rows, "title", "p1")
	if _, ok := p1["author"].(Values); !ok {
		t.Errorf(`p1["author"] = %#v`, p1["author"])
	}
	if _, ok := p1["user"]; ok {
		t.Error("the default key was used as well as the override")
	}
	if got := childRows(t, p1, "replies"); len(got) != 2 {
		t.Errorf(`p1["replies"] = %v`, got)
	}
}

func TestIncludeRejectsBadPlans(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	if _, err := s.user.FindAll(ctx, Query{Include: []Include{{Association: "nope"}}}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("unknown association = %v, want ErrInvalidQuery", err)
	}
	if _, err := s.user.FindAll(ctx, Query{Include: []Include{{Association: "posts", As: "1bad"}}}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Errorf("bad As = %v, want ErrInvalidIdentifier", err)
	}
	if _, err := s.user.FindAll(ctx, Query{Include: []Include{{Association: "posts", Attrs: []string{"nope"}}}}); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("unknown include attribute = %v, want ErrUnknownAttribute", err)
	}
	if _, err := s.user.FindAll(ctx, Query{
		Include: []Include{{Association: "posts", Include: []Include{{Association: "nope"}}}},
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("unknown nested association = %v, want ErrInvalidQuery", err)
	}
	if _, err := s.user.Count(ctx, Query{
		Group:   []string{"name"},
		Include: []Include{{Association: "posts"}},
	}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Group with Include = %v, want ErrUnsupported", err)
	}

	// An association whose declaration failed cannot be included.
	broken := s.user.HasMany(s.comment, As("broken"))
	if broken.Err() == nil {
		t.Fatal("expected the declaration to fail")
	}
	if _, err := s.user.FindAll(ctx, Query{Include: []Include{{Association: "broken"}}}); err == nil {
		t.Error("a broken association was included without complaint")
	}
}

func TestIncludeInsideATransaction(t *testing.T) {
	s := newAssocSchema(t)
	ctx := context.Background()

	err := s.db.Transaction(ctx, func(tx *Tx) error {
		if _, err := s.post.Create(ctx, Values{"title": "p4", "userId": s.ids["carol"]}, Query{Tx: tx}); err != nil {
			return err
		}
		rows, err := s.user.FindAll(ctx, Query{
			Where:   Op.Eq("name", "carol"),
			Include: []Include{{Association: "posts"}},
			Tx:      tx,
		})
		if err != nil {
			return err
		}
		if got := childRows(t, rows[0], "posts"); len(got) != 1 {
			t.Errorf("the batched query did not join the transaction: %v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
}

func TestAssociationKeyNormalisesTypes(t *testing.T) {
	same := func(a, b any) bool {
		ka, oka := associationKey(a)
		kb, okb := associationKey(b)
		return oka && okb && ka == kb
	}
	if !same(int64(7), 7) {
		t.Error("an int and an int64 key did not match")
	}
	if !same([]byte("x"), "x") {
		t.Error("a []byte and a string key did not match")
	}
	if _, ok := associationKey(nil); ok {
		t.Error("nil produced a usable key")
	}
	if k, ok := associationKey(true); !ok || k != "b:1" {
		t.Errorf("bool key = %q, %v", k, ok)
	}
	if k, ok := associationKey(struct{ A int }{1}); !ok || !strings.HasPrefix(k, "x:") {
		t.Errorf("fallback key = %q, %v", k, ok)
	}
}

func TestEnsureAttrsAndGatherParents(t *testing.T) {
	if got := ensureAttrs([]string{"a"}, "a", "b"); len(got) != 2 || got[1] != "b" {
		t.Errorf("ensureAttrs = %v", got)
	}
	rows := []Values{
		{"child": Values{"leaf": 1}},
		{"child": nil},
		{"kids": []Values{{"leaf": 2}, {"leaf": 3}}},
	}
	if got := gatherParents(rows, []string{"child"}); len(got) != 1 {
		t.Errorf("gatherParents through a to-one = %v", got)
	}
	if got := gatherParents(rows, []string{"kids"}); len(got) != 2 {
		t.Errorf("gatherParents through a to-many = %v", got)
	}
	if got := gatherParents(rows, nil); len(got) != 3 {
		t.Errorf("gatherParents with no path = %v", got)
	}
}

// ExampleModel_HasMany shows eager loading a to-many association: one query for
// the parents, one for all of their children.
func ExampleModel_HasMany() {
	ctx := context.Background()
	db, _ := New("sqlite", "file:example_hasmany?mode=memory&cache=shared")
	defer db.Close()

	author := db.Define("Author", Attributes{"name": {Type: STRING(32)}})
	book := db.Define("Book", Attributes{
		"title":    {Type: STRING(32)},
		"authorId": {Type: INTEGER()},
	})
	author.HasMany(book)
	book.BelongsTo(author)
	_ = db.Sync(ctx)

	rowling, _ := author.Create(ctx, Values{"name": "Rowling"})
	_, _ = book.Create(ctx, Values{"title": "Philosopher's Stone", "authorId": rowling["id"]})

	rows, _ := author.FindAll(ctx, Query{Include: []Include{{Association: "books"}}})
	books := rows[0]["books"].([]Values)
	fmt.Println(rows[0]["name"], "wrote", books[0]["title"])
	// Output: Rowling wrote Philosopher's Stone
}
