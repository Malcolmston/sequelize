package sequelize

import (
	"fmt"
	"sort"
	"sync"
)

// ScopeFunc produces the [Query] a named scope contributes. It is called once
// per statement, so a scope may depend on the clock or on configuration that
// changes between calls — which is upstream's parameterised-scope case expressed
// as a closure.
type ScopeFunc func() Query

// scopeDefs holds a model's scope definitions. It is shared by every scoped view
// of a model, so a scope added to a model is visible from Model.Scope(...) and
// from Model.Unscoped().
type scopeDefs struct {
	mu       sync.RWMutex
	def      ScopeFunc
	named    map[string]ScopeFunc
	defOrder []string
}

// scopeState is a model view's scoping. It is a value, so the copy
// [Model.Scope] returns has its own active set while sharing the definitions.
type scopeState struct {
	defs     *scopeDefs
	active   []string
	unscoped bool
}

func newScopeState() scopeState {
	return scopeState{defs: &scopeDefs{named: map[string]ScopeFunc{}}}
}

// SetDefaultScope installs a default scope: the [Query] merged into every read
// and write on the model, as upstream's `defaultScope` is. Pass the zero Query
// to remove it.
//
// A default scope composes rather than replaces. Its Where is ANDed with the
// caller's Where and with a paranoid model's deletedAt filter, and its Limit,
// Order and Attrs apply only when the caller left them unset.
func (m *Model) SetDefaultScope(q Query) {
	m.SetDefaultScopeFunc(func() Query { return q })
}

// SetDefaultScopeFunc is [Model.SetDefaultScope] with the query computed per
// statement. A nil fn removes the default scope.
func (m *Model) SetDefaultScopeFunc(fn ScopeFunc) {
	defs := m.scopeDefs()
	defs.mu.Lock()
	defs.def = fn
	defs.mu.Unlock()
}

// AddScope defines a named scope, which [Model.Scope] composes on demand. It
// returns an error wrapping [ErrInvalidQuery] when the name is empty or already
// defined.
func (m *Model) AddScope(name string, q Query) error {
	return m.AddScopeFunc(name, func() Query { return q })
}

// AddScopeFunc is [Model.AddScope] with the query computed per statement.
func (m *Model) AddScopeFunc(name string, fn ScopeFunc) error {
	if name == "" {
		return fmt.Errorf("%w: a scope needs a name", ErrInvalidQuery)
	}
	if fn == nil {
		return fmt.Errorf("%w: scope %q has no query", ErrInvalidQuery, name)
	}
	defs := m.scopeDefs()
	defs.mu.Lock()
	defer defs.mu.Unlock()
	if _, exists := defs.named[name]; exists {
		return fmt.Errorf("%w: scope %q is already defined on %s", ErrInvalidQuery, name, m.name)
	}
	defs.named[name] = fn
	defs.defOrder = append(defs.defOrder, name)
	return nil
}

