-- PRD C4: the statistics (and the S8 report) read every visit in a date range across all houses. The
-- existing indexes lead with house_id, or cover only guests who are inside, so such a range scan reads the
-- whole table. This one serves it.
CREATE INDEX visits_checkin_at_idx ON visits (checkin_at);
