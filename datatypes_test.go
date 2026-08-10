package sequelize

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestDataTypeConstructorsAndStrings(t *testing.T) {
	cases := []struct {
		dt   DataType
		kind TypeKind
		want string
	}{
		{INTEGER(), KindInteger, "INTEGER"},
		{BIGINT(), KindBigInt, "BIGINT"},
		{FLOAT(), KindFloat, "FLOAT"},
		{DECIMAL(), KindDecimal, "DECIMAL"},
		{DECIMAL(10, 2), KindDecimal, "DECIMAL(10,2)"},
		{STRING(64), KindString, "STRING(64)"},
		{STRING(0), KindString, "STRING"},
		{STRING(-5), KindString, "STRING"},
		{TEXT(), KindText, "TEXT"},
		{BOOLEAN(), KindBoolean, "BOOLEAN"},
		{DATE(), KindDate, "DATE"},
		{DATEONLY(), KindDateOnly, "DATEONLY"},
		{JSON(), KindJSON, "JSON"},
		{BLOB(), KindBlob, "BLOB"},
		{UUID(), KindUUID, "UUID"},
		{ENUM("a", "b"), KindEnum, "ENUM(a, b)"},
	}
	for _, c := range cases {
		if c.dt.Kind != c.kind {
			t.Errorf("%s: Kind = %v, want %v", c.want, c.dt.Kind, c.kind)
		}
		if got := c.dt.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
		if c.dt.IsZero() {
			t.Errorf("%s: IsZero() = true for a constructed type", c.want)
		}
	}
	if !(DataType{}).IsZero() {
		t.Error("zero DataType.IsZero() = false")
	}
	if got := TypeKind(99).String(); got != "INVALID" {
		t.Errorf("unknown kind String() = %q, want INVALID", got)
	}
}

func TestEnumCopiesValues(t *testing.T) {
	src := []string{"a", "b"}
	dt := ENUM(src...)
	src[0] = "mutated"
	if dt.Values[0] != "a" {
		t.Errorf("ENUM kept a reference to the caller's slice: %v", dt.Values)
	}
}

func TestDecodeValueNilStaysNil(t *testing.T) {
	for _, dt := range []DataType{INTEGER(), STRING(10), DATE(), JSON(), BLOB(), BOOLEAN()} {
		got, err := decodeValue(dt, nil)
		if err != nil || got != nil {
			t.Errorf("decodeValue(%s, nil) = %v, %v; want nil, nil", dt, got, err)
		}
	}
}

func TestDecodeValueByKind(t *testing.T) {
	cases := []struct {
		name string
		dt   DataType
		raw  any
		want any
	}{
		{"int from int64", INTEGER(), int64(7), int64(7)},
		{"int from float", INTEGER(), 7.0, int64(7)},
		{"int from text", BIGINT(), []byte("42"), int64(42)},
		{"int from bool", INTEGER(), true, int64(1)},
		{"float from int", FLOAT(), int64(3), 3.0},
		{"float from text", FLOAT(), "2.5", 2.5},
		{"bool from int", BOOLEAN(), int64(1), true},
		{"bool from zero", BOOLEAN(), int64(0), false},
		{"bool from text", BOOLEAN(), "true", true},
		{"string from bytes", TEXT(), []byte("hi"), "hi"},
		{"string from int", STRING(10), int64(5), "5"},
		{"decimal as string", DECIMAL(10, 2), "1.25", "1.25"},
		{"blob from string", BLOB(), "ab", []byte("ab")},
		{"json object", JSON(), `{"a":1}`, map[string]any{"a": 1.0}},
		{"json empty", JSON(), "  ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeValue(c.dt, c.raw)
			if err != nil {
				t.Fatalf("decodeValue: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("decodeValue(%s, %v) = %#v, want %#v", c.dt, c.raw, got, c.want)
			}
		})
	}
}

