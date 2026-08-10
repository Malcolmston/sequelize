package sequelize

import (
	"context"
	"errors"
	"testing"
)

func TestQueryTypeString(t *testing.T) {
	cases := map[QueryType]string{
		QueryTypeAuto:   "AUTO",
		QueryTypeSelect: "SELECT",
		QueryTypeInsert: "INSERT",
		QueryTypeUpdate: "UPDATE",
		QueryTypeDelete: "DELETE",
		QueryTypeRaw:    "RAW",
	}
	for qt, want := range cases {
		if got := qt.String(); got != want {
			t.Errorf("QueryType(%d).String() = %q, want %q", qt, got, want)
		}
	}
	if got := QueryType(99).String(); got != "AUTO" {
		t.Errorf("unknown QueryType String() = %q", got)
	}
}

func TestDetectQueryType(t *testing.T) {
	cases := map[string]QueryType{
		"SELECT 1":                      QueryTypeSelect,
		"  select * from t":             QueryTypeSelect,
		"(SELECT 1)":                    QueryTypeSelect,
		"WITH x AS (SELECT 1) SELECT 1": QueryTypeSelect,
		"PRAGMA table_info(t)":          QueryTypeSelect,
		"INSERT INTO t VALUES (1)":      QueryTypeInsert,
		"UPDATE t SET a = 1":            QueryTypeUpdate,
		"DELETE FROM t":                 QueryTypeDelete,
		"CREATE TABLE t (a int)":        QueryTypeRaw,
		"VACUUM":                        QueryTypeRaw,
	}
	for query, want := range cases {
		if got := detectQueryType(query); got != want {
			t.Errorf("detectQueryType(%q) = %v, want %v", query, got, want)
		}
	}
}

func TestQueryReturnsRows(t *testing.T) {
	db, _ := usersFixture(t)
	res, err := db.Query(context.Background(),
		`SELECT name, age FROM users WHERE age > ? ORDER BY age`, WithArgs(40))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Columns) != 2 || res.Columns[0] != "name" {
		t.Errorf("Columns = %v", res.Columns)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("Rows = %v, want 2", res.Rows)
	}
	if res.Rows[0]["name"] != "alan" {
		t.Errorf("first row = %v", res.Rows[0])
	}
	if res.Rows[0]["age"] != int64(41) {
		t.Errorf("age = %#v, want the driver's int64", res.Rows[0]["age"])
	}
}

func TestQueryEmptyResultIsAnEmptySlice(t *testing.T) {
	db, _ := usersFixture(t)
	res, err := db.Query(context.Background(), `SELECT name FROM users WHERE name = ?`, WithArgs("nobody"))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Rows == nil || len(res.Rows) != 0 {
		t.Errorf("Rows = %v, want an empty slice", res.Rows)
	}
}

func TestQueryRunsStatements(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()

	ins, err := db.Query(ctx, `INSERT INTO users (name, created_at, updated_at) VALUES (?, ?, ?)`,
		WithArgs("raw", "2024-01-01T00:00:00.000000Z", "2024-01-01T00:00:00.000000Z"))
	if err != nil {
		t.Fatalf("Query(insert): %v", err)
	}
	if ins.RowsAffected != 1 {
		t.Errorf("RowsAffected = %d, want 1", ins.RowsAffected)
	}
	if ins.LastInsertID == 0 {
		t.Error("LastInsertID = 0, want the generated key")
	}

	upd, err := db.Query(ctx, `UPDATE users SET age = ? WHERE name = ?`, WithArgs(1, "raw"))
	if err != nil {
		t.Fatalf("Query(update): %v", err)
	}
	if upd.RowsAffected != 1 {
		t.Errorf("RowsAffected = %d, want 1", upd.RowsAffected)
	}

	del, err := db.Query(ctx, `DELETE FROM users WHERE name = ?`, WithArgs("raw"))
	if err != nil {
		t.Fatalf("Query(delete): %v", err)
	}
	if del.RowsAffected != 1 {
		t.Errorf("RowsAffected = %d, want 1", del.RowsAffected)
	}
	if n, err := user.Count(ctx, Query{}); err != nil || n != 3 {
		t.Errorf("Count = %d, %v; want the fixture rows only", n, err)
	}
}

func TestQueryRunsDDL(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Query(ctx, `CREATE TABLE scratch (a INTEGER)`); err != nil {
		t.Fatalf("Query(DDL): %v", err)
	}
	if _, err := db.Query(ctx, `INSERT INTO scratch (a) VALUES (1)`); err != nil {
		t.Fatalf("Query(insert): %v", err)
	}
	res, err := db.Query(ctx, `SELECT a FROM scratch`)
	if err != nil {
		t.Fatalf("Query(select): %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["a"] != int64(1) {
		t.Errorf("Rows = %v", res.Rows)
	}
}

func TestQueryWithExplicitType(t *testing.T) {
	db, _ := usersFixture(t)
	// Forcing a SELECT to run as a statement collects no rows.
	res, err := db.Query(context.Background(), `SELECT name FROM users`, WithQueryType(QueryTypeRaw))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("Rows = %v, want none for a statement", res.Rows)
	}
}

func TestQueryInsideATransaction(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := db.Query(ctx, `DELETE FROM users`, WithTx(tx)); err != nil {
		t.Fatalf("Query: %v", err)
	}
	res, err := db.Query(ctx, `SELECT COUNT(*) AS n FROM users`, WithTx(tx))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Rows[0]["n"] != int64(0) {
		t.Errorf("inside the transaction the delete is not visible: %v", res.Rows)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if n, err := user.Count(ctx, Query{}); err != nil || n != 3 {
		t.Errorf("Count = %d, %v; want the rollback to have restored the rows", n, err)
	}
	if _, err := db.Query(ctx, `SELECT 1`, WithTx(tx)); !errors.Is(err, ErrClosed) {
		t.Errorf("Query on a finished Tx = %v, want ErrClosed", err)
	}
}

func TestQueryReportsDriverErrors(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.Query(ctx, `SELECT * FROM nope`); err == nil {
		t.Error("Query against a missing table succeeded")
	}
	if _, err := db.Query(ctx, `DELETE FROM nope`); err == nil {
		t.Error("statement against a missing table succeeded")
	}
}

func TestQueryTranslatesConstraintErrors(t *testing.T) {
	db, _ := usersFixture(t)
	_, err := db.Query(context.Background(),
		`INSERT INTO users (name, email, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		WithArgs("dup", "ada@example.com", "2024-01-01T00:00:00.000000Z", "2024-01-01T00:00:00.000000Z"))
	if !errors.Is(err, ErrUniqueConstraint) {
		t.Errorf("Query = %v, want ErrUniqueConstraint", err)
	}
}

func TestWithArgsAccumulates(t *testing.T) {
	var o RawOptions
	WithArgs(1)(&o)
	WithArgs(2, 3)(&o)
	if len(o.Args) != 3 {
		t.Errorf("Args = %v, want three of them", o.Args)
	}
}

func TestQueryIgnoresNilOptions(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Query(context.Background(), "SELECT 1", nil); err != nil {
		t.Errorf("Query with a nil option = %v", err)
	}
}
