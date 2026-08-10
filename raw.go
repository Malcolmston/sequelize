package sequelize

import (
	"context"
	"strings"
)

// QueryType tells [Sequelize.Query] how to run a raw statement: whether to
// collect rows or to report an affected-row count.
type QueryType int

// The raw query types.
const (
	// QueryTypeAuto picks between a row-returning query and a statement by
	// looking at the statement's leading keyword. It is the default.
	QueryTypeAuto QueryType = iota
	// QueryTypeSelect collects rows.
	QueryTypeSelect
	// QueryTypeInsert runs a statement and reports RowsAffected and LastInsertID.
	QueryTypeInsert
	// QueryTypeUpdate runs a statement and reports RowsAffected.
	QueryTypeUpdate
	// QueryTypeDelete runs a statement and reports RowsAffected.
	QueryTypeDelete
	// QueryTypeRaw runs a statement and reports nothing but success, which is
	// what DDL needs.
	QueryTypeRaw
)

// String returns the type's name.
func (t QueryType) String() string {
	switch t {
	case QueryTypeSelect:
		return "SELECT"
	case QueryTypeInsert:
		return "INSERT"
	case QueryTypeUpdate:
		return "UPDATE"
	case QueryTypeDelete:
		return "DELETE"
	case QueryTypeRaw:
		return "RAW"
	default:
		return "AUTO"
	}
}

// returnsRows reports whether the type collects rows.
func (t QueryType) returnsRows() bool { return t == QueryTypeSelect }

// RawOptions carries the settings a [RawOption] sets.
type RawOptions struct {
	// Args are the bind arguments for the statement's placeholders.
	Args []any
	// Type selects how the statement is run; the zero value is [QueryTypeAuto].
	Type QueryType
	// Tx runs the statement inside a transaction.
	Tx *Tx
}

// RawOption configures [Sequelize.Query].
type RawOption func(*RawOptions)

// WithArgs supplies the bind arguments for the statement's placeholders.
func WithArgs(args ...any) RawOption {
	return func(o *RawOptions) { o.Args = append(o.Args, args...) }
}

// WithQueryType overrides the automatic choice between collecting rows and
// running a statement.
func WithQueryType(t QueryType) RawOption {
	return func(o *RawOptions) { o.Type = t }
}

// WithTx runs the statement inside tx.
func WithTx(tx *Tx) RawOption {
	return func(o *RawOptions) { o.Tx = tx }
}

// RawResult is what [Sequelize.Query] returns.
type RawResult struct {
	// Columns names the projected columns, in order, for a row-returning query.
	Columns []string
	// Rows holds the result rows, keyed by column name as the driver reported it.
	Rows []Values
	// RowsAffected is the driver's count for a statement, or 0 when it does not
	// report one.
	RowsAffected int64
	// LastInsertID is the driver's generated key for an insert, or 0.
	LastInsertID int64
}

// Query runs raw SQL. It is the escape hatch for everything the model API cannot
// express, and the only place where the SQL text is the caller's responsibility.
//
// Values still belong in [WithArgs], not in the statement: the placeholders are
// whatever the dialect uses, "?" for SQLite. Rows come back keyed by the column
// names the driver reports, with no model to say how to type them, so a value is
// whatever the driver produced except that []byte is converted to string.
//
// The statement's leading keyword decides whether rows are collected; override
// that with [WithQueryType].
func (s *Sequelize) Query(ctx context.Context, query string, opts ...RawOption) (*RawResult, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	var o RawOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.Type == QueryTypeAuto {
		o.Type = detectQueryType(query)
	}
	var querier Querier = s.db
	if o.Tx != nil {
		if o.Tx.Done() {
			return nil, ErrClosed
		}
		querier = o.Tx.tx
	}
	s.log(ctx, query, o.Args)

	if !o.Type.returnsRows() {
		res, err := querier.ExecContext(ctx, query, o.Args...)
		if err != nil {
			return nil, s.dialect.TranslateError(err)
		}
		out := &RawResult{}
		if n, err := res.RowsAffected(); err == nil {
			out.RowsAffected = n
		}
		if o.Type == QueryTypeInsert {
			if id, err := res.LastInsertId(); err == nil {
				out.LastInsertID = id
			}
		}
		return out, nil
	}

	rows, err := querier.QueryContext(ctx, query, o.Args...)
	if err != nil {
		return nil, s.dialect.TranslateError(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := &RawResult{Columns: cols, Rows: []Values{}}
	for rows.Next() {
		raw := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(Values, len(cols))
		for i, col := range cols {
			if b, ok := raw[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = raw[i]
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out, rows.Err()
}

// detectQueryType guesses a statement's type from its first keyword. Anything
// that reads is treated as a SELECT; anything else is run as a statement.
func detectQueryType(query string) QueryType {
	trimmed := strings.TrimLeft(query, " \t\r\n(")
	end := strings.IndexAny(trimmed, " \t\r\n(")
	if end < 0 {
		end = len(trimmed)
	}
	switch strings.ToUpper(trimmed[:end]) {
	case "SELECT", "WITH", "PRAGMA", "SHOW", "EXPLAIN", "VALUES", "TABLE":
		return QueryTypeSelect
	case "INSERT":
		return QueryTypeInsert
	case "UPDATE":
		return QueryTypeUpdate
	case "DELETE":
		return QueryTypeDelete
	default:
		return QueryTypeRaw
	}
}
