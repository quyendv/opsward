CREATE TABLE sessions (
    id_hash     BYTEA   NOT NULL PRIMARY KEY,
    subject     TEXT    NOT NULL,
    groups      TEXT[]  NOT NULL,
    credential  TEXT    NOT NULL,
    created_ns  BIGINT  NOT NULL,
    expires_ns  BIGINT  NOT NULL
);

CREATE INDEX sessions_expires ON sessions (expires_ns);
