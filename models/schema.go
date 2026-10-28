package models

import "github.com/go-behemoth/behemoth/schema"

// The tables core declares (CoreDeclareSchema), built from the same column
// constants as each model's ToMap/FromMap. Other declarers may extend them
// (ExtendColumn); a model carries those columns in its Extension.
//
// Ids are bounded strings rather than a database UUID type, so every driver
// stores and returns them as the string the models hold.

// UserTableSchema declares users. Its own columns are all public: they are
// what a route answers with when it returns a user (schema.PublicView). A
// column another declarer contributes is private unless that declarer marks
// it Public.
//
// The other core tables mark nothing public. No route returns a session, a
// token or an account today; a route that will should mark the columns it
// means to return.
func UserTableSchema() schema.Table {
	return schema.Table{
		Name: UserTable,
		Columns: []schema.Column{
			{Name: UserID, Type: schema.ColTypeString, Length: 36, PrimaryKey: true, Public: true},
			{Name: UserEmail, Type: schema.ColTypeString, Length: 255, Unique: true, Public: true},
			{Name: UserFirstname, Type: schema.ColTypeString, Length: 255, Nullable: true, Public: true},
			{Name: UserLastname, Type: schema.ColTypeString, Length: 255, Nullable: true, Public: true},
			{Name: UserEmailVerified, Type: schema.ColTypeBoolean, Default: false, Public: true},
			{Name: UserImageURL, Type: schema.ColTypeText, Nullable: true, Public: true},
			{Name: UserCreatedAt, Type: schema.ColTypeTimestamp, Public: true},
			{Name: UserUpdatedAt, Type: schema.ColTypeTimestamp, Public: true},
		},
	}
}

