package sequelize

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

// AssociationKind distinguishes the four relationships a model may declare.
type AssociationKind int

// The association kinds, mirroring upstream's four association methods.
const (
	// AssocHasOne is one row of the target referring back to this model.
	AssocHasOne AssociationKind = iota + 1
	// AssocHasMany is many rows of the target referring back to this model.
	AssocHasMany
	// AssocBelongsTo is this model holding the foreign key of one target row.
	AssocBelongsTo
	// AssocBelongsToMany is a many-to-many relationship through a join table.
	AssocBelongsToMany
)

// String returns the upstream spelling of the kind, for diagnostics.
func (k AssociationKind) String() string {
	switch k {
	case AssocHasOne:
		return "hasOne"
	case AssocHasMany:
		return "hasMany"
	case AssocBelongsTo:
		return "belongsTo"
	case AssocBelongsToMany:
		return "belongsToMany"
	default:
		return "unknown"
	}
}

// Multiple reports whether the association yields a list of rows rather than a
// single row. It is what decides between a JOIN and a batched follow-up query
// during eager loading.
func (k AssociationKind) Multiple() bool {
	return k == AssocHasMany || k == AssocBelongsToMany
}

// associationOptions collects what the [AssociationOption] functions set.
type associationOptions struct {
	as         string
	foreignKey string
	sourceKey  string
	targetKey  string
	otherKey   string
}

// AssociationOption overrides one of the names an association would otherwise
// infer.
type AssociationOption func(*associationOptions)

// As names the association, which is both the key eager-loaded rows are stored
// under and the name [Include.Association] refers to. It defaults to the target
// model's name lowercased — pluralised for the to-many kinds.
func As(name string) AssociationOption {
	return func(o *associationOptions) { o.as = name }
}

// ForeignKey overrides the foreign-key attribute. For HasOne and HasMany it is
// an attribute of the *target*; for BelongsTo an attribute of the *source*; for
// BelongsToMany the attribute of the join table pointing at the source.
//
// It is inferred as the owning model's name lowercased with "Id" appended, so
// User.HasMany(Post) looks for Post.userId.
func ForeignKey(attribute string) AssociationOption {
	return func(o *associationOptions) { o.foreignKey = attribute }
}

// SourceKey overrides the attribute of the source model the foreign key points
// at. It defaults to the source's primary key.
func SourceKey(attribute string) AssociationOption {
	return func(o *associationOptions) { o.sourceKey = attribute }
}

// TargetKey overrides the attribute of the target model the foreign key points
// at. It defaults to the target's primary key, and is used by BelongsTo and
// BelongsToMany.
func TargetKey(attribute string) AssociationOption {
	return func(o *associationOptions) { o.targetKey = attribute }
}

// OtherKey overrides the join table's attribute pointing at the target, for
// BelongsToMany. It is inferred as the target model's name lowercased with "Id"
// appended.
func OtherKey(attribute string) AssociationOption {
	return func(o *associationOptions) { o.otherKey = attribute }
}

// Association is a declared relationship between two models. Build one with
// [Model.HasOne], [Model.HasMany], [Model.BelongsTo] or [Model.BelongsToMany],
// then eager-load it by naming it in [Include.Association].
//
// This port does not synthesise the foreign-key column: the attribute an
// association names must already be declared on the model that holds it, so that
// [Sequelize.Sync] has exactly one description of the table. Declaring it with a
// [Reference] additionally renders the SQL FOREIGN KEY.
type Association struct {
	kind    AssociationKind
	as      string
	source  *Model
	target  *Model
	through *Model

	foreignKey string
	otherKey   string
	sourceKey  string
	targetKey  string

	err error
}

// Kind returns which of the four relationships this is.
func (a *Association) Kind() AssociationKind { return a.kind }

// Name returns the association's name, which is the key eager-loaded rows land
// under.
func (a *Association) Name() string { return a.as }

// Source returns the model the association is declared on.
func (a *Association) Source() *Model { return a.source }

// Target returns the model at the far end.
func (a *Association) Target() *Model { return a.target }

