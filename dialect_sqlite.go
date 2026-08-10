package sequelize

import (
	"fmt"
	"strings"
)

// SQLite is the SQLite [Dialect]: double-quoted identifiers, "?" placeholders,
// INTEGER PRIMARY KEY AUTOINCREMENT, and booleans stored as 0/1.
//
// It is registered under the names "sqlite" and "sqlite3" when the package is
// loaded. The dialect itself pulls in no driver: to use it, register a
// database/sql driver yourself — modernc.org/sqlite is the pure-Go choice the
// package's own tests use — and pass its DSN to [New], or hand an open *sql.DB
// to [Open].
var SQLite Dialect = sqliteDialect{name: "sqlite"}

func init() {
	// Registration errors here would mean the package is registering the same
	// name twice, which is a programming error in this file alone.
	if err := Register(sqliteDialect{name: "sqlite"}); err != nil {
		panic(err)
	}
	if err := Register(sqliteDialect{name: "sqlite3"}); err != nil {
		panic(err)
	}
}

// sqliteDialect implements Dialect for SQLite. The name field lets the same
// implementation answer to both "sqlite" and "sqlite3".
type sqliteDialect struct{ name string }

func (d sqliteDialect) Name() string { return d.name }

func (d sqliteDialect) Quote(name string) (string, error) {
	if err := ValidateIdentifier(name); err != nil {
		return "", err
	}
	return `"` + name + `"`, nil
}

func (d sqliteDialect) Placeholder(int) string { return "?" }

func (d sqliteDialect) ColumnType(t DataType) (string, error) {
	switch t.Kind {
	case KindInteger, KindBigInt:
		return "INTEGER", nil
	case KindFloat:
		return "REAL", nil
	case KindDecimal:
		if t.Precision > 0 {
			return fmt.Sprintf("NUMERIC(%d,%d)", t.Precision, t.Scale), nil
		}
		return "NUMERIC", nil
	case KindString:
		length := t.Length
		if length <= 0 {
			length = 255
		}
		return fmt.Sprintf("VARCHAR(%d)", length), nil
	case KindText, KindJSON:
		return "TEXT", nil
	case KindBoolean:
		return "BOOLEAN", nil
	case KindDate:
		return "DATETIME", nil
	case KindDateOnly:
		return "DATE", nil
	case KindBlob:
		return "BLOB", nil
	case KindUUID:
		return "VARCHAR(36)", nil
	case KindEnum:
		if len(t.Values) == 0 {
			return "", fmt.Errorf("%w: ENUM with no values", ErrUnsupported)
		}
		return "TEXT", nil
	default:
		return "", fmt.Errorf("%w: sqlite cannot express %s", ErrUnsupported, t)
	}
}

func (d sqliteDialect) AutoIncrementColumn(quoted string, t DataType) (string, error) {
	switch t.Kind {
	case KindInteger, KindBigInt, KindInvalid:
		// SQLite only auto-increments a column declared exactly INTEGER PRIMARY KEY.
		return quoted + " INTEGER PRIMARY KEY AUTOINCREMENT", nil
	default:
		return "", fmt.Errorf("%w: sqlite auto-increment requires an integer column, got %s", ErrUnsupported, t)
	}
}

func (d sqliteDialect) BindValue(t DataType, v any) (any, error) {
	switch tv := v.(type) {
	case nil:
		return nil, nil
	case bool:
		// SQLite has no boolean storage class.
		if tv {
			return int64(1), nil
		}
		return int64(0), nil
	case int:
		return int64(tv), nil
	case int8:
		return int64(tv), nil
	case int16:
		return int64(tv), nil
	case int32:
		return int64(tv), nil
	case uint:
		return int64(tv), nil
	case uint8:
		return int64(tv), nil
	case uint16:
		return int64(tv), nil
	case uint32:
		return int64(tv), nil
	case float32:
		return float64(tv), nil
	}
	return v, nil
}

func (d sqliteDialect) SupportsReturning() bool { return true }

func (d sqliteDialect) TranslateError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return &UniqueConstraintError{Fields: sqliteConstraintFields(msg, "UNIQUE constraint failed:"), Cause: err}
	case strings.Contains(msg, "PRIMARY KEY constraint failed"):
		return &UniqueConstraintError{Fields: sqliteConstraintFields(msg, "PRIMARY KEY constraint failed:"), Cause: err}
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return &ForeignKeyError{Cause: err}
	default:
		return err
	}
}

// sqliteConstraintFields pulls the "table.column, table.column" list SQLite puts
// after a constraint message and returns the bare column names.
func sqliteConstraintFields(msg, prefix string) []string {
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return nil
	}
	rest := msg[idx+len(prefix):]
	if end := strings.IndexByte(rest, '('); end >= 0 {
		rest = rest[:end]
	}
	var fields []string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dot := strings.LastIndexByte(part, '.'); dot >= 0 {
			part = part[dot+1:]
		}
		if part != "" {
			fields = append(fields, part)
		}
	}
	return fields
}
