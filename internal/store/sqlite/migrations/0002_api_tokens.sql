CREATE TABLE api_tokens (
    id           TEXT    NOT NULL PRIMARY KEY,
    secret_hash  BLOB    NOT NULL,
    subject      TEXT    NOT NULL,
    groups_json  TEXT    NOT NULL,
    description  TEXT    NOT NULL,
    created_ns   INTEGER NOT NULL,
    expires_ns   INTEGER NOT NULL,
    revoked_ns   INTEGER NOT NULL DEFAULT 0,
    last_used_ns INTEGER NOT NULL DEFAULT 0
);
