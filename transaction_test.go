package sequelize

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestTransactionCommits(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	err := db.Transaction(ctx, func(tx *Tx) error {
		if !tx.InTransaction() {
			t.Error("InTransaction() = false inside a transaction")
		}
		if tx.Connection() != db {
			t.Error("Connection() did not return the owning connection")
		}
		if tx.SQLTx() == nil {
			t.Error("SQLTx() = nil")
		}
		_, err := user.Create(ctx, Values{"name": "committed", "email": "c@example.com"}, Query{Tx: tx})
		return err
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if n, err := user.Count(ctx, Query{Where: Op.Eq("name", "committed")}); err != nil || n != 1 {
		t.Errorf("Count = %d, %v; want the committed row", n, err)
	}
}

func TestTransactionRollsBackOnError(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	sentinel := errors.New("nope")
	err := db.Transaction(ctx, func(tx *Tx) error {
		if _, err := user.Create(ctx, Values{"name": "doomed", "email": "d@example.com"}, Query{Tx: tx}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Transaction = %v, want the function's own error", err)
	}
	if n, err := user.Count(ctx, Query{Where: Op.Eq("name", "doomed")}); err != nil || n != 0 {
		t.Errorf("Count = %d, %v; want the write rolled back", n, err)
	}
}

func TestTransactionRollsBackOnPanic(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed, want it re-raised")
			}
		}()
		_ = db.Transaction(ctx, func(tx *Tx) error {
			if _, err := user.Create(ctx, Values{"name": "panicky", "email": "p@example.com"}, Query{Tx: tx}); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	if n, err := user.Count(ctx, Query{Where: Op.Eq("name", "panicky")}); err != nil || n != 0 {
		t.Errorf("Count = %d, %v; want the write rolled back", n, err)
	}
}

func TestTransactionIsolatesUncommittedWrites(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := user.Create(ctx, Values{"name": "pending", "email": "q@example.com"}, Query{Tx: tx}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	inside, err := user.Count(ctx, Query{Tx: tx})
	if err != nil {
		t.Fatalf("Count inside: %v", err)
	}
	if inside != 4 {
		t.Errorf("Count inside the transaction = %d, want 4", inside)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	after, err := user.Count(ctx, Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if after != 3 {
		t.Errorf("Count after rollback = %d, want 3", after)
	}
}

func TestTxIsSingleUse(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if tx.Done() {
		t.Error("Done() = true on a fresh transaction")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !tx.Done() {
		t.Error("Done() = false after Commit")
	}
	if err := tx.Commit(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Commit = %v, want ErrClosed", err)
	}
	if err := tx.Rollback(); !errors.Is(err, ErrClosed) {
		t.Errorf("Rollback after Commit = %v, want ErrClosed", err)
	}
	if _, err := user.FindAll(ctx, Query{Tx: tx}); !errors.Is(err, ErrClosed) {
		t.Errorf("FindAll on a finished Tx = %v, want ErrClosed", err)
	}
}

func TestSavepointRollsBackOnlyTheNestedBlock(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	err := db.Transaction(ctx, func(tx *Tx) error {
		if _, err := user.Create(ctx, Values{"name": "outer", "email": "o@example.com"}, Query{Tx: tx}); err != nil {
			return err
		}
		inner := tx.Transaction(ctx, func(nested *Tx) error {
			if _, err := user.Create(ctx, Values{"name": "inner", "email": "i@example.com"}, Query{Tx: nested}); err != nil {
				return err
			}
			return errors.New("abandon the nested block")
		})
		if inner == nil {
			t.Error("nested Transaction returned nil, want the propagated error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if n, err := user.Count(ctx, Query{Where: Op.Eq("name", "outer")}); err != nil || n != 1 {
		t.Errorf("outer row count = %d, %v; want it committed", n, err)
	}
	if n, err := user.Count(ctx, Query{Where: Op.Eq("name", "inner")}); err != nil || n != 0 {
		t.Errorf("inner row count = %d, %v; want it rolled back to the savepoint", n, err)
	}
}

func TestSavepointCommitKeepsTheNestedWrite(t *testing.T) {
	db, user := usersFixture(t)
	ctx := context.Background()
	err := db.Transaction(ctx, func(tx *Tx) error {
		return tx.Transaction(ctx, func(nested *Tx) error {
			_, err := user.Create(ctx, Values{"name": "nested", "email": "n@example.com"}, Query{Tx: nested})
			return err
		})
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if n, err := user.Count(ctx, Query{Where: Op.Eq("name", "nested")}); err != nil || n != 1 {
		t.Errorf("Count = %d, %v; want the released savepoint's write", n, err)
	}
}

func TestSavepointNamesAreDistinct(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	first, err := tx.Savepoint(ctx)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}
	second, err := tx.Savepoint(ctx)
	if err != nil {
		t.Fatalf("Savepoint: %v", err)
	}
	if first.savepoint == second.savepoint {
		t.Errorf("two savepoints share the name %q", first.savepoint)
	}
	if err := ValidateIdentifier(first.savepoint); err != nil {
		t.Errorf("generated savepoint name is not a legal identifier: %v", err)
	}
	// A savepoint nested inside a savepoint counts from the outermost transaction.
	third, err := second.Savepoint(ctx)
	if err != nil {
		t.Fatalf("nested Savepoint: %v", err)
	}
	if third.rootTx() != tx {
		t.Error("a nested savepoint did not resolve to the outermost transaction")
	}
	if err := second.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := first.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestSavepointOnAFinishedTransaction(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := tx.Savepoint(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Savepoint on a finished Tx = %v, want ErrClosed", err)
	}
}

func TestTransactionOptions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx, WithIsolation(sql.LevelSerializable), WithReadOnly(false), nil)
	if err != nil {
		t.Fatalf("Begin with options: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback: %v", err)
	}
}

func TestBeginOnAClosedConnection(t *testing.T) {
	db := newTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.Begin(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Begin after Close = %v, want ErrClosed", err)
	}
	// The fixture's cleanup calls Close again, which must stay a no-op.
}

func TestTransactionableIsSatisfiedByBoth(t *testing.T) {
	db := newTestDB(t)
	var conn Transactionable = db
	if conn.InTransaction() {
		t.Error("a bare connection reports InTransaction() = true")
	}
	if conn.Querier() == nil {
		t.Error("Querier() = nil")
	}
	if conn.Connection() != db {
		t.Error("Connection() did not return the connection")
	}
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	var inTx Transactionable = tx
	if !inTx.InTransaction() {
		t.Error("a Tx reports InTransaction() = false")
	}
	if inTx.Querier() == nil {
		t.Error("Tx.Querier() = nil")
	}
}
