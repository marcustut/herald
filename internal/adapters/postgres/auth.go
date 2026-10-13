package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Default display names for the rows provisioned on first sign-in. The human
// is identified by the user row's email; the account/tenant names are cosmetic.
const (
	accountName = "Herald account"
	tenantName  = "default"
)

// Identities is a PostgreSQL app.IdentityStore. EnsureUser resolves or
// provisions an account, its user and a default tenant in one transaction, so
// concurrent first sign-ins converge on a single identity keyed by email.
type Identities struct{ pool *pgxpool.Pool }

func NewIdentities(pool *pgxpool.Pool) *Identities { return &Identities{pool: pool} }

func (s *Identities) EnsureUser(ctx context.Context, email string, ids app.NewIdentityIDs, now time.Time) (app.Identity, error) {
	// A concurrent insert of the same new email surfaces as a unique
	// violation; retry once to read the winner's row instead.
	for attempt := 0; attempt < 3; attempt++ {
		id, err := s.ensureUser(ctx, email, ids, now)
		if err == nil {
			return id, nil
		}
		if isUniqueViolation(err) {
			continue
		}
		return app.Identity{}, err
	}
	return app.Identity{}, errors.New("ensure user: repeated unique conflicts")
}

func (s *Identities) ensureUser(ctx context.Context, email string, ids app.NewIdentityIDs, now time.Time) (app.Identity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return app.Identity{}, fmt.Errorf("ensure user begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		userID    = string(ids.User)
		accountID = string(ids.Account)
		tenantID  = string(ids.Tenant)
	)
	err = tx.QueryRow(ctx, `SELECT id, account_id FROM users WHERE email = $1`, email).Scan(&userID, &accountID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`INSERT INTO accounts (id, name, created_at) VALUES ($1, $2, $3)`,
			accountID, accountName, now); err != nil {
			return app.Identity{}, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, account_id, email, created_at) VALUES ($1, $2, $3, $4)`,
			userID, accountID, email, now); err != nil {
			return app.Identity{}, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, account_id, name, created_at) VALUES ($1, $2, $3, $4)`,
			tenantID, accountID, tenantName, now); err != nil {
			return app.Identity{}, err
		}
	case err != nil:
		return app.Identity{}, fmt.Errorf("lookup user: %w", err)
	default:
		// An existing user reuses its account's tenant; the tenant rows are
		// created here, so the only way none exists is a database seeded
		// outside this flow.
		err = tx.QueryRow(ctx,
			`SELECT id FROM tenants WHERE account_id = $1 ORDER BY created_at, id LIMIT 1`,
			accountID).Scan(&tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			tenantID = string(ids.Tenant)
			if _, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, account_id, name, created_at) VALUES ($1, $2, $3, $4)`,
				tenantID, accountID, tenantName, now); err != nil {
				return app.Identity{}, err
			}
		} else if err != nil {
			return app.Identity{}, fmt.Errorf("lookup tenant: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return app.Identity{}, fmt.Errorf("ensure user commit: %w", err)
	}
	return app.Identity{
		UserID:    domain.UserID(userID),
		AccountID: domain.AccountID(accountID),
		Email:     email,
		TenantID:  domain.TenantID(tenantID),
	}, nil
}

func (s *Identities) DeleteAccount(ctx context.Context, account domain.AccountID) error {
	// Cascades remove the account's users, tenants, projects, destinations and
	// sessions. Deleting a missing account is a no-op (idempotent).
	if _, err := s.pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, string(account)); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	return nil
}

var _ app.IdentityStore = (*Identities)(nil)

// Sessions is a PostgreSQL app.SessionStore.
type Sessions struct{ pool *pgxpool.Pool }

func NewSessions(pool *pgxpool.Pool) *Sessions { return &Sessions{pool: pool} }

func (s *Sessions) PutSession(ctx context.Context, r app.SessionRecord) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, account_id, tenant_id, csrf_hash, created_at, expires_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (token_hash) DO NOTHING`,
		r.Hash, string(r.UserID), string(r.AccountID), string(r.TenantID), r.CSRFHash,
		r.CreatedAt, r.ExpiresAt, nullTime(r.RevokedAt)); err != nil {
		return fmt.Errorf("put session: %w", err)
	}
	return nil
}

func (s *Sessions) SessionByHash(ctx context.Context, hash string, now time.Time) (app.SessionRecord, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT s.token_hash, s.user_id, s.account_id, s.tenant_id, s.csrf_hash, s.created_at, s.expires_at, s.revoked_at, u.email
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > $2`, hash, now)
	var (
		r         app.SessionRecord
		userID    string
		accountID string
		tenantID  string
		revokedAt *time.Time
	)
	if err := row.Scan(&r.Hash, &userID, &accountID, &tenantID, &r.CSRFHash, &r.CreatedAt, &r.ExpiresAt, &revokedAt, &r.Email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.SessionRecord{}, app.ErrSessionNotFound
		}
		return app.SessionRecord{}, fmt.Errorf("session by hash: %w", err)
	}
	r.UserID = domain.UserID(userID)
	r.AccountID = domain.AccountID(accountID)
	r.TenantID = domain.TenantID(tenantID)
	r.RevokedAt = timeOrZero(revokedAt)
	return r, nil
}

func (s *Sessions) RevokeSession(ctx context.Context, hash string, now time.Time) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`, hash, now); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (s *Sessions) RevokeAccountSessions(ctx context.Context, account domain.AccountID, now time.Time) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = $2 WHERE account_id = $1 AND revoked_at IS NULL`, string(account), now); err != nil {
		return fmt.Errorf("revoke account sessions: %w", err)
	}
	return nil
}

var _ app.SessionStore = (*Sessions)(nil)

// LoginTokens is a PostgreSQL app.LoginTokenStore.
type LoginTokens struct{ pool *pgxpool.Pool }

func NewLoginTokens(pool *pgxpool.Pool) *LoginTokens { return &LoginTokens{pool: pool} }

func (s *LoginTokens) PutLoginToken(ctx context.Context, t app.LoginTokenRecord) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO login_tokens (token_hash, email, created_at, expires_at, used_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (token_hash) DO NOTHING`,
		t.Hash, t.Email, t.CreatedAt, t.ExpiresAt, nullTime(t.UsedAt)); err != nil {
		return fmt.Errorf("put login token: %w", err)
	}
	return nil
}

func (s *LoginTokens) ConsumeLoginToken(ctx context.Context, hash string, now time.Time) (app.LoginTokenRecord, error) {
	// The UPDATE ... WHERE used_at IS NULL is the single-use guarantee: the row
	// is atomically claimed, so two concurrent redemptions cannot both succeed.
	row := s.pool.QueryRow(ctx, `
		UPDATE login_tokens SET used_at = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING token_hash, email, created_at, expires_at, used_at`, hash, now)
	var (
		t      app.LoginTokenRecord
		usedAt *time.Time
	)
	if err := row.Scan(&t.Hash, &t.Email, &t.CreatedAt, &t.ExpiresAt, &usedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.LoginTokenRecord{}, app.ErrLoginTokenNotFound
		}
		return app.LoginTokenRecord{}, fmt.Errorf("consume login token: %w", err)
	}
	t.UsedAt = timeOrZero(usedAt)
	return t, nil
}

var _ app.LoginTokenStore = (*LoginTokens)(nil)
