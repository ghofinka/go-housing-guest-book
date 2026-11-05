ALTER TABLE visits DROP COLUMN IF EXISTS watchlist_hit;
DROP FUNCTION IF EXISTS watchlist_check(text, text);
DROP TABLE IF EXISTS watchlist_entries;
