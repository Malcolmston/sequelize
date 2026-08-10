package sequelize

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// hookRecorder collects the order hooks fired in, which is the only way to test
// an ordering requirement: asserting on the outcome cannot tell "validate then
// create" from "create then validate".
type hookRecorder struct {
	mu    sync.Mutex
	seen  []string
	fails map[HookType]error
}

func newHookRecorder() *hookRecorder {
	return &hookRecorder{fails: map[HookType]error{}}
}

// attach registers a recording hook at every lifecycle point.
func (r *hookRecorder) attach(t *testing.T, m *Model) {
	t.Helper()
	for _, hookType := range HookTypes() {
		if err := m.AddHook(hookType, "recorder", r.hook(hookType)); err != nil {
			t.Fatalf("AddHook(%s): %v", hookType, err)
		}
	}
}

func (r *hookRecorder) hook(hookType HookType) Hook {
	return func(_ context.Context, hc *HookContext) error {
		r.mu.Lock()
		r.seen = append(r.seen, string(hookType))
		fail := r.fails[hookType]
		r.mu.Unlock()
		if hc.Type != hookType {
			return fmt.Errorf("hook fired with Type %q, want %q", hc.Type, hookType)
		}
		if hc.Model == nil || hc.Query == nil {
			return errors.New("hook context is missing its model or query")
		}
		return fail
	}
}

func (r *hookRecorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *hookRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = nil
}

func wantOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("hooks fired [%s], want [%s]", strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

func hookFixture(t *testing.T) (*Sequelize, *Model, *hookRecorder) {
	t.Helper()
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{
		"name":  {Type: STRING(32), AllowNull: NotNull()},
		"count": {Type: INTEGER()},
	})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	if err := db.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	r := newHookRecorder()
	r.attach(t, m)
	return db, m, r
}

