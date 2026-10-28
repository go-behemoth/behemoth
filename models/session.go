package models

import (
	"strings"
	"time"

	"github.com/go-behemoth/behemoth"
)

type SessionState string

const (
	SessionPending SessionState = "pending" // credentials verified, awaiting a secondary factor
	SessionActive  SessionState = "active"
	SessionRevoked SessionState = "revoked"
)

// Canonical names of the sessions table and its columns — the single source
// for ToMap/FromMap, schema declarations, query conditions and update maps.
const (
	SessionTable = "sessions"

	SessionID             = "id"
	SessionUserID         = "user_id"
	SessionLookupHash     = "lookup_hash"
	SessionTokenHash      = "token_hash"
	SessionKeyVersion     = "key_version"
	SessionStateColumn    = "state"
	SessionExpiresAt      = "expires_at"
	SessionLastActiveAt   = "last_active_at"
	SessionFreshAt        = "fresh_at"
	SessionFactors        = "factors"
	SessionIPAddress      = "ip_address"
	SessionUserAgent      = "user_agent"
	SessionImpersonatorID = "impersonator_id"
	SessionRevokedAt      = "revoked_at"
	SessionRevokedReason  = "revoked_reason"
	SessionVersion        = "version"
	SessionCreatedAt      = "created_at"
	SessionUpdatedAt      = "updated_at"
)

// sessionColumns are Session's own columns; any other column FromMap
// receives is a contribution, kept in the Extension.
var sessionColumns = columnSet(SessionID, SessionUserID, SessionLookupHash, SessionTokenHash,
	SessionKeyVersion, SessionStateColumn, SessionExpiresAt, SessionLastActiveAt, SessionFreshAt, SessionFactors,
	SessionIPAddress, SessionUserAgent, SessionImpersonatorID, SessionRevokedAt, SessionRevokedReason,
	SessionVersion, SessionCreatedAt, SessionUpdatedAt)

type Session struct {
	Extension
	ID             string       `db:"id"`
	UserID         string       `db:"user_id"`     // stored as string regardless of behemoth.Session.UserID's `any
	LookupHash     string       `db:"lookup_hash"` // unkeyed sha256(rawToken), indexed, unique, lookup key
	TokenHash      string       `db:"token_hash"`  // keyed HMAC(rawToken)
	KeyVersion     int          `db:"key_version"` // which KeyManager version produced TokenHash; required for ByVersion after rotation
	State          SessionState `db:"state"`       // "pending" | "active" | "revoked"
	ExpiresAt      time.Time    `db:"expires_at"`
	LastActiveAt   time.Time    `db:"last_active_at"` // drives the updateAge throttle; deliberately separated from UpdatedAt
	FreshAt        time.Time    `db:"fresh_at"`       // last full credential re-check; step-up auth compares against this
	Factors        []string     `db:"factors"`        // what the sign-in proved, in order: core.SessionMeta.Factor, and the second factor once promoted. Stored separated by spaces; nil when none was recorded
	IPAddress      string       `db:"ip_address"`
	UserAgent      string       `db:"user_agent"`
	ImpersonatorID any          `db:"impersonator_id"` // nullable; set when an admin is impersonating this session's user
	RevokedAt      *time.Time   `db:"revoked_at"`      // nullable; session soft-revoked, row kept for audit
	RevokedReason  string       `db:"revoked_reason"`  // "user_logout" | "admin_revoked" | "password_changed" | etc...

	// Version counts the updates of the row: Store.UpdateSessionAt writes
	// each one with the next version, guarded on the one it read. A copy of
	// the row carries the version of its contents, which is how the session
	// cache keeps the newest copy whatever order the copies reach it in.
	Version int64 `db:"version"`

	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func (s *Session) GetID() string {
	return s.ID
}

func (s *Session) SetExpiresAt(expiry time.Time) {
	s.ExpiresAt = expiry
}

func (s *Session) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}

// Implementations of the Model interface.
func (s *Session) SchemaName() string     { return SessionTable }
func (s *Session) PrimaryKeyName() string { return SessionID }
func (s *Session) PrimaryKeyField() any   { return s.ID }
func (s *Session) New() behemoth.Model    { return &Session{} }

