package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	DisplayName  string     `json:"display_name"`
	PasswordHash string     `json:"-"`
	Role         string     `json:"role"`
	IsBlocked    bool       `json:"is_blocked"`
	CreatedBy    *string    `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at"`
}

const userColumns = `id, username, display_name, password_hash, role, is_blocked, created_by, created_at, last_login_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.IsBlocked,
		&u.CreatedBy, &u.CreatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

type NewUser struct {
	Username     string
	DisplayName  string
	PasswordHash string
	Role         string
	CreatedBy    *string
}

func (s *Store) CreateUser(ctx context.Context, n NewUser) (*User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `
		INSERT INTO users (username, display_name, password_hash, role, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+userColumns,
		n.Username, n.DisplayName, n.PasswordHash, n.Role, n.CreatedBy))
	if isUniqueViolation(err) {
		return nil, ErrDuplicate
	}
	return u, err
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE lower(username) = lower($1)`, username))
}

// ListUsers returns users whose username or display name contains query (all users if empty).
func (s *Store) ListUsers(ctx context.Context, query string) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+userColumns+` FROM users
		WHERE $1 = '' OR username ILIKE '%' || $1 || '%' OR display_name ILIKE '%' || $1 || '%'
		ORDER BY created_at DESC`, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, rows.Err()
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND NOT is_blocked`).Scan(&n)
	return n, err
}

// UserPatch holds optional changes; nil fields are left untouched.
type UserPatch struct {
	DisplayName  *string
	PasswordHash *string
	Role         *string
	IsBlocked    *bool
}

func (s *Store) UpdateUser(ctx context.Context, id string, p UserPatch) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		UPDATE users SET
			display_name  = COALESCE($2, display_name),
			password_hash = COALESCE($3, password_hash),
			role          = COALESCE($4::user_role, role),
			is_blocked    = COALESCE($5, is_blocked)
		WHERE id = $1
		RETURNING `+userColumns, id, p.DisplayName, p.PasswordHash, p.Role, p.IsBlocked))
}

// DeleteUser removes the user, closes the rooms they still host and returns the IDs of those rooms.
func (s *Store) DeleteUser(ctx context.Context, id, actorID string) ([]string, error) {
	var closed []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE rooms SET status = 'closed_by_admin', ended_at = now(), ended_by = $2
			WHERE status = 'active' AND (owner_id = $1 OR host_id = $1) RETURNING id`, id, actorID)
		if err != nil {
			return err
		}
		closed, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE participants SET left_at = now(), leave_reason = 'room_closed'
			WHERE left_at IS NULL AND room_id = ANY($1::uuid[])`, closed); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	return closed, err
}

func (s *Store) TouchLogin(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
	return err
}

func (s *Store) CreateRefreshToken(ctx context.Context, userID, hash string, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, hash, expires)
	return err
}

// ConsumeRefreshToken revokes a valid token and returns its owner, so every refresh rotates the token.
func (s *Store) ConsumeRefreshToken(ctx context.Context, hash string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
		RETURNING user_id`, hash).Scan(&userID)
	return userID, notFound(err)
}

func (s *Store) RevokeUserTokens(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

func (s *Store) DeleteExpiredTokens(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now() OR revoked_at < now() - interval '1 day'`)
	return err
}
