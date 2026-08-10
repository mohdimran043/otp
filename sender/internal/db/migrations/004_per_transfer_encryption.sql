-- Per-transfer encryption. The cipher and key move from configuration onto the
-- transmission row, because the operator chooses them per transfer: the row is what
-- the pipeline reads, so the chunker and renderer cannot disagree with each other or
-- with a config change made while a transfer was in flight.
--
-- The key lives beside the data it protects. That adds no exposure: this database
-- already holds the uploaded file in plaintext. What encryption defends is the optical
-- channel, which anything with line of sight to the display can read.
ALTER TABLE transmissions ADD COLUMN encryption_id  smallint NOT NULL DEFAULT 0;
ALTER TABLE transmissions ADD COLUMN encryption_key bytea;
