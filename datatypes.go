package sequelize

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TypeKind enumerates the abstract column types the port understands. Each
// dialect maps a kind to concrete SQL; see [Dialect.ColumnType].
type TypeKind int

// The abstract column kinds, mirroring Sequelize's DataTypes.
const (
	// KindInvalid is the zero value and is never produced by a constructor.
	KindInvalid TypeKind = iota
	// KindInteger is a 32-bit signed integer.
	KindInteger
	// KindBigInt is a 64-bit signed integer.
	KindBigInt
	// KindFloat is a double-precision floating point number.
	KindFloat
	// KindDecimal is an exact numeric with optional precision and scale.
	KindDecimal
	// KindString is a bounded variable-length string.
	KindString
	// KindText is an unbounded string.
	KindText
	// KindBoolean is a truth value.
	KindBoolean
	// KindDate is a timestamp.
	KindDate
	// KindDateOnly is a calendar date with no time component.
	KindDateOnly
	// KindJSON is a JSON document, decoded into any on read.
	KindJSON
	// KindBlob is opaque binary data.
	KindBlob
	// KindUUID is a 36-character UUID string.
	KindUUID
	// KindEnum is a string restricted to a fixed set of values.
	KindEnum
)

// String returns the kind's Sequelize-style name, for example "STRING".
func (k TypeKind) String() string {
	switch k {
	case KindInteger:
		return "INTEGER"
	case KindBigInt:
		return "BIGINT"
	case KindFloat:
		return "FLOAT"
	case KindDecimal:
		return "DECIMAL"
	case KindString:
		return "STRING"
	case KindText:
		return "TEXT"
	case KindBoolean:
		return "BOOLEAN"
	case KindDate:
		return "DATE"
	case KindDateOnly:
		return "DATEONLY"
	case KindJSON:
		return "JSON"
	case KindBlob:
		return "BLOB"
	case KindUUID:
		return "UUID"
	case KindEnum:
		return "ENUM"
	default:
		return "INVALID"
	}
}

// DataType describes a column's abstract type. Build one with the constructors
// ([INTEGER], [STRING], [ENUM], ...) rather than by literal, so that the
// kind-specific fields stay consistent.
type DataType struct {
	// Kind is the abstract type.
	Kind TypeKind
	// Length is the maximum length for [KindString]; zero means the dialect
	// default (255).
	Length int
	// Precision is the total digit count for [KindDecimal]; zero means unspecified.
	Precision int
	// Scale is the fractional digit count for [KindDecimal].
	Scale int
	// Values enumerates the permitted strings for [KindEnum].
	Values []string
}

// String renders the type the way Sequelize spells it, e.g. "STRING(64)".
func (t DataType) String() string {
	switch t.Kind {
	case KindString:
		if t.Length > 0 {
			return fmt.Sprintf("STRING(%d)", t.Length)
		}
	case KindDecimal:
		if t.Precision > 0 {
			return fmt.Sprintf("DECIMAL(%d,%d)", t.Precision, t.Scale)
		}
	case KindEnum:
		return "ENUM(" + strings.Join(t.Values, ", ") + ")"
	}
	return t.Kind.String()
}

// IsZero reports whether the type was never initialised by a constructor.
func (t DataType) IsZero() bool { return t.Kind == KindInvalid }

// INTEGER returns a 32-bit signed integer column type.
func INTEGER() DataType { return DataType{Kind: KindInteger} }

// BIGINT returns a 64-bit signed integer column type.
func BIGINT() DataType { return DataType{Kind: KindBigInt} }

// FLOAT returns a double-precision floating point column type.
func FLOAT() DataType { return DataType{Kind: KindFloat} }

// DECIMAL returns an exact numeric column type. Pass no arguments for the
// dialect default, one for precision, or two for precision and scale; extra
// arguments are ignored. Decimal values are carried as strings to avoid binary
// floating-point rounding.
func DECIMAL(precisionScale ...int) DataType {
	t := DataType{Kind: KindDecimal}
	if len(precisionScale) > 0 {
		t.Precision = precisionScale[0]
	}
	if len(precisionScale) > 1 {
		t.Scale = precisionScale[1]
	}
	return t
}

// STRING returns a bounded variable-length string column type. A non-positive
// length selects the dialect default of 255.
func STRING(length int) DataType {
	if length < 0 {
		length = 0
	}
	return DataType{Kind: KindString, Length: length}
}

// TEXT returns an unbounded string column type.
func TEXT() DataType { return DataType{Kind: KindText} }

// BOOLEAN returns a truth-valued column type.
func BOOLEAN() DataType { return DataType{Kind: KindBoolean} }

// DATE returns a timestamp column type. Values round-trip as [time.Time] in UTC.
func DATE() DataType { return DataType{Kind: KindDate} }

// DATEONLY returns a calendar-date column type. Values round-trip as a
// [time.Time] whose clock is midnight UTC.
func DATEONLY() DataType { return DataType{Kind: KindDateOnly} }

// JSON returns a JSON document column type. Values are marshalled on write and
// unmarshalled into any on read.
func JSON() DataType { return DataType{Kind: KindJSON} }

// BLOB returns an opaque binary column type. Values round-trip as []byte.
func BLOB() DataType { return DataType{Kind: KindBlob} }

// UUID returns a UUID column type. The port does not generate UUIDs; supply one
// as a value or a DefaultValue.
func UUID() DataType { return DataType{Kind: KindUUID} }

