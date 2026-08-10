package sequelize

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// HookType names one point in a model's write lifecycle. The constants below are
// the complete vocabulary; anything else is rejected by [Model.AddHook].
//
// The order for a single-row write mirrors upstream exactly:
//
//	beforeValidate -> (validators) -> afterValidate ->
//	beforeCreate | beforeUpdate | beforeDestroy -> (the statement) ->
//	afterCreate  | afterUpdate  | afterDestroy
type HookType string

// The lifecycle points hooks may be registered for.
const (
	// BeforeValidate runs before the attribute validators, on [Model.Create],
	// [Model.BulkCreate] and [Model.Update]. The values it receives are the row as
	// it will be written, defaults and timestamps already applied, and mutating
	// them changes what is validated and written.
	BeforeValidate HookType = "beforeValidate"
	// AfterValidate runs after the validators have passed. It does not run when
	// validation failed.
	AfterValidate HookType = "afterValidate"
	// BeforeCreate runs after validation and immediately before the INSERT.
	BeforeCreate HookType = "beforeCreate"
	// AfterCreate runs after a successful INSERT, with the row as written
	// including any generated primary key.
	AfterCreate HookType = "afterCreate"
	// BeforeUpdate runs after validation and immediately before the UPDATE.
	BeforeUpdate HookType = "beforeUpdate"
	// AfterUpdate runs after a successful UPDATE, with the number of rows changed
	// in [HookContext.RowsAffected].
	AfterUpdate HookType = "afterUpdate"
	// BeforeDestroy runs immediately before a delete or soft delete. Validation
	// does not run for a destroy, so no validate hooks fire around it.
	BeforeDestroy HookType = "beforeDestroy"
	// AfterDestroy runs after a successful delete or soft delete.
	AfterDestroy HookType = "afterDestroy"
	// BeforeBulkCreate runs once before a [Model.BulkCreate], with every prepared
	// row in [HookContext.Rows]. Mutating a row changes what is written.
	BeforeBulkCreate HookType = "beforeBulkCreate"
	// AfterBulkCreate runs once after a successful [Model.BulkCreate].
	AfterBulkCreate HookType = "afterBulkCreate"
)

// hookTypes is the set HookType values are checked against, so that a typo in a
// hook name is an error rather than a hook that silently never fires.
var hookTypes = map[HookType]bool{
	BeforeValidate: true, AfterValidate: true,
	BeforeCreate: true, AfterCreate: true,
	BeforeUpdate: true, AfterUpdate: true,
	BeforeDestroy: true, AfterDestroy: true,
	BeforeBulkCreate: true, AfterBulkCreate: true,
}

