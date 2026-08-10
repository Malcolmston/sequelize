package sequelize_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/malcolmston/sequelize"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Example walks the whole lifecycle: connect, define, sync, write, read, update,
// soft-delete and restore.
func Example() {
	dir, err := os.MkdirTemp("", "sequelize-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	db, err := sequelize.New("sqlite", "file:"+filepath.Join(dir, "example.db"))
	if err != nil {
		panic(err)
	}
	defer db.Close()

	// Only Type is required. AllowNull defaults to nullable, as upstream; ask for
	// NotNull() to require a value. Paranoid(true) turns Destroy into a soft
	// delete.
	Task := db.Define("Task", sequelize.Attributes{
		"title": {Type: sequelize.STRING(64), AllowNull: sequelize.NotNull()},
		"done":  {Type: sequelize.BOOLEAN(), DefaultValue: false},
		"size":  {Type: sequelize.INTEGER()},
	}, sequelize.Paranoid(true))

	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		panic(err)
	}

	if _, err := Task.BulkCreate(ctx, []sequelize.Values{
		{"title": "write the port", "size": 3},
		{"title": "write the tests", "size": 5},
		{"title": "write the docs", "size": 1},
	}); err != nil {
		panic(err)
	}

	// Read with a filter, an order and a limit.
	rows, err := Task.FindAll(ctx, sequelize.Query{
		Where: sequelize.Op.Gte("size", 3),
		Order: []sequelize.OrderTerm{sequelize.Desc("size")},
	})
	if err != nil {
		panic(err)
	}
	for _, row := range rows {
		fmt.Printf("%s (%v)\n", row["title"], row["size"])
	}

	// Update by filter; updatedAt moves on its own.
	changed, err := Task.Update(ctx, sequelize.Values{"done": true},
		sequelize.Query{Where: sequelize.Op.Eq("title", "write the docs")})
	if err != nil {
		panic(err)
	}
	fmt.Println("updated:", changed)

	// A soft delete hides the row from every finder...
	if _, err := Task.Destroy(ctx, sequelize.Query{Where: sequelize.Op.Eq("size", 1)}); err != nil {
		panic(err)
	}
	visible, err := Task.Count(ctx, sequelize.Query{})
	if err != nil {
		panic(err)
	}
	fmt.Println("visible:", visible)

	// ...until Restore brings it back.
	if _, err := Task.Restore(ctx, sequelize.Query{Where: sequelize.Op.Eq("size", 1)}); err != nil {
		panic(err)
	}
	visible, err = Task.Count(ctx, sequelize.Query{})
	if err != nil {
		panic(err)
	}
	fmt.Println("restored:", visible)

	// A miss is a typed error, not a zero record.
	if _, err := Task.FindOne(ctx, sequelize.Query{Where: sequelize.Op.Eq("title", "nap")}); err != nil {
		fmt.Println("not found:", errors.Is(err, sequelize.ErrRecordNotFound))
	}

	// Output:
	// write the tests (5)
	// write the port (3)
	// updated: 1
	// visible: 2
	// restored: 3
	// not found: true
}

// ExampleOp shows the operator vocabulary. An empty set matches nothing rather
// than producing invalid SQL, so a filter that came back empty is safe to pass
// straight through.
func ExampleOp() {
	all := sequelize.And(
		sequelize.Op.Gte("age", 18),
		sequelize.Op.In("name", []string{"ada", "grace"}),
		sequelize.Op.NotNull("email"),
	)
	fmt.Println("empty:", all.IsEmpty())
	fmt.Println("nothing matches:", sequelize.Op.In("name", []string{}).IsEmpty())

	// Output:
	// empty: false
	// nothing matches: false
}

// ExampleSequelize_Transaction commits a unit of work, or rolls all of it back.
func ExampleSequelize_Transaction() {
	dir, err := os.MkdirTemp("", "sequelize-tx")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	db, err := sequelize.New("sqlite", "file:"+filepath.Join(dir, "tx.db"))
	if err != nil {
		panic(err)
	}
	defer db.Close()

	Account := db.Define("Account", sequelize.Attributes{
		"owner":   {Type: sequelize.STRING(32), AllowNull: sequelize.NotNull()},
		"balance": {Type: sequelize.INTEGER(), DefaultValue: 0},
	})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		panic(err)
	}

	err = db.Transaction(ctx, func(tx *sequelize.Tx) error {
		if _, err := Account.Create(ctx, sequelize.Values{"owner": "ada", "balance": 100},
			sequelize.Query{Tx: tx}); err != nil {
			return err
		}
		return errors.New("changed my mind")
	})
	fmt.Println("rolled back:", err != nil)

	n, err := Account.Count(ctx, sequelize.Query{})
	if err != nil {
		panic(err)
	}
	fmt.Println("accounts:", n)

	// Output:
	// rolled back: true
	// accounts: 0
}