// ENUM returns a string column type restricted to values. Enum columns with no
// values are rejected at Sync time by dialects that need the list.
func ENUM(values ...string) DataType {
	copied := make([]string, len(values))
	copy(copied, values)
	return DataType{Kind: KindEnum, Values: copied}
}

// The wire formats the port writes timestamps in. Both are fixed-width and in
// UTC so that a textual column sorts and compares chronologically, which
// time.RFC3339Nano does not guarantee: it trims trailing zeros from the
// fractional part, making ".5Z" sort after ".55Z".
const (
	dateOnlyLayout = "2006-01-02"
	dateLayout     = "2006-01-02T15:04:05.000000Z07:00"
)

// decodeValue converts a value as returned by a database/sql driver into the Go
// type the port promises for t. A nil input stays nil so that NULL is
// distinguishable from a zero value.
func decodeValue(t DataType, raw any) (any, error) {
	if raw == nil {
		return nil, nil
	}
	switch t.Kind {
	case KindInteger, KindBigInt:
		return decodeInt(t, raw)
	case KindFloat:
		return decodeFloat(t, raw)
	case KindBoolean:
		return decodeBool(t, raw)
	case KindDecimal, KindString, KindText, KindUUID, KindEnum:
		return decodeString(t, raw)
	case KindDate, KindDateOnly:
		return decodeTime(t, raw)
	case KindJSON:
		return decodeJSON(t, raw)
	case KindBlob:
		switch v := raw.(type) {
		case []byte:
			out := make([]byte, len(v))
			copy(out, v)
			return out, nil
		case string:
			return []byte(v), nil
		}
		return nil, typeError(t, raw)
	default:
		return raw, nil
	}
}

func decodeInt(t DataType, raw any) (any, error) {
	switch v := raw.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case float64:
		return int64(v), nil
	case bool:
		if v {
			return int64(1), nil
		}
		return int64(0), nil
	case []byte:
		return parseInt(t, string(v))
	case string:
		return parseInt(t, v)
	}
	return nil, typeError(t, raw)
}

func parseInt(t DataType, s string) (any, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil, typeError(t, s)
	}
	return n, nil
}

func decodeFloat(t DataType, raw any) (any, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case []byte:
		return parseFloat(t, string(v))
	case string:
		return parseFloat(t, v)
	}
	return nil, typeError(t, raw)
}

func parseFloat(t DataType, s string) (any, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil, typeError(t, s)
	}
	return f, nil
}

func decodeBool(t DataType, raw any) (any, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case int64:
		return v != 0, nil
	case float64:
		return v != 0, nil
	case []byte:
		return parseBool(t, string(v))
	case string:
		return parseBool(t, v)
	}
	return nil, typeError(t, raw)
}

func parseBool(t DataType, s string) (any, error) {
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return nil, typeError(t, s)
	}
	return b, nil
}

func decodeString(t DataType, raw any) (any, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(v), nil
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano), nil
	}
	return nil, typeError(t, raw)
}

// timeLayouts are tried in order when decoding a textual timestamp. Drivers and
// hand-written SQL disagree on separators and fractional precision, so the list
// is deliberately generous.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	dateOnlyLayout,
}

func decodeTime(t DataType, raw any) (any, error) {
	var s string
	switch v := raw.(type) {
	case time.Time:
		return normaliseTime(t, v), nil
	case string:
		s = v
	case []byte:
		s = string(v)
	case int64:
		return normaliseTime(t, time.Unix(v, 0).UTC()), nil
	default:
		return nil, typeError(t, raw)
	}
	s = strings.TrimSpace(s)
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return normaliseTime(t, parsed), nil
		}
	}
	return nil, typeError(t, s)
}

func normaliseTime(t DataType, v time.Time) time.Time {
	v = v.UTC()
	if t.Kind == KindDateOnly {
		return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)
	}
	return v
}

func decodeJSON(t DataType, raw any) (any, error) {
	var data []byte
	switch v := raw.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		// A driver that already decoded the document hands it back as-is.
		return raw, nil
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidType, t, err)
	}
	return out, nil
}

// encodeValue converts a Go value into the portable representation the port
// binds as a placeholder argument. Dialects may refine it further in
// Dialect.BindValue.
func encodeValue(t DataType, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch t.Kind {
	case KindJSON:
		switch tv := v.(type) {
		case string:
			return tv, nil
		case []byte:
			return string(tv), nil
		}
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrInvalidType, t, err)
		}
		return string(data), nil
	case KindDate:
		if tv, ok := v.(time.Time); ok {
			return tv.UTC().Format(dateLayout), nil
		}
	case KindDateOnly:
		if tv, ok := v.(time.Time); ok {
			return tv.UTC().Format(dateOnlyLayout), nil
		}
	case KindEnum:
		s, ok := v.(string)
		if !ok {
			return nil, typeError(t, v)
		}
		if len(t.Values) > 0 && !containsString(t.Values, s) {
			return nil, fmt.Errorf("%w: %q is not a member of %s", ErrInvalidType, s, t)
		}
		return s, nil
	case KindBlob:
		switch tv := v.(type) {
		case []byte:
			return tv, nil
		case string:
			return []byte(tv), nil
		}
		return nil, typeError(t, v)
	}
	return v, nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func typeError(t DataType, v any) error {
	return fmt.Errorf("%w: cannot use %T (%v) as %s", ErrInvalidType, v, v, t)
}
