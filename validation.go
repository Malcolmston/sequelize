package sequelize

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Validate checks values against the model's attribute validators the way a
// [Model.Create] would: every declared validator runs for every attribute the
// map supplies, and an attribute declared NOT NULL that the map leaves out (or
// sets to nil) is a failure.
//
// It reports *every* problem it finds as a single *[ValidationError], not the
// first, so a form can be rendered with all of its errors at once. A nil return
// means the values are acceptable. The model's own defaults are not applied
// here; [Model.Create] applies them before validating.
func (m *Model) Validate(values Values) error {
	return m.validateValues(values, false)
}

// ValidateChanges checks values the way a [Model.Update] would: the validators
// run only for the attributes the map supplies, and an omitted NOT NULL
// attribute is not a failure because an update names a subset of the row. An
// attribute set explicitly to nil is still a failure when it is NOT NULL.
func (m *Model) ValidateChanges(values Values) error {
	return m.validateValues(values, true)
}

// validateValues is the enforcement point every write goes through.
func (m *Model) validateValues(values Values, partial bool) error {
	if err := m.Err(); err != nil {
		return err
	}
	return NewValidationError(m.name, m.validateFields(values, partial))
}

// validateFields gathers the failures for one row. It never stops early: the
// aggregate report is the point.
func (m *Model) validateFields(values Values, partial bool) []FieldError {
	var failures []FieldError
	for _, name := range m.names {
		attr := m.attrs[name]
		value, present := values[name]

		if value == nil {
			if attr.Nullable() {
				continue
			}
			// A database-generated key, and a timestamp the model stamps itself, are
			// legitimately absent before the insert.
			if m.isGeneratedKey(name) || m.isManagedTimestamp(name) {
				continue
			}
			if !present && partial {
				continue
			}
			failures = append(failures, FieldError{
				Attribute: name,
				Message:   "cannot be null",
				Value:     nil,
			})
			continue
		}

		for _, validate := range attr.Validate {
			if validate == nil {
				continue
			}
			if err := validate(value); err != nil {
				failures = append(failures, FieldError{
					Attribute: name,
					Message:   err.Error(),
					Value:     value,
				})
			}
		}
	}
	return failures
}

// isManagedTimestamp reports whether the model fills the attribute in itself, in
// which case its absence from a caller's values is not a missing value.
func (m *Model) isManagedTimestamp(name string) bool {
	if m.Timestamps() && (name == m.CreatedAtName() || name == m.UpdatedAtName()) {
		return true
	}
	return m.IsParanoid() && name == m.DeletedAtName()
}

// translateWriteError turns a driver-reported NOT NULL violation into the same
// *[ValidationError] the in-Go validators would have produced, so a caller has
// one thing to branch on whichever side caught the problem. UNIQUE and FOREIGN
// KEY violations are already typed by the [Dialect]; anything unrecognised is
// returned unchanged.
func (m *Model) translateWriteError(err error) error {
	if err == nil {
		return nil
	}
	var (
		ve  *ValidationError
		uce *UniqueConstraintError
		fke *ForeignKeyError
	)
	if errors.As(err, &ve) || errors.As(err, &uce) || errors.As(err, &fke) {
		return err
	}
	column, ok := notNullColumn(err.Error())
	if !ok {
		return err
	}
	attribute := column
	if name, found := m.attributeForColumn(column); found {
		attribute = name
	}
	return &ValidationError{
		Model:  m.name,
		Errors: []FieldError{{Attribute: attribute, Message: "cannot be null"}},
	}
}

// attributeForColumn reverses [Model.Column]: it finds the attribute a physical
// column belongs to, which is what a driver error naming a column needs.
func (m *Model) attributeForColumn(column string) (string, bool) {
	for _, name := range m.names {
		if m.attrs[name].Field == column {
			return name, true
		}
	}
	return "", false
}

// notNullColumn extracts the offending column from a driver's NOT NULL message.
// Two phrasings are recognised: SQLite's "NOT NULL constraint failed: t.c" and
// the PostgreSQL/MySQL family's "null value in column \"c\" ...".
func notNullColumn(msg string) (string, bool) {
	const sqlitePrefix = "NOT NULL constraint failed:"
	if idx := strings.Index(msg, sqlitePrefix); idx >= 0 {
		rest := strings.TrimSpace(msg[idx+len(sqlitePrefix):])
		if end := strings.IndexAny(rest, " ,("); end >= 0 {
			rest = rest[:end]
		}
		if dot := strings.LastIndexByte(rest, '.'); dot >= 0 {
			rest = rest[dot+1:]
		}
		if rest != "" {
			return rest, true
		}
		return "", false
	}
	if !strings.Contains(msg, "not-null constraint") && !strings.Contains(msg, "cannot be null") {
		return "", false
	}
	for _, quote := range []byte{'"', '\''} {
		start := strings.IndexByte(msg, quote)
		if start < 0 {
			continue
		}
		if end := strings.IndexByte(msg[start+1:], quote); end > 0 {
			return msg[start+1 : start+1+end], true
		}
	}
	return "", false
}

// The built-in validators. Each returns a [Validator] for [Attribute.Validate],
// and each is a no-op on a nil value: nullability is [Attribute.AllowNull]'s
// business, so a validator never doubles as a NOT NULL check.

// NotEmpty rejects a string that is empty or contains only whitespace.
func NotEmpty() Validator {
	return func(value any) error {
		s, ok := validatorString(value)
		if !ok {
			return typeExpectation("a string", value)
		}
		if strings.TrimSpace(s) == "" {
			return errors.New("must not be empty")
		}
		return nil
	}
}

