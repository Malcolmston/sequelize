package sequelize

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// Querier is the subset of database/sql that the port executes through. Both
// *sql.DB and *sql.Tx satisfy it, which is how a [Query] can be aimed at either
// by setting [Query.Tx].
type Querier interface {
	// ExecContext runs a statement that returns no rows.
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	// QueryContext runs a statement that returns rows.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Transactionable is satisfied by *Sequelize and *Tx: anything that can hand out
// a [Querier] and say whether it is inside a transaction. It is the plumbing that
// lets the finders and mutations be written once.
type Transactionable interface {
	// Querier returns the handle statements should run against.
	Querier() Querier
	// InTransaction reports whether that handle is a transaction.
	InTransaction() bool
	// Connection returns the owning connection.
	Connection() *Sequelize
}

// Querier returns the connection's *sql.DB.
func (s *Sequelize) Querier() Querier { return s.db }

// InTransaction reports false: a bare connection is not a transaction.
func (s *Sequelize) InTransaction() bool { return false }

// Connection returns s.
func (s *Sequelize) Connection() *Sequelize { return s }

// txConfig holds the settings a [TxOption] may change.
type txConfig struct {
	isolation sql.IsolationLevel
	readOnly  bool
}

// TxOption configures a transaction at begin time.
type TxOption func(*txConfig)

// WithIsolation sets the transaction's isolation level. Levels a driver cannot
// honour make BeginTx fail; sql.LevelDefault leaves it to the database.
func WithIsolation(level sql.IsolationLevel) TxOption {
	return func(c *txConfig) { c.isolation = level }
}

// WithReadOnly marks the transaction read-only, when the driver supports it.
func WithReadOnly(readOnly bool) TxOption {
	return func(c *txConfig) { c.readOnly = readOnly }
}

// Tx is an open transaction, or a savepoint inside one. Pass it to an operation
// through [Query.Tx] so the statement joins the transaction.
//
// A Tx is single-use: after [Tx.Commit] or [Tx.Rollback], every method returns
// [ErrClosed].
type Tx struct {
	seq  *Sequelize
	tx   *sql.Tx
	root *Tx // nil on the outermost transaction

	mu        sync.Mutex
	done      bool
	savepoint string
	counter   int
}

// Begin starts a transaction. The caller is responsible for calling
// [Tx.Commit] or [Tx.Rollback]; prefer [Sequelize.Transaction], which does that
// for you.
func (s *Sequelize) Begin(ctx context.Context, opts ...TxOption) (*Tx, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	var cfg txConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: cfg.isolation, ReadOnly: cfg.readOnly})
	if err != nil {
		return nil, err
	}
	return &Tx{seq: s, tx: tx}, nil
}

// Transaction runs fn inside a transaction, committing when fn returns nil and
// rolling back when it returns an error or panics. The panic is re-raised after
// the rollback.
//
// A commit failure is returned as-is; a rollback failure is joined onto fn's
// error so neither is lost.
func (s *Sequelize) Transaction(ctx context.Context, fn func(tx *Tx) error, opts ...TxOption) error {
	tx, err := s.Begin(ctx, opts...)
	if err != nil {
		return err
	}
	return runInTx(tx, fn)
}

// Transaction runs fn inside a savepoint nested in t, releasing the savepoint
// when fn returns nil and rolling back to it otherwise. The enclosing
// transaction survives a failed nested block.
func (t *Tx) Transaction(ctx context.Context, fn func(tx *Tx) error) error {
	child, err := t.Savepoint(ctx)
	if err != nil {
		return err
	}
	return runInTx(child, fn)
}

func runInTx(tx *Tx, fn func(tx *Tx) error) (err error) {
	panicked := true
	defer func() {
		if panicked {
			_ = tx.Rollback()
		}
	}()
	err = fn(tx)
	panicked = false
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback failed: %v)", err, rbErr)
		}
		return err
	}
	return tx.Commit()
}

// Savepoint opens a nested transaction inside t and returns it. Committing the
// returned Tx releases the savepoint; rolling it back returns to it.
func (t *Tx) Savepoint(ctx context.Context) (*Tx, error) {
	root := t.rootTx()
	root.mu.Lock()
	if root.done {
		root.mu.Unlock()
		return nil, ErrClosed
	}
	root.counter++
	name := fmt.Sprintf("sequelize_sp_%d", root.counter)
	root.mu.Unlock()

	// The name is generated here and matches the identifier rule by construction;
	// validate anyway so the rule holds for every identifier we emit.
	if err := ValidateIdentifier(name); err != nil {
		return nil, err
	}
	child := &Tx{seq: t.seq, tx: t.tx, root: root, savepoint: name}
	if _, err := child.exec(ctx, "SAVEPOINT "+name); err != nil {
		return nil, err
	}
	return child, nil
}

// Commit commits the transaction, or releases the savepoint when t is a nested
// transaction.
func (t *Tx) Commit() error {
	return t.finish(func() error {
		if t.savepoint != "" {
			_, err := t.exec(context.Background(), "RELEASE SAVEPOINT "+t.savepoint)
			return err
		}
		return t.tx.Commit()
	})
}

// Rollback rolls the transaction back, or rolls back to the savepoint when t is
// a nested transaction.
func (t *Tx) Rollback() error {
	return t.finish(func() error {
		if t.savepoint != "" {
			if _, err := t.exec(context.Background(), "ROLLBACK TO SAVEPOINT "+t.savepoint); err != nil {
				return err
			}
			_, err := t.exec(context.Background(), "RELEASE SAVEPOINT "+t.savepoint)
			return err
		}
		return t.tx.Rollback()
	})
}

func (t *Tx) finish(fn func() error) error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return ErrClosed
	}
	t.done = true
	t.mu.Unlock()
	return fn()
}

// Done reports whether t has been committed or rolled back.
func (t *Tx) Done() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// Querier returns the underlying *sql.Tx.
func (t *Tx) Querier() Querier { return t.tx }

// InTransaction reports true.
func (t *Tx) InTransaction() bool { return true }

// Connection returns the connection the transaction was begun on.
func (t *Tx) Connection() *Sequelize { return t.seq }

// SQLTx exposes the underlying *sql.Tx, for the things this package does not do.
func (t *Tx) SQLTx() *sql.Tx { return t.tx }

func (t *Tx) rootTx() *Tx {
	if t.root != nil {
		return t.root
	}
	return t
}

// exec runs a savepoint control statement, logging it like any other.
func (t *Tx) exec(ctx context.Context, query string) (sql.Result, error) {
	t.seq.log(ctx, query, nil)
	return t.tx.ExecContext(ctx, query)
}

// querierFor picks the handle a query should run against: its transaction when
// one is set, the connection otherwise.
func (m *Model) querierFor(q Query) (Querier, error) {
	if q.Tx != nil {
		if q.Tx.Done() {
			return nil, ErrClosed
		}
		if q.Tx.seq != m.seq {
			return nil, fmt.Errorf("%w: transaction belongs to another connection", ErrInvalidQuery)
		}
		return q.Tx.tx, nil
	}
	if err := m.seq.checkOpen(); err != nil {
		return nil, err
	}
	return m.seq.db, nil
}
