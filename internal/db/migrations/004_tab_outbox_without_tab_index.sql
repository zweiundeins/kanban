-- The index on (tab, id) cost every message a random page. A tab's view
-- reads the ids after the newest message it has instead, starting at the
-- page's position.
DROP INDEX tab_outbox_tab;
