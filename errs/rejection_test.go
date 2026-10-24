package errs

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A rejection is a typed error in one of the categories that refuse a
// request. Every other category, and every untyped error, is a failure of
// the system.
func TestIsRejection(t *testing.T) {
	rejections := map[Category]bool{
		CategoryValidation: true, CategoryConflict: true, CategoryUnauthorized: true,
		CategoryForbidden: true, CategoryRateLimited: true,
	}
	for _, category := range []Category{
		CategoryValidation, CategoryNotFound, CategoryUndefinedTable, CategoryDuplicateKey,
		CategoryForeignKey, CategoryConflict, CategoryUnauthorized, CategoryForbidden,
		CategoryRateLimited, CategoryUnavailable, CategorySession, CategoryToken, CategoryTransaction,
		CategoryDatabase, CategoryMigration, CategoryInvalidKey, CategoryKeyNotFound,
		CategoryNotImplemented, CategoryConfiguration, CategorySecurity, CategoryInternal,
	} {
		assert.Equal(t, rejections[category], category.IsRejection(), category)
		assert.Equal(t, rejections[category], IsRejection(&DomainError{Category: category}), category)
	}

	assert.False(t, IsRejection(nil))
	assert.False(t, IsRejection(errors.New("connection refused")), "an untyped error is a failure of the system")
	wrapped := fmt.Errorf("handler: %w", &DomainError{Category: CategoryForbidden})
	assert.True(t, IsRejection(wrapped), "a wrapped rejection is still one")
}