// HookTypes returns every registrable [HookType], sorted, which is what a
// caller validating configuration against the package needs.
func HookTypes() []HookType {
	out := make([]HookType, 0, len(hookTypes))
	for t := range hookTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HookContext is the single argument a [Hook] receives. Its mutable fields are
// Values and Rows: changing them before the statement runs changes what is
// written, which is how upstream's hooks normalise data.
type HookContext struct {
	// Type is the lifecycle point that is firing.
	Type HookType
	// Model is the model being written.
	Model *Model
	// Values is the row being created or the SET map of an update, with defaults
	// and timestamps already applied. It is nil for a destroy and for the bulk
	// hooks.
	Values Values
	// Rows is every row of a [Model.BulkCreate]. It is nil for single-row writes.
	Rows []Values
	// Query is the query governing the write: the filter for an update or a
	// destroy, and the (Tx-carrying) options for a create. It is never nil.
	Query *Query
	// Tx is the transaction the write is running in, or nil when it is not in one.
	// It is Query.Tx, exposed here so a hook can run its own statements on the
	// same transaction.
	Tx *Tx
	// RowsAffected is set on afterUpdate and afterDestroy, and is zero elsewhere.
	RowsAffected int64
}

// Hook is a lifecycle callback. Returning an error aborts the operation: the
// statement does not run (for a before hook), the error is returned to the
// caller wrapping the hook's own error, and any transaction the write is running
// in is rolled back.
type Hook func(ctx context.Context, hc *HookContext) error

// hookEntry is one registration. The name exists so that [Model.RemoveHook] can
// find it again; hooks registered with an empty name cannot be removed.
type hookEntry struct {
	name string
	fn   Hook
}

// hookRegistry holds a model's hooks. It is shared by every scoped view of a
// model, so a hook added to Model applies to Model.Scope(...) too, and is
// mutex-guarded because models are safe for concurrent use.
type hookRegistry struct {
	mu     sync.RWMutex
	byType map[HookType][]hookEntry
}

func newHookRegistry() *hookRegistry {
	return &hookRegistry{byType: map[HookType][]hookEntry{}}
}

// AddHook registers fn to run at t. The name is for [Model.RemoveHook]; pass ""
// for an anonymous hook that cannot be removed. Hooks run in registration order.
//
// It returns an error wrapping [ErrInvalidQuery] when t is not one of the
// [HookType] constants or fn is nil.
func (m *Model) AddHook(t HookType, name string, fn Hook) error {
	if !hookTypes[t] {
		return fmt.Errorf("%w: unknown hook type %q", ErrInvalidQuery, string(t))
	}
	if fn == nil {
		return fmt.Errorf("%w: nil %s hook", ErrInvalidQuery, string(t))
	}
	r := m.hookRegistry()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byType[t] = append(r.byType[t], hookEntry{name: name, fn: fn})
	return nil
}

// RemoveHook drops every hook registered at t under name and reports whether it
// removed any.
func (m *Model) RemoveHook(t HookType, name string) bool {
	if m.hooks == nil || name == "" {
		return false
	}
	r := m.hooks
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := make([]hookEntry, 0, len(r.byType[t]))
	for _, e := range r.byType[t] {
		if e.name != name {
			kept = append(kept, e)
		}
	}
	removed := len(kept) != len(r.byType[t])
	r.byType[t] = kept
	return removed
}

// HookNames returns the names of the hooks registered at t, in the order they
// run. Anonymous hooks appear as empty strings.
func (m *Model) HookNames(t HookType) []string {
	if m.hooks == nil {
		return nil
	}
	m.hooks.mu.RLock()
	defer m.hooks.mu.RUnlock()
	out := make([]string, 0, len(m.hooks.byType[t]))
	for _, e := range m.hooks.byType[t] {
		out = append(out, e.name)
	}
	return out
}

// hookRegistry returns the model's registry, creating it when the model was
// built without one. Definition-time construction fills it in, so this only
// matters for the unusable model [Sequelize.Define] returns on failure.
func (m *Model) hookRegistry() *hookRegistry {
	if m.hooks == nil {
		m.hooks = newHookRegistry()
	}
	return m.hooks
}

// hasHooks reports whether anything is registered at t, so that dispatch can be
// skipped entirely on the common hook-free path.
func (m *Model) hasHooks(t HookType) bool {
	if m.hooks == nil {
		return false
	}
	m.hooks.mu.RLock()
	defer m.hooks.mu.RUnlock()
	return len(m.hooks.byType[t]) > 0
}

// runHooks fires every hook registered at hc.Type, in registration order,
// stopping at the first error.
//
// An error aborts the operation. When the write is inside a transaction that
// transaction is rolled back before the error is returned, which is upstream's
// behaviour and the reason a hook is a safe place to enforce an invariant: a
// half-written transaction cannot survive one.
func (m *Model) runHooks(ctx context.Context, hc *HookContext) error {
	if m.hooks == nil {
		return nil
	}
	m.hooks.mu.RLock()
	entries := append([]hookEntry(nil), m.hooks.byType[hc.Type]...)
	m.hooks.mu.RUnlock()
	if len(entries) == 0 {
		return nil
	}
	hc.Model = m
	if hc.Query != nil {
		hc.Tx = hc.Query.Tx
	}
	for _, e := range entries {
		if err := e.fn(ctx, hc); err != nil {
			return m.abortForHook(hc, e, err)
		}
	}
	return nil
}

// abortForHook rolls back the write's transaction, if any, and wraps err so that
// errors.Is still finds whatever the hook returned.
func (m *Model) abortForHook(hc *HookContext, e hookEntry, err error) error {
	if hc.Tx != nil && !hc.Tx.Done() {
		// A rollback failure must not hide the hook's error, which is the reason
		// the caller is here; report it alongside instead.
		if rbErr := hc.Tx.Rollback(); rbErr != nil {
			return fmt.Errorf("sequelize: %s hook%s on %s aborted the operation (rollback failed: %v): %w",
				string(hc.Type), hookLabel(e.name), m.name, rbErr, err)
		}
	}
	return fmt.Errorf("sequelize: %s hook%s on %s aborted the operation: %w",
		string(hc.Type), hookLabel(e.name), m.name, err)
}

func hookLabel(name string) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf(" %q", name)
}