// TestHookOrderOnCreate is the parity case for upstream's hook order.
func TestHookOrderOnCreate(t *testing.T) {
	_, m, r := hookFixture(t)
	if _, err := m.Create(context.Background(), Values{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantOrder(t, r.order(), "beforeValidate", "afterValidate", "beforeCreate", "afterCreate")
}

func TestHookOrderOnUpdate(t *testing.T) {
	_, m, r := hookFixture(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, Values{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.reset()
	if _, err := m.Update(ctx, Values{"name": "b"}, Query{Where: Op.Eq("name", "a")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	wantOrder(t, r.order(), "beforeValidate", "afterValidate", "beforeUpdate", "afterUpdate")
}

func TestHookOrderOnDestroy(t *testing.T) {
	_, m, r := hookFixture(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, Values{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.reset()
	if _, err := m.Destroy(ctx, Query{Where: Op.Eq("name", "a")}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// No validation happens for a destroy, so no validate hooks fire around it.
	wantOrder(t, r.order(), "beforeDestroy", "afterDestroy")
}

func TestHookOrderOnBulkCreate(t *testing.T) {
	_, m, r := hookFixture(t)
	rows := []Values{{"name": "a"}, {"name": "b"}}
	if _, err := m.BulkCreate(context.Background(), rows); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	wantOrder(t, r.order(),
		"beforeBulkCreate",
		"beforeValidate", "afterValidate",
		"beforeValidate", "afterValidate",
		"afterBulkCreate")
}

func TestHookContextCarriesTheWrite(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(32)}, "count": {Type: INTEGER()}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var (
		createdID any
		affected  int64
		bulkRows  int
		destroyed int64
		sawTx     bool
	)
	mustAddHook(t, m, AfterCreate, "id", func(_ context.Context, hc *HookContext) error {
		createdID = hc.Values["id"]
		sawTx = hc.Tx != nil
		return nil
	})
	mustAddHook(t, m, AfterUpdate, "n", func(_ context.Context, hc *HookContext) error {
		affected = hc.RowsAffected
		return nil
	})
	mustAddHook(t, m, AfterDestroy, "n", func(_ context.Context, hc *HookContext) error {
		destroyed = hc.RowsAffected
		if hc.Values != nil {
			return errors.New("a destroy hook should not carry Values")
		}
		return nil
	})
	mustAddHook(t, m, BeforeBulkCreate, "n", func(_ context.Context, hc *HookContext) error {
		bulkRows = len(hc.Rows)
		return nil
	})

	if _, err := m.Create(ctx, Values{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if createdID == nil {
		t.Error("afterCreate did not see the generated primary key")
	}
	if sawTx {
		t.Error("afterCreate reported a transaction for an unwrapped Create")
	}
	if _, err := m.BulkCreate(ctx, []Values{{"name": "b"}, {"name": "c"}}); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	if bulkRows != 2 {
		t.Errorf("beforeBulkCreate saw %d rows, want 2", bulkRows)
	}
	if _, err := m.Update(ctx, Values{"count": 1}, Query{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if affected != 3 {
		t.Errorf("afterUpdate reported %d rows, want 3", affected)
	}
	if _, err := m.Destroy(ctx, Query{Where: Op.Eq("name", "a")}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if destroyed != 1 {
		t.Errorf("afterDestroy reported %d rows, want 1", destroyed)
	}

	err := db.Transaction(ctx, func(tx *Tx) error {
		_, err := m.Create(ctx, Values{"name": "d"}, Query{Tx: tx})
		return err
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if !sawTx {
		t.Error("a hook inside a transaction did not see it on HookContext.Tx")
	}
}

func TestHookMutatesTheRowBeforeItIsWritten(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{
		"name":  {Type: STRING(32)},
		"count": {Type: INTEGER()},
	})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	mustAddHook(t, m, BeforeCreate, "normalise", func(_ context.Context, hc *HookContext) error {
		hc.Values["name"] = strings.ToUpper(fmt.Sprint(hc.Values["name"]))
		hc.Values["count"] = 42
		return nil
	})
	mustAddHook(t, m, BeforeUpdate, "bump", func(_ context.Context, hc *HookContext) error {
		hc.Values["count"] = 7
		return nil
	})

	row, err := m.Create(ctx, Values{"name": "quiet"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row["name"] != "QUIET" || row["count"] != 42 {
		t.Errorf("the returned row does not reflect the hook: %v", row)
	}
	stored, err := m.FindOne(ctx, Query{})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if stored["name"] != "QUIET" || stored["count"] != int64(42) {
		t.Errorf("the stored row does not reflect the hook: %v", stored)
	}

	if _, err := m.Update(ctx, Values{"name": "x"}, Query{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stored, err = m.FindOne(ctx, Query{})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if stored["count"] != int64(7) {
		t.Errorf("the update hook's change was not written: %v", stored)
	}
}

func TestHookMutationInBulkCreateIsWritten(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(32)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	mustAddHook(t, m, BeforeBulkCreate, "prefix", func(_ context.Context, hc *HookContext) error {
		for _, row := range hc.Rows {
			row["name"] = "x-" + fmt.Sprint(row["name"])
		}
		return nil
	})
	if _, err := m.BulkCreate(ctx, []Values{{"name": "a"}, {"name": "b"}}); err != nil {
		t.Fatalf("BulkCreate: %v", err)
	}
	rows, err := m.FindAll(ctx, Query{Order: []OrderTerm{Asc("name")}})
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(rows) != 2 || rows[0]["name"] != "x-a" || rows[1]["name"] != "x-b" {
		t.Errorf("bulk rows = %v, want the prefixed names", rows)
	}
}

// TestHookAbortStopsTheWrite covers a before hook failing outside a transaction.
func TestHookAbortStopsTheWrite(t *testing.T) {
	_, m, r := hookFixture(t)
	ctx := context.Background()
	sentinel := errors.New("no widgets today")

	for _, failing := range []HookType{BeforeValidate, AfterValidate, BeforeCreate} {
		r.reset()
		r.fails = map[HookType]error{failing: sentinel}
		_, err := m.Create(ctx, Values{"name": "a"})
		if !errors.Is(err, sentinel) {
			t.Fatalf("%s abort = %v, want the hook's own error", failing, err)
		}
		if !strings.Contains(err.Error(), string(failing)) || !strings.Contains(err.Error(), "recorder") {
			t.Errorf("%s abort message does not name the hook: %v", failing, err)
		}
		count, err := m.Count(ctx, Query{})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if count != 0 {
			t.Fatalf("%s abort still wrote the row", failing)
		}
		// Nothing after the failing hook may have run.
		got := r.order()
		if got[len(got)-1] != string(failing) {
			t.Errorf("%s abort ran further hooks: %v", failing, got)
		}
	}
}

// TestHookAbortRollsBackItsTransaction is the parity case for a hook failing
// inside a transaction: the whole transaction goes, not just the one write.
func TestHookAbortRollsBackItsTransaction(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(32)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	sentinel := errors.New("rejected")
	mustAddHook(t, m, BeforeCreate, "gate", func(_ context.Context, hc *HookContext) error {
		if hc.Values["name"] == "bad" {
			return sentinel
		}
		return nil
	})

	var tx *Tx
	err := db.Transaction(ctx, func(inner *Tx) error {
		tx = inner
		if _, err := m.Create(ctx, Values{"name": "good"}, Query{Tx: inner}); err != nil {
			return err
		}
		_, err := m.Create(ctx, Values{"name": "bad"}, Query{Tx: inner})
		return err
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Transaction = %v, want the hook's error", err)
	}
	if !tx.Done() {
		t.Error("the hook did not close the transaction it aborted")
	}
	count, err := m.Count(ctx, Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("%d rows survived the rollback, want 0: the earlier write was not undone", count)
	}
}

// TestAfterHookAbortRollsBackTheStatementItFollows checks that an after hook,
// which runs once the statement has already been executed, still undoes it when
// there is a transaction to undo it with.
func TestAfterHookAbortRollsBackTheStatementItFollows(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(32)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	sentinel := errors.New("audit failed")
	mustAddHook(t, m, AfterCreate, "audit", func(_ context.Context, hc *HookContext) error {
		return sentinel
	})

	err := db.Transaction(ctx, func(tx *Tx) error {
		_, err := m.Create(ctx, Values{"name": "a"}, Query{Tx: tx})
		return err
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Transaction = %v, want the hook's error", err)
	}
	count, err := m.Count(ctx, Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("the INSERT survived an afterCreate abort: %d rows", count)
	}
}

func TestHookAbortInsideAManagedTransaction(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(32), Unique: true}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	sentinel := errors.New("nope")
	mustAddHook(t, m, BeforeCreate, "gate", func(_ context.Context, hc *HookContext) error {
		return sentinel
	})
	// FindOrCreate opens its own transaction, so the hook aborts a transaction it
	// did not know about.
	if _, _, err := m.FindOrCreate(ctx, Query{Where: Op.Eq("name", "a")}, nil); !errors.Is(err, sentinel) {
		t.Fatalf("FindOrCreate = %v, want the hook's error", err)
	}
	count, err := m.Count(ctx, Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Errorf("FindOrCreate wrote a row despite the hook: %d", count)
	}
}

func TestBulkCreateHookAbort(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(32)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	sentinel := errors.New("no bulk")
	mustAddHook(t, m, BeforeBulkCreate, "gate", func(_ context.Context, hc *HookContext) error {
		return sentinel
	})
	if _, err := m.BulkCreate(ctx, []Values{{"name": "a"}}); !errors.Is(err, sentinel) {
		t.Fatalf("BulkCreate = %v, want the hook's error", err)
	}
	if count, _ := m.Count(ctx, Query{}); count != 0 {
		t.Errorf("BulkCreate wrote %d rows despite the hook", count)
	}
}

func TestAddHookValidatesItsArguments(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(8)}})
	if err := m.AddHook("beforeLunch", "x", func(context.Context, *HookContext) error { return nil }); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("unknown hook type = %v, want ErrInvalidQuery", err)
	}
	if err := m.AddHook(BeforeCreate, "x", nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("nil hook = %v, want ErrInvalidQuery", err)
	}
}

func TestHookNamesAndRemoveHook(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(8)}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var fired []string
	record := func(label string) Hook {
		return func(context.Context, *HookContext) error {
			fired = append(fired, label)
			return nil
		}
	}
	mustAddHook(t, m, BeforeCreate, "first", record("first"))
	mustAddHook(t, m, BeforeCreate, "", record("anonymous"))
	mustAddHook(t, m, BeforeCreate, "second", record("second"))

	if got := m.HookNames(BeforeCreate); len(got) != 3 || got[0] != "first" || got[1] != "" || got[2] != "second" {
		t.Errorf("HookNames = %#v", got)
	}
	if got := m.HookNames(AfterCreate); len(got) != 0 {
		t.Errorf("HookNames reported hooks that were never added: %v", got)
	}
	if _, err := m.Create(ctx, Values{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if strings.Join(fired, ",") != "first,anonymous,second" {
		t.Errorf("hooks fired in %v, want registration order", fired)
	}

	if !m.RemoveHook(BeforeCreate, "first") {
		t.Error("RemoveHook did not report the removal")
	}
	if m.RemoveHook(BeforeCreate, "first") {
		t.Error("RemoveHook removed a hook twice")
	}
	if m.RemoveHook(BeforeCreate, "") {
		t.Error("RemoveHook removed the anonymous hook, which has no name to match")
	}
	if got := m.HookNames(BeforeCreate); len(got) != 2 {
		t.Errorf("HookNames after removal = %#v", got)
	}
}

func TestHooksAreSharedByScopedViews(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Widget", Attributes{"name": {Type: STRING(8)}})
	if err := m.AddScope("all", Query{}); err != nil {
		t.Fatalf("AddScope: %v", err)
	}
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	var fired int
	mustAddHook(t, m, BeforeCreate, "count", func(context.Context, *HookContext) error {
		fired++
		return nil
	})
	if _, err := m.Scope("all").Create(ctx, Values{"name": "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fired != 1 {
		t.Errorf("a scoped view fired %d hooks, want 1", fired)
	}
}

func TestHookTypesIsSortedAndComplete(t *testing.T) {
	got := HookTypes()
	if len(got) != 10 {
		t.Fatalf("HookTypes() has %d entries, want 10: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("HookTypes() is not sorted: %v", got)
			break
		}
	}
}

func TestNoHooksMeansNoOverhead(t *testing.T) {
	// A model with no hooks must still write, which is the path every other test
	// in the package exercises; this pins the nil-registry branches.
	m := &Model{name: "Bare"}
	if m.hasHooks(BeforeCreate) {
		t.Error("a model with no registry reported hooks")
	}
	if m.HookNames(BeforeCreate) != nil {
		t.Error("HookNames on a model with no registry returned names")
	}
	if m.RemoveHook(BeforeCreate, "x") {
		t.Error("RemoveHook on a model with no registry reported a removal")
	}
	if err := m.runHooks(context.Background(), newHookContext(BeforeCreate, Query{}, nil, nil)); err != nil {
		t.Errorf("runHooks on a model with no registry = %v", err)
	}
}

func mustAddHook(t *testing.T, m *Model, hookType HookType, name string, fn Hook) {
	t.Helper()
	if err := m.AddHook(hookType, name, fn); err != nil {
		t.Fatalf("AddHook(%s): %v", hookType, err)
	}
}

// ExampleModel_AddHook normalises a value before it is written.
func ExampleModel_AddHook() {
	ctx := context.Background()
	db, _ := New("sqlite", "file:example_hooks?mode=memory&cache=shared")
	defer db.Close()

	user := db.Define("User", Attributes{"email": {Type: STRING(64)}})
	_ = user.AddHook(BeforeCreate, "lowercase", func(_ context.Context, hc *HookContext) error {
		hc.Values["email"] = strings.ToLower(fmt.Sprint(hc.Values["email"]))
		return nil
	})
	_ = db.Sync(ctx)

	row, _ := user.Create(ctx, Values{"email": "Ada@Example.COM"})
	fmt.Println(row["email"])
	// Output: ada@example.com
}
