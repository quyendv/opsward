CREATE TABLE api_tokens (
    id           TEXT    NOT NULL PRIMARY KEY,
    secret_hash  BYTEA   NOT NULL,
    subject      TEXT    NOT NULL,
    groups       TEXT[]  NOT NULL,
    description  TEXT    NOT NULL,
    created_ns   BIGINT  NOT NULL,
    expires_ns   BIGINT  NOT NULL,
    revoked_ns   BIGINT  NOT NULL DEFAULT 0,
    last_used_ns BIGINT  NOT NULL DEFAULT 0
);
