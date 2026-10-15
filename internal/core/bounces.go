package core

import (
	"net/http"
	"strings"

	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
)

var bounceQuerySortFields = []string{"email", "campaign_name", "source", "created_at", "type"}

// QueryBounces retrieves paginated bounce entries based on the given params.
// It also returns the total number of bounce records in the DB.
func (c *Core) QueryBounces(campID, subID int, source, typ, orderBy, order string, offset, limit int) ([]models.Bounce, int, error) {
	if !strSliceContains(orderBy, bounceQuerySortFields) {
		orderBy = "created_at"
	}
	if order != SortAsc && order != SortDesc {
		order = SortDesc
	}

	out := []models.Bounce{}
	stmt := strings.ReplaceAll(c.q.QueryBounces, "%order%", orderBy+" "+order)
	if err := c.db.Select(&out, stmt, 0, campID, subID, source, typ, offset, limit); err != nil {
		c.log.Printf("error fetching bounces: %v", err)
		return nil, 0, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorFetching", "name", "{globals.terms.bounce}", "error", pqErrMsg(err)))
	}

	total := 0
	if len(out) > 0 {
		total = out[0].Total
	}

	return out, total, nil
}

// GetBounce retrieves bounce entries based on the given params.
func (c *Core) GetBounce(id int) (models.Bounce, error) {
	var out []models.Bounce
	stmt := strings.ReplaceAll(c.q.QueryBounces, "%order%", "id "+SortAsc)
	if err := c.db.Select(&out, stmt, id, 0, 0, "", "", 0, 1); err != nil {
		c.log.Printf("error fetching bounces: %v", err)
		return models.Bounce{}, echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorFetching", "name", "{globals.terms.bounce}", "error", pqErrMsg(err)))
	}

	if len(out) == 0 {
		return models.Bounce{}, echo.NewHTTPError(http.StatusBadRequest,
			c.i18n.Ts("globals.messages.notFound", "name", "{globals.terms.bounce}"))

	}

	return out[0], nil
}

// recordBounceResult is the row returned by the `record-bounce` query.
type recordBounceResult struct {
	SubscriberID int  `db:"subscriber_id"`
	Num          int  `db:"num"`
	Inserted     bool `db:"inserted"`
}

// RecordBounce records a new bounce. Notifications carrying an event_id that
// have already been recorded for the same source and subscriber are skipped
// (no new row, no bounce action), making provider webhook re-deliveries
// idempotent for as long as the original bounce record is retained.
func (c *Core) RecordBounce(b models.Bounce) error {
	action, ok := c.consts.BounceActions[b.Type]
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, c.i18n.Ts("globals.messages.invalidData")+": "+b.Type)
	}

	// The insert and the follow-up action must be atomic so that a duplicate
	// that loses the insert race (ON CONFLICT) never runs the action.
	tx, err := c.db.Beginx()
	if err != nil {
		c.log.Printf("error beginning bounce transaction: %v", err)
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var res recordBounceResult
	if err := tx.Stmtx(c.q.RecordBounce).QueryRowx(
		b.SubscriberUUID,
		b.Email,
		b.CampaignUUID,
		b.Type,
		b.Source,
		b.Meta,
		b.CreatedAt,
		action.Count,
		strings.TrimSpace(b.EventID),
	).StructScan(&res); err != nil {
		c.log.Printf("error recording bounce: %v", err)
		return err
	}

	if res.SubscriberID == 0 {
		// Subscriber does not exist. Nothing was recorded.
		c.log.Printf("bounced subscriber (%s / %s) not found", b.SubscriberUUID, b.Email)
		return nil
	}

	// Duplicate re-delivery, an already blocklisted subscriber, or a bounce
	// beyond the configured threshold: nothing new was recorded.
	if !res.Inserted || res.Num < action.Count {
		return tx.Commit()
	}

	// Apply the configured action exactly once, alongside the insert.
	switch action.Action {
	case "blocklist":
		_, err = tx.Stmtx(c.q.BlocklistBouncedSubscriber).Exec(res.SubscriberID)
	case "unsubscribe":
		_, err = tx.Stmtx(c.q.UnsubscribeBouncedSubscriber).Exec(res.SubscriberID)
	case "delete":
		_, err = tx.Stmtx(c.q.DeleteBouncedSubscriber).Exec(res.SubscriberID)
	default:
		// "none" or an unknown action. Nothing to do.
	}
	if err != nil {
		c.log.Printf("error applying bounce action '%s': %v", action.Action, err)
		return err
	}

	return tx.Commit()
}

// BlocklistBouncedSubscribers blocklists subscribers matching bounce IDs or filters.
func (c *Core) BlocklistBouncedSubscribers(ids []int, campID int, source, typ string) error {
	if _, err := c.q.BlocklistBouncedSubscribers.Exec(pq.Array(ids), campID, source, typ); err != nil {
		c.log.Printf("error blocklisting bounced subscribers: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, c.i18n.Ts("subscribers.errorBlocklisting", "error", err.Error()))
	}

	return nil
}

// DeleteBounce deletes a bounce record.
func (c *Core) DeleteBounce(id int) error {
	return c.DeleteBounces([]int{id}, false, 0, "", "")
}

// DeleteBounces deletes bounce records by IDs or filters.
func (c *Core) DeleteBounces(ids []int, all bool, campID int, source, typ string) error {
	if _, err := c.q.DeleteBounces.Exec(pq.Array(ids), all, campID, source, typ); err != nil {
		c.log.Printf("error deleting lists: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			c.i18n.Ts("globals.messages.errorDeleting", "name", "{globals.terms.list}", "error", pqErrMsg(err)))
	}
	return nil
}
