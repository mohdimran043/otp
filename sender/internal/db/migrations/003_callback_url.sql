-- The callback URL a transfer was created with.
--
-- It is kept on the transmission as well as in the callbacks table because it has to travel: the
-- manifest carries it across the optical channel so the receiver — the side that ends up holding a
-- merged, verified file — knows where to deliver it. The callbacks row records what became of that
-- delivery; this column records what was asked for.
ALTER TABLE transmissions ADD COLUMN callback_url text NOT NULL DEFAULT '';
