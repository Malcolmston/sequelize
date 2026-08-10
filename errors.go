package sequelize

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors returned by the package. Every failure wraps exactly one of
// these, so callers can branch with [errors.Is] instead of matching strings.
var (
	// ErrUnknownDialect is returned by [New], [Open] and [DialectByName] when no
	// dialect has been registered under the requested name.
	ErrUnknownDialect = errors.New("sequelize: unknown dialect")

	// ErrDialectRegistered is returned by [Register] when a dialect with the same
	// name has already been registered.
	ErrDialectRegistered = errors.New("sequelize: dialect already registered")

	// ErrInvalidIdentifier is returned whenever a table, column, index or alias
	// name fails identifier validation. Identifiers must match
	// ^[A-Za-z_][A-Za-z0-9_]*$; the package never escapes its way around a bad
	// name, because a name that reaches the database unvalidated is an injection
	// vector.
	ErrInvalidIdentifier = errors.New("sequelize: invalid SQL identifier")

	// ErrUnknownAttribute is returned when a [Query], [Values] map or order term
	// names an attribute the model does not define.
	ErrUnknownAttribute = errors.New("sequelize: unknown attribute")

	// ErrUnknownModel is returned by [Sequelize.MustModel] and by operations that
	// reference a model name that was never defined.
	ErrUnknownModel = errors.New("sequelize: unknown model")

	// ErrModelRedefined is returned by [Sequelize.Define] when a model with the
	// same name already exists on the connection.
	ErrModelRedefined = errors.New("sequelize: model already defined")

	// ErrNoPrimaryKey is returned by operations that need to address a single row
	// ([Model.FindByPk], [Model.Upsert], returning the created row) on a model
	// that declares no primary key.
	ErrNoPrimaryKey = errors.New("sequelize: model has no primary key")

	// ErrNoValues is returned by [Model.Create], [Model.BulkCreate],
	// [Model.Update] and [Model.Upsert] when handed nothing to write.
	ErrNoValues = errors.New("sequelize: no values supplied")

	// ErrRecordNotFound is returned by [Model.FindOne] and [Model.FindByPk] when
	// no row matches. It is also wrapped by [RecordNotFoundError].
	ErrRecordNotFound = errors.New("sequelize: record not found")

	// ErrValidation is wrapped by [ValidationError].
	ErrValidation = errors.New("sequelize: validation failed")

	// ErrUniqueConstraint is wrapped by [UniqueConstraintError].
	ErrUniqueConstraint = errors.New("sequelize: unique constraint violated")

	// ErrForeignKeyConstraint is wrapped by [ForeignKeyError].
	ErrForeignKeyConstraint = errors.New("sequelize: foreign key constraint violated")

	// ErrInvalidType is returned when a value cannot be bound to, or decoded
	// from, the column type declared for its attribute.
	ErrInvalidType = errors.New("sequelize: invalid value for data type")

	// ErrUnsupported is returned when a dialect cannot express what was asked of
	// it, for example an unmapped [DataType].
	ErrUnsupported = errors.New("sequelize: unsupported by dialect")

	// ErrClosed is returned when a [Sequelize] whose database handle has been
	// closed, or a [Tx] that has already been committed or rolled back, is used
	// again.
	ErrClosed = errors.New("sequelize: connection or transaction already closed")

	// ErrInvalidQuery is returned when a [Query] is internally inconsistent, for
	// example a negative Limit or an aggregate over no attribute.
	ErrInvalidQuery = errors.New("sequelize: invalid query")
)

// FieldError is a single attribute-level validation failure, as collected by
// [ValidationError].
type FieldError struct {
	// Attribute is the model attribute name that failed.
	Attribute string
	// Message describes the failure, without the attribute name.
	Message string
	// Value is the offending value, if one was supplied.
	Value any
}

// Error implements the error interface.
func (e FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Attribute, e.Message)
}

// ValidationError reports every attribute that failed validation for one write,
// mirroring upstream's aggregate ValidationError rather than stopping at the
// first problem. It wraps [ErrValidation].
type ValidationError struct {
	// Model is the name of the model being written.
	Model string
	// Errors holds one entry per failed attribute, in attribute-name order.
	Errors []FieldError
}

