package sequelize

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// identifierPattern is the only shape of identifier the port will emit. Anything
// else is rejected with [ErrInvalidIdentifier] rather than escaped.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateIdentifier reports whether name is a legal SQL identifier for this
// package, returning an error wrapping [ErrInvalidIdentifier] if it is not.
//
// The rule is deliberately strict — ^[A-Za-z_][A-Za-z0-9_]*$ — because the port
// never interpolates a user value into SQL text, and an identifier is the one
// piece of a statement that cannot become a placeholder. A name that fails here
// is a bug or an attack, never something to quote around.
func ValidateIdentifier(name string) error {
	if !identifierPattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidIdentifier, name)
	}
	return nil
}

// Dialect captures everything that differs between database backends: how
// identifiers are quoted, how bind parameters are spelled, how abstract
// [DataType] values map to concrete SQL, how driver errors map to the package's
// typed errors, and how Go values are bound.
//
// Implement it to support a backend the package does not ship, then make it
// available to [New] and [Open] with [Register]. The package ships one
// implementation, [SQLite].
type Dialect interface {
	// Name returns the dialect's short lowercase name, as passed to [New].
	Name() string

	// Quote validates name with [ValidateIdentifier] and returns it quoted for
	// the backend. It must not attempt to escape an invalid identifier.
	Quote(name string) (string, error)

	// Placeholder renders the bind parameter marker for a 1-based argument
	// position, for example "?" or "$1".
	Placeholder(position int) string

	// ColumnType renders the concrete SQL type for an abstract [DataType].
	ColumnType(t DataType) (string, error)

	// AutoIncrementColumn returns the complete column definition for a
	// single-column auto-increment primary key. quoted is the already-quoted
	// column name.
	AutoIncrementColumn(quoted string, t DataType) (string, error)

	// BindValue converts an already portably-encoded value (see the DataType
	// encoding rules) into something the backend's driver accepts.
	BindValue(t DataType, v any) (any, error)

	// TranslateError maps a driver error onto [UniqueConstraintError],
	// [ForeignKeyError] or, when it recognises neither, returns err unchanged.
	TranslateError(err error) error

	// SupportsReturning reports whether INSERT ... RETURNING is available.
	SupportsReturning() bool
}

var (
	dialectsMu sync.RWMutex
	dialects   = map[string]Dialect{}
)

// Register makes d available to [New] and [Open] under d.Name(). It returns an
// error wrapping [ErrDialectRegistered] if the name is taken, or
// [ErrInvalidIdentifier] if the name is empty. Registration is safe for
// concurrent use and is typically done from an init function.
func Register(d Dialect) error {
	if d == nil {
		return fmt.Errorf("%w: nil dialect", ErrUnknownDialect)
	}
	name := strings.ToLower(strings.TrimSpace(d.Name()))
	if name == "" {
		return fmt.Errorf("%w: empty dialect name", ErrInvalidIdentifier)
	}
	dialectsMu.Lock()
	defer dialectsMu.Unlock()
	if _, ok := dialects[name]; ok {
		return fmt.Errorf("%w: %q", ErrDialectRegistered, name)
	}
	dialects[name] = d
	return nil
}

// DialectByName returns the dialect registered under name, matched
// case-insensitively, or an error wrapping [ErrUnknownDialect].
func DialectByName(name string) (Dialect, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	dialectsMu.RLock()
	d, ok := dialects[key]
	dialectsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownDialect, name)
	}
	return d, nil
}

// Dialects returns the names of every registered dialect, sorted.
func Dialects() []string {
	dialectsMu.RLock()
	names := make([]string, 0, len(dialects))
	for name := range dialects {
		names = append(names, name)
	}
	dialectsMu.RUnlock()
	sort.Strings(names)
	return names
}