func SessionTableSchema() schema.Table {
	return schema.Table{
		Name: SessionTable,
		Columns: []schema.Column{
			{Name: SessionID, Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
			{Name: SessionUserID, Type: schema.ColTypeString, Length: 36},
			{Name: SessionLookupHash, Type: schema.ColTypeString, Length: 128, Unique: true},
			{Name: SessionTokenHash, Type: schema.ColTypeText},
			{Name: SessionKeyVersion, Type: schema.ColTypeInteger},
			{Name: SessionStateColumn, Type: schema.ColTypeString, Length: 16},
			{Name: SessionExpiresAt, Type: schema.ColTypeTimestamp},
			{Name: SessionLastActiveAt, Type: schema.ColTypeTimestamp},
			{Name: SessionFreshAt, Type: schema.ColTypeTimestamp},
			{Name: SessionFactors, Type: schema.ColTypeString, Length: 255, Nullable: true},
			{Name: SessionIPAddress, Type: schema.ColTypeString, Length: 64, Nullable: true},
			{Name: SessionUserAgent, Type: schema.ColTypeText, Nullable: true},
			{Name: SessionImpersonatorID, Type: schema.ColTypeString, Length: 36, Nullable: true},
			{Name: SessionRevokedAt, Type: schema.ColTypeTimestamp, Nullable: true},
			{Name: SessionRevokedReason, Type: schema.ColTypeString, Length: 64, Nullable: true},
			{Name: SessionVersion, Type: schema.ColTypeBigInt, Default: 0}, // rows that exist get 0. See ADR-0011.
			{Name: SessionCreatedAt, Type: schema.ColTypeTimestamp},
			{Name: SessionUpdatedAt, Type: schema.ColTypeTimestamp},
		},
		Indexes: []schema.Index{
			{Name: "idx_sessions_user_id", Columns: []string{SessionUserID}},
			{Name: "idx_sessions_expires_at", Columns: []string{SessionExpiresAt}}, // Store.PurgeExpiredSessions
		},
		ForeignKeys: []schema.ForeignKey{{
			Name: "fk_sessions_user", Columns: []string{SessionUserID},
			RefTable: UserTable, RefColumns: []string{UserID}, OnDelete: schema.FKCascade,
		}},
	}
}

func TokenTableSchema() schema.Table {
	return schema.Table{
		Name: TokenTable,
		Columns: []schema.Column{
			{Name: TokenID, Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
			{Name: TokenKindColumn, Type: schema.ColTypeString, Length: 64},
			{Name: TokenSubject, Type: schema.ColTypeString, Length: 255, Nullable: true},
			{Name: TokenLookupHash, Type: schema.ColTypeString, Length: 128},
			{Name: TokenTokenHash, Type: schema.ColTypeText},
			{Name: TokenKeyVersion, Type: schema.ColTypeInteger},
			{Name: TokenMetadata, Type: schema.ColTypeJson, Nullable: true},
			{Name: TokenExpiresAt, Type: schema.ColTypeTimestamp, Nullable: true},
			{Name: TokenConsumedAt, Type: schema.ColTypeTimestamp, Nullable: true},
			{Name: TokenRevokedAt, Type: schema.ColTypeTimestamp, Nullable: true},
			{Name: TokenCreatedAt, Type: schema.ColTypeTimestamp},
		},
		Indexes: []schema.Index{
			{Name: "uq_tokens_kind_lookup_hash", Columns: []string{TokenKindColumn, TokenLookupHash}, Unique: true},
			{Name: "idx_tokens_kind_subject", Columns: []string{TokenKindColumn, TokenSubject}},
			{Name: "idx_tokens_expires_at", Columns: []string{TokenExpiresAt}}, // Store.PurgeExpiredTokens
		},
	}
}

func AccountTableSchema() schema.Table {
	return schema.Table{
		Name: AccountTable,
		Columns: []schema.Column{
			{Name: AccountID, Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
			{Name: AccountUserID, Type: schema.ColTypeString, Length: 36},
			{Name: AccountProviderID, Type: schema.ColTypeString, Length: 64},
			{Name: AccountAccountID, Type: schema.ColTypeString, Length: 255},
			{Name: AccountPasswordHash, Type: schema.ColTypeText, Nullable: true},
			{Name: AccountAccessToken, Type: schema.ColTypeText, Nullable: true},
			{Name: AccountRefreshToken, Type: schema.ColTypeText, Nullable: true},
			{Name: AccountIDToken, Type: schema.ColTypeText, Nullable: true},
			{Name: AccountAccessTokenExpiresAt, Type: schema.ColTypeTimestamp, Nullable: true},
			{Name: AccountRefreshTokenExpiresAt, Type: schema.ColTypeTimestamp, Nullable: true},
			{Name: AccountScope, Type: schema.ColTypeText, Nullable: true},
			{Name: AccountCreatedAt, Type: schema.ColTypeTimestamp},
			{Name: AccountUpdatedAt, Type: schema.ColTypeTimestamp},
		},
		Indexes: []schema.Index{
			{Name: "uq_accounts_provider_account", Columns: []string{AccountProviderID, AccountAccountID}, Unique: true},
			{Name: "idx_accounts_user_id", Columns: []string{AccountUserID}},
		},
		ForeignKeys: []schema.ForeignKey{{
			Name: "fk_accounts_user", Columns: []string{AccountUserID},
			RefTable: UserTable, RefColumns: []string{UserID}, OnDelete: schema.FKCascade,
		}},
	}
}

func RateLimitTableSchema() schema.Table {
	return schema.Table{
		Name: RateLimitTable,
		Columns: []schema.Column{
			{Name: RateLimitKey, Type: schema.ColTypeString, Length: 255, PrimaryKey: true},
			{Name: RateLimitCount, Type: schema.ColTypeBigInt},
			{Name: RateLimitExpiresAt, Type: schema.ColTypeTimestamp},
		},
		Indexes: []schema.Index{{Name: "idx_rate_limits_expires_at", Columns: []string{RateLimitExpiresAt}}},
	}
}

// AuditLogTableSchema has an index per column Store.QueryAuditEvents filters
// on. The id needs none beyond the primary key, which is also the order
// events are paged in.
func AuditLogTableSchema() schema.Table {
	return schema.Table{
		Name: AuditLogTable,
		Columns: []schema.Column{
			{Name: AuditLogID, Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
			{Name: AuditLogEventType, Type: schema.ColTypeString, Length: 128},
			{Name: AuditLogOutcome, Type: schema.ColTypeString, Length: 16},
			{Name: AuditLogActorType, Type: schema.ColTypeString, Length: 16},
			{Name: AuditLogActorID, Type: schema.ColTypeString, Length: 255, Nullable: true},
			{Name: AuditLogDelegateType, Type: schema.ColTypeString, Length: 16, Nullable: true},
			{Name: AuditLogDelegateID, Type: schema.ColTypeString, Length: 255, Nullable: true},
			{Name: AuditLogSubjectType, Type: schema.ColTypeString, Length: 64, Nullable: true},
			{Name: AuditLogSubjectID, Type: schema.ColTypeString, Length: 255, Nullable: true},
			{Name: AuditLogSessionID, Type: schema.ColTypeString, Length: 36, Nullable: true},
			{Name: AuditLogRequestID, Type: schema.ColTypeString, Length: 128, Nullable: true},
			{Name: AuditLogIPAddress, Type: schema.ColTypeString, Length: 64, Nullable: true},
			{Name: AuditLogUserAgent, Type: schema.ColTypeText, Nullable: true},
			{Name: AuditLogMetadata, Type: schema.ColTypeJson, Nullable: true},
			{Name: AuditLogCreatedAt, Type: schema.ColTypeTimestamp},
		},
		Indexes: []schema.Index{
			{Name: "idx_audit_log_event_type", Columns: []string{AuditLogEventType}},
			{Name: "idx_audit_log_actor_id", Columns: []string{AuditLogActorID}},
			{Name: "idx_audit_log_subject_id", Columns: []string{AuditLogSubjectID}},
			{Name: "idx_audit_log_request_id", Columns: []string{AuditLogRequestID}},
			{Name: "idx_audit_log_created_at", Columns: []string{AuditLogCreatedAt}},
		},
	}
}
