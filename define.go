package sequelize

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"
)

// LogFunc receives every statement the package executes, together with its bind
// arguments, before it runs. Install one with [WithLogger].
type LogFunc func(ctx context.Context, query string, args []any)

// config holds the settings a [Option] may change. It is resolved once by [New]
// or [Open].
type config struct {
	driver string
	logger LogFunc
	clock  func() time.Time
}

// Option configures a connection at [New] or [Open] time.
type Option func(*config)

// WithDriver overrides the database/sql driver name used by [New]. It defaults
// to the dialect name, which is right for drivers registered as "sqlite",
// "postgres" or "mysql" and wrong for anything else.
func WithDriver(name string) Option {
	return func(c *config) { c.driver = name }
}

// WithLogger installs a statement logger. It is called with the rendered SQL and
// bind arguments immediately before execution; it must not retain args.
func WithLogger(fn LogFunc) Option {
	return func(c *config) { c.logger = fn }
}

// WithClock overrides the source of timestamps for createdAt, updatedAt and
// deletedAt. It exists for deterministic tests; production code should leave it
// alone.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.clock = now }
}

// Sequelize is a connection: a database/sql handle, the [Dialect] that renders
// SQL for it, and the models defined against it. It is safe for concurrent use.
//
// Create one with [New] (which opens the handle) or [Open] (which adopts one you
// already have). [Sequelize.Close] closes the handle only when the connection
// opened it.
type Sequelize struct {
	db      *sql.DB
	dialect Dialect
	logger  LogFunc
	clock   func() time.Time
	ownsDB  bool

	mu     sync.RWMutex
	models map[string]*Model
	order  []string
	closed bool
}

// New opens a database/sql handle for dsn using the driver named after dialect
// (override it with [WithDriver]) and returns a connection rendering SQL for
// that dialect.
//
// The driver must already be registered with database/sql; this package
// deliberately imports no driver. As with sql.Open, no connection is
// established until the handle is first used, so a bad DSN surfaces on the first
// query or on [Sequelize.Ping].
func New(dialect, dsn string, opts ...Option) (*Sequelize, error) {
	d, err := DialectByName(dialect)
	if err != nil {
		return nil, err
	}
	cfg := resolveConfig(d, opts)
	db, err := sql.Open(cfg.driver, dsn)
	if err != nil {
		return nil, err
	}
	s := newSequelize(db, d, cfg)
	s.ownsDB = true
	return s, nil
}

// Open adopts an already-open *sql.DB. The caller keeps ownership of the handle:
// [Sequelize.Close] will not close it.
func Open(dialect string, db *sql.DB, opts ...Option) (*Sequelize, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: nil *sql.DB", ErrClosed)
	}
	d, err := DialectByName(dialect)
	if err != nil {
		return nil, err
	}
	return newSequelize(db, d, resolveConfig(d, opts)), nil
}

func resolveConfig(d Dialect, opts []Option) config {
	cfg := config{driver: d.Name(), clock: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.clock == nil {
		cfg.clock = time.Now
	}
	if cfg.driver == "" {
		cfg.driver = d.Name()
	}
	return cfg
}

func newSequelize(db *sql.DB, d Dialect, cfg config) *Sequelize {
	return &Sequelize{
		db:      db,
		dialect: d,
		logger:  cfg.logger,
		clock:   cfg.clock,
		models:  map[string]*Model{},
	}
}

// DB returns the underlying database/sql handle, for the things this package
// does not do.
func (s *Sequelize) DB() *sql.DB { return s.db }

// Dialect returns the dialect rendering SQL for this connection.
func (s *Sequelize) Dialect() Dialect { return s.dialect }

// Define registers a model and returns it, mirroring sequelize.define. Only
// [Attribute.Type] is required per attribute; everything else defaults the way
// upstream defaults it, most importantly AllowNull, which permits NULL.
//
// The signature carries no error, as upstream's does not. A rejected definition
// — a bad identifier, a missing Type, a redefinition — is reported by
// [Model.Err] and by every operation on the returned model. Use
// [Sequelize.DefineModel] when you would rather have the error immediately.
func (s *Sequelize) Define(name string, attrs Attributes, opts ...ModelOption) *Model {
	m, err := s.DefineModel(name, attrs, opts...)
	if err != nil && m == nil {
		m = &Model{seq: s, name: name, attrs: map[string]Attribute{}, err: err}
	}
	return m
}

// DefineModel is [Sequelize.Define] with the resolution error returned rather
// than stored. It is an extension: upstream has no such method.
//
// On failure it still returns the (unusable) model, so callers may inspect it.
func (s *Sequelize) DefineModel(name string, attrs Attributes, opts ...ModelOption) (*Model, error) {
	m := newModel(s, name, attrs, opts...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.models[name]; exists {
		err := fmt.Errorf("%w: %q", ErrModelRedefined, name)
		m.err = err
		return m, err
	}
	if m.err != nil {
		return m, m.err
	}
	s.models[name] = m
	s.order = append(s.order, name)
	return m, nil
}

// Model returns the model defined under name.
func (s *Sequelize) Model(name string) (*Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.models[name]
	return m, ok
}

// MustModel returns the model defined under name, or an error wrapping
// [ErrUnknownModel]. It is an extension, for call sites that would otherwise
// have to check a bool.
func (s *Sequelize) MustModel(name string) (*Model, error) {
	if m, ok := s.Model(name); ok {
		return m, m.Err()
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownModel, name)
}

// ModelNames returns the names of every defined model, sorted.
func (s *Sequelize) ModelNames() []string {
	s.mu.RLock()
	names := make([]string, 0, len(s.models))
	for name := range s.models {
		names = append(names, name)
	}
	s.mu.RUnlock()
	sort.Strings(names)
	return names
}

// definedModels returns the models in definition order, which is the order Sync
// creates their tables in.
func (s *Sequelize) definedModels() []*Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Model, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.models[name])
	}
	return out
}

// Ping verifies that the database is reachable.
func (s *Sequelize) Ping(ctx context.Context) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	return s.db.PingContext(ctx)
}

// Close releases the connection. The underlying *sql.DB is closed only when this
// package opened it, that is when the connection came from [New]. Close is
// idempotent.
func (s *Sequelize) Close() error {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if already || !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

func (s *Sequelize) checkOpen() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}

// now reads the connection's clock.
func (s *Sequelize) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// log hands a statement to the installed logger, if any.
func (s *Sequelize) log(ctx context.Context, query string, args []any) {
	if s.logger != nil {
		s.logger(ctx, query, args)
	}
}
