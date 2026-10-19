package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type AuditEntry struct {
	ID         int64           `json:"id"`
	ActorID    *string         `json:"actor_id"`
	ActorName  *string         `json:"actor_name"`
	Action     string          `json:"action"`
	TargetType *string         `json:"target_type"`
	TargetID   *string         `json:"target_id"`
	Meta       json.RawMessage `json:"meta"`
	IP         *string         `json:"ip"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (s *Store) Audit(ctx context.Context, actorID, action, targetType, targetID string, meta any, ip string) error {
	var metaJSON []byte
	if meta != nil {
		var err error
		if metaJSON, err = json.Marshal(meta); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (actor_id, action, target_type, target_id, meta, ip)
		VALUES (NULLIF($1, '')::uuid, $2, NULLIF($3, ''), NULLIF($4, ''), $5, NULLIF($6, '')::inet)`,
		actorID, action, targetType, targetID, metaJSON, ip)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit, offset int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.actor_id, u.display_name, a.action, a.target_type, a.target_id,
		       COALESCE(a.meta, 'null'::jsonb), host(a.ip), a.created_at
		FROM audit_log a LEFT JOIN users u ON u.id = a.actor_id
		ORDER BY a.id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorID, &e.ActorName, &e.Action, &e.TargetType, &e.TargetID,
			&e.Meta, &e.IP, &e.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}

// Tables the admin may browse and export. Secrets (password and token hashes) are never selected.
var browsable = []struct {
	Name    string
	Columns string
	Order   string
}{
	{"users", "id, username, display_name, role, is_blocked, created_by, created_at, last_login_at", "created_at DESC"},
	{"rooms", "id, slug, name, owner_id, host_id, max_participants, status, created_at, ended_at, ended_by", "created_at DESC"},
	{"participants", "id, room_id, user_id, display_name, kind, requested_at, admitted_at, admitted_by, left_at, leave_reason, host(ip) AS ip", "requested_at DESC"},
	{"messages", "id, room_id, participant_id, body, deleted_at, created_at", "id DESC"},
	{"audit_log", "id, actor_id, action, target_type, target_id, meta, host(ip) AS ip, created_at", "id DESC"},
}

func TableNames() []string {
	names := make([]string, len(browsable))
	for i, t := range browsable {
		names[i] = t.Name
	}
	return names
}

