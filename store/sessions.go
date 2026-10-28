package store

import (
	"context"
	"fmt"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
)

// CreateSession inserts sess, assigning its ID and created/updated
// timestamps. Everything else — hashes, state, expiry — is the session
// manager's to set. sess holds the stored session afterwards.
func (s *Store) CreateSession(ctx context.Context, sess *models.Session) error {
	now := s.now()
	sess.ID = s.newID()
	sess.CreatedAt, sess.UpdatedAt = now, now
	return s.create(ctx, sess, nil)
}

func (s *Store) FindSessionByID(ctx context.Context, id string) (*models.Session, error) {
	return s.findSession(ctx, eq(models.SessionID, id))
}

func (s *Store) FindSessionByLookupHash(ctx context.Context, lookupHash string) (*models.Session, error) {
	return s.findSession(ctx, eq(models.SessionLookupHash, lookupHash))
}

// UpdateSessionAt applies changes (canonical column names) to the session
// with id if the session is still at version when the change is written. It
// stamps updated_at, writes version+1, and returns the stored session. The
// version is checked in the write itself (UpdateOne re-checks its
// expression), so of several calls that read the session at one version,
// one writes. A session at another version gets a NotFound error, as a
// missing one does.
//
// It is the store's only update of a session row. Every update writes the
// next version, which is how the session cache keeps the newest copy of a
// session whatever order the copies reach it in. changes can't name the
// version, and a BeforeUpdate hook can't change it.
func (s *Store) UpdateSessionAt(ctx context.Context, id string, version int64, changes behemoth.M) (*models.Session, error) {
	if _, ok := changes[models.SessionVersion]; ok {
		return nil, errs.NewValidationError("Store.UpdateSessionAt", models.SessionTable,
			fmt.Errorf("the store writes %q, changes can't", models.SessionVersion))
	}
	next := func(row behemoth.M) { row[models.SessionVersion] = version + 1 } // after the hooks
	updated, err := s.update(ctx, &models.Session{}, id, changes, models.SessionUpdatedAt, next, sessionAt(version))
	if err != nil {
		return nil, err
	}
	return updated.(*models.Session), nil
}

// sessionAt is the guard of a session row at version. A row stored before
// the version column existed is at version 0: an SQL migration gives it the
// column's default, and a MongoDB document has no such field, which the
// guard for 0 matches too.
func sessionAt(version int64) *clause.Expression {
	at := clause.Condition{Field: models.SessionVersion, Operator: clause.OpEqual, Value: version}
	if version != 0 {
		return &clause.Expression{Conditions: []clause.Condition{at}}
	}
	return &clause.Expression{Logic: clause.OpOr, Conditions: []clause.Condition{
		at, {Field: models.SessionVersion, Operator: clause.OpIsNull},
	}}
}

// CountActiveSessions counts userID's sessions that are active and have not
// expired: the sessions SessionConfig.MaxConcurrent limits. A session that
// waits for a second factor is not counted, and neither is one past its
// expiry that nothing revoked.
//
// The expiry is compared with the store's clock (Now) in the database. On
// SQLite, which stores a time as text with its zone, a session whose expiry
// was written in a zone other than UTC can be counted wrongly; the session
// manager writes expiries from Now.
func (s *Store) CountActiveSessions(ctx context.Context, userID string) (int64, error) {
	return s.db.Count(ctx, &models.Session{}, s.activeSessionsOf(userID))
}

// ListActiveSessions lists the sessions CountActiveSessions counts, oldest
// first. limit <= 0 means no limit.
func (s *Store) ListActiveSessions(ctx context.Context, userID string, limit int) ([]*models.Session, error) {
	return s.findSessions(ctx, s.activeSessionsOf(userID),
		&behemoth.QueryOptions{OrderBy: behemoth.Order{Field: models.SessionCreatedAt, Direction: behemoth.Asc}, Limit: limit})
}

// ListLiveSessions lists userID's sessions that aren't revoked, pending and
// expired ones included, oldest first, except exceptID ("" for none).
// limit <= 0 means no limit.
func (s *Store) ListLiveSessions(ctx context.Context, userID, exceptID string, limit int) ([]*models.Session, error) {
	return s.findSessions(ctx, liveSessionsOf(userID, exceptID),
		&behemoth.QueryOptions{OrderBy: behemoth.Order{Field: models.SessionCreatedAt, Direction: behemoth.Asc}, Limit: limit})
}

// ListSessionsForUser lists all of userID's sessions, revoked included,
// newest first.
func (s *Store) ListSessionsForUser(ctx context.Context, userID string) ([]*models.Session, error) {
	return s.findSessions(ctx, eq(models.SessionUserID, userID),
		&behemoth.QueryOptions{OrderBy: behemoth.Order{Field: models.SessionCreatedAt, Direction: behemoth.Desc}})
}

// PurgeExpiredSessions deletes the sessions that expired before the given
// time, revoked ones included. A session keeps its expiry when it is
// revoked, so a revoked row goes once the cutoff passes that expiry, which
// is later than the revocation. Until the row is gone, the session's token
// gets errs.CodeSessionExpired or errs.CodeSessionRevoked; after it, a
// not-found error. A time later than the store's (Now) is taken as Now, so a
// live session is never removed. Like the other purges it does one pass and
// is safe to run from several processes.
func (s *Store) PurgeExpiredSessions(ctx context.Context, before time.Time) error {
	return s.db.DeleteMany(ctx, &models.Session{}, clause.Expression{Conditions: []clause.Condition{
		{Field: models.SessionExpiresAt, Operator: clause.OpLessThan, Value: s.notAfterNow(before)},
	}})
}

func liveSessionsOf(userID, exceptID string) clause.Expression {
	conds := []clause.Condition{
		{Field: models.SessionUserID, Operator: clause.OpEqual, Value: userID},
		{Field: models.SessionStateColumn, Operator: clause.OpNotEqual, Value: string(models.SessionRevoked)},
	}
	if exceptID != "" {
		conds = append(conds, clause.Condition{Field: models.SessionID, Operator: clause.OpNotEqual, Value: exceptID})
	}
	return clause.Expression{Conditions: conds, Logic: clause.OpAnd}
}

func (s *Store) activeSessionsOf(userID string) clause.Expression {
	return clause.Expression{Logic: clause.OpAnd, Conditions: []clause.Condition{
		{Field: models.SessionUserID, Operator: clause.OpEqual, Value: userID},
		{Field: models.SessionStateColumn, Operator: clause.OpEqual, Value: string(models.SessionActive)},
		{Field: models.SessionExpiresAt, Operator: clause.OpGreaterThan, Value: s.now()},
	}}
}

func (s *Store) findSession(ctx context.Context, where clause.Expression) (*models.Session, error) {
	found, err := s.db.FindOne(ctx, &models.Session{}, where)
	if err != nil {
		return nil, err
	}
	return found.(*models.Session), nil
}

func (s *Store) findSessions(ctx context.Context, where clause.Expression, opts *behemoth.QueryOptions) ([]*models.Session, error) {
	found, err := s.db.FindMany(ctx, &models.Session{}, where, opts)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Session, len(found))
	for i, f := range found {
		sess, ok := f.(*models.Session)
		if !ok {
			return nil, fmt.Errorf("store: unexpected %T in sessions", f)
		}
		out[i] = sess
	}
	return out, nil
}
