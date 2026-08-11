# API deviations

Every intentional difference between this port and upstream
[Sequelize](https://github.com/sequelize/sequelize) (`sequelize@6.37.8`), with
the reason. `ARCHITECTURE.md` requires this file: "where Go forces a difference,
implement the closest idiomatic thing and record it here." A difference not
listed here is a bug, not a deviation.

Several of these are confirmed by the cross-language parity harness in the
aggregator repo (`parity/sequelize/`); the relevant case ids are named.

## Map-based values, not model instances

Upstream returns hydrated `Model` **instances** with methods (`save`, `reload`,
`changed`, `get`/`set`, `toJSON`). This port is **map-based**: reads and writes
speak `Values` (`map[string]any`) keyed by attribute name. Sequelize is
dynamically typed and a struct-tag mapping would fight Go's type system, so the
map is the closest idiomatic thing.

Consequences: the instance API (`build`, `bulkBuild`, `findOrBuild`, `save`,
`reload`, `changed`, `previous`, `equals`) is absent by design; a "row" is a
plain map you can marshal directly. This is the single largest deliberate
deviation.

## A missing row is a typed error, not `null`

`FindOne`/`FindByPk` return a `*RecordNotFoundError` wrapping `ErrRecordNotFound`
when nothing matches; upstream returns `null`. A `nil` map and a "no such row"
are otherwise indistinguishable in Go, and silently returning an empty record is
the kind of bug this avoids. Test the miss with
`errors.Is(err, sequelize.ErrRecordNotFound)`.

Parity cases: `find-one-miss`, `find-by-pk-miss` (marked as deviations, not
failures, in the harness).

## `DECIMAL` reads back as a string on SQLite

The port decodes a `DECIMAL`/`NUMERIC` column to a **string** (e.g. `"9.99"`) to
preserve exact precision. Upstream Sequelize returns a **JavaScript number**
(`9.99`) *only on SQLite* — on PostgreSQL and MySQL it too returns DECIMAL as a
string, because a float cannot hold arbitrary decimal precision. Upstream's
SQLite behaviour is therefore the outlier across its own dialects, and the port
chooses the consistent, lossless representation.

Parity case: `type-decimal-create` (marked as a deviation).

## Snake-cased columns, not camelCase

Attribute keys are snake_cased to physical column names (`createdAt` →
`created_at`, `userId` → `user_id`) and the table name is the model name
snake_cased then pluralised. Upstream defaults to camelCase columns unless
`underscored: true`. Snake_case is the more conventional SQL spelling; override
per column with `Attribute.Field` and per table with `TableName(...)`. The
attribute *keys the caller sees* are unchanged, so this is invisible to `Values`.

## `AllowNull` is a `*bool`

`Attribute.AllowNull` is a `*bool`, defaulting to `nil` = nullable (upstream's
default). A plain `bool` zero value would be `false`, leaving no value that means
"nullable by default"; the pointer plus the `NotNull()` / `Null()` helpers makes
`AllowNull: false` explicit rather than an accident of the zero value.

## `Op.ILike` renders `ILIKE`

`Op.ILike`/`Op.NotILike` render the `ILIKE` keyword, which only PostgreSQL
provides. On SQLite (whose `LIKE` is already ASCII-case-insensitive) and MySQL
these are not usable; use `Op.Like` there. Upstream silently rewrites `iLike`
per dialect; the port keeps the operator explicit. Not exercised in the SQLite
harness because upstream and port disagree only on an unsupported backend.

## Extensions (Go-only, clearly marked)

These have no upstream equivalent and are additive, not behavioural changes:

- `DefineModel` / `MustModel` — error-returning variants of `Define` / `Model`.
- `FindAllEager` / `FindOneEager` — eager loading that honours per-`Include`
  `Order`/`Limit`/`Offset` (upstream's `separate: true`), which the plain
  `FindAll` path does not.
- Schema-migration helpers — `AlterTable`, `SchemaDiff`, `CreateTableSQL`,
  `DropTableSQL`, `CreateIndexSQL`, `InspectTable`.
- `Ping` (upstream spells it `authenticate`).
