# Cross-agent handoff — open items

Wave 1 was cut short: of fifteen agents, three completed (A1 dialects/postgres,
A4 schema migration, A6 include options) and the rest were lost to an API session
limit before they started. This file carries the work they identified but could
not do, because it lives in a file another agent owns.

**If you are picking up an agent slot from `PARALLEL-WAVE.md`, read the items for
your files first.** They are concrete and already diagnosed — implementing them is
cheaper than rediscovering them.

## For A3 (`mutations.go`, `model.go`, `errors.go`)

1. **`Create` returns a nil primary key on PostgreSQL.** `Create`/`BulkCreate` get
   the generated key from `res.LastInsertId()`, which no Postgres driver
   implements. The seam exists — when `d.SupportsReturning()`, append
   `DialectReturning(d, []string{pkColumn})` to the INSERT and `QueryRow`-scan the
   key instead of `Exec` + `LastInsertId`. Without this the Postgres dialect is
   only half-usable: rows insert but the caller never learns the id.
2. **`Upsert` is still select-then-write inside a transaction.** Correct, but two
   round trips. `DialectUpsert(d, conflictCols, updateCols)` now renders
   `ON CONFLICT … DO UPDATE SET` (and `DO NOTHING`), returning `ErrUnsupported`
   where a dialect has no spelling — so the single-statement path is available
   behind a capability check.
3. **Missing error sentinels.** A `23502` not-null violation is currently
   translated into a `*ValidationError` because that is the closest existing type.
   A dedicated `ErrNotNull` + `NotNullError{Model, Field, Cause}` would fit
   properly. There are also no sentinels for serialization failure (`40001`),
   deadlock (`40P01`) or check violation (`23514`), which pass through
   untranslated.

## For A4 (`sync.go`, `sync_alter.go`)

4. **`defaultLiteral` renders `bool` as `1`/`0`.** PostgreSQL rejects that for a
   `BOOLEAN DEFAULT`; it needs `TRUE`/`FALSE` where the dialect wants them.
5. **Native enums are never created.** `CreateTableSQL`/`dropWith` must call
   `DialectBeforeCreateTable` / `DialectAfterDropTable`, and use
   `DialectColumnType(d, table, column, attr.Type)` rather than `d.ColumnType`.
   Until then an enum column silently degrades to `TEXT` with no constraint on
   every dialect — the same class of silent-intent-loss the `migrate` sibling port
   was filed for.

## For A6 (`builder.go`)

6. **`Op.ILike` generates SQL SQLite rejects.** `clause()` writes `string(c.op)`
   unconditionally. Gate it on the capability seam:
   `if !DialectSupportsOperator(b.d, c.op) { return fmt.Errorf("%w: %s cannot render %s", ErrUnsupported, b.d.Name(), c.op) }`
   — a clear error at build time beats a parse error from the driver.
7. **`clauseRaw` passes caller SQL through verbatim** while binding args, so a
   `Literal("x = ?")` is wrong under Postgres's `$N`. Raw literals are inherently
   dialect-specific; document that, or render the placeholders through the
   dialect.

## For A5 (`associations.go`) — the highest-value item in this file

12. **`FindAll` rejects `Include.Order`/`Limit`/`Offset` instead of honouring them.**
    A6 implemented per-parent limiting (window function, with a batched non-N+1
    fallback) but could only expose it as `Model.FindAllEager`, because
    `findAllIncluded` → `loadBatched` lives in your file and builds the child query
    without consulting the include node's options. There is no seam between plan
    and batch load. Three lines fix it: in `findAllIncluded`, pair `p.batched[i]`
    with the include it came from — `m.batchedIncludes(q.Include)` in
    `include_opts.go` returns them in `p.batched` order — and call
    `loadEagerBatch(ctx, q, result, n, inc)` instead of `loadBatched`. Alternatively
    add `order []OrderTerm; limit, offset *int` to `includeNode`, copied from `inc`
    in `walk`, and A6's loader will read them from there. `FindAllEager` then
    collapses to a thin alias.
13. **Pre-existing bug in both loaders: `Query.Paranoid: false` is not propagated to
    batched children**, so soft-deleted children stay hidden even when the root
    explicitly asked to see deleted rows. A6 kept its loader bug-compatible rather
    than diverging unilaterally; fix it alongside item 12.

## For A12 (`datatypes.go`)

9. **`ENUM` renders as bare `TEXT`, so its values are unenforced and undiffable.**
   A4 emits and diffs DDL entirely through `Dialect.ColumnType` and never
   type-switches on a `DataType`, so it needs a method — something like
   `DataType.CheckConstraint(quotedColumn string) (string, bool)` — to emit and
   compare an enum's CHECK. With that, both `columnDefinition` and the table
   rebuild pick it up with no further change. This is the same silent-loss-of-intent
   class the `migrate` sibling port was filed for, so it is worth closing.
10. **`Attribute.Comment` is accepted and discarded.** No dialect renders it. Either
    expose a way to emit it or make the field's inertness explicit.

## For A15 (docs)

8. Record as deviations: MySQL maps `Op.ILike` to plain `LIKE` (its default
   collations are already case-insensitive); `Truncate` is a `DELETE`, so a
   PostgreSQL `SERIAL` sequence is not reset; and `AlterDrop`/`AlterRebuild` split
   what upstream expresses as a single `alter: {drop: true}`.
11. `doc_example_test.go` is yours, so the new schema-migration API has no runnable
    example. `ExampleAlter` and `ExampleModel_SchemaDiff` are the two worth adding.

## Known sharp edges landed in wave 1

- **A rebuild of a table other tables reference by foreign key can fail** while
  `PRAGMA foreign_keys` is on, because `DROP TABLE` fires the implicit delete. It
  fails loudly inside the transaction and rolls back cleanly. The pragma cannot be
  toggled inside a transaction and toggling it outside one is unreliable through
  `database/sql`'s connection pool, so this is a documented limitation rather than
  a bug to paper over.
- **`AlterRebuild` is opt-in on purpose.** The copy goes through SQLite's coercion
  rules, so retyping `VARCHAR`→`INTEGER` silently rewrites values. That is a data
  migration and should be a decision, not a side effect of `Alter(true)`.
- **A rebuild carries the model's indexes; undeclared ones are dropped**, and every
  dropped index appears in the returned change list. Preserving them would have
  made the in-place and rebuild paths disagree.
- **Nothing in the Postgres dialect has been executed against a real server.** It
  is verified as string generation only. An integration test needs a
  `SEQUELIZE_POSTGRES_DSN` env guard and a driver import behind
  `//go:build integration` — no dialect change required.

## Note on the capability seams

A1 deliberately did **not** add methods to `Dialect` — that would have broken
every other implementation, including third-party ones. Instead `dialect.go` grew
optional interfaces (`ColumnTyper`, `Returner`, `Upserter`, `OperatorSupporter`,
`TableDDLer`) with package-level helpers that fall back to a portable default. Use
the helpers (`DialectReturning`, `DialectUpsert`, `DialectColumnType`,
`DialectSupportsOperator`, `DialectBeforeCreateTable`, `DialectAfterDropTable`),
never a direct type assertion, so a dialect implementing only the v0.1.0 interface
keeps working.