// MinLen rejects a string shorter than n characters. Length is counted in runes,
// not bytes, so a multi-byte character counts once.
func MinLen(n int) Validator {
	return func(value any) error {
		s, ok := validatorString(value)
		if !ok {
			return typeExpectation("a string", value)
		}
		if got := utf8.RuneCountInString(s); got < n {
			return fmt.Errorf("must be at least %d characters, got %d", n, got)
		}
		return nil
	}
}

// MaxLen rejects a string longer than n characters, counted in runes.
func MaxLen(n int) Validator {
	return func(value any) error {
		s, ok := validatorString(value)
		if !ok {
			return typeExpectation("a string", value)
		}
		if got := utf8.RuneCountInString(s); got > n {
			return fmt.Errorf("must be at most %d characters, got %d", n, got)
		}
		return nil
	}
}

// Len rejects a string whose rune length is outside the inclusive range
// [min, max]. It is upstream's `len: [min, max]`.
func Len(min, max int) Validator {
	return func(value any) error {
		s, ok := validatorString(value)
		if !ok {
			return typeExpectation("a string", value)
		}
		got := utf8.RuneCountInString(s)
		if got < min || got > max {
			return fmt.Errorf("must be between %d and %d characters, got %d", min, max, got)
		}
		return nil
	}
}

// MinValue rejects a number below min. Every Go integer, unsigned integer and
// floating-point type is accepted.
func MinValue(min float64) Validator {
	return func(value any) error {
		f, ok := validatorFloat(value)
		if !ok {
			return typeExpectation("a number", value)
		}
		if f < min {
			return fmt.Errorf("must be at least %s", formatNumber(min))
		}
		return nil
	}
}

// MaxValue rejects a number above max.
func MaxValue(max float64) Validator {
	return func(value any) error {
		f, ok := validatorFloat(value)
		if !ok {
			return typeExpectation("a number", value)
		}
		if f > max {
			return fmt.Errorf("must be at most %s", formatNumber(max))
		}
		return nil
	}
}

// Matches rejects a string that the regular expression does not match anywhere.
// Anchor the pattern yourself when you mean the whole value.
//
// A pattern that does not compile produces a validator that fails every value
// with the compilation error, rather than panicking at definition time or
// silently accepting everything.
func Matches(pattern string) Validator {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return func(any) error {
			return fmt.Errorf("cannot be validated: bad pattern %q: %v", pattern, err)
		}
	}
	return func(value any) error {
		s, ok := validatorString(value)
		if !ok {
			return typeExpectation("a string", value)
		}
		if !re.MatchString(s) {
			return fmt.Errorf("must match %s", pattern)
		}
		return nil
	}
}

// IsIn rejects anything that is not one of allowed. Values are compared by their
// printed form, so IsIn(1, 2) accepts an int64 1 read back from the database as
// readily as an int 1 written to it.
func IsIn(allowed ...any) Validator {
	set := make(map[string]bool, len(allowed))
	for _, v := range allowed {
		set[validatorCompare(v)] = true
	}
	return func(value any) error {
		if set[validatorCompare(value)] {
			return nil
		}
		return fmt.Errorf("must be one of %s", strings.Join(printableList(allowed), ", "))
	}
}

// NotIn rejects anything that is one of denied, compared as [IsIn] compares.
func NotIn(denied ...any) Validator {
	set := make(map[string]bool, len(denied))
	for _, v := range denied {
		set[validatorCompare(v)] = true
	}
	return func(value any) error {
		if set[validatorCompare(value)] {
			return fmt.Errorf("must not be one of %s", strings.Join(printableList(denied), ", "))
		}
		return nil
	}
}

// IsEmail rejects a string that is not a plausible email address. It is
// deliberately "email-ish": it accepts what net/mail can parse as a single
// address with a domain part, and makes no attempt to decide whether the
// mailbox exists.
func IsEmail() Validator {
	return func(value any) error {
		s, ok := validatorString(value)
		if !ok {
			return typeExpectation("a string", value)
		}
		addr, err := mail.ParseAddress(s)
		if err != nil || addr.Address != s {
			return errors.New("must be a valid email address")
		}
		at := strings.LastIndexByte(s, '@')
		if at <= 0 || !strings.Contains(s[at+1:], ".") || strings.HasSuffix(s, ".") {
			return errors.New("must be a valid email address")
		}
		return nil
	}
}

// Custom builds a validator from a predicate, for the checks the built-ins do
// not cover. message is what the resulting [FieldError] reports; ok is called
// only for non-nil values.
//
//	"age": {Type: sequelize.INTEGER(), Validate: []sequelize.Validator{
//	    sequelize.Custom("must be even", func(v any) bool {
//	        n, _ := v.(int64)
//	        return n%2 == 0
//	    }),
//	}}
func Custom(message string, ok func(value any) bool) Validator {
	return func(value any) error {
		if ok == nil || ok(value) {
			return nil
		}
		return errors.New(message)
	}
}

// validatorString coerces the string-ish values a driver may hand back.
func validatorString(value any) (string, bool) {
	switch s := value.(type) {
	case string:
		return s, true
	case []byte:
		return string(s), true
	default:
		return "", false
	}
}

// validatorFloat coerces every Go numeric type to a float64 for range checks.
// A float64 holds every int64 a database column realistically carries, and the
// comparison is against a float64 bound in any case.
func validatorFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// validatorCompare renders a value for set membership. Numbers collapse to one
// spelling so that an int written and an int64 read back compare equal.
func validatorCompare(value any) string {
	if f, ok := validatorFloat(value); ok {
		return "n:" + formatNumber(f)
	}
	if s, ok := validatorString(value); ok {
		return "s:" + s
	}
	return fmt.Sprintf("x:%v", value)
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func printableList(values []any) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, fmt.Sprintf("%v", v))
	}
	return out
}

func typeExpectation(want string, value any) error {
	return fmt.Errorf("must be %s, got %T", want, value)
}
