package sequelize

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// validationFixture defines a model whose attributes carry one of each kind of
// built-in validator.
func validationFixture(t *testing.T) (*Sequelize, *Model) {
	t.Helper()
	db := newTestDB(t)
	m := db.Define("Account", Attributes{
		"email":    {Type: STRING(64), AllowNull: NotNull(), Validate: []Validator{IsEmail()}},
		"handle":   {Type: STRING(32), AllowNull: NotNull(), Validate: []Validator{NotEmpty(), Len(3, 12)}},
		"age":      {Type: INTEGER(), Validate: []Validator{MinValue(18), MaxValue(120)}},
		"country":  {Type: STRING(2), Validate: []Validator{IsIn("GB", "US")}},
		"nickname": {Type: STRING(32), Validate: []Validator{Matches(`^[a-z]+$`)}},
	})
	if err := m.Err(); err != nil {
		t.Fatalf("Define: %v", err)
	}
	if err := db.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return db, m
}

func asValidationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T (%v), want *ValidationError", err, err)
	}
	if !errors.Is(err, ErrValidation) {
		t.Error("a ValidationError did not match ErrValidation")
	}
	return ve
}

// TestCreateReportsEveryFailedAttribute is the parity case: upstream aggregates,
// so stopping at the first failure would be wrong.
func TestCreateReportsEveryFailedAttribute(t *testing.T) {
	_, m := validationFixture(t)
	_, err := m.Create(context.Background(), Values{
		"email":    "not-an-email",
		"handle":   "x",
		"age":      9,
		"country":  "FR",
		"nickname": "Mixed Case",
	})
	ve := asValidationError(t, err)
	want := []string{"age", "country", "email", "handle", "nickname"}
	if got := ve.Attributes(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Attributes() = %v, want every failure in name order %v", got, want)
	}
	if ve.Model != "Account" {
		t.Errorf("Model = %q", ve.Model)
	}
	for _, fe := range ve.Errors {
		if fe.Message == "" {
			t.Errorf("%s has no message", fe.Attribute)
		}
		if fe.Value == nil {
			t.Errorf("%s did not carry the offending value", fe.Attribute)
		}
	}
	count, err := m.Count(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Error("a rejected Create still reached the database")
	}
}