// Through returns the join-table model of a BelongsToMany, and nil for the other
// kinds.
func (a *Association) Through() *Model { return a.through }

// ForeignKey returns the resolved foreign-key attribute name.
func (a *Association) ForeignKey() string { return a.foreignKey }

// OtherKey returns the resolved join-table attribute pointing at the target, and
// "" for the kinds that have no join table.
func (a *Association) OtherKey() string { return a.otherKey }

// SourceKey returns the resolved source-side key attribute.
func (a *Association) SourceKey() string { return a.sourceKey }

// TargetKey returns the resolved target-side key attribute.
func (a *Association) TargetKey() string { return a.targetKey }

// Err reports why the association was rejected, or nil when it is usable. As
// with [Sequelize.Define], the constructors carry no error so that a chain of
// declarations reads like upstream's; the error surfaces here and from any query
// that includes the association.
func (a *Association) Err() error { return a.err }

// String renders the association for diagnostics.
func (a *Association) String() string {
	if a.err != nil {
		return fmt.Sprintf("%s(invalid: %v)", a.kind, a.err)
	}
	return fmt.Sprintf("%s.%s: %s %s", a.source.name, a.as, a.kind, a.target.name)
}

// associationSet holds a model's associations. It is shared with every scoped
// view of the model.
type associationSet struct {
	mu     sync.RWMutex
	byName map[string]*Association
	order  []string
}

func newAssociationSet() *associationSet {
	return &associationSet{byName: map[string]*Association{}}
}

func (m *Model) associationSet() *associationSet {
	if m.assoc == nil {
		m.assoc = newAssociationSet()
	}
	if m.assoc.byName == nil {
		m.assoc.byName = map[string]*Association{}
	}
	return m.assoc
}

// Association returns the association declared under name.
func (m *Model) Association(name string) (*Association, bool) {
	if m.assoc == nil {
		return nil, false
	}
	m.assoc.mu.RLock()
	defer m.assoc.mu.RUnlock()
	a, ok := m.assoc.byName[name]
	return a, ok
}

// Associations returns every association declared on the model, in declaration
// order.
func (m *Model) Associations() []*Association {
	if m.assoc == nil {
		return nil
	}
	m.assoc.mu.RLock()
	defer m.assoc.mu.RUnlock()
	out := make([]*Association, 0, len(m.assoc.order))
	for _, name := range m.assoc.order {
		out = append(out, m.assoc.byName[name])
	}
	return out
}

// HasOne declares that one row of target refers back to this model through a
// foreign key the target holds.
//
//	user.HasOne(profile) // looks for Profile.userId, stores rows under "profile"
func (m *Model) HasOne(target *Model, opts ...AssociationOption) *Association {
	return m.addAssociation(AssocHasOne, target, nil, opts)
}

// HasMany declares that many rows of target refer back to this model through a
// foreign key the target holds.
//
//	user.HasMany(post) // looks for Post.userId, stores rows under "posts"
func (m *Model) HasMany(target *Model, opts ...AssociationOption) *Association {
	return m.addAssociation(AssocHasMany, target, nil, opts)
}

// BelongsTo declares that this model holds the foreign key of one target row.
//
//	post.BelongsTo(user) // looks for Post.userId, stores the row under "user"
func (m *Model) BelongsTo(target *Model, opts ...AssociationOption) *Association {
	return m.addAssociation(AssocBelongsTo, target, nil, opts)
}

// BelongsToMany declares a many-to-many relationship with target through the
// join-table model through, which must declare an attribute pointing at each
// side.
//
//	post.BelongsToMany(tag, postTag) // PostTag.postId and PostTag.tagId
func (m *Model) BelongsToMany(target, through *Model, opts ...AssociationOption) *Association {
	return m.addAssociation(AssocBelongsToMany, target, through, opts)
}