func tableQuery(name string) (string, error) {
	for _, t := range browsable {
		if t.Name == name {
			// Identifiers come only from the whitelist above.
			return fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s`, t.Columns, t.Name, t.Order), nil
		}
	}
	return "", ErrNotFound
}

type TablePage struct {
	Table string            `json:"table"`
	Total int               `json:"total"`
	Rows  []json.RawMessage `json:"rows"`
}

// BrowseTable returns one page of a whitelisted table, each row as a JSON object.
func (s *Store) BrowseTable(ctx context.Context, name string, limit, offset int) (*TablePage, error) {
	q, err := tableQuery(name)
	if err != nil {
		return nil, err
	}
	page := &TablePage{Table: name, Rows: []json.RawMessage{}}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+name).Scan(&page.Total); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT row_to_json(t) FROM (`+q+` LIMIT $1 OFFSET $2) t`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row json.RawMessage
		if err := rows.Scan(&row); err != nil {
			return nil, err
		}
		page.Rows = append(page.Rows, row)
	}
	return page, rows.Err()
}

// ExportTable streams the whole table to w as a JSON array.
func (s *Store) ExportTable(ctx context.Context, name string, w io.Writer) error {
	q, err := tableQuery(name)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx, `SELECT row_to_json(t) FROM (`+q+`) t`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err := io.WriteString(w, "[\n"); err != nil {
		return err
	}
	first := true
	for rows.Next() {
		var row []byte
		if err := rows.Scan(&row); err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(w, ",\n"); err != nil {
				return err
			}
		}
		first = false
		if _, err := w.Write(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n]\n")
	return err
}

type DayStat struct {
	Day          string `json:"day"`
	Rooms        int    `json:"rooms"`
	Participants int    `json:"participants"`
	Messages     int    `json:"messages"`
}

type UserTime struct {
	UserID      string  `json:"user_id"`
	DisplayName string  `json:"display_name"`
	Minutes     float64 `json:"minutes"`
	Rooms       int     `json:"rooms"`
}

type Stats struct {
	From           time.Time  `json:"from"`
	To             time.Time  `json:"to"`
	UsersTotal     int        `json:"users_total"`
	UsersBlocked   int        `json:"users_blocked"`
	Admins         int        `json:"admins"`
	ActiveUsers    int        `json:"active_users"`
	RoomsCreated   int        `json:"rooms_created"`
	RoomsActiveNow int        `json:"rooms_active_now"`
	AvgRoomMinutes float64    `json:"avg_room_minutes"`
	MaxRoomMinutes float64    `json:"max_room_minutes"`
	Participants   int        `json:"participants"`
	Guests         int        `json:"guests"`
	OnlineNow      int        `json:"online_now"`
	AvgWaitSeconds float64    `json:"avg_wait_seconds"`
	Rejected       int        `json:"rejected"`
	Messages       int        `json:"messages"`
	Daily          []DayStat  `json:"daily"`
	TopUsers       []UserTime `json:"top_users"`
}

// Stats aggregates usage for rooms created in [from, to).
func (s *Store) Stats(ctx context.Context, from, to time.Time) (*Stats, error) {
	st := &Stats{From: from, To: to, Daily: []DayStat{}, TopUsers: []UserTime{}}
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM users),
		  (SELECT count(*) FROM users WHERE is_blocked),
		  (SELECT count(*) FROM users WHERE role = 'admin'),
		  (SELECT count(*) FROM users WHERE last_login_at >= $1 AND last_login_at < $2),
		  (SELECT count(*) FROM rooms WHERE created_at >= $1 AND created_at < $2),
		  (SELECT count(*) FROM rooms WHERE status = 'active'),
		  (SELECT COALESCE(avg(extract(epoch FROM ended_at - created_at)) / 60, 0)
		     FROM rooms WHERE ended_at IS NOT NULL AND created_at >= $1 AND created_at < $2),
		  (SELECT COALESCE(max(extract(epoch FROM ended_at - created_at)) / 60, 0)
		     FROM rooms WHERE ended_at IS NOT NULL AND created_at >= $1 AND created_at < $2),
		  (SELECT count(*) FROM participants WHERE admitted_at >= $1 AND admitted_at < $2),
		  (SELECT count(*) FROM participants WHERE kind = 'guest' AND admitted_at >= $1 AND admitted_at < $2),
		  (SELECT count(*) FROM participants WHERE admitted_at IS NOT NULL AND left_at IS NULL),
		  (SELECT COALESCE(avg(extract(epoch FROM admitted_at - requested_at)), 0)
		     FROM participants WHERE admitted_at >= $1 AND admitted_at < $2 AND kind <> 'host'),
		  (SELECT count(*) FROM participants WHERE leave_reason = 'rejected' AND requested_at >= $1 AND requested_at < $2),
		  (SELECT count(*) FROM messages WHERE created_at >= $1 AND created_at < $2)`,
		from, to).Scan(&st.UsersTotal, &st.UsersBlocked, &st.Admins, &st.ActiveUsers, &st.RoomsCreated,
		&st.RoomsActiveNow, &st.AvgRoomMinutes, &st.MaxRoomMinutes, &st.Participants, &st.Guests,
		&st.OnlineNow, &st.AvgWaitSeconds, &st.Rejected, &st.Messages)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT to_char(d, 'YYYY-MM-DD'),
		  (SELECT count(*) FROM rooms WHERE created_at >= d AND created_at < d + interval '1 day'),
		  (SELECT count(*) FROM participants WHERE admitted_at >= d AND admitted_at < d + interval '1 day'),
		  (SELECT count(*) FROM messages WHERE created_at >= d AND created_at < d + interval '1 day')
		FROM generate_series(date_trunc('day', $1::timestamptz), $2::timestamptz - interval '1 microsecond', interval '1 day') d
		ORDER BY d`, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Day, &d.Rooms, &d.Participants, &d.Messages); err != nil {
			rows.Close()
			return nil, err
		}
		st.Daily = append(st.Daily, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT u.id, u.display_name,
		       sum(extract(epoch FROM COALESCE(p.left_at, now()) - p.admitted_at)) / 60,
		       count(DISTINCT p.room_id)
		FROM participants p JOIN users u ON u.id = p.user_id
		WHERE p.admitted_at >= $1 AND p.admitted_at < $2
		GROUP BY u.id, u.display_name ORDER BY 3 DESC LIMIT 20`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u UserTime
		if err := rows.Scan(&u.UserID, &u.DisplayName, &u.Minutes, &u.Rooms); err != nil {
			return nil, err
		}
		st.TopUsers = append(st.TopUsers, u)
	}
	return st, rows.Err()
}
