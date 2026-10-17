CREATE TABLE sessions (
    id_hash     BLOB    NOT NULL PRIMARY KEY,
    subject     TEXT    NOT NULL,
    groups_json TEXT    NOT NULL,
    credential  TEXT    NOT NULL,
    created_ns  INTEGER NOT NULL,
    expires_ns  INTEGER NOT NULL
);

CREATE INDEX sessions_expires ON sessions (expires_ns);
