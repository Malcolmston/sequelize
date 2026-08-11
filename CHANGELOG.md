# Changelog

All notable changes to this project are documented in this file.

## [Unreleased]

## [0.1.0] - 2026-08-11
### Added
- First tagged release of the Sequelize port: model definition, CRUD, the full
  operator set, `attributes`/`order`/`limit`/`offset`, aggregates, eager loading
  (batched `hasMany`, joined `belongsTo`), a PostgreSQL dialect behind optional
  capability interfaces, and schema migration with `Alter`/`AlterDrop`/
  `AlterRebuild`.
- Measured parity against `sequelize@6.37.8` over SQLite: 100% of comparable
  cases (58/58), with three documented deviations. See `parity/sequelize/`.

### Changed
- `Op.ILike`/`Op.NotILike` now fail at build time with `ErrUnsupported` on a
  dialect that cannot render them, instead of emitting SQL the driver rejects.