// ScopeNames returns the names of every named scope defined on the model,
// sorted.
func (m *Model) ScopeNames() []string {
	if m.scope.defs == nil {
		return nil
	}
	m.scope.defs.mu.RLock()
	defer m.scope.defs.mu.RUnlock()
	out := make([]string, 0, len(m.scope.defs.named))
	for name := range m.scope.defs.named {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Scope returns a view of the model with the named scopes applied on top of the
// default scope, in the order given. It is upstream's `Model.scope(...)`.
//
// The result is a distinct *Model that shares the original's attributes, hooks
// and associations, so it is cheap to build one per query:
//
//	published, err := post.Scope("published").FindAll(ctx, sequelize.Query{})
//
// Naming a scope that was never defined does not panic; the returned model
// carries the error, so [Model.Err] and every operation on it report it.
func (m *Model) Scope(names ...string) *Model {
	view := *m
	view.scope.active = append(append([]string(nil), m.scope.active...), names...)
	view.scope.unscoped = false
	if m.scope.defs != nil {
		m.scope.defs.mu.RLock()
		for _, name := range names {
			if _, ok := m.scope.defs.named[name]; !ok {
				view.err = fmt.Errorf("%w: no scope %q on %s", ErrInvalidQuery, name, m.name)
				break
			}
		}
		m.scope.defs.mu.RUnlock()
	} else if len(names) > 0 {
		view.err = fmt.Errorf("%w: no scope %q on %s", ErrInvalidQuery, names[0], m.name)
	}
	return &view
}

// Unscoped returns a view of the model with the default scope and every active
// named scope removed, which is upstream's `Model.unscoped()`.
//
// Soft-delete filtering is *not* a scope here and survives Unscoped: a paranoid
// model still hides its deleted rows. Set [Query.Paranoid] to false to see them.
func (m *Model) Unscoped() *Model {
	view := *m
	view.scope.active = nil
	view.scope.unscoped = true
	return &view
}

// ActiveScopes returns the named scopes this view will apply, in order.
func (m *Model) ActiveScopes() []string {
	return append([]string(nil), m.scope.active...)
}

// IsUnscoped reports whether this view drops the default scope.
func (m *Model) IsUnscoped() bool { return m.scope.unscoped }

// scopeDefs returns the shared definition table, creating it when the model was
// built without one.
func (m *Model) scopeDefs() *scopeDefs {
	if m.scope.defs == nil {
		m.scope.defs = &scopeDefs{named: map[string]ScopeFunc{}}
	}
	if m.scope.defs.named == nil {
		m.scope.defs.named = map[string]ScopeFunc{}
	}
	return m.scope.defs
}

// scopeQueries returns the queries to merge into a statement, default first.
func (m *Model) scopeQueries() []Query {
	if m.scope.unscoped || m.scope.defs == nil {
		return nil
	}
	defs := m.scope.defs
	defs.mu.RLock()
	def := defs.def
	fns := make([]ScopeFunc, 0, len(m.scope.active))
	for _, name := range m.scope.active {
		if fn, ok := defs.named[name]; ok {
			fns = append(fns, fn)
		}
	}
	defs.mu.RUnlock()

	out := make([]Query, 0, len(fns)+1)
	if def != nil {
		out = append(out, def())
	}
	for _, fn := range fns {
		out = append(out, fn())
	}
	return out
}

// scoped merges the model's scopes into q and marks the result, so that a query
// which passes through more than one builder is scoped exactly once.
//
// It is called at the top of every statement builder. Composition rules:
// Where accumulates with AND so a scope can never be escaped by supplying a
// Where of your own; Order, Limit, Offset, Attrs, Group and Having are taken
// from the scope only when the caller left them unset, so an explicit value
// wins; Include concatenates; Tx is always the caller's.
func (m *Model) scoped(q Query) Query {
	if q.scoped {
		return q
	}
	q.scoped = true
	for _, s := range m.scopeQueries() {
		q = mergeScope(s, q)
	}
	return q
}

// mergeScope merges the scope's query into the caller's.
func mergeScope(scope, q Query) Query {
	q.Where = And(scope.Where, q.Where)
	if len(q.Order) == 0 {
		q.Order = scope.Order
	}
	if q.Limit == nil {
		q.Limit = scope.Limit
	}
	if q.Offset == nil {
		q.Offset = scope.Offset
	}
	if len(q.Attrs) == 0 {
		q.Attrs = scope.Attrs
	}
	if len(q.Group) == 0 {
		q.Group = scope.Group
	}
	if q.Having == nil {
		q.Having = scope.Having
	}
	if q.Paranoid == nil {
		q.Paranoid = scope.Paranoid
	}
	if !q.Distinct {
		q.Distinct = scope.Distinct
	}
	if len(scope.Include) > 0 {
		q.Include = append(append([]Include(nil), scope.Include...), q.Include...)
	}
	return q
}
