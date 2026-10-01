PRAGMA journal_mode=WAL;
PRAGMA wal_autocheckpoint=0;
CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB);
CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB);
INSERT INTO cursorDiskKV VALUES ('composerData:33333333-3333-4333-8333-333333333333', '{"composerId":"33333333-3333-4333-8333-333333333333","name":"Virtual context","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"status":"none","fullConversationHeadersOnly":[{"bubbleId":"user","type":1},{"bubbleId":"thinking","type":2},{"bubbleId":"assistant","type":2}]}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:33333333-3333-4333-8333-333333333333:user', '{"_v":3,"type":1,"bubbleId":"user","text":"virtual context checkpoint question","createdAt":"2026-09-28T07:00:00Z"}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:33333333-3333-4333-8333-333333333333:thinking', '{"_v":3,"type":2,"bubbleId":"thinking","thinking":{"text":"excluded reasoning between requested windows"},"createdAt":"2026-09-28T07:00:00.500Z"}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:33333333-3333-4333-8333-333333333333:assistant', '{"_v":3,"type":2,"bubbleId":"assistant","text":"virtual context checkpoint answer","createdAt":"2026-09-28T07:00:01Z","toolFormerData":{"name":"read_file","rawArgs":"{\"path\":\"README.md\"}","result":"excluded tool output","status":"success"}}');
PRAGMA wal_checkpoint(TRUNCATE);
