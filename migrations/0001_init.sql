CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TYPE user_role        AS ENUM ('admin', 'user');
CREATE TYPE room_status      AS ENUM ('active', 'ended', 'closed_by_admin');
CREATE TYPE participant_kind AS ENUM ('host', 'member', 'guest', 'admin');

CREATE TABLE users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  username      TEXT NOT NULL,
  display_name  TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  role          user_role NOT NULL DEFAULT 'user',
  is_blocked    BOOLEAN NOT NULL DEFAULT false,
  created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX users_username_idx ON users (lower(username));

CREATE TABLE rooms (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slug             TEXT UNIQUE NOT NULL,
  name             TEXT NOT NULL,
  owner_id         UUID REFERENCES users(id) ON DELETE SET NULL,
  host_id          UUID REFERENCES users(id) ON DELETE SET NULL,
  max_participants INT  NOT NULL DEFAULT 10,
  status           room_status NOT NULL DEFAULT 'active',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at         TIMESTAMPTZ,
  ended_by         UUID REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX rooms_status_idx ON rooms (status);
CREATE INDEX rooms_owner_idx  ON rooms (owner_id);

CREATE TABLE participants (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  room_id      UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  user_id      UUID REFERENCES users(id) ON DELETE SET NULL,
  display_name TEXT NOT NULL,
  kind         participant_kind NOT NULL,
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  admitted_at  TIMESTAMPTZ,
  admitted_by  UUID REFERENCES users(id) ON DELETE SET NULL,
  left_at      TIMESTAMPTZ,
  leave_reason TEXT,
  ip           INET
);
CREATE INDEX participants_room_idx ON participants (room_id);
CREATE INDEX participants_user_idx ON participants (user_id);

CREATE TABLE messages (
  id             BIGSERIAL PRIMARY KEY,
  room_id        UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
  participant_id UUID NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
  body           TEXT NOT NULL,
  deleted_at     TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX messages_room_idx ON messages (room_id, id);

CREATE TABLE refresh_tokens (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT UNIQUE NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
  id          BIGSERIAL PRIMARY KEY,
  actor_id    UUID REFERENCES users(id) ON DELETE SET NULL,
  action      TEXT NOT NULL,
  target_type TEXT,
  target_id   TEXT,
  meta        JSONB,
  ip          INET,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_created_idx ON audit_log (created_at DESC);
