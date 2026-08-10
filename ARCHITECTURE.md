# Architecture

A dependency-light Go port of [Sequelize](https://github.com/sequelize/sequelize).
This file is the contract every contributor (human or agent) works to: it fixes
the package layout, the file ownership, and the core types, so independent work
composes instead of colliding.

## Non-negotiables

- **Module path** `github.com/malcolmston/sequelize`, Go 1.24.7. Flat package
  `sequelize` at the repo root — no `internal/` split, no subpackages in v0.1.0.
  A user writes `sequelize.Define(...)`, mirroring `sequelize.define(...)`.
- **No cgo.** The only permitted third-party dependency is the pure-Go SQLite
  driver `modernc.org/sqlite`, and only as a *test* dependency plus the optional
  `sqlite` dialect registration. The core must compile with the standard library
  alone, against any `database/sql` driver the caller registers.
- **`database/sql` underneath, always.** Never open a connection yourself; take a
  `*sql.DB` or a DSN and hand it to `sql.Open`. Every query goes through
  `QueryContext`/`ExecContext` with the caller's `context.Context`.
- **Parameterised SQL only.** Never interpolate a user value into SQL text. Every
  value becomes a placeholder plus an arg. Identifiers are quoted by the dialect
  and validated against `^[A-Za-z_][A-Za-z0-9_]*$`; anything else is an error, not
  an escape attempt. This is the one rule with no exceptions — a port that lets a
  column name reach the database unquoted is an injection vector.
- **Errors are typed and wrapped.** Every failure is one of the sentinels in
  `errors.go`, testable with `errors.Is`. Never return a bare `fmt.Errorf` string.
- **Upstream is the spec.** Where Go forces a difference, implement the closest
  idiomatic thing and record it in `API-DEVIATIONS.md` with the reason. Do not
  invent API that upstream lacks without marking it clearly as an extension.

## File ownership

One concern per file. Do not edit a file you do not own; add to your own and
depend on the exported surface of others.

| File | Owns |
| --- | --- |
| `doc.go` | package doc, the worked example |
| `errors.go` | sentinels, `ValidationError`, `UniqueConstraintError`, `ForeignKeyError`, `RecordNotFoundError` |
| `datatypes.go` | `DataType` and the constructors: `INTEGER`, `BIGINT`, `FLOAT`, `DECIMAL`, `STRING(n)`, `TEXT`, `BOOLEAN`, `DATE`, `DATEONLY`, `JSON`, `BLOB`, `UUID`, `ENUM(...)` |
| `dialect.go` | `Dialect` interface, identifier quoting, placeholder style, type mapping, `Register`/lookup |
| `dialect_sqlite.go` | the SQLite dialect |
| `model.go` | `Model`, `Attribute`, `ModelOptions`, attribute resolution, primary keys, timestamps |
| `define.go` | `New`, `Sequelize`, `Define`, model registry |
| `operators.go` | the `Op` vocabulary and `Where` clause tree |
| `builder.go` | SELECT/INSERT/UPDATE/DELETE generation from a `Query` |
| `finders.go` | `FindAll`, `FindOne`, `FindByPk`, `FindOrCreate`, `FindAndCountAll`, `Count`, `Sum`, `Min`, `Max` |
| `mutations.go` | `Create`, `BulkCreate`, `Update`, `Destroy`, `Upsert`, `Increment`, `Decrement`, `Restore` |
| `associations.go` | `HasOne`, `HasMany`, `BelongsTo`, `BelongsToMany`, `Include` eager loading |
| `transaction.go` | `Transaction`, savepoints, isolation levels, the `Transactionable` plumbing |
| `hooks.go` | the hook registry and dispatch |
| `validation.go` | attribute validators and `Model.Validate` |
| `sync.go` | DDL generation, `Sync`, `Drop`, `Truncate`, index creation |
| `raw.go` | `Query` for raw SQL with `QueryType` |
| `scopes.go` | default and named scopes, `Scope`, `Unscoped` |

Tests live beside their file as `<file>_test.go`. Every file needs an
`Example*` function in `doc_example_test.go` if it adds user-facing API.

## Core types

These signatures are fixed — build against them, do not redesign them.

```go
// Values is an attribute-name-keyed record. Sequelize is dynamically typed, so
// the port is map-based rather than struct-tag based; this is the single biggest
// deliberate deviation and is recorded in API-DEVIATIONS.md.
type Values map[string]any

type Sequelize struct { /* db, dialect, models, logger */ }

func New(dialect, dsn string, opts ...Option) (*Sequelize, error)
func Open(dialect string, db *sql.DB, opts ...Option) (*Sequelize, error)
func (s *Sequelize) Define(name string, attrs Attributes, opts ...ModelOption) *Model
func (s *Sequelize) Model(name string) (*Model, bool)
func (s *Sequelize) Sync(ctx context.Context, opts ...SyncOption) error
func (s *Sequelize) Transaction(ctx context.Context, fn func(tx *Tx) error) error
func (s *Sequelize) Close() error

type Attributes map[string]Attribute

type Attribute struct {
	Type          DataType
	AllowNull     bool // mirrors upstream: default is nullable, so the zero value is nullable
	PrimaryKey    bool
	AutoIncrement bool
	Unique        bool
	DefaultValue  any
	Field         string   // physical column name; defaults to the snake_cased key
	References    *Reference
	Validate      []Validator
	Comment       string
}

// Query is what every finder and mutation is built from.
type Query struct {
	Where    *Clause
	Order    []OrderTerm
	Limit    *int
	Offset   *int
	Attrs    []string
	Group    []string
	Having   *Clause
	Include  []Include
	Distinct bool
	Paranoid *bool
	Tx       *Tx
}
```

## Semantics that must match upstream

Get these right; they are where ports usually drift, and each one is a parity case.

- **`AllowNull` defaults to true.** Upstream columns are nullable unless told
  otherwise. The Go zero value must therefore mean *nullable*.
- **Timestamps on by default.** `createdAt`/`updatedAt` are added unless
  `Timestamps(false)`. `updatedAt` changes on every update, `createdAt` never does.
- **Paranoid deletes.** With `Paranoid(true)`, `Destroy` sets `deletedAt` and every
  finder adds `deletedAt IS NULL` unless `Paranoid(false)` is passed on the query.
  `Restore` clears it.
- **Table naming.** Upstream pluralises the model name for the table and
  camelCases attributes to columns; be explicit about the rule you implement and
  record it. `TableName(...)` and `Attribute.Field` must override it.
- **`FindOrCreate` is atomic** — inside a transaction, and it reports whether it
  created.
- **`Upsert` reports whether it inserted.**
- **Validation runs before write** on `Create`/`Update`/`BulkCreate`, and a
  failure returns a `*ValidationError` carrying every failed attribute, not just
  the first.
- **Hooks fire in upstream's order**: `beforeValidate`, `afterValidate`,
  `beforeCreate`/`beforeUpdate`/`beforeDestroy`, then the `after*` counterpart.
  A hook returning an error aborts the operation and rolls back its transaction.
- **Eager loading must not N+1.** `Include` for a `BelongsTo`/`HasOne` is a JOIN;
  for `HasMany`/`BelongsToMany` it is one extra batched query per association
  using `IN (...)`. Never one query per parent row.
- **`Op.In` with an empty slice** must produce a clause that matches nothing, not
  invalid SQL.
- **`Count` with `Include` and `Distinct`** counts distinct parents, not joined rows.

## Definition of done for v0.1.0

`go build ./...` and `go test ./...` clean; `go vet` clean; every exported symbol
documented; `README.md` quick-start compiles verbatim; `API-DEVIATIONS.md` lists
every intentional difference; a runnable `examples/sequelize` module; and a
`parity/sequelize` harness scoring against real `sequelize` on SQLite.
