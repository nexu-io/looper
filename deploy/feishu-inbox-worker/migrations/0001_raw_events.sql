ALTER TABLE events ADD COLUMN event_id TEXT;
ALTER TABLE events ADD COLUMN raw_json TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS events_event_id ON events(event_id);
