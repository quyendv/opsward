CREATE TABLE audit (
    writer  TEXT    NOT NULL,
    seq     INTEGER NOT NULL,
    id      TEXT    NOT NULL UNIQUE,
    time_ns INTEGER NOT NULL,
    phase   TEXT    NOT NULL,
    hash    TEXT    NOT NULL,
    prev    TEXT    NOT NULL,
    body    BLOB    NOT NULL,
    PRIMARY KEY (writer, seq)
);

CREATE INDEX audit_writer_time ON audit (writer, time_ns);