func (s *Session) FromMap(m map[string]any) error {
	s.ID, _ = m[SessionID].(string)
	s.UserID, _ = m[SessionUserID].(string)
	s.LookupHash, _ = m[SessionLookupHash].(string)
	s.TokenHash, _ = m[SessionTokenHash].(string)
	if kv, ok := m[SessionKeyVersion].(int64); ok {
		s.KeyVersion = int(kv)
	}
	if state, ok := m[SessionStateColumn].(string); ok {
		s.State = SessionState(state)
	}
	s.ExpiresAt, _ = m[SessionExpiresAt].(time.Time)
	s.LastActiveAt, _ = m[SessionLastActiveAt].(time.Time)
	s.FreshAt, _ = m[SessionFreshAt].(time.Time)
	s.Factors = nil
	if factors, _ := m[SessionFactors].(string); factors != "" {
		s.Factors = strings.Fields(factors)
	}
	s.IPAddress, _ = m[SessionIPAddress].(string)
	s.UserAgent, _ = m[SessionUserAgent].(string)
	s.ImpersonatorID, _ = m[SessionImpersonatorID].(string)
	if t, ok := m[SessionRevokedAt].(time.Time); ok {
		s.RevokedAt = &t
	}
	s.RevokedReason, _ = m[SessionRevokedReason].(string)
	s.Version, _ = m[SessionVersion].(int64) // a document written before the column existed has none: version 0
	s.CreatedAt, _ = m[SessionCreatedAt].(time.Time)
	s.UpdatedAt, _ = m[SessionUpdatedAt].(time.Time)
	s.collectExtras(m, sessionColumns)
	return nil
}

// SessionFactorsValue returns the stored form of factors: the names separated
// by spaces, or nil (NULL) when there are none. ToMap writes it, and an
// update of the factors column passes it.
func SessionFactorsValue(factors []string) any {
	if len(factors) == 0 {
		return nil
	}
	return strings.Join(factors, " ")
}

func (s *Session) ToMap() (map[string]any, error) {
	var revokedAt any
	if s.RevokedAt != nil {
		revokedAt = *s.RevokedAt
	}

	return s.mergeExtras(map[string]any{
		SessionID:             s.ID,
		SessionUserID:         s.UserID,
		SessionLookupHash:     s.LookupHash,
		SessionTokenHash:      s.TokenHash,
		SessionKeyVersion:     s.KeyVersion,
		SessionStateColumn:    s.State,
		SessionExpiresAt:      s.ExpiresAt,
		SessionLastActiveAt:   s.LastActiveAt,
		SessionFreshAt:        s.FreshAt,
		SessionFactors:        SessionFactorsValue(s.Factors),
		SessionIPAddress:      s.IPAddress,
		SessionUserAgent:      s.UserAgent,
		SessionImpersonatorID: s.ImpersonatorID,
		SessionRevokedAt:      revokedAt,
		SessionRevokedReason:  s.RevokedReason,
		SessionVersion:        s.Version,
		SessionCreatedAt:      s.CreatedAt,
		SessionUpdatedAt:      s.UpdatedAt,
	}), nil
}

// type SessionStore struct {
// 	DB behemoth.Database
// }

// func (s *SessionStore) SaveSession(ctx context.Context, session behemoth.Session) error {
// 	return s.DB.Create(ctx, session)
// }

// func (s *SessionStore) GetSession(ctx context.Context, sessionModel behemoth.Session, id any) (behemoth.Session, error) {
// 	whereClause := clause.Expression{
// 		Logic: clause.OpAnd,
// 		Conditions: []clause.Condition{
// 			{Field: sessionModel.PrimaryKeyName(), Operator: clause.OpEqual, Value: id},
// 		},
// 	}
// 	found, err := s.DB.FindOne(ctx, sessionModel, whereClause)
// 	if err != nil {
// 		return nil, err
// 	}

// 	return found.(behemoth.Session), nil
// }

// func (s *SessionStore) UpdateSession(ctx context.Context, sessionModel behemoth.Session) error {
// 	return s.DB.Update(ctx, sessionModel)
// }

// func (s *SessionStore) DeleteSession(ctx context.Context, sessionModel behemoth.Session) error {
// 	return s.DB.Delete(ctx, sessionModel)
// }

// NewDefaultSession creates a new DefaultSession.
// func NewDefaultSession(sessionContext behemoth.SessionContext) *Session {
// 	return &Session{
// 		ID:        utils.GenerateUUID(),
// 		UserID:    sessionContext.UserID.(string),
// 		IPAddress: sessionContext.IpAddress,
// 		UserAgent: sessionContext.UserAgent,
// 		CreatedAt: time.Now(),
// 	}
// }
