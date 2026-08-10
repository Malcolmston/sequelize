// Package sequelize is a dependency-light Go port of the Sequelize ORM, built
// entirely on top of the standard library's database/sql.
//
// It keeps upstream's shape: you Define models on a connection, Sync their
// tables, and then Create, Find, Update and Destroy through the model. Because
// Sequelize is dynamically typed, records here are [Values] — maps keyed by
// attribute name — rather than tagged structs. That is the port's largest
// deliberate deviation and is recorded in API-DEVIATIONS.md.
//
// # Getting started
//
// Register a database/sql driver, hand its DSN to [New] (or an open *sql.DB to
// [Open]), define a model, and sync it:
//
//	import (
//		"context"
//
//		"github.com/malcolmston/sequelize"
//		_ "modernc.org/sqlite" // registers the "sqlite" driver
//	)
//
//	db, err := sequelize.New("sqlite", "file:app.db")
//	if err != nil {
//		return err
//	}
//	defer db.Close()
//
//	User := db.Define("User", sequelize.Attributes{
//		"name":  {Type: sequelize.STRING(64), AllowNull: sequelize.NotNull()},
//		"email": {Type: sequelize.STRING(255), Unique: true},
//		"age":   {Type: sequelize.INTEGER()},
//	})
//	if err := db.Sync(context.Background()); err != nil {
//		return err
//	}
//
// The package imports no driver itself: only the SQLite [Dialect] ships, and it
// is pure SQL generation. Which driver backs it is the caller's choice.
//
// # Models and attributes
//
// [Sequelize.Define] mirrors sequelize.define, so the defaults are upstream's
// defaults and not Go's instincts:
//
//   - An attribute is nullable unless AllowNull is set to [NotNull], which is why
//     the zero [Attribute] is nullable.
//   - A model with no declared primary key gets an auto-increment "id".
//   - createdAt and updatedAt are managed automatically unless
//     [Timestamps](false) is passed. updatedAt moves on every update; createdAt
//     never moves.
//   - [Paranoid](true) turns Destroy into a soft delete: deletedAt is stamped,
//     every finder hides the row, and [Model.Restore] brings it back.
//
// The table name is the model name snake_cased and pluralised — "UserProfile"
// becomes "user_profiles" — and a column is its attribute name snake_cased.
// Override either with [TableName] and [Attribute.Field].
//
// # Querying
//
// Every read and write is built from a [Query], and filters are [Clause] trees
// built with the [Op] vocabulary:
//
//	rows, err := User.FindAll(ctx, sequelize.Query{
//		Where: sequelize.And(
//			sequelize.Op.Gte("age", 18),
//			sequelize.Op.In("name", []string{"ada", "grace"}),
//		),
//		Order: []sequelize.OrderTerm{sequelize.Desc("age")},
//		Limit: sequelize.Int(10),
//	})
//
// [Op.In] with an empty set matches nothing rather than emitting invalid SQL, so
// it is safe to pass a filter result straight through.
//
// # Safety
//
// Two rules hold everywhere. Values never reach the SQL text: each one becomes a
// placeholder and a bind argument. Identifiers are validated against
// ^[A-Za-z_][A-Za-z0-9_]*$ and quoted by the dialect; a name that fails is an
// error wrapping [ErrInvalidIdentifier], never something the package escapes its
// way around. The single exception is DDL, which cannot take bind parameters, so
// [Attribute.DefaultValue] literals are restricted to what can be rendered
// provably safely — see the documentation on [Attribute.DefaultValue].
//
// Errors are typed. Every failure wraps one of the package's sentinels, so
// callers branch with errors.Is rather than on message text:
//
//	if _, err := User.FindByPk(ctx, 42); errors.Is(err, sequelize.ErrRecordNotFound) {
//		// no such user
//	}
//
// # Transactions
//
// [Sequelize.Transaction] runs a function inside a transaction, committing on nil
// and rolling back on an error or a panic. Aim an operation at the transaction by
// setting [Query.Tx]. [Tx.Transaction] nests via savepoints.
//
// # What is not here yet
//
// Associations and eager loading ([Model.HasMany], [Include]), hooks
// ([Model.AddHook]), attribute validation ([Attribute.Validate]) and scopes
// ([Model.Scope]) are all in place. What is still missing: an association does
// not synthesise its foreign-key column, so the attribute holding it must be
// declared; an [Include] cannot order or limit its children; there are no
// association setter methods (add/remove/set) — write the foreign key or the join
// row yourself; and [Sequelize.Sync] has no ALTER path, so an existing table is
// left as it is unless [Force] is passed.
package sequelize
