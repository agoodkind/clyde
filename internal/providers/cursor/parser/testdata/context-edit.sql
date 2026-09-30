PRAGMA journal_mode=WAL;
PRAGMA wal_autocheckpoint=0;
UPDATE cursorDiskKV
SET value = replace(CAST(value AS TEXT), 'composer question', 'changed composer question')
WHERE key = 'bubbleId:33333333-3333-4333-8333-333333333333:composer-user';
