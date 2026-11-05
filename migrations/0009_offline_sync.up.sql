-- PRD S5: the pos can lose signal, so the client records check-ins and check-outs offline and replays
-- them later. The server clock stays authoritative (AGENTS.md rule 1): checkin_at and checkout_at are
-- still the time the server received the request. The time the device recorded the event is stored
-- beside them, never instead of them, and nothing filters, sorts or expires by it.
ALTER TABLE visits
    ADD COLUMN client_captured_at          timestamptz,
    ADD COLUMN client_checkout_captured_at timestamptz;
