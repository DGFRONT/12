package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	RoomActive        = "active"
	RoomEnded         = "ended"
	RoomClosedByAdmin = "closed_by_admin"
)

type Room struct {
	ID              string     `json:"id"`
	Slug            string     `json:"slug"`
	Name            string     `json:"name"`
	OwnerID         *string    `json:"owner_id"`
	OwnerName       *string    `json:"owner_name"`
	HostID          *string    `json:"host_id"`
	HostName        *string    `json:"host_name"`
	MaxParticipants int        `json:"max_participants"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	EndedAt         *time.Time `json:"ended_at"`
	EndedBy         *string    `json:"ended_by"`
	Online          int        `json:"online"`
}

const roomSelect = `
	SELECT r.id, r.slug, r.name, r.owner_id, o.display_name, r.host_id, h.display_name,
	       r.max_participants, r.status, r.created_at, r.ended_at, r.ended_by,
	       (SELECT count(*) FROM participants p
	         WHERE p.room_id = r.id AND p.admitted_at IS NOT NULL AND p.left_at IS NULL)
	FROM rooms r
	LEFT JOIN users o ON o.id = r.owner_id
	LEFT JOIN users h ON h.id = r.host_id`

func scanRoom(row pgx.Row) (*Room, error) {
	var r Room
	err := row.Scan(&r.ID, &r.Slug, &r.Name, &r.OwnerID, &r.OwnerName, &r.HostID, &r.HostName,
		&r.MaxParticipants, &r.Status, &r.CreatedAt, &r.EndedAt, &r.EndedBy, &r.Online)
	if err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

func collectRooms(rows pgx.Rows, err error) ([]Room, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms := []Room{}
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, *r)
	}
	return rooms, rows.Err()
}

func (s *Store) CreateRoom(ctx context.Context, ownerID, name, slug string, maxParticipants int) (*Room, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO rooms (slug, name, owner_id, host_id, max_participants)
		VALUES ($1, $2, $3, $3, $4) RETURNING id`, slug, name, ownerID, maxParticipants).Scan(&id)
	if isUniqueViolation(err) {
		return nil, ErrDuplicate
	}
	if err != nil {
		return nil, err
	}
	return s.RoomByID(ctx, id)
}

func (s *Store) RoomByID(ctx context.Context, id string) (*Room, error) {
	return scanRoom(s.pool.QueryRow(ctx, roomSelect+` WHERE r.id = $1`, id))
}

func (s *Store) RoomBySlug(ctx context.Context, slug string) (*Room, error) {
	return scanRoom(s.pool.QueryRow(ctx, roomSelect+` WHERE r.slug = $1`, slug))
}

// RoomsForUser returns rooms the user created or currently hosts, newest first.
func (s *Store) RoomsForUser(ctx context.Context, userID string) ([]Room, error) {
	return collectRooms(s.pool.Query(ctx, roomSelect+`
		WHERE r.owner_id = $1 OR r.host_id = $1 ORDER BY r.created_at DESC LIMIT 100`, userID))
}

// ListRooms returns rooms filtered by status ("" means any), newest first.
func (s *Store) ListRooms(ctx context.Context, status string, limit, offset int) ([]Room, error) {
	return collectRooms(s.pool.Query(ctx, roomSelect+`
		WHERE $1 = '' OR r.status::text = $1 ORDER BY r.created_at DESC LIMIT $2 OFFSET $3`, status, limit, offset))
}

// FinishRoom moves an active room to a final status. It returns ErrNotFound if the room is not active.
func (s *Store) FinishRoom(ctx context.Context, id, status, actorID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE rooms SET status = $2, ended_at = now(), ended_by = $3
			WHERE id = $1 AND status = 'active'`, id, status, actorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		reason := "room_ended"
		if status == RoomClosedByAdmin {
			reason = "room_closed"
		}
		_, err = tx.Exec(ctx, `
			UPDATE participants SET left_at = now(), leave_reason = $2
			WHERE room_id = $1 AND left_at IS NULL`, id, reason)
		return err
	})
}

type Participant struct {
	ID          string     `json:"id"`
	UserID      *string    `json:"user_id"`
	DisplayName string     `json:"display_name"`
	Kind        string     `json:"kind"`
	RequestedAt time.Time  `json:"requested_at"`
	AdmittedAt  *time.Time `json:"admitted_at"`
	LeftAt      *time.Time `json:"left_at"`
	LeaveReason *string    `json:"leave_reason"`
}

func (s *Store) RoomParticipants(ctx context.Context, roomID string) ([]Participant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, display_name, kind, requested_at, admitted_at, left_at, leave_reason
		FROM participants WHERE room_id = $1 ORDER BY requested_at`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Participant{}
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.ID, &p.UserID, &p.DisplayName, &p.Kind, &p.RequestedAt, &p.AdmittedAt,
			&p.LeftAt, &p.LeaveReason); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

type Message struct {
	ID         int64     `json:"id"`
	Author     string    `json:"author"`
	AuthorKind string    `json:"author_kind"`
	Body       string    `json:"body"`
	Deleted    bool      `json:"deleted"`
	CreatedAt  time.Time `json:"created_at"`
}

// RoomMessages returns the chat log of a room, including deleted messages (flagged) for the admin archive.
func (s *Store) RoomMessages(ctx context.Context, roomID string) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, p.display_name, p.kind, m.body, m.deleted_at IS NOT NULL, m.created_at
		FROM messages m JOIN participants p ON p.id = m.participant_id
		WHERE m.room_id = $1 ORDER BY m.id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Author, &m.AuthorKind, &m.Body, &m.Deleted, &m.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

// PurgeOldChats deletes chat messages of rooms finished more than retention ago and drops guest IPs.
func (s *Store) PurgeOldChats(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention)
	var deleted int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM messages m USING rooms r
			WHERE m.room_id = r.id AND r.status <> 'active' AND r.ended_at < $1`, cutoff)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		_, err = tx.Exec(ctx, `
			UPDATE participants p SET ip = NULL FROM rooms r
			WHERE p.room_id = r.id AND p.ip IS NOT NULL AND r.status <> 'active' AND r.ended_at < $1`, cutoff)
		return err
	})
	return deleted, err
}
