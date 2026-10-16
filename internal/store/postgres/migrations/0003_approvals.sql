CREATE TABLE approvals (
    id              TEXT    NOT NULL PRIMARY KEY,
    requester       TEXT    NOT NULL,
    tool            TEXT    NOT NULL,
    environment     TEXT    NOT NULL,
    namespace       TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    args_hash       TEXT    NOT NULL,
    diff            TEXT    NOT NULL,
    approver_users  TEXT[]  NOT NULL,
    approver_groups TEXT[]  NOT NULL,
    status          TEXT    NOT NULL,
    created_ns      BIGINT  NOT NULL,
    expires_ns      BIGINT  NOT NULL,
    decided_by      TEXT    NOT NULL DEFAULT '',
    decided_ns      BIGINT  NOT NULL DEFAULT 0,
    reason          TEXT    NOT NULL DEFAULT '',
    used_ns         BIGINT  NOT NULL DEFAULT 0
);

CREATE INDEX approvals_created ON approvals (created_ns);
