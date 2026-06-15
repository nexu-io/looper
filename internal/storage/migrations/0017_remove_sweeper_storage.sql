DELETE FROM queue_items
WHERE type LIKE 'sweeper:%';

DROP TABLE IF EXISTS sweeper_proposals;
DROP TABLE IF EXISTS sweeper_cases;
