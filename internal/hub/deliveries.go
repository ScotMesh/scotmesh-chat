package hub

import (
	"context"
	"time"

	"github.com/ScotMesh/scotmesh-chat/internal/store"
)

// DeliveryOutcome is what an LXMF adapter reports after an attempt.
type DeliveryOutcome int

// Outcomes of one attempt.
const (
	// Delivered: the recipient's proof arrived, or the propagation node took it.
	Delivered DeliveryOutcome = iota
	// RetryAt: not yet; try again at the given time.
	RetryAt
	// GaveUp: final failure; the member can still read it with /history.
	GaveUp
)

// DueDeliveries returns LXMF deliveries whose time has come, oldest message
// first, with the message each one carries. It reads a snapshot and does not
// go through the hub loop.
func (h *Hub) DueDeliveries(ctx context.Context, limit int) ([]PendingDelivery, error) {
	var out []PendingDelivery
	err := h.st.View(ctx, func(tx *store.Tx) error {
		due, err := tx.DueDeliveries(h.nowMS(), limit)
		if err != nil {
			return err
		}
		members, err := tx.Members()
		if err != nil {
			return err
		}
		away := map[string]int64{}
		for _, m := range members {
			away[hexID(m.Identity)] = m.AwayUntil
		}
		for _, d := range due {
			pd := PendingDelivery{Delivery: d, AwayUntil: away[hexID(d.Identity)]}
			if d.Whisper {
				w, ok, err := tx.Whisper(d.MessageID)
				if err != nil {
					return err
				}
				if !ok {
					continue // pruned; the row goes with it
				}
				pd.Whisper = &w
			} else {
				m, ok, err := tx.Message(d.MessageID)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				pd.Message = m
			}
			out = append(out, pd)
		}
		return nil
	})
	return out, err
}

// PendingDelivery is one message, or one whisper, waiting to reach one
// member.
type PendingDelivery struct {
	store.Delivery
	Message   store.Message
	Whisper   *store.Whisper // set for a whisper; Message is empty then
	AwayUntil int64          // the member's direct delivery is failing until then
}

// Text is what the member's LXMF app shows.
func (d *PendingDelivery) Text() string {
	if d.Whisper != nil {
		return WhisperText(d.Whisper)
	}
	return LXMFLine(&d.Message)
}

// SaidAt is when it was said, for giving up on old deliveries.
func (d *PendingDelivery) SaidAt() int64 {
	if d.Whisper != nil {
		return d.Whisper.SaidAt
	}
	return d.Message.HubAt
}

// LXMFLine is how a message reads in the LXMF group: "Name: message", or
// "* Name waves" for an action.
func LXMFLine(m *store.Message) string {
	if m.Kind == store.KindAction {
		return "* " + m.AuthorName + " " + m.Body
	}
	return m.AuthorName + ": " + m.Body
}

// RecordDelivery stores the outcome of an attempt. propagated says the
// attempt went to the propagation node; awayUntil, when non-zero, marks the
// member as unreachable directly until then.
func (h *Hub) RecordDelivery(ctx context.Context, d store.Delivery, outcome DeliveryOutcome, propagated bool, retryAt time.Time, awayUntil time.Time) error {
	return h.update(ctx, func(tx *store.Tx, _ *outbox) error {
		now := h.nowMS()
		d.UpdatedAt = now
		switch outcome {
		case Delivered:
			d.State = store.DeliveryProven
			if propagated {
				d.State = store.DeliveryPropagated
			}
		case RetryAt:
			d.State = store.DeliveryQueued
			d.NextAt = retryAt.UnixMilli()
		case GaveUp:
			d.State = store.DeliveryFailed
		}
		if err := tx.PutDelivery(&d); err != nil {
			return err
		}
		if !awayUntil.IsZero() {
			return tx.SetAway(d.Identity, awayUntil.UnixMilli())
		}
		if outcome == Delivered && !propagated {
			return tx.SetAway(d.Identity, 0)
		}
		return nil
	})
}
