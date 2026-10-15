CREATE TABLE audit (
    writer  TEXT   NOT NULL,
    seq     BIGINT NOT NULL,
    id      TEXT   NOT NULL UNIQUE,
    time_ns BIGINT NOT NULL,
    phase   TEXT   NOT NULL,
    hash    TEXT   NOT NULL,
    prev    TEXT   NOT NULL,
    body    BYTEA  NOT NULL,
    PRIMARY KEY (writer, seq)
);

CREATE INDEX audit_writer_time ON audit (writer, time_ns);
