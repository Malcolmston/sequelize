package sequelize

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNewValidationErrorEmptyIsNil(t *testing.T) {
	if err := NewValidationError("User", nil); err != nil {
		t.Fatalf("NewValidationError with no failures = %v, want nil", err)
	}
}

func TestValidationErrorSortsAndWraps(t *testing.T) {
	err := NewValidationError("User", []FieldError{
		{Attribute: "name", Message: "must not be empty"},
		{Attribute: "age", Message: "must be positive", Value: -1},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("errors.Is(err, ErrValidation) = false for %v", err)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As(*ValidationError) failed for %v", err)
	}
	if got, want := ve.Attributes(), []string{"age", "name"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Attributes() = %v, want %v", got, want)
	}
	if !strings.Contains(err.Error(), "age: must be positive") {
		t.Errorf("Error() = %q, want it to mention every failure", err.Error())
	}
	if !strings.Contains(err.Error(), "name: must not be empty") {
		t.Errorf("Error() = %q, want it to mention every failure", err.Error())
	}
}

func TestFieldErrorMessage(t *testing.T) {
	fe := FieldError{Attribute: "email", Message: "is not an address"}
	if got, want := fe.Error(), "email: is not an address"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestUniqueConstraintError(t *testing.T) {
	cause := errors.New("UNIQUE constraint failed: users.email")
	err := &UniqueConstraintError{Model: "User", Fields: []string{"email"}, Cause: cause}
	if !errors.Is(err, ErrUniqueConstraint) {
		t.Fatal("errors.Is(err, ErrUniqueConstraint) = false")
	}
	if !errors.Is(err.Underlying(), cause) {
		t.Error("Underlying() lost the driver error")
	}
	msg := err.Error()
	for _, want := range []string{"User", "email", cause.Error()} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}

func TestForeignKeyError(t *testing.T) {
	cause := errors.New("FOREIGN KEY constraint failed")
	err := &ForeignKeyError{Model: "Post", Cause: cause}
	if !errors.Is(err, ErrForeignKeyConstraint) {
		t.Fatal("errors.Is(err, ErrForeignKeyConstraint) = false")
	}
	if err.Underlying() != cause {
		t.Error("Underlying() lost the driver error")
	}
	if !strings.Contains(err.Error(), "Post") {
		t.Errorf("Error() = %q, want it to name the model", err.Error())
	}
}

func TestRecordNotFoundError(t *testing.T) {
	bare := &RecordNotFoundError{Model: "User"}
	if !errors.Is(bare, ErrRecordNotFound) {
		t.Fatal("errors.Is(err, ErrRecordNotFound) = false")
	}
	if strings.Contains(bare.Error(), "where") {
		t.Errorf("Error() = %q, want no where clause", bare.Error())
	}
	withWhere := &RecordNotFoundError{Model: "User", Where: `"id" = ?`}
	if !strings.Contains(withWhere.Error(), "where") {
		t.Errorf("Error() = %q, want the where clause", withWhere.Error())
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	sentinels := []error{
		ErrUnknownDialect, ErrDialectRegistered, ErrInvalidIdentifier, ErrUnknownAttribute,
		ErrUnknownModel, ErrModelRedefined, ErrNoPrimaryKey, ErrNoValues, ErrRecordNotFound,
		ErrValidation, ErrUniqueConstraint, ErrForeignKeyConstraint, ErrInvalidType,
		ErrUnsupported, ErrClosed, ErrInvalidQuery,
	}
	seen := map[string]bool{}
	for _, err := range sentinels {
		if !strings.HasPrefix(err.Error(), "sequelize: ") {
			t.Errorf("sentinel %q does not carry the package prefix", err)
		}
		if seen[err.Error()] {
			t.Errorf("duplicate sentinel message %q", err)
		}
		seen[err.Error()] = true
	}
}
