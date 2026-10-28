package store

import (
	"context"
	"strings"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/models"
)

// NormalizeEmail is the one form emails are stored and looked up in.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateUser inserts u, assigning its ID and timestamps and normalizing its
// email. BeforeCreate hooks see the normalized email and may rewrite the
// row; an email they set is normalized again before the insert. u holds the
// stored user afterwards.
func (s *Store) CreateUser(ctx context.Context, u *models.User) error {
	now := s.now()
	u.ID = s.newID()
	u.Email = NormalizeEmail(u.Email)
	u.CreatedAt, u.UpdatedAt = now, now
	return s.create(ctx, u, normalizeUserRow)
}

// normalizeUserRow puts a user row's email, if it has one, in the stored
// form. It runs on the row a create or an update is about to write, after
// the before hooks, so no path stores an email FindUserByEmail can't find.
func normalizeUserRow(row behemoth.M) {
	if email, ok := row[models.UserEmail].(string); ok {
		row[models.UserEmail] = NormalizeEmail(email)
	}
}

func (s *Store) FindUserByID(ctx context.Context, id string) (*models.User, error) {
	return s.findUser(ctx, eq(models.UserID, id))
}

func (s *Store) FindUserByEmail(ctx context.Context, email string) (*models.User, error) {
	return s.findUser(ctx, eq(models.UserEmail, NormalizeEmail(email)))
}

// FindUserBy returns the user whose column holds value, compared as it is:
// the caller passes value in its stored form. column is a canonical column
// name, one of the table's own or one a plugin contributed. It is for a
// plugin that identifies users by a column of its own, as the username
// plugin does; the column should be unique, since one of several matching
// rows is returned otherwise. A NotFound error means no user matches.
func (s *Store) FindUserBy(ctx context.Context, column string, value any) (*models.User, error) {
	return s.findUser(ctx, eq(column, value))
}

// UpdateUser applies updates (canonical column names, e.g. models.UserEmail)
// to the user with id — after BeforeUpdate hooks, which may rewrite them —
// stamps updated_at, and returns the stored user. The caller's map is never
// modified.
func (s *Store) UpdateUser(ctx context.Context, id string, updates behemoth.M) (*models.User, error) {
	updated, err := s.update(ctx, &models.User{}, id, updates, models.UserUpdatedAt, normalizeUserRow, nil)
	if err != nil {
		return nil, err
	}
	return updated.(*models.User), nil
}

// UpdateUserIfEmail is UpdateUser for a user whose email is still email when
// the change is written. email is compared as it is stored: pass the Email
// of the user that was read. The email is checked in the write itself
// (UpdateOne re-checks its expression), so a flow that read the user and
// decided from its address writes only if no other write changed the
// address since. A user with another email gets a NotFound error, as a
// missing one does.
func (s *Store) UpdateUserIfEmail(ctx context.Context, id, email string, updates behemoth.M) (*models.User, error) {
	updated, err := s.update(ctx, &models.User{}, id, updates, models.UserUpdatedAt, normalizeUserRow, &clause.Expression{
		Conditions: []clause.Condition{{Field: models.UserEmail, Operator: clause.OpEqual, Value: email}},
	})
	if err != nil {
		return nil, err
	}
	return updated.(*models.User), nil
}

// DeleteUser deletes the user with id and everything that lets someone act
// as that user: the user's sessions, accounts, database-backed tokens and
// one-time codes, and the values reserved for the user. It returns a
// NotFound error when there is no such user.
//
// The deletes share one transaction, the caller's when s is bound to one,
// and run children first, so they work whether or not the database enforces
// the foreign keys to users:
//
//  1. BeforeDelete (data.user.beforeDelete), which may refuse the delete
//  2. the user's sessions, revoked ones included
//  3. the user's accounts
//  4. the tokens and the one-time codes whose subject is the user's id, and
//     the user's reserved values
//  5. the user
//  6. AfterDelete (data.user.afterDelete), whose error rolls all of it back
//
// The store does not rely on ON DELETE CASCADE: a cascade removes session
// rows without telling anyone, so their cache entries would keep the tokens
// valid and no hook would fire. AfterDelete is given the deleted sessions for
// that reason; under Boot it clears their cache entries and, once the
// transaction has committed, fires auth.session.afterRevoke for each one that
// was live, then data.user.deleted.
//
// Not covered: a token in a key-value storage can't be found by its subject
// and expires on its own, and a token issued to something other than the
// user's id (an email address) is the issuing plugin's to delete, from a
// handler on data.user.beforeDelete. Sessions in which the user is the
// impersonator are left alone.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.Transaction(ctx, func(ctx context.Context, tx *Store) error {
		ctx = tx.hooks.Begin(ctx, models.UserTable)
		user, err := tx.findUser(ctx, eq(models.UserID, id))
		if err != nil {
			return err
		}
		if err := tx.hooks.BeforeDelete(ctx, tx, models.UserTable, user); err != nil {
			return err // the hook's error is the abort; returned as-is
		}

		// A session that a promotion moves to a new token, or a sign-in
		// creates, after this list and before the delete below keeps its
		// cache entry: docs/ongoing.md, M35.
		sessions, err := tx.ListSessionsForUser(ctx, id)
		if err != nil {
			return err
		}
		for _, owned := range []struct {
			model  behemoth.Model
			column string
		}{
			{&models.Session{}, models.SessionUserID},
			{&models.Account{}, models.AccountUserID},
			{&models.Token{}, models.TokenSubject},
			{&models.VerificationCode{}, models.VerificationCodeSubject},
			{&models.ReservedValue{}, models.ReservedValueUserID},
		} {
			if err := tx.db.DeleteMany(ctx, owned.model, eq(owned.column, id)); err != nil {
				return err
			}
		}
		if err := tx.db.DeleteOne(ctx, &models.User{}, eq(models.UserID, id)); err != nil {
			return err
		}
		// The hook's error rolls the delete back; returned as-is.
		return tx.hooks.AfterDelete(ctx, tx, models.UserTable, user, sessions)
	})
}

func (s *Store) findUser(ctx context.Context, where clause.Expression) (*models.User, error) {
	found, err := s.db.FindOne(ctx, &models.User{}, where)
	if err != nil {
		return nil, err
	}
	return found.(*models.User), nil
}

func eq(field string, value any) clause.Expression {
	return clause.Expression{Conditions: []clause.Condition{{Field: field, Operator: clause.OpEqual, Value: value}}}
}
