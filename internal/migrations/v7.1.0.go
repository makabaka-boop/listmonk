package migrations

import (
	"log"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/stuffbin"
)

func V7_1_0(db *sqlx.DB, fs stuffbin.FileSystem, ko *koanf.Koanf, lo *log.Logger) error {
	// Optional event identifier on bounce records, used to deduplicate
	// re-delivered webhook notifications.
	if _, err := db.Exec(`ALTER TABLE bounces ADD COLUMN IF NOT EXISTS event_id TEXT NOT NULL DEFAULT '';`); err != nil {
		return err
	}

	// Deduplicate notifications from the same source+event for a subscriber.
	// Legacy/mailbox bounces carry an empty event_id and are excluded.
	if _, err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_bounces_event
			ON bounces (source, event_id, subscriber_id) WHERE event_id <> '';
	`); err != nil {
		return err
	}

	return nil
}