func TestDecodeTimeLayouts(t *testing.T) {
	want := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, raw := range []any{
		"2024-03-04T05:06:07Z",
		"2024-03-04 05:06:07",
		"2024-03-04T05:06:07.000000Z",
		want,
	} {
		got, err := decodeValue(DATE(), raw)
		if err != nil {
			t.Fatalf("decodeValue(DATE(), %v): %v", raw, err)
		}
		if !got.(time.Time).Equal(want) {
			t.Errorf("decodeValue(DATE(), %v) = %v, want %v", raw, got, want)
		}
	}
}

func TestDecodeDateOnlyTruncatesClock(t *testing.T) {
	got, err := decodeValue(DATEONLY(), "2024-03-04T05:06:07Z")
	if err != nil {
		t.Fatalf("decodeValue: %v", err)
	}
	want := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	if !got.(time.Time).Equal(want) {
		t.Errorf("DATEONLY decoded to %v, want %v", got, want)
	}
}

func TestDecodeValueRejectsNonsense(t *testing.T) {
	cases := []struct {
		dt  DataType
		raw any
	}{
		{INTEGER(), "not a number"},
		{FLOAT(), "not a number"},
		{BOOLEAN(), "maybe"},
		{DATE(), "yesterday"},
		{BLOB(), 3.5},
		{STRING(4), struct{}{}},
		{JSON(), "{not json"},
	}
	for _, c := range cases {
		if _, err := decodeValue(c.dt, c.raw); !errors.Is(err, ErrInvalidType) {
			t.Errorf("decodeValue(%s, %v) error = %v, want ErrInvalidType", c.dt, c.raw, err)
		}
	}
}

func TestEncodeValue(t *testing.T) {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name string
		dt   DataType
		in   any
		want any
	}{
		{"nil", STRING(4), nil, nil},
		{"date fixed width", DATE(), ts, "2024-01-02T03:04:05.000000Z"},
		{"date only", DATEONLY(), ts, "2024-01-02"},
		{"json marshalled", JSON(), map[string]any{"a": 1}, `{"a":1}`},
		{"json passthrough", JSON(), `{"a":1}`, `{"a":1}`},
		{"enum member", ENUM("a", "b"), "b", "b"},
		{"blob from string", BLOB(), "xy", []byte("xy")},
		{"scalar untouched", INTEGER(), 3, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := encodeValue(c.dt, c.in)
			if err != nil {
				t.Fatalf("encodeValue: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("encodeValue(%s, %v) = %#v, want %#v", c.dt, c.in, got, c.want)
			}
		})
	}
}

func TestEncodeValueRejectsBadEnumAndBlob(t *testing.T) {
	if _, err := encodeValue(ENUM("a"), "z"); !errors.Is(err, ErrInvalidType) {
		t.Errorf("encodeValue(ENUM, non-member) error = %v, want ErrInvalidType", err)
	}
	if _, err := encodeValue(ENUM("a"), 1); !errors.Is(err, ErrInvalidType) {
		t.Errorf("encodeValue(ENUM, int) error = %v, want ErrInvalidType", err)
	}
	if _, err := encodeValue(BLOB(), 1.5); !errors.Is(err, ErrInvalidType) {
		t.Errorf("encodeValue(BLOB, float) error = %v, want ErrInvalidType", err)
	}
	if _, err := encodeValue(JSON(), func() {}); !errors.Is(err, ErrInvalidType) {
		t.Errorf("encodeValue(JSON, func) error = %v, want ErrInvalidType", err)
	}
}

func TestDateWireFormatSortsChronologically(t *testing.T) {
	early, err := encodeValue(DATE(), time.Date(2024, 1, 1, 0, 0, 0, 500000000, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	late, err := encodeValue(DATE(), time.Date(2024, 1, 1, 0, 0, 0, 550000000, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if early.(string) >= late.(string) {
		t.Errorf("wire format is not lexicographically ordered: %q >= %q", early, late)
	}
}