// newHookContext builds the context a write's hooks share.
func newHookContext(t HookType, q Query, values Values, rows []Values) *HookContext {
	q.Include = nil // a hook has no business with the caller's eager-loading plan
	return &HookContext{Type: t, Values: values, Rows: rows, Query: &q, Tx: q.Tx}
}

// beforeWrite runs the validation hooks, the attribute validators and the
// before hook for a single-row write, in upstream's order. It is the one call
// [Model.Create] and [Model.Update] need to be fully hooked and validated.
//
// partial selects update semantics for validation: only the attributes present
// in values are checked, because an update names a subset of the row.
func (m *Model) beforeWrite(ctx context.Context, q Query, before HookType, values Values, partial bool) error {
	hc := newHookContext(BeforeValidate, q, values, nil)
	if err := m.runHooks(ctx, hc); err != nil {
		return err
	}
	if err := m.validateValues(values, partial); err != nil {
		return err
	}
	hc.Type = AfterValidate
	if err := m.runHooks(ctx, hc); err != nil {
		return err
	}
	hc.Type = before
	return m.runHooks(ctx, hc)
}

// afterWrite runs an after hook for a single-row write.
func (m *Model) afterWrite(ctx context.Context, q Query, after HookType, values Values, affected int64) error {
	if !m.hasHooks(after) {
		return nil
	}
	hc := newHookContext(after, q, values, nil)
	hc.RowsAffected = affected
	return m.runHooks(ctx, hc)
}

// beforeBulkWrite runs beforeBulkCreate and then validates every row, gathering
// every failure across every row into one [ValidationError].
//
// Per-row create hooks deliberately do not fire, as upstream's bulkCreate does
// not run them unless asked to; the validate hooks do fire once per row, because
// validation is not optional.
func (m *Model) beforeBulkWrite(ctx context.Context, q Query, rows []Values) error {
	if err := m.runHooks(ctx, newHookContext(BeforeBulkCreate, q, nil, rows)); err != nil {
		return err
	}
	var failures []FieldError
	for _, row := range rows {
		hc := newHookContext(BeforeValidate, q, row, rows)
		if err := m.runHooks(ctx, hc); err != nil {
			return err
		}
		failures = append(failures, m.validateFields(row, false)...)
		hc.Type = AfterValidate
		if err := m.runHooks(ctx, hc); err != nil {
			return err
		}
	}
	return NewValidationError(m.name, failures)
}

// afterBulkWrite runs afterBulkCreate.
func (m *Model) afterBulkWrite(ctx context.Context, q Query, rows []Values) error {
	if !m.hasHooks(AfterBulkCreate) {
		return nil
	}
	return m.runHooks(ctx, newHookContext(AfterBulkCreate, q, nil, rows))
}
