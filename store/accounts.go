package store

import (
	"context"
	"fmt"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
)

// CreateAccount inserts a, assigning its ID and timestamps and sealing its
// OAuth tokens (models.AccountSecretColumns): data hooks and the database
// only ever see the sealed form, a keeps the plaintext. A second account
// with the same provider and account id is a DuplicateKey error.
func (s *Store) CreateAccount(ctx context.Context, a *models.Account) error {
	const op = "Store.CreateAccount"
	now := s.now()
	a.ID = s.newID()
	a.CreatedAt, a.UpdatedAt = now, now

	access, refresh, id := a.AccessToken, a.RefreshToken, a.IDToken
	defer func() { a.AccessToken, a.RefreshToken, a.IDToken = access, refresh, id }()
	for _, secret := range accountSecrets(a) {
		if *secret.value == "" {
			continue
		}
		sealed, err := s.seal(op, *secret.value, accountBinding(a.ID, secret.column))
		if err != nil {
			return err
		}
		*secret.value = sealed
	}
	return s.create(ctx, a, nil)
}

// accountSecret is a sealed column of an account and its field.
type accountSecret struct {
	column string
	value  *string
}

// accountSecrets returns the sealed columns of a (models.AccountSecretColumns)
// with their fields.
func accountSecrets(a *models.Account) []accountSecret {
	return []accountSecret{
		{models.AccountAccessToken, &a.AccessToken},
		{models.AccountRefreshToken, &a.RefreshToken},
		{models.AccountIDToken, &a.IDToken},
	}
}

// accountBinding binds a token to its account and its column, so that it
// opens neither in another account's row nor as another of its tokens.
func accountBinding(accountID, column string) SealBinding {
	return SealBinding{Purpose: models.AccountTable + "." + column, Owner: accountID}
}

func (s *Store) FindAccountByID(ctx context.Context, id string) (*models.Account, error) {
	return s.findAccount(ctx, eq(models.AccountID, id))
}

// FindAccount finds the account a provider knows as accountID.
func (s *Store) FindAccount(ctx context.Context, providerID, accountID string) (*models.Account, error) {
	return s.findAccount(ctx, clause.Expression{Conditions: []clause.Condition{
		{Field: models.AccountProviderID, Operator: clause.OpEqual, Value: providerID},
		{Field: models.AccountAccountID, Operator: clause.OpEqual, Value: accountID},
	}, Logic: clause.OpAnd})
}

// ListAccountsForUser lists userID's accounts, oldest first.
func (s *Store) ListAccountsForUser(ctx context.Context, userID string) ([]*models.Account, error) {
	found, err := s.db.FindMany(ctx, &models.Account{}, eq(models.AccountUserID, userID),
		&behemoth.QueryOptions{OrderBy: behemoth.Order{Field: models.AccountCreatedAt, Direction: behemoth.Asc}})
	if err != nil {
		return nil, err
	}
	out := make([]*models.Account, len(found))
	for i, f := range found {
		if out[i], err = s.openAccount(f, "Store.ListAccountsForUser"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateAccount applies changes (canonical column names) to the account with
// id, stamps updated_at, and returns the stored account. A token column is
// given in plaintext and sealed before hooks see it; "" or nil clears it.
// The caller's map is never modified.
func (s *Store) UpdateAccount(ctx context.Context, id string, changes behemoth.M) (*models.Account, error) {
	const op = "Store.UpdateAccount"
	changes = copyRow(changes)
	for _, col := range models.AccountSecretColumns {
		v, ok := changes[col]
		if !ok {
			continue
		}
		plaintext, isString := v.(string)
		if v != nil && !isString {
			return nil, errs.NewValidationError(op, models.AccountTable, fmt.Errorf("%s must be a string, got %T", col, v))
		}
		if plaintext == "" {
			changes[col] = nil
			continue
		}
		sealed, err := s.seal(op, plaintext, accountBinding(id, col))
		if err != nil {
			return nil, err
		}
		changes[col] = sealed
	}
	updated, err := s.update(ctx, &models.Account{}, id, changes, models.AccountUpdatedAt, nil, nil)
	if err != nil {
		return nil, err
	}
	return s.openAccount(updated, op)
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	return s.db.DeleteOne(ctx, &models.Account{}, eq(models.AccountID, id))
}

func (s *Store) findAccount(ctx context.Context, where clause.Expression) (*models.Account, error) {
	found, err := s.db.FindOne(ctx, &models.Account{}, where)
	if err != nil {
		return nil, err
	}
	return s.openAccount(found, "Store.FindAccount")
}

// openAccount returns a copy of the stored account with its tokens opened.
// A copy, because the stored model may already have been handed to an
// after-hook, which must keep seeing the sealed form.
func (s *Store) openAccount(found behemoth.Model, op string) (*models.Account, error) {
	stored, ok := found.(*models.Account)
	if !ok {
		return nil, fmt.Errorf("store: unexpected %T in accounts", found)
	}
	a := *stored
	for _, secret := range accountSecrets(&a) {
		if *secret.value == "" {
			continue
		}
		plaintext, err := s.open(op, *secret.value, accountBinding(a.ID, secret.column))
		if err != nil {
			return nil, err
		}
		*secret.value = plaintext
	}
	return &a, nil
}
