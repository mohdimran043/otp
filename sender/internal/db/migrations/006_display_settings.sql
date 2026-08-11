-- Display settings an operator changed through the UI, so the change outlives the restart it needs.
--
-- Before this, the settings API applied a change to the running configuration and stored nothing. Most of
-- those settings are reloadable, so that was survivable; the display sink is not. It is read once when the
-- process starts, which meant choosing "camera" as the transfer channel took effect on the next restart —
-- and the restart re-read the file and the environment, throwing the choice away. The control could never
-- do anything.
--
-- Key/value rather than one wide row, because the storage has to be sparse. Only settings an operator
-- actually changed are stored; everything else keeps following sender.yaml and the environment. A single
-- row of columns would have to invent a value for every field the moment any one of them changed, pinning
-- the whole configuration on the first edit.
CREATE TABLE display_settings (
    key        text        PRIMARY KEY,
    value      text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
