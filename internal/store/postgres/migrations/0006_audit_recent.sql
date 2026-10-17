CREATE INDEX audit_recent ON audit (time_ns, writer, seq);
