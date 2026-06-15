DELETE FROM queue_items
WHERE type = 'sweeper' OR type LIKE 'sweeper:%';

DROP TABLE IF EXISTS sweeper_proposals;
DROP TABLE IF EXISTS sweeper_cases;
