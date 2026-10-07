package app

import (
	"fmt"

	"kanban/internal/db"
)

const gap = 1024

// endPosition is a position after every active row in the scope.
func endPosition(tx *db.Tx, table, scope string, id int64) (int64, error) {
	var max int64
	err := tx.QueryRow(fmt.Sprintf(`SELECT coalesce(max(position), 0) FROM %s WHERE %s = ? AND archived_at IS NULL`, table, scope), id).Scan(&max)
	return max + gap, err
}

// positionBefore finds a position for self just before the row before (0:
// at the end) among the active rows of the scope. When there is no room
// between two neighbours, it renumbers the scope with even gaps first.
func positionBefore(tx *db.Tx, table, scope string, id, self, before int64) (int64, error) {
	if before == 0 {
		var max int64
		err := tx.QueryRow(fmt.Sprintf(`SELECT coalesce(max(position), 0) FROM %s WHERE %s = ? AND archived_at IS NULL AND id != ?`, table, scope), id, self).Scan(&max)
		return max + gap, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		var at, prev int64
		err := tx.QueryRow(fmt.Sprintf(`SELECT position FROM %s WHERE id = ?`, table), before).Scan(&at)
		if err != nil {
			return 0, err
		}
		err = tx.QueryRow(fmt.Sprintf(`SELECT coalesce(max(position), 0) FROM %s WHERE %s = ? AND archived_at IS NULL AND id != ? AND id != ? AND (position < ? OR (position = ? AND id < ?))`, table, scope),
			id, self, before, at, at, before).Scan(&prev)
		if err != nil {
			return 0, err
		}
		if at-prev > 1 {
			return prev + (at-prev)/2, nil
		}
		if err := renumber(tx, table, scope, id, self); err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("positions: no room before %d after renumbering", before)
}

// renumber spreads the scope's active rows (except self) out by gap,
// keeping their order.
func renumber(tx *db.Tx, table, scope string, id, self int64) error {
	_, err := tx.Exec(fmt.Sprintf(`
		UPDATE %[1]s SET position = r.n * %[3]d
		FROM (SELECT id, row_number() OVER (ORDER BY position, id) AS n FROM %[1]s WHERE %[2]s = ? AND archived_at IS NULL AND id != ?) AS r
		WHERE %[1]s.id = r.id`, table, scope, gap), id, self)
	return err
}
