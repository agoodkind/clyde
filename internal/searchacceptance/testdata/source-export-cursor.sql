CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB);
CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB);
INSERT INTO cursorDiskKV(key, value) VALUES (
    'composerData:33333333-3333-4333-8333-333333333333',
    '{"composerId":"33333333-3333-4333-8333-333333333333","name":"Frozen Composer","createdAt":1710000000000,"fullConversationHeadersOnly":[{"bubbleId":"user","type":1},{"bubbleId":"assistant","type":2}]}'
);
INSERT INTO cursorDiskKV(key, value) VALUES (
    'bubbleId:33333333-3333-4333-8333-333333333333:user',
    '{"_v":3,"type":1,"bubbleId":"user","text":"frozen composer question","createdAt":"2026-09-28T07:00:00Z"}'
);
INSERT INTO cursorDiskKV(key, value) VALUES (
    'bubbleId:33333333-3333-4333-8333-333333333333:assistant',
    '{"_v":3,"type":2,"bubbleId":"assistant","text":"frozen composer answer","createdAt":"2026-09-28T07:00:01Z"}'
);
