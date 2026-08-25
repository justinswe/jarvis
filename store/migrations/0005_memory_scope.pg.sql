-- Quarantine legacy auto-generated memories and make all future active memory local to
-- one Discord channel/thread and one attributed subject.

ALTER TABLE memory_records ADD COLUMN subject_user_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE memory_records ADD COLUMN origin_role TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_records ADD COLUMN expires_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE memory_records ADD COLUMN quarantined_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE memory_records ADD COLUMN quarantine_reason TEXT NOT NULL DEFAULT '';

UPDATE memory_records
SET quarantined_at = extract(epoch from now())::bigint,
    quarantine_reason = 'legacy_auto_memory'
WHERE deleted_at = 0 AND pinned = 0;

DROP INDEX memory_records_entity;
DROP INDEX memory_records_scope;
DROP INDEX memory_records_summary_range;

CREATE INDEX memory_records_scope
ON memory_records(guild_id, channel_id, character_id, updated_at)
WHERE deleted_at = 0 AND quarantined_at = 0;

CREATE UNIQUE INDEX memory_records_entity
ON memory_records(guild_id, channel_id, character_id, subject_user_id, entity_key)
WHERE deleted_at = 0 AND quarantined_at = 0 AND entity_key <> '';

CREATE UNIQUE INDEX memory_records_summary_range
ON memory_records(guild_id, character_id, channel_id, source_to_id)
WHERE deleted_at = 0 AND quarantined_at = 0 AND kind = 'summary' AND source_to_id <> 0;