// NewValidationError builds a [ValidationError] for model from errs. It returns
// nil when errs is empty, so callers can return its result directly.
func NewValidationError(model string, errs []FieldError) error {
	if len(errs) == 0 {
		return nil
	}
	sorted := make([]FieldError, len(errs))
	copy(sorted, errs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Attribute < sorted[j].Attribute })
	return &ValidationError{Model: model, Errors: sorted}
}

// Error implements the error interface, listing every failed attribute.
func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, fe.Error())
	}
	return fmt.Sprintf("%s on %s: %s", ErrValidation, e.Model, strings.Join(parts, "; "))
}

// Unwrap reports [ErrValidation] so errors.Is matches.
func (e *ValidationError) Unwrap() error { return ErrValidation }

// Attributes returns the names of the attributes that failed, in order.
func (e *ValidationError) Attributes() []string {
	names := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		names = append(names, fe.Attribute)
	}
	return names
}

// UniqueConstraintError reports a violated UNIQUE or PRIMARY KEY constraint. It
// wraps [ErrUniqueConstraint] and retains the driver error as its cause.
type UniqueConstraintError struct {
	// Model is the model being written, when known.
	Model string
	// Fields lists the columns the driver named, when it named any.
	Fields []string
	// Cause is the underlying driver error.
	Cause error
}

// Error implements the error interface.
func (e *UniqueConstraintError) Error() string {
	return constraintMessage(ErrUniqueConstraint, e.Model, e.Fields, e.Cause)
}

// Unwrap reports [ErrUniqueConstraint] so errors.Is matches.
func (e *UniqueConstraintError) Unwrap() error { return ErrUniqueConstraint }

// Underlying returns the driver error that produced this failure, or nil.
func (e *UniqueConstraintError) Underlying() error { return e.Cause }

// ForeignKeyError reports a violated FOREIGN KEY constraint. It wraps
// [ErrForeignKeyConstraint] and retains the driver error as its cause.
type ForeignKeyError struct {
	// Model is the model being written, when known.
	Model string
	// Fields lists the columns the driver named, when it named any.
	Fields []string
	// Cause is the underlying driver error.
	Cause error
}

// Error implements the error interface.
func (e *ForeignKeyError) Error() string {
	return constraintMessage(ErrForeignKeyConstraint, e.Model, e.Fields, e.Cause)
}

// Unwrap reports [ErrForeignKeyConstraint] so errors.Is matches.
func (e *ForeignKeyError) Unwrap() error { return ErrForeignKeyConstraint }

// Underlying returns the driver error that produced this failure, or nil.
func (e *ForeignKeyError) Underlying() error { return e.Cause }

// RecordNotFoundError is returned by [Model.FindOne] and [Model.FindByPk] when
// no row matches. Upstream returns null from findOne; the port returns an error
// so that a miss cannot be mistaken for an empty record. It wraps
// [ErrRecordNotFound].
type RecordNotFoundError struct {
	// Model is the model that was searched.
	Model string
	// Where renders the clause that found nothing, for diagnostics only.
	Where string
}

// Error implements the error interface.
func (e *RecordNotFoundError) Error() string {
	if e.Where == "" {
		return fmt.Sprintf("%s: %s", ErrRecordNotFound, e.Model)
	}
	return fmt.Sprintf("%s: %s where %s", ErrRecordNotFound, e.Model, e.Where)
}

// Unwrap reports [ErrRecordNotFound] so errors.Is matches.
func (e *RecordNotFoundError) Unwrap() error { return ErrRecordNotFound }

func constraintMessage(sentinel error, model string, fields []string, cause error) string {
	var b strings.Builder
	b.WriteString(sentinel.Error())
	if model != "" {
		b.WriteString(" on ")
		b.WriteString(model)
	}
	if len(fields) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(fields, ", "))
		b.WriteString(")")
	}
	if cause != nil {
		b.WriteString(": ")
		b.WriteString(cause.Error())
	}
	return b.String()
}
