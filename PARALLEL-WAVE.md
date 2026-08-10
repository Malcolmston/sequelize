# Parallel work contract — feature wave 1

Fifteen agents are expanding this package at once. It is a single flat Go package,
so a half-written file breaks everyone's build. These rules exist to make that
survivable. Read them before writing anything.

## The one rule that matters

**Only touch the files you own.** If you need a change in a file you do not own,
do not make it — put a helper in your own file, or report the need in your final
report. A "quick fix" to someone else's file will be overwritten by its owner and
may break the build for thirteen other agents.

## File ownership

| Agent | Owns (may create and edit) |
| --- | --- |
| A1 dialects/postgres | `dialect.go`, `dialect_postgres.go` |
| A2 dialects/mysql | `dialect_mysql.go` |
| A3 instance API | `instance.go`, `mutations.go`, `model.go`, `define.go`, `errors.go` |
| A4 schema migration | `sync.go`, `sync_alter.go` |
| A5 association setters | `associations.go`, `assoc_setters.go` |
| A6 include options | `builder.go`, `include_opts.go` |
| A7 aggregates | `finders.go`, `aggregate.go` |
| A8 validators | `validation.go`, `validators_ext.go` |
| A9 hooks | `hooks.go` |
| A10 scopes | `scopes.go` |
| A11 operators | `operators.go` |
| A12 datatypes | `datatypes.go` |
| A13 transactions/raw | `transaction.go`, `raw.go` |
| A14 parity harness | `../parity/sequelize/**` only — nothing in this module |
| A15 docs & examples | `README.md`, `API-DEVIATIONS.md`, `CHANGELOG.md`, `doc.go`, `../examples/sequelize/**` |

Each agent also owns the `_test.go` file beside every file it owns
(`instance_test.go`, `sync_alter_test.go`, …). Nobody else writes those.

`ARCHITECTURE.md` and this file are owned by nobody — do not edit them.

## Cross-agent dependencies

A3 owns `model.go`/`define.go`/`errors.go` because the instance API needs them,
which makes A3 everyone's bottleneck for new struct fields and new sentinels. If
you need either:

1. Check whether you can avoid it. A new unexported field on a type you own, or a
   `map` keyed by `*Model` in your own file, usually suffices.
2. If you genuinely cannot, say so in your report with the exact declaration you
   need. Do not add it yourself.

A4 emits DDL and A12 defines the types that DDL renders. A12 must expose whatever
A4 needs as an exported method or function on `DataType` rather than expecting A4
to type-switch on new concrete types.

A6 owns `builder.go`, which nearly everything routes through. Treat it as
load-bearing: additive changes only, and keep every existing exported signature.

## Expect transient breakage

Your siblings are writing to the same directory. `go build` will sometimes fail
because of a file you do not own and did not break. When that happens:

- Re-run after a short pause. Most breakage is a file mid-write.
- Confirm the error is in a file you do not own before concluding it is not yours.
- **Never** "fix" it. Report it if it persists across several minutes.
- Judge your own work with `GOWORK=off go vet ./...` and the tests you wrote, and
  do not block on the package-wide build being green if the failure is not yours.

## Non-negotiables, unchanged from ARCHITECTURE.md

- No cgo in the core. `modernc.org/sqlite` stays pinned at **v1.37.0** — newer
  releases require `go 1.25` and would push the module off the mandated 1.24.7.
  Do not run `go get -u`.
- Parameterised SQL only. Identifiers are dialect-quoted and validated against
  `^[A-Za-z_][A-Za-z0-9_]*$`; anything else is an error, never an escape.
- Typed errors wrapping the sentinels in `errors.go`, testable with `errors.Is`.
- Every exported symbol documented. Real SQLite round-trips in tests, via
  `newTestDB` in `define_test.go` — a test that never touches a database does not
  count.
- Do **not** create or modify `VERSION`. `release.yml` cuts a tag whenever VERSION
  changes on main, and this port is not ready to release.
- No `git commit`, `push`, `add`, `checkout` or `restore`. Leave work in the tree.
- `GOWORK=off` on every Go command.

## Definition of done, per agent

`GOWORK=off go vet ./...` clean for your files, your tests pass under
`GOWORK=off go test -run <YourTests> ./...`, and the package still builds once
your siblings have settled. Report what you added, what you could not do without
someone else's file, and anything you deliberately left out.
