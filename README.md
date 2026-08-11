# sequelize

[![Release](https://img.shields.io/github/v/tag/malcolmston/sequelize?sort=semver&label=release)](https://github.com/malcolmston/sequelize/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/malcolmston/sequelize.svg)](https://pkg.go.dev/github.com/malcolmston/sequelize)
[![Go Version](https://img.shields.io/github/go-mod/go-version/malcolmston/sequelize)](go.mod)
[![License: MIT](https://img.shields.io/github/license/malcolmston/sequelize)](LICENSE)
[![Parity](https://img.shields.io/badge/parity-100%25%20of%2058%20compared%20cases-brightgreen)](https://github.com/malcolmston/go/tree/main/parity/sequelize)
[![Docs](https://img.shields.io/badge/docs-vercel-2f9bff)](https://go-malcolms-projects-18e573c3.vercel.app)

**Node's Sequelize ORM, for Go — on nothing but `database/sql`.**

`sequelize` is a Go port of [Sequelize](https://sequelize.org/). It keeps
upstream's shape: you `Define` models on a connection, `Sync` their tables, and
then `Create`, `Find`, `Update` and `Destroy` through the model. Filters are
`Op` trees, transactions are a closure, and every failure is a typed error you
branch on with `errors.Is`.

Because Sequelize is dynamically typed, a record here is a **`Values`** — a
`map[string]any` keyed by attribute name — rather than a tagged struct. That is
the port's largest deliberate deviation, and it is recorded, with every other
one, in [`API-DEVIATIONS.md`](API-DEVIATIONS.md).

The package itself imports **only the standard library**. It ships no driver:
which `database/sql` driver backs a dialect is your choice. (`go test` here uses
the pure-Go [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite),
which is why it is the module's one non-stdlib requirement.)

## Install

```sh
go get github.com/malcolmston/sequelize@v0.1.0
```

## Quick start

Register a driver, hand its DSN to `New`, define a model, sync it:

```go
import (
	"context"

	"github.com/malcolmston/sequelize"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

db, err := sequelize.New("sqlite", "file:app.db")
if err != nil {
	return err
}
defer db.Close()

// Only Type is required. As upstream, an attribute is nullable unless you ask
// for NotNull(); a model with no declared primary key gets an auto-increment
// "id"; createdAt and updatedAt are managed for you.
User := db.Define("User", sequelize.Attributes{
	"name":  {Type: sequelize.STRING(64), AllowNull: sequelize.NotNull()},
	"email": {Type: sequelize.STRING(255), Unique: true},
	"age":   {Type: sequelize.INTEGER()},
})
if err := db.Sync(context.Background()); err != nil {
	return err
}
```

Then read and write through the model:

```go
rows, err := User.FindAll(ctx, sequelize.Query{
	Where: sequelize.And(
		sequelize.Op.Gte("age", 18),
		sequelize.Op.In("name", []string{"ada", "grace"}),
	),
	Order: []sequelize.OrderTerm{sequelize.Desc("age")},
	Limit: sequelize.Int(10),
})

changed, err := User.Update(ctx, sequelize.Values{"age": 41},
	sequelize.Query{Where: sequelize.Op.Eq("name", "ada")})

if _, err := User.FindByPk(ctx, 42); errors.Is(err, sequelize.ErrRecordNotFound) {
	// no such user
}
```

`Op.In` with an empty set matches nothing rather than emitting invalid SQL, so a
filter result is safe to pass straight through.

The package's own `Example` in
[`doc_example_test.go`](doc_example_test.go) walks the whole lifecycle —
connect, define, sync, bulk-create, filtered read, update, soft delete, restore
and a typed miss — with asserted output, so it is the one worked example
guaranteed to be current:

```sh
go test -run Example ./...
```

## What is here

| | |
| --- | --- |
| **Connection** | `New` (DSN) or `Open` (an existing `*sql.DB`), `Sequelize.Close`, `Sequelize.Sync` |
| **Models** | `Define` with `Attributes`, `TableName`, `Timestamps(false)`, `Paranoid(true)`, `Force` |
| **Data types** | `STRING`, `TEXT`, `INTEGER`, `BIGINT`, `FLOAT`, `DECIMAL`, `BOOLEAN`, `DATE`, `JSON`, … |
| **Reads** | `FindAll`, `FindOne`, `FindByPk`, `FindOrCreate`, `FindAndCountAll`, `Count`, and `Query` with `Where`/`Order`/`Limit`/`Offset`/`Attrs`/`Group`/`Having`/`Distinct` |
| **Writes** | `Create`, `BulkCreate`, `Update`, `Destroy`, `Restore`, `Increment`, `Decrement` |
| **Filters** | the `Op` vocabulary — `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Like`, `NotLike`, `ILike`, `NotILike`, `In`, `NotIn`, `Between`, `NotBetween`, `IsNull`, `NotNull`, `Is` — plus `And`/`Or`/`Not` |
| **Associations** | `HasMany`, `HasOne`, `BelongsTo`, `BelongsToMany`; eager loading with `Query.Include` through `FindAllEager`/`FindOneEager` |
| **Lifecycle** | `AddHook`, per-attribute `Validate`, named scopes via `AddScope`/`Scope`/`Unscoped` |
| **Transactions** | `Sequelize.Transaction` (commit on nil, roll back on error *or* panic), nested via savepoints with `Tx.Transaction` |
| **Dialects** | SQLite and PostgreSQL, as pure SQL generation |
| **Escape hatch** | `Sequelize.Query(ctx, sql, WithArgs(…))` for SQL the builder will not write for you |

## Safety

Two rules hold everywhere, and they are the reason to use this rather than
string concatenation:

- **Values never reach the SQL text.** Each becomes a placeholder and a bind
  argument.
- **Identifiers are validated,** against `^[A-Za-z_][A-Za-z0-9_]*$`, and quoted
  by the dialect. A name that fails is an error wrapping `ErrInvalidIdentifier`
  — never something the package escapes its way around.

The single exception is DDL, which cannot take bind parameters, so
`Attribute.DefaultValue` literals are restricted to what can be rendered
provably safely. See the doc comment on `Attribute.DefaultValue`.

## Measured parity

Compared case-for-case against the real **`sequelize@6.37.8`** running under
Node by the harness in the aggregator repo,
[`parity/sequelize/`](https://github.com/malcolmston/go/tree/main/parity/sequelize):

| | |
| --- | --- |
| Parity | **100%** |
| Cases | 61 |
| Matching | 58 |
| Mismatching | 0 |
| Declared deviations | 3 |

The three declared deviations are documented differences, excluded from the
denominator and named in the harness report, not silent failures. Regenerate the
score with `go test ./parity/sequelize/` from the aggregator root.

## Deviations from upstream

Every intentional difference is in [`API-DEVIATIONS.md`](API-DEVIATIONS.md),
with its reason: a difference not listed there is a bug, not a deviation. The
headline ones:

- **Map-based `Values`, not model instances.** The instance API (`build`,
  `save`, `reload`, `changed`, `previous`, `equals`) is absent by design.
- **A missing row is a typed error, not `null`.** `FindOne`/`FindByPk` return a
  `*RecordNotFoundError` wrapping `ErrRecordNotFound`.
- **Snake-cased columns**, not camelCase.
- **`AllowNull` is a `*bool`**, so the zero `Attribute` is nullable — upstream's
  default rather than Go's instinct.
- **`DECIMAL` reads back as a string on SQLite.**

## Not there yet

Honest gaps, from the package doc: an association does not synthesise its
foreign-key column, so the attribute holding it must be declared; an `Include`
cannot order or limit its children; there are no association setter methods
(add/remove/set) — write the foreign key or the join row yourself; and `Sync`
has no `ALTER` path, so an existing table is left as it is unless `Force` is
passed (`Model.AlterTable` and `Model.InspectTable` are the per-model way to
migrate one deliberately).

## Docs

- Full API reference: [pkg.go.dev](https://pkg.go.dev/github.com/malcolmston/sequelize)
- Design notes: [`ARCHITECTURE.md`](ARCHITECTURE.md)
- Release history: [`CHANGELOG.md`](CHANGELOG.md)

## License

MIT. An independent re-implementation, not affiliated with or endorsed by the
Sequelize project.
