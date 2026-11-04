-- 一次性修复 M5 update 未同步 hash 的历史数据。执行前先备份；可重复执行。
-- 不在 ApplySchema 自动运行：去重和历史改写必须由维护者显式触发。
BEGIN;

WITH ranked AS (
  SELECT id, user_id, text,
         row_number() OVER (PARTITION BY user_id, text ORDER BY updated_at DESC, id DESC) AS rn
  FROM memories
)
INSERT INTO memory_ops (user_id, op, target_type, target_id, before_text, after_text, source)
SELECT user_id, 'delete', 'memory', id, text, NULL, 'repair:memory-hash-v1'
FROM ranked WHERE rn > 1;

WITH ranked AS (
  SELECT id, row_number() OVER (PARTITION BY user_id, text ORDER BY updated_at DESC, id DESC) AS rn
  FROM memories
)
DELETE FROM memories WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

INSERT INTO memory_ops (user_id, op, target_type, target_id, before_text, after_text, source)
SELECT user_id, 'update', 'memory', id, text, text, 'repair:memory-hash-v1'
FROM memories WHERE hash <> md5(text);

UPDATE memories SET hash = md5(text), updated_at = now()
WHERE hash <> md5(text);

COMMIT;