// addAssociation resolves and registers one association.
func (m *Model) addAssociation(kind AssociationKind, target, through *Model, opts []AssociationOption) *Association {
	a := &Association{kind: kind, source: m, target: target, through: through}
	var o associationOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if a.err = a.resolve(o); a.err != nil {
		return a
	}
	set := m.associationSet()
	set.mu.Lock()
	defer set.mu.Unlock()
	if _, dup := set.byName[a.as]; dup {
		a.err = fmt.Errorf("%w: %s already has an association named %q", ErrInvalidQuery, m.name, a.as)
		return a
	}
	set.byName[a.as] = a
	set.order = append(set.order, a.as)
	return a
}

// resolve fills in the inferred names and checks that every attribute the
// association will read actually exists.
func (a *Association) resolve(o associationOptions) error {
	if a.source == nil {
		return fmt.Errorf("%w: association with no source model", ErrInvalidQuery)
	}
	if err := a.source.Err(); err != nil {
		return err
	}
	if a.target == nil {
		return fmt.Errorf("%w: %s.%s has no target model", ErrInvalidQuery, a.source.name, a.kind)
	}
	if err := a.target.Err(); err != nil {
		return err
	}
	if a.source.seq != a.target.seq {
		return fmt.Errorf("%w: %s and %s are defined on different connections",
			ErrInvalidQuery, a.source.name, a.target.name)
	}
	if a.kind == AssocBelongsToMany {
		if a.through == nil {
			return fmt.Errorf("%w: %s.belongsToMany %s needs a through model",
				ErrInvalidQuery, a.source.name, a.target.name)
		}
		if err := a.through.Err(); err != nil {
			return err
		}
		if a.through.seq != a.source.seq {
			return fmt.Errorf("%w: join table %s is defined on another connection",
				ErrInvalidQuery, a.through.name)
		}
	}

	a.as = o.as
	if a.as == "" {
		a.as = defaultAssociationName(a.target, a.kind.Multiple())
	}
	if err := ValidateIdentifier(a.as); err != nil {
		return fmt.Errorf("association name on %s: %w", a.source.name, err)
	}
	if _, clash := a.source.attrs[a.as]; clash {
		return fmt.Errorf("%w: association %q on %s collides with an attribute of the same name",
			ErrInvalidQuery, a.as, a.source.name)
	}

	var err error
	if a.sourceKey, err = keyOrPrimary(a.source, o.sourceKey); err != nil {
		return err
	}
	if a.targetKey, err = keyOrPrimary(a.target, o.targetKey); err != nil {
		return err
	}

	switch a.kind {
	case AssocBelongsTo:
		a.foreignKey = orDefault(o.foreignKey, foreignKeyName(a.target))
		return hasAttribute(a.source, a.foreignKey, "foreign key")
	case AssocHasOne, AssocHasMany:
		a.foreignKey = orDefault(o.foreignKey, foreignKeyName(a.source))
		return hasAttribute(a.target, a.foreignKey, "foreign key")
	case AssocBelongsToMany:
		a.foreignKey = orDefault(o.foreignKey, foreignKeyName(a.source))
		a.otherKey = orDefault(o.otherKey, foreignKeyName(a.target))
		if err := hasAttribute(a.through, a.foreignKey, "foreign key"); err != nil {
			return err
		}
		return hasAttribute(a.through, a.otherKey, "other key")
	default:
		return fmt.Errorf("%w: unknown association kind %d", ErrInvalidQuery, int(a.kind))
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// hasAttribute reports whether m declares attribute, naming the role it plays in
// the error so a typo is easy to place.
func hasAttribute(m *Model, attribute, role string) error {
	if _, ok := m.attrs[attribute]; !ok {
		return fmt.Errorf("%w: %s.%s (the association's %s) is not declared",
			ErrUnknownAttribute, m.name, attribute, role)
	}
	return nil
}

// keyOrPrimary validates an explicit key attribute, or falls back to the model's
// single primary key.
func keyOrPrimary(m *Model, explicit string) (string, error) {
	if explicit != "" {
		return explicit, hasAttribute(m, explicit, "key")
	}
	if len(m.pk) != 1 {
		return "", fmt.Errorf("%w: %s has no single-column primary key to associate on; name one explicitly",
			ErrNoPrimaryKey, m.name)
	}
	return m.pk[0], nil
}

// defaultAssociationName derives the alias upstream would: the model name with a
// lowercase first letter, pluralised for the to-many kinds.
func defaultAssociationName(target *Model, plural bool) string {
	name := lowerFirst(target.name)
	if plural {
		return pluralize(name)
	}
	return name
}

// foreignKeyName derives the inferred foreign key for an owning model: "User"
// becomes "userId", which the model layer renders as the column user_id.
func foreignKeyName(owner *Model) string {
	return lowerFirst(owner.name) + "Id"
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// includeRootAlias is the table alias the queried model itself gets. Every
// column of an included query is alias-qualified, so a shared column name
// between two joined tables can never be ambiguous.
const includeRootAlias = "t0"

// includeNode is one resolved [Include].
type includeNode struct {
	assoc    *Association
	as       string
	where    *Clause
	attrs    []string
	required bool
	nested   []Include

	// alias and throughAlias are set for nodes rendered as a JOIN.
	alias        string
	throughAlias string
	parentAlias  string

	// parentPath is the chain of association names from the root row down to the
	// rows this node hangs off, and is how a batched load finds its parents.
	parentPath []string

	// cols is the projection for a joined node.
	cols []string
}

// includePlan is one query's eager-loading plan: the joins that go into the
// SELECT, and the to-many associations that become batched follow-up queries.
type includePlan struct {
	root     *Model
	rootCols []string
	joined   []*includeNode
	batched  []*includeNode
	aliases  int
}

// planIncludes resolves a query's [Include] tree.
//
// With joinAll false — the SELECT path — a to-many association stops the walk
// and becomes a batched node; with joinAll true — the aggregate path — every
// association is rendered as a JOIN instead, because a COUNT has no rows to
// batch into.
func (m *Model) planIncludes(q Query, joinAll bool) (*includePlan, error) {
	rootCols, err := m.selectColumns(q.Attrs)
	if err != nil {
		return nil, err
	}
	p := &includePlan{root: m, rootCols: rootCols}
	if err := p.walk(m, includeRootAlias, nil, q.Include, joinAll); err != nil {
		return nil, err
	}
	if err := p.ensureBatchKeys(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *includePlan) walk(parent *Model, parentAlias string, parentPath []string, incs []Include, joinAll bool) error {
	for _, inc := range incs {
		assoc, ok := parent.Association(inc.Association)
		if !ok {
			return fmt.Errorf("%w: %s declares no association %q",
				ErrInvalidQuery, parent.name, inc.Association)
		}
		if err := assoc.Err(); err != nil {
			return err
		}
		as := assoc.as
		if inc.As != "" {
			if err := ValidateIdentifier(inc.As); err != nil {
				return fmt.Errorf("include alias: %w", err)
			}
			as = inc.As
		}
		n := &includeNode{
			assoc:       assoc,
			as:          as,
			where:       inc.Where,
			attrs:       inc.Attrs,
			required:    inc.Required,
			nested:      inc.Include,
			parentAlias: parentAlias,
			parentPath:  parentPath,
		}
		if assoc.kind.Multiple() && !joinAll {
			p.batched = append(p.batched, n)
			continue
		}
		p.aliases++
		n.alias = fmt.Sprintf("t%d", p.aliases)
		if assoc.kind == AssocBelongsToMany {
			p.aliases++
			n.throughAlias = fmt.Sprintf("t%d", p.aliases)
		}
		if !joinAll {
			cols, err := assoc.target.selectColumns(inc.Attrs)
			if err != nil {
				return err
			}
			// The primary key is always projected: it is how a LEFT JOIN that
			// matched nothing is told apart from a row of all-NULL columns.
			n.cols = ensureAttrs(cols, assoc.target.pk...)
		}
		p.joined = append(p.joined, n)
		childPath := append(append([]string(nil), parentPath...), as)
		if err := p.walk(assoc.target, n.alias, childPath, inc.Include, joinAll); err != nil {
			return err
		}
	}
	return nil
}

// ensureBatchKeys makes sure every batched association's parent projection
// carries the key the batch is grouped by, even when the caller restricted
// Attrs and left it out.
func (p *includePlan) ensureBatchKeys() error {
	for _, n := range p.batched {
		key := n.assoc.sourceKey
		if n.parentAlias == includeRootAlias {
			p.rootCols = ensureAttrs(p.rootCols, key)
			continue
		}
		for _, j := range p.joined {
			if j.alias == n.parentAlias {
				j.cols = ensureAttrs(j.cols, key)
			}
		}
	}
	return nil
}

// ensureAttrs appends the named attributes that are missing, preserving order.
func ensureAttrs(cols []string, extra ...string) []string {
	for _, e := range extra {
		found := false
		for _, c := range cols {
			if c == e {
				found = true
				break
			}
		}
		if !found {
			cols = append(cols, e)
		}
	}
	return cols
}

// findAllIncluded is the eager-loading path of [Model.FindAll].
//
// It runs exactly one query for the model itself plus its BelongsTo and HasOne
// includes (they are JOINs), then one batched query per HasMany association and
// two per BelongsToMany (the join table, then the targets), at every nesting
// level. The count depends on the shape of the include tree and never on the
// number of rows: eager loading here cannot become N+1.
func (m *Model) findAllIncluded(ctx context.Context, q Query) ([]Values, error) {
	p, err := m.planIncludes(q, false)
	if err != nil {
		return nil, err
	}
	query, args, err := m.buildIncludedSelect(q, p)
	if err != nil {
		return nil, err
	}
	querier, err := m.querierFor(q)
	if err != nil {
		return nil, err
	}
	m.seq.log(ctx, query, args)
	rows, err := querier.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, m.seq.dialect.TranslateError(err)
	}
	result, scanErr := m.scanIncluded(rows, p)
	closeErr := rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// The root rows are fully read and the cursor closed before any batch runs, so
	// a single-connection pool cannot deadlock on the follow-up queries.
	for _, n := range p.batched {
		if err := loadBatched(ctx, q, result, n); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// buildIncludedSelect renders the joined SELECT for a plan.
func (m *Model) buildIncludedSelect(q Query, p *includePlan) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	b := newBuilder(m)
	b.alias = includeRootAlias
	w := &joinWriter{b: b}
	w.raw("SELECT ")
	if q.Distinct {
		w.raw("DISTINCT ")
	}
	for i, attr := range p.rootCols {
		if i > 0 {
			w.raw(", ")
		}
		w.col(m, includeRootAlias, attr)
	}
	for _, n := range p.joined {
		for _, attr := range n.cols {
			w.raw(", ")
			w.col(n.assoc.target, n.alias, attr)
		}
	}
	w.raw(" FROM ")
	w.tbl(m, includeRootAlias)
	for _, n := range p.joined {
		w.join(n, q)
	}
	if w.err != nil {
		return "", nil, w.err
	}
	b.m, b.alias = m, includeRootAlias
	if err := b.writeWhere(m.effectiveWhere(q)); err != nil {
		return "", nil, err
	}
	if err := b.writeTail(q, true); err != nil {
		return "", nil, err
	}
	return b.sb.String(), b.args, nil
}

// buildIncludedAggregate renders an aggregate over a joined query. Every
// association becomes a JOIN here, which is what makes
// Count(Query{Include: ..., Distinct: true}) count distinct parents: the joins
// multiply rows and the DISTINCT over the primary key collapses them again.
func (m *Model) buildIncludedAggregate(fn, attribute string, q Query) (string, []any, error) {
	if err := m.Err(); err != nil {
		return "", nil, err
	}
	if err := ValidateIdentifier(fn); err != nil {
		return "", nil, err
	}
	if len(q.Group) > 0 || q.Having != nil {
		return "", nil, fmt.Errorf("%w: Group with Include", ErrUnsupported)
	}
	p, err := m.planIncludes(Query{Include: q.Include}, true)
	if err != nil {
		return "", nil, err
	}
	b := newBuilder(m)
	b.alias = includeRootAlias
	w := &joinWriter{b: b}
	w.raw("SELECT ", fn, "(")
	if q.Distinct {
		w.raw("DISTINCT ")
	}
	if attribute == "" {
		w.raw("*")
	} else {
		w.col(m, includeRootAlias, attribute)
	}
	w.raw(") FROM ")
	w.tbl(m, includeRootAlias)
	for _, n := range p.joined {
		w.join(n, q)
	}
	if w.err != nil {
		return "", nil, w.err
	}
	b.m, b.alias = m, includeRootAlias
	if err := b.writeWhere(m.effectiveWhere(q)); err != nil {
		return "", nil, err
	}
	if err := b.writeLimit(q); err != nil {
		return "", nil, err
	}
	return b.sb.String(), b.args, nil
}

// joinWriter emits join syntax, carrying the first error so that the emission
// reads as a sequence of clauses rather than a ladder of error checks.
type joinWriter struct {
	b   *builder
	err error
}

func (w *joinWriter) raw(parts ...string) {
	if w.err == nil {
		w.b.write(parts...)
	}
}

func (w *joinWriter) col(m *Model, alias, attribute string) {
	if w.err == nil {
		w.err = w.b.qualified(m, alias, attribute)
	}
}

func (w *joinWriter) tbl(m *Model, alias string) {
	if w.err == nil {
		w.err = w.b.aliasedTable(m, alias)
	}
}

func (w *joinWriter) cond(m *Model, alias string, c *Clause) {
	if w.err == nil {
		w.err = w.b.clauseAs(m, alias, c)
	}
}

// join emits one association's JOIN, including the extra ON conditions from
// [Include.Where] and from a paranoid target. Those conditions belong in the ON
// rather than the WHERE, so that a LEFT JOIN stays outer: filtering an optional
// association must not drop the parent row.
func (w *joinWriter) join(n *includeNode, q Query) {
	a := n.assoc
	keyword := " LEFT JOIN "
	if n.required {
		keyword = " INNER JOIN "
	}
	switch a.kind {
	case AssocBelongsTo:
		w.raw(keyword)
		w.tbl(a.target, n.alias)
		w.raw(" ON ")
		w.col(a.target, n.alias, a.targetKey)
		w.raw(" = ")
		w.col(a.source, n.parentAlias, a.foreignKey)
	case AssocHasOne, AssocHasMany:
		w.raw(keyword)
		w.tbl(a.target, n.alias)
		w.raw(" ON ")
		w.col(a.target, n.alias, a.foreignKey)
		w.raw(" = ")
		w.col(a.source, n.parentAlias, a.sourceKey)
	case AssocBelongsToMany:
		w.raw(keyword)
		w.tbl(a.through, n.throughAlias)
		w.raw(" ON ")
		w.col(a.through, n.throughAlias, a.foreignKey)
		w.raw(" = ")
		w.col(a.source, n.parentAlias, a.sourceKey)
		w.raw(keyword)
		w.tbl(a.target, n.alias)
		w.raw(" ON ")
		w.col(a.target, n.alias, a.targetKey)
		w.raw(" = ")
		w.col(a.through, n.throughAlias, a.otherKey)
	default:
		if w.err == nil {
			w.err = fmt.Errorf("%w: unknown association kind %d", ErrInvalidQuery, int(a.kind))
		}
		return
	}
	extra := n.where
	if a.target.IsParanoid() && !(q.Paranoid != nil && !*q.Paranoid) {
		extra = And(extra, Op.IsNull(a.target.DeletedAtName()))
	}
	if extra != nil {
		w.raw(" AND ")
		w.cond(a.target, n.alias, extra)
	}
}

// qualified writes one alias-qualified column of another model, restoring the
// builder's own model afterwards so bind-parameter numbering stays in one place.
func (b *builder) qualified(m *Model, alias, attribute string) error {
	prevModel, prevAlias := b.m, b.alias
	b.m, b.alias = m, alias
	err := b.column(attribute)
	b.m, b.alias = prevModel, prevAlias
	return err
}

// clauseAs renders a clause against another model under an alias.
func (b *builder) clauseAs(m *Model, alias string, c *Clause) error {
	prevModel, prevAlias := b.m, b.alias
	b.m, b.alias = m, alias
	err := b.clause(c)
	b.m, b.alias = prevModel, prevAlias
	return err
}

// aliasedTable writes `"table" AS "alias"`.
func (b *builder) aliasedTable(m *Model, alias string) error {
	table, err := b.d.Quote(m.table)
	if err != nil {
		return err
	}
	quotedAlias, err := b.d.Quote(alias)
	if err != nil {
		return err
	}
	b.write(table, " AS ", quotedAlias)
	return nil
}

// scanIncluded decodes a joined result set into nested [Values]: an included
// BelongsTo or HasOne row lands under its association name as a Values, or as a
// nil entry when the outer join matched nothing.
func (m *Model) scanIncluded(rows *sql.Rows, p *includePlan) ([]Values, error) {
	type colSpec struct {
		node *includeNode
		attr string
		typ  DataType
	}
	specs := make([]colSpec, 0, len(p.rootCols))
	for _, attr := range p.rootCols {
		specs = append(specs, colSpec{attr: attr, typ: m.attrs[attr].Type})
	}
	for _, n := range p.joined {
		for _, attr := range n.cols {
			specs = append(specs, colSpec{node: n, attr: attr, typ: n.assoc.target.attrs[attr].Type})
		}
	}

	out := []Values{}
	for rows.Next() {
		raw := make([]any, len(specs))
		dest := make([]any, len(specs))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		root := make(Values, len(p.rootCols))
		byAlias := make(map[string]Values, len(p.joined))
		for i, s := range specs {
			decoded, err := decodeValue(s.typ, raw[i])
			if err != nil {
				owner := m.name
				if s.node != nil {
					owner = s.node.assoc.target.name
				}
				return nil, fmt.Errorf("%s.%s: %w", owner, s.attr, err)
			}
			if s.node == nil {
				root[s.attr] = decoded
				continue
			}
			child, ok := byAlias[s.node.alias]
			if !ok {
				child = Values{}
				byAlias[s.node.alias] = child
			}
			child[s.attr] = decoded
		}
		// Joined nodes are in walk order, so a parent is always attached before
		// its children are considered.
		for _, n := range p.joined {
			container := root
			if n.parentAlias != includeRootAlias {
				container = byAlias[n.parentAlias]
			}
			if container == nil {
				continue
			}
			child := byAlias[n.alias]
			if child == nil || allKeysNil(child, n.assoc.target.pk) {
				container[n.as] = nil
				delete(byAlias, n.alias)
				continue
			}
			container[n.as] = child
		}
		out = append(out, root)
	}
	return out, rows.Err()
}

// allKeysNil reports whether every primary-key column came back NULL, which is
// how an unmatched LEFT JOIN presents itself.
func allKeysNil(row Values, keys []string) bool {
	for _, k := range keys {
		if row[k] != nil {
			return false
		}
	}
	return true
}

// loadBatched loads one to-many association for every parent row in one go.
//
// The parents are the rows the association hangs off — the root rows, or the
// joined rows some levels down — and their keys are collected into a single
// IN (...) filter. There is deliberately no per-parent query anywhere in here.
func loadBatched(ctx context.Context, q Query, rows []Values, n *includeNode) error {
	a := n.assoc
	parents := gatherParents(rows, n.parentPath)
	for _, parent := range parents {
		parent[n.as] = []Values{}
	}
	keys := distinctKeyValues(parents, a.sourceKey)
	if len(keys) == 0 {
		return nil
	}
	switch a.kind {
	case AssocHasMany:
		children, err := a.target.FindAll(ctx, Query{
			Where:   And(n.where, Op.In(a.foreignKey, keys)),
			Attrs:   restrictedAttrs(n.attrs, a.foreignKey),
			Include: n.nested,
			Tx:      q.Tx,
		})
		if err != nil {
			return err
		}
		grouped := groupByKey(children, a.foreignKey)
		assignGroups(parents, a.sourceKey, n.as, grouped)
		return nil
	case AssocBelongsToMany:
		pairs, err := a.through.FindAll(ctx, Query{
			Attrs: ensureAttrs([]string{a.foreignKey}, a.otherKey),
			Where: Op.In(a.foreignKey, keys),
			Tx:    q.Tx,
		})
		if err != nil {
			return err
		}
		targetKeys := distinctKeyValues(pairs, a.otherKey)
		if len(targetKeys) == 0 {
			return nil
		}
		targets, err := a.target.FindAll(ctx, Query{
			Where:   And(n.where, Op.In(a.targetKey, targetKeys)),
			Attrs:   restrictedAttrs(n.attrs, a.targetKey),
			Include: n.nested,
			Tx:      q.Tx,
		})
		if err != nil {
			return err
		}
		byTarget := make(map[string]Values, len(targets))
		for _, t := range targets {
			if key, ok := associationKey(t[a.targetKey]); ok {
				byTarget[key] = t
			}
		}
		grouped := map[string][]Values{}
		for _, pair := range pairs {
			source, okSource := associationKey(pair[a.foreignKey])
			target, okTarget := associationKey(pair[a.otherKey])
			if !okSource || !okTarget {
				continue
			}
			if row, ok := byTarget[target]; ok {
				grouped[source] = append(grouped[source], row)
			}
		}
		assignGroups(parents, a.sourceKey, n.as, grouped)
		return nil
	default:
		return fmt.Errorf("%w: %s is not a batched association", ErrInvalidQuery, a.kind)
	}
}

// restrictedAttrs keeps a caller's attribute restriction but adds the key the
// batch is grouped by, which would otherwise be missing from the result.
func restrictedAttrs(attrs []string, key string) []string {
	if len(attrs) == 0 {
		return nil
	}
	return ensureAttrs(append([]string(nil), attrs...), key)
}

// gatherParents walks a chain of association names to the rows a batched node
// hangs off.
func gatherParents(rows []Values, path []string) []Values {
	current := rows
	for _, key := range path {
		next := make([]Values, 0, len(current))
		for _, row := range current {
			switch child := row[key].(type) {
			case Values:
				if child != nil {
					next = append(next, child)
				}
			case []Values:
				next = append(next, child...)
			}
		}
		current = next
	}
	return current
}

// distinctKeyValues collects the distinct non-nil values of attribute, in first
// appearance order so the emitted IN list is deterministic.
func distinctKeyValues(rows []Values, attribute string) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		value := row[attribute]
		key, ok := associationKey(value)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func groupByKey(rows []Values, attribute string) map[string][]Values {
	grouped := map[string][]Values{}
	for _, row := range rows {
		if key, ok := associationKey(row[attribute]); ok {
			grouped[key] = append(grouped[key], row)
		}
	}
	return grouped
}

func assignGroups(parents []Values, key, as string, grouped map[string][]Values) {
	for _, parent := range parents {
		k, ok := associationKey(parent[key])
		if !ok {
			continue
		}
		if children, found := grouped[k]; found {
			parent[as] = children
		}
	}
}

// associationKey renders a key value as a comparable string, collapsing the
// numeric types so a key written as an int and read back as an int64 still
// matches. It reports false for nil, which never joins to anything.
func associationKey(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return "s:" + v, true
	case []byte:
		return "s:" + string(v), true
	case bool:
		if v {
			return "b:1", true
		}
		return "b:0", true
	case time.Time:
		return "t:" + v.UTC().Format(time.RFC3339Nano), true
	default:
		if f, ok := validatorFloat(value); ok {
			return "n:" + formatNumber(f), true
		}
		return "x:" + fmt.Sprintf("%v", value), true
	}
}

// AssociationSummary renders the model's associations one per line, which is
// handy in a test failure or a startup log.
func (m *Model) AssociationSummary() string {
	assocs := m.Associations()
	lines := make([]string, 0, len(assocs))
	for _, a := range assocs {
		lines = append(lines, a.String())
	}
	return strings.Join(lines, "\n")
}
