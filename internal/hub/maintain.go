package hub

import (
	"context"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// maintain runs on the loop every minute: cursors for people present, and
// retention. It never fails the hub; problems are logged.
func (h *Hub) maintain(ctx context.Context) {
	h.saveAllCursors(ctx)
	now := h.now()
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		pruned, err := tx.PruneMessages(now.Add(-h.cfg.Retention).UnixMilli())
		if err != nil {
			return err
		}
		if pruned > 0 {
			h.log.Info("pruned old messages", "count", pruned, "retention", h.cfg.Retention.String())
		}
		if _, err := tx.PruneInvites(now.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.FinishDeliveries(now.Add(-24 * time.Hour).UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.PruneLinkCodes(now.UnixMilli()); err != nil {
			return err
		}
		if err := h.liftExpiredBans(tx, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.PruneWhispers(now.Add(-WhisperRetention).UnixMilli()); err != nil {
			return err
		}
		return h.pruneRooms(tx, now)
	})
	if err != nil && ctx.Err() == nil {
		h.log.Error("maintenance", "err", err)
	}
	for k, b := range h.rate {
		if now.Sub(b.at) > time.Minute {
			delete(h.rate, k)
		}
	}
	for k, at := range h.historyAt {
		if now.Sub(at) > h.cfg.HistoryCooldown {
			delete(h.historyAt, k)
		}
	}
	for k, times := range h.linkIssued {
		if len(takeRecent(times, now, time.Hour)) == 0 {
			delete(h.linkIssued, k)
		}
	}
	for k, times := range h.linkFailures {
		if len(takeRecent(times, now, linkLockout)) == 0 {
			delete(h.linkFailures, k)
		}
	}
}

// pruneRooms forgets registered rooms nobody has used for PruneRoomsAfter.
// Configured rooms are never pruned.
func (h *Hub) pruneRooms(tx *store.Tx, now time.Time) error {
	rooms, err := tx.Rooms()
	if err != nil {
		return err
	}
	cutoff := now.Add(-h.cfg.PruneRoomsAfter).UnixMilli()
	for _, r := range rooms {
		if h.cfg.isDefaultRoom(r.Name) || len(h.presence[r.Name]) > 0 || r.LastUsed >= cutoff {
			continue
		}
		if err := tx.DeleteRoom(r.Name); err != nil {
			return err
		}
		if err := tx.Audit(now.UnixMilli(), nil, "prune-room", r.Name, "unused"); err != nil {
			return err
		}
	}
	return nil
}