func TestCreateAcceptsValidValues(t *testing.T) {
	_, m := validationFixture(t)
	row, err := m.Create(context.Background(), Values{
		"email":    "ada@example.com",
		"handle":   "ada",
		"age":      36,
		"country":  "GB",
		"nickname": "ada",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row["handle"] != "ada" {
		t.Errorf("Create returned %v", row)
	}
}

// TestValidatorsSkipNilValues pins the rule that nullability is AllowNull's job.
func TestValidatorsSkipNilValues(t *testing.T) {
	_, m := validationFixture(t)
	// age, country and nickname are nullable and omitted; their validators would
	// all reject a nil, and none of them must run.
	if _, err := m.Create(context.Background(), Values{
		"email":  "ada@example.com",
		"handle": "ada",
	}); err != nil {
		t.Fatalf("Create with nullable attributes omitted: %v", err)
	}
	if _, err := m.Create(context.Background(), Values{
		"email":  "grace@example.com",
		"handle": "grace",
		"age":    nil,
	}); err != nil {
		t.Fatalf("Create with an explicit nil for a nullable attribute: %v", err)
	}
}

func TestCreateRejectsAMissingNotNullAttribute(t *testing.T) {
	_, m := validationFixture(t)
	_, err := m.Create(context.Background(), Values{"email": "ada@example.com"})
	ve := asValidationError(t, err)
	if len(ve.Errors) != 1 || ve.Errors[0].Attribute != "handle" {
		t.Fatalf("Errors = %v, want just handle", ve.Errors)
	}
	if ve.Errors[0].Message != "cannot be null" {
		t.Errorf("message = %q", ve.Errors[0].Message)
	}

	// An explicit nil is rejected the same way.
	if _, err := m.Create(context.Background(), Values{"email": "a@b.com", "handle": nil}); !errors.Is(err, ErrValidation) {
		t.Errorf("explicit nil for a NOT NULL attribute = %v", err)
	}
}

func TestUpdateValidatesOnlyWhatItSets(t *testing.T) {
	_, m := validationFixture(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, Values{"email": "ada@example.com", "handle": "ada"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// handle is NOT NULL and is not part of this update, which is fine.
	if _, err := m.Update(ctx, Values{"age": 40}, Query{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// The attributes it does set are validated.
	if _, e := m.Update(ctx, Values{"age": 4, "email": "nope"}, Query{}); !errors.Is(e, ErrValidation) {
		t.Fatalf("Update = %v, want ErrValidation", e)
	} else {
		ve := asValidationError(t, e)
		if got := ve.Attributes(); len(got) != 2 {
			t.Errorf("Attributes() = %v, want both failures", got)
		}
	}
	// Setting a NOT NULL attribute to nil is still rejected.
	if _, e := m.Update(ctx, Values{"handle": nil}, Query{}); !errors.Is(e, ErrValidation) {
		t.Errorf("Update handle=nil = %v, want ErrValidation", e)
	}
	row, err := m.FindOne(ctx, Query{})
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if row["age"] != int64(40) {
		t.Errorf("the accepted update did not land: %v", row)
	}
}

func TestBulkCreateAggregatesFailuresAcrossRows(t *testing.T) {
	_, m := validationFixture(t)
	_, err := m.BulkCreate(context.Background(), []Values{
		{"email": "ok@example.com", "handle": "fine"},
		{"email": "bad", "handle": "ok2"},
		{"email": "also@example.com", "handle": "x"},
	})
	ve := asValidationError(t, err)
	if got := ve.Attributes(); strings.Join(got, ",") != "email,handle" {
		t.Errorf("Attributes() = %v, want the failures of both rows", got)
	}
	count, err := m.Count(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Error("BulkCreate wrote the valid rows of a rejected batch")
	}
}

func TestValidateAndValidateChangesAreCallableDirectly(t *testing.T) {
	_, m := validationFixture(t)
	if err := m.Validate(Values{"email": "ada@example.com", "handle": "ada"}); err != nil {
		t.Errorf("Validate on good values = %v", err)
	}
	if err := m.Validate(Values{"email": "ada@example.com"}); !errors.Is(err, ErrValidation) {
		t.Errorf("Validate with a missing NOT NULL attribute = %v", err)
	}
	if err := m.ValidateChanges(Values{"email": "ada@example.com"}); err != nil {
		t.Errorf("ValidateChanges with a missing NOT NULL attribute = %v", err)
	}
	if err := m.ValidateChanges(Values{"age": 2}); !errors.Is(err, ErrValidation) {
		t.Errorf("ValidateChanges on a bad value = %v", err)
	}

	broken := m.seq.Define("Broken", Attributes{"x": {}})
	if err := broken.Validate(Values{}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Validate on an unusable model = %v", err)
	}
}

func TestBuiltinValidators(t *testing.T) {
	cases := []struct {
		name      string
		validator Validator
		ok        []any
		bad       []any
	}{
		{"NotEmpty", NotEmpty(), []any{"x", " x "}, []any{"", "   ", 7}},
		{"MinLen", MinLen(3), []any{"abc", "abcd", []byte("abc")}, []any{"ab", 7}},
		{"MaxLen", MaxLen(3), []any{"abc", "a"}, []any{"abcd", 7}},
		{"Len", Len(2, 4), []any{"ab", "abcd", "héé"}, []any{"a", "abcde", 7}},
		{"MinValue", MinValue(10), []any{10, int64(11), 10.5, uint8(200)}, []any{9, int64(-1), 9.99, "x"}},
		{"MaxValue", MaxValue(10), []any{10, 1.5, int32(-4)}, []any{11, 10.01, "x"}},
		{"Matches", Matches(`^a+$`), []any{"a", "aaa"}, []any{"b", "aab", 7}},
		{"IsIn", IsIn("GB", "US", 7), []any{"GB", "US", 7, int64(7)}, []any{"FR", 8}},
		{"NotIn", NotIn("root", "admin"), []any{"ada"}, []any{"root", "admin"}},
		{"IsEmail", IsEmail(), []any{"ada@example.com", "a.b+c@sub.example.co.uk"},
			[]any{"ada", "ada@example", "Ada <ada@example.com>", "a@b.", 7}},
		{"Custom", Custom("must be even", func(v any) bool { n, _ := v.(int); return n%2 == 0 }),
			[]any{2, 4}, []any{3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range tc.ok {
				if err := tc.validator(v); err != nil {
					t.Errorf("%v was rejected: %v", v, err)
				}
			}
			for _, v := range tc.bad {
				if err := tc.validator(v); err == nil {
					t.Errorf("%v was accepted", v)
				} else if err.Error() == "" {
					t.Errorf("%v was rejected without a message", v)
				}
			}
		})
	}
}

func TestMatchesWithAnUncompilablePatternFailsClosed(t *testing.T) {
	v := Matches(`([`)
	err := v("anything")
	if err == nil {
		t.Fatal("a bad pattern accepted the value")
	}
	if !strings.Contains(err.Error(), "bad pattern") {
		t.Errorf("message = %q", err.Error())
	}
}

func TestCustomWithNoPredicateAcceptsEverything(t *testing.T) {
	if err := Custom("never", nil)("x"); err != nil {
		t.Errorf("Custom(nil) = %v, want nil", err)
	}
}

func TestNilValidatorEntriesAreIgnored(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Loose", Attributes{"a": {Type: STRING(8), Validate: []Validator{nil}}})
	ctx := context.Background()
	if err := db.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := m.Create(ctx, Values{"a": "x"}); err != nil {
		t.Errorf("a nil validator entry was not ignored: %v", err)
	}
}

// TestDriverNotNullViolationBecomesAValidationError round-trips a NOT NULL
// violation that the in-Go validators cannot see: a second model over the same
// table, declaring the column nullable.
func TestDriverNotNullViolationBecomesAValidationError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	strict := db.Define("Strict", Attributes{
		"label": {Type: STRING(16), AllowNull: NotNull(), Field: "label"},
	}, TableName("strict_rows"))
	loose := db.Define("Loose", Attributes{
		"label": {Type: STRING(16), Field: "label"},
	}, TableName("strict_rows"))
	if err := strict.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	_, err := loose.Create(ctx, Values{"label": nil})
	ve := asValidationError(t, err)
	if len(ve.Errors) != 1 || ve.Errors[0].Attribute != "label" {
		t.Errorf("Errors = %v, want the label column mapped to its attribute", ve.Errors)
	}
	if ve.Model != "Loose" {
		t.Errorf("Model = %q, want the model that was written", ve.Model)
	}
}

func TestTranslateWriteErrorLeavesOtherErrorsAlone(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Thing", Attributes{"name": {Type: STRING(8), Unique: true}})

	if got := m.translateWriteError(nil); got != nil {
		t.Errorf("translateWriteError(nil) = %v", got)
	}
	plain := errors.New("connection reset")
	if got := m.translateWriteError(plain); got != plain {
		t.Errorf("an unrecognised error was rewritten to %v", got)
	}
	unique := &UniqueConstraintError{Fields: []string{"name"}}
	if got := m.translateWriteError(unique); got != error(unique) {
		t.Errorf("a UniqueConstraintError was rewritten to %v", got)
	}
	fk := &ForeignKeyError{}
	if got := m.translateWriteError(fk); got != error(fk) {
		t.Errorf("a ForeignKeyError was rewritten to %v", got)
	}
	already := &ValidationError{Model: "Thing"}
	if got := m.translateWriteError(already); got != error(already) {
		t.Errorf("a ValidationError was rewritten to %v", got)
	}
	// A column the model does not know is reported under its own name.
	unknown := errors.New("NOT NULL constraint failed: things.mystery")
	ve := asValidationError(t, m.translateWriteError(unknown))
	if ve.Errors[0].Attribute != "mystery" {
		t.Errorf("Attribute = %q, want the raw column name", ve.Errors[0].Attribute)
	}
}

func TestNotNullColumnRecognisesEachPhrasing(t *testing.T) {
	cases := []struct {
		msg    string
		column string
		ok     bool
	}{
		{"NOT NULL constraint failed: users.name (1299)", "name", true},
		{"NOT NULL constraint failed: name", "name", true},
		{`pq: null value in column "email" violates not-null constraint`, "email", true},
		{"Column 'email' cannot be null", "email", true},
		{"NOT NULL constraint failed:", "", false},
		{"some other failure", "", false},
		{"violates not-null constraint but names nothing", "", false},
	}
	for _, tc := range cases {
		column, ok := notNullColumn(tc.msg)
		if ok != tc.ok || column != tc.column {
			t.Errorf("notNullColumn(%q) = %q, %v; want %q, %v", tc.msg, column, ok, tc.column, tc.ok)
		}
	}
}

func TestAttributeForColumnReversesTheMapping(t *testing.T) {
	db := newTestDB(t)
	m := db.Define("Person", Attributes{"givenName": {Type: STRING(8)}})
	if got, ok := m.attributeForColumn("given_name"); !ok || got != "givenName" {
		t.Errorf("attributeForColumn(given_name) = %q, %v", got, ok)
	}
	if _, ok := m.attributeForColumn("nope"); ok {
		t.Error("attributeForColumn invented an attribute")
	}
}

func TestValidatorHelpers(t *testing.T) {
	if _, ok := validatorString(7); ok {
		t.Error("validatorString accepted an int")
	}
	if s, ok := validatorString([]byte("x")); !ok || s != "x" {
		t.Errorf("validatorString([]byte) = %q, %v", s, ok)
	}
	if _, ok := validatorFloat("x"); ok {
		t.Error("validatorFloat accepted a string")
	}
	for _, v := range []any{int8(1), int16(1), int32(1), int64(1), uint(1), uint16(1), uint32(1), uint64(1), float32(1)} {
		if f, ok := validatorFloat(v); !ok || f != 1 {
			t.Errorf("validatorFloat(%T) = %v, %v", v, f, ok)
		}
	}
	if got := validatorCompare(struct{ A int }{1}); !strings.HasPrefix(got, "x:") {
		t.Errorf("validatorCompare fallback = %q", got)
	}
	if got := printableList([]any{1, "x"}); len(got) != 2 || got[1] != "x" {
		t.Errorf("printableList = %v", got)
	}
}

// ExampleModel_Validate reports every failure at once.
func ExampleModel_Validate() {
	db, _ := New("sqlite", "file:example_validate?mode=memory&cache=shared")
	defer db.Close()

	user := db.Define("User", Attributes{
		"email": {Type: STRING(64), AllowNull: NotNull(), Validate: []Validator{IsEmail()}},
		"age":   {Type: INTEGER(), Validate: []Validator{MinValue(18)}},
	})

	err := user.Validate(Values{"email": "nope", "age": 12})
	var ve *ValidationError
	if errors.As(err, &ve) {
		fmt.Println(strings.Join(ve.Attributes(), " "))
	}
	// Output: age email
}
