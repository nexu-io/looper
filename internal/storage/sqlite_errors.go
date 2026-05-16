package storage

import (
	"errors"
	"strings"

	"github.com/mattn/go-sqlite3"
)

const queueItemsActiveDedupeIndexName = "idx_queue_items_one_active_dedupe"

func isQueueActiveDedupeConstraintError(err error) bool {
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	if sqliteErr.ExtendedCode != sqlite3.ErrConstraintUnique {
		return false
	}
	message := sqliteErr.Error()
	return strings.Contains(message, queueItemsActiveDedupeIndexName) || strings.Contains(message, "queue_items.dedupe_key")
}
