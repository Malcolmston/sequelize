package sequelize

import (
	"context"
	"errors"
	"testing"
	"time"
)

// usersFixture defines and syncs a User model and seeds three rows against a
// real SQLite database.
func usersFixture(t *testing.T, opts ...Option) (*Sequelize, *Model) {
	t.Helper()
	db := newTestDB(t, opts...)
	user := db.Define("User", Attributes{
		"name":  {Type: STRING(32), AllowNull: NotNull()},
		"email": {Type: STRING(255), Unique: true},
		"age":   {Type: INTEGER()},
		"score": {Type: FLOAT()},
		"admin": {Type: BOOLEAN(), DefaultValue: false},
		"meta":  {Type: JSON()},
	})
	if err := user.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	seed := []Values{
		{"name": "ada", "email": "ada@example.com", "age": 36, "score": 9.5},
		{"name": "grace", "email": "grace@example.com", "age": 45, "score": 8.5},
		{"name": "alan", "email": "alan@example.com", "age": 41, "score": 7.0},
	}
	if _, err := user.BulkCreate(ctx, seed); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	return db, user
}

func TestFindAllReturnsRealRows(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	rows, err := user.FindAll(ctx, Query{Order: []OrderTerm{Asc("name")}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("FindAll returned %d rows, want 3", len(rows))
	}
	if rows[0]["name"] != "ada" || rows[1]["name"] != "alan" || rows[2]["name"] != "grace" {
		t.Errorf("rows are not in name order: %v %v %v", rows[0]["name"], rows[1]["name"], rows[2]["name"])
	}
	if id, ok := rows[0]["id"].(int64); !ok || id == 0 {
		t.Errorf("id = %#v, want a generated int64", rows[0]["id"])
	}
	if age, ok := rows[0]["age"].(int64); !ok || age != 36 {
		t.Errorf("age = %#v, want int64(36)", rows[0]["age"])
	}
	if score, ok := rows[0]["score"].(float64); !ok || score != 9.5 {
		t.Errorf("score = %#v, want float64(9.5)", rows[0]["score"])
	}
	if admin, ok := rows[0]["admin"].(bool); !ok || admin {
		t.Errorf("admin = %#v, want false from the attribute default", rows[0]["admin"])
	}
	if _, ok := rows[0][DefaultCreatedAt].(time.Time); !ok {
		t.Errorf("createdAt = %#v, want a time.Time", rows[0][DefaultCreatedAt])
	}
	if v, ok := rows[0]["meta"]; !ok || v != nil {
		t.Errorf("meta = %#v, want a present nil for a NULL column", v)
	}
}

func TestFindAllFiltersAndPaginates(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()

	rows, err := user.FindAll(ctx, Query{Where: Op.Gte("age", 41), Order: []OrderTerm{Asc("age")}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 2 || rows[0]["name"] != "alan" {
		t.Errorf("Op.Gte filter returned %v", rows)
	}

	rows, err = user.FindAll(ctx, Query{
		Order:  []OrderTerm{Asc("name")},
		Limit:  Int(1),
		Offset: Int(1),
		Attrs:  []string{"name"},
	})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "alan" {
		t.Errorf("limit/offset returned %v", rows)
	}
	if len(rows[0]) != 1 {
		t.Errorf("Attrs did not restrict the projection: %v", rows[0])
	}
}

// TestFindAllEmptyInMatchesNothing exercises the empty-set rule against a real
// database: valid SQL, zero rows.
func TestFindAllEmptyInMatchesNothing(t *testing.T) {
	_, user := usersFixture(t)
	rows, err := user.FindAll(context.Background(), Query{Where: Op.In("name", []string{})})
	if err != nil {
		t.Fatalf("FindAll with an empty IN: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("FindAll returned %d rows, want none", len(rows))
	}
}

func TestFindAllEmptyResultIsNotAnError(t *testing.T) {
	_, user := usersFixture(t)
	rows, err := user.FindAll(context.Background(), Query{Where: Op.Eq("name", "nobody")})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Errorf("FindAll = %v, want an empty slice", rows)
	}
}

func TestFindOne(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	row, err := user.FindOne(ctx, Query{Where: Op.Eq("name", "grace")})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if row["email"] != "grace@example.com" {
		t.Errorf("FindOne returned %v", row)
	}
}

func TestFindOneMissIsATypedError(t *testing.T) {
	_, user := usersFixture(t)
	_, err := user.FindOne(context.Background(), Query{Where: Op.Eq("name", "nobody")})
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("FindOne = %v, want ErrRecordNotFound", err)
	}
	var rnf *RecordNotFoundError
	if !errors.As(err, &rnf) || rnf.Model != "User" {
		t.Errorf("error = %#v, want a *RecordNotFoundError naming User", err)
	}
}

func TestFindByPk(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	first, err := user.FindOne(ctx, Query{Where: Op.Eq("name", "ada")})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	row, err := user.FindByPk(ctx, first["id"])
	if err != nil {
		t.Fatalf("FindByPk: %v", err)
	}
	if row["name"] != "ada" {
		t.Errorf("FindByPk returned %v", row)
	}
	if _, err := user.FindByPk(ctx, 99999); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("FindByPk(missing) = %v, want ErrRecordNotFound", err)
	}
	if _, err := user.FindByPk(ctx, first["id"], Query{}, Query{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("FindByPk with two queries = %v, want ErrInvalidQuery", err)
	}
	row, err = user.FindByPk(ctx, first["id"], Query{Attrs: []string{"name"}})
	if err != nil {
		t.Fatalf("FindByPk with Attrs: %v", err)
	}
	if len(row) != 1 {
		t.Errorf("Attrs did not restrict the projection: %v", row)
	}
}

func TestFindByPkComposite(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Membership", Attributes{
		"userID":  {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
		"groupID": {Type: INTEGER(), PrimaryKey: true, AllowNull: NotNull()},
		"role":    {Type: STRING(10)},
	}, Timestamps(false))
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.Create(ctx, Values{"userID": 1, "groupID": 2, "role": "owner"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	row, err := m.FindByPk(ctx, Values{"userID": 1, "groupID": 2})
	if err != nil {
		t.Fatalf("FindByPk: %v", err)
	}
	if row["role"] != "owner" {
		t.Errorf("FindByPk returned %v", row)
	}
	if _, err := m.FindByPk(ctx, 1); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("FindByPk with a scalar on a composite key = %v, want ErrInvalidQuery", err)
	}
	if _, err := m.FindByPk(ctx, Values{"userID": 1}); !errors.Is(err, ErrNoValues) {
		t.Errorf("FindByPk with a partial key = %v, want ErrNoValues", err)
	}
}

func TestCount(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()
	total, err := user.Count(ctx, Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 3 {
		t.Errorf("Count = %d, want 3", total)
	}
	filtered, err := user.Count(ctx, Query{Where: Op.Gt("age", 40)})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if filtered != 2 {
		t.Errorf("filtered Count = %d, want 2", filtered)
	}
	distinct, err := user.Count(ctx, Query{Distinct: true})
	if err != nil {
		t.Fatalf("distinct Count: %v", err)
	}
	if distinct != 3 {
		t.Errorf("distinct Count = %d, want 3", distinct)
	}
	grouped, err := user.Count(ctx, Query{Group: []string{"age"}})
	if err != nil {
		t.Fatalf("grouped Count: %v", err)
	}
	if grouped != 3 {
		t.Errorf("grouped Count = %d, want one per distinct age", grouped)
	}
	none, err := user.Count(ctx, Query{Where: Op.Eq("name", "nobody")})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if none != 0 {
		t.Errorf("Count of nothing = %d, want 0", none)
	}
}

func TestFindAndCountAllIgnoresPaginationInTheCount(t *testing.T) {
	_, user := usersFixture(t)
	rows, total, err := user.FindAndCountAll(context.Background(), Query{
		Limit: Int(2),
		Order: []OrderTerm{Asc("name")},
	})
	if err != nil {
		t.Fatalf("FindAndCountAll: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want the page of 2", len(rows))
	}
	if total != 3 {
		t.Errorf("total = %d, want every matching row", total)
	}
}

func TestAggregates(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()

	sum, err := user.Sum(ctx, "age", Query{})
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if sum != int64(36+45+41) {
		t.Errorf("Sum(age) = %#v, want int64(122)", sum)
	}
	min, err := user.Min(ctx, "age", Query{})
	if err != nil {
		t.Fatalf("Min: %v", err)
	}
	if min != int64(36) {
		t.Errorf("Min(age) = %#v, want int64(36)", min)
	}
	max, err := user.Max(ctx, "score", Query{})
	if err != nil {
		t.Fatalf("Max: %v", err)
	}
	if max != 9.5 {
		t.Errorf("Max(score) = %#v, want 9.5", max)
	}
	filtered, err := user.Sum(ctx, "age", Query{Where: Op.Eq("name", "ada")})
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if filtered != int64(36) {
		t.Errorf("filtered Sum = %#v", filtered)
	}
}

func TestAggregateOverNoRowsIsNil(t *testing.T) {
	_, user := usersFixture(t)
	got, err := user.Sum(context.Background(), "age", Query{Where: Op.Eq("name", "nobody")})
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got != nil {
		t.Errorf("Sum over no rows = %#v, want nil", got)
	}
}

func TestAggregateRejectsUnknownAttribute(t *testing.T) {
	_, user := usersFixture(t)
	if _, err := user.Sum(context.Background(), "nope", Query{}); !errors.Is(err, ErrUnknownAttribute) {
		t.Errorf("Sum(nope) = %v, want ErrUnknownAttribute", err)
	}
}

// TestFindOrCreateReportsCreation is the parity case: the bool says whether the
// row was created, and the second call must find rather than create.
func TestFindOrCreateReportsCreation(t *testing.T) {
	_, user := usersFixture(t)
	ctx := context.Background()

	row, created, err := user.FindOrCreate(ctx,
		Query{Where: Op.Eq("email", "hopper@example.com")},
		Values{"name": "hopper", "age": 85})
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	if !created {
		t.Fatal("FindOrCreate reported created=false for a row that did not exist")
	}
	if row["name"] != "hopper" || row["email"] != "hopper@example.com" {
		t.Errorf("created row = %v, want the filter merged with the defaults", row)
	}

	again, created, err := user.FindOrCreate(ctx,
		Query{Where: Op.Eq("email", "hopper@example.com")},
		Values{"name": "somebody else"})
	if err != nil {
		t.Fatalf("FindOrCreate: %v", err)
	}
	if created {
		t.Fatal("FindOrCreate created a second row")
	}
	if again["name"] != "hopper" {
		t.Errorf("found row = %v, want the stored row untouched", again)
	}

	total, err := user.Count(ctx, Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 4 {
		t.Errorf("Count = %d, want exactly one new row", total)
	}
}

func TestFindOrCreateInsideACallerTransaction(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	err := db.Transaction(ctx, func(tx *Tx) error {
		_, created, err := user.FindOrCreate(ctx,
			Query{Where: Op.Eq("email", "tx@example.com"), Tx: tx},
			Values{"name": "txuser"})
		if err != nil {
			return err
		}
		if !created {
			t.Error("FindOrCreate inside a transaction did not create")
		}
		return errors.New("roll back please")
	})
	if err == nil {
		t.Fatal("Transaction returned nil, want the propagated error")
	}
	if n, err := user.Count(ctx, Query{Where: Op.Eq("name", "txuser")}); err != nil || n != 0 {
		t.Errorf("Count = %d, %v; want the creation rolled back", n, err)
	}
}

func TestEqualityValuesOnlyTakesImpliedPairs(t *testing.T) {
	got := equalityValues(And(
		Op.Eq("a", 1),
		Op.Gt("b", 2),
		Or(Op.Eq("c", 3), Op.Eq("d", 4)),
	))
	if len(got) != 1 || got["a"] != 1 {
		t.Errorf("equalityValues = %v, want only the ANDed equality", got)
	}
	if got := equalityValues(nil); len(got) != 0 {
		t.Errorf("equalityValues(nil) = %v", got)
	}
}

func TestFindersRejectAForeignTransaction(t *testing.T) {
	_, user := usersFixture(t)
	other := newTestDB(t)
	ctx := context.Background()
	tx, err := other.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := user.FindAll(ctx, Query{Tx: tx}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("FindAll with a foreign Tx = %v, want ErrInvalidQuery", err)
	}
}
