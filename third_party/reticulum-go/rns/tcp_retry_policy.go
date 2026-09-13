package rns

import (
	"math/rand/v2"
	"time"
)

// Backoff policy for ReconnectingTCPClient's supervisor, kept as a pure
// function so the ramp can be asserted without a socket or a listener.
//
// Two ramps:
//
//   - read failure — a usable attachment existed and then its read loop
//     died (NAT idle eviction, middlebox timeout, peer restart). Floor
//     5s, ceiling 60s: the node wants us, something transient broke,
//     come back quickly.
//   - connect failure — we never got a usable attachment: DNS failure,
//     refused, or a peer that accepts and immediately closes. Floor 15s,
//     ceiling 5min: the node is not serving us, so knock gently.
//
// The distinction that matters is USABLE, not ESTABLISHED. A node that
// has denied our address still completes the TCP handshake and only then
// closes — measured against a real denylisted address by the sibling
// mobile client, the socket was accepted in 62ms and shut in under one.
//
// This is what the previous supervisor got wrong, and it got it wrong in
// the worst available direction. Its backoff was re-initialised at the
// top of every disconnect cycle, so the exponential ramp only ever
// applied to consecutive *dial* failures. Against a peer that accepts
// and closes, the dial keeps succeeding, so the ramp never advanced past
// its 1s floor: roughly 86,400 connections a day to a door that was shut,
// from every deployment pointed at that address. That is a plausible way
// to earn a denylisting and a certain way to keep one. On the connect
// ramp the same client settles at ~288 attempts a day.
//
// usableAfter is the line between the two ramps. It sits far above any
// refusal — sub-second by construction, since the peer closes as soon as
// it has decided — and far below any real session, so a misclassification
// in either direction costs one backoff step and nothing else.
type retryPolicy struct {
	// usableAfter is how long an attachment must survive to count as
	// having been usable.
	usableAfter time.Duration

	readFailFloor   time.Duration
	readFailCeiling time.Duration

	connectFailFloor   time.Duration
	connectFailCeiling time.Duration

	// healthyAfter is the lifetime above which an attachment's death is
	// treated as a NAT/middlebox idle timeout rather than flapping, so
	// the read ramp restarts from its floor instead of carrying over
	// whatever it had climbed to.
	healthyAfter time.Duration
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		usableAfter:        3 * time.Second,
		readFailFloor:      5 * time.Second,
		readFailCeiling:    60 * time.Second,
		connectFailFloor:   15 * time.Second,
		connectFailCeiling: 5 * time.Minute,
		healthyAfter:       60 * time.Second,
	}
}

type retryDecision struct {
	wasReadFailure bool

	// delayBase is how long to wait before the next attempt, before
	// jitter is applied.
	delayBase time.Duration

	nextReadFailBackoff    time.Duration
	nextConnectFailBackoff time.Duration
}

// decide plans the next attempt after one failure.
//
// survived is how long the attachment lasted, or nil if it never
// established at all.
func (p retryPolicy) decide(survived *time.Duration, readBackoff, connectBackoff time.Duration) retryDecision {
	if survived == nil || *survived < p.usableAfter {
		// Never usable. Note what is deliberately NOT done here: the
		// connect ramp is not reset. Resetting it on a bare established
		// socket is what pinned a refused client at the floor — the
		// refusing peer reset the ramp on every single attempt, making
		// the ramp worse than useless.
		return retryDecision{
			wasReadFailure:         false,
			delayBase:              connectBackoff,
			nextReadFailBackoff:    readBackoff,
			nextConnectFailBackoff: min(connectBackoff*2, p.connectFailCeiling),
		}
	}

	base := readBackoff
	if *survived >= p.healthyAfter {
		base = p.readFailFloor
	}
	return retryDecision{
		wasReadFailure:      true,
		delayBase:           base,
		nextReadFailBackoff: min(base*2, p.readFailCeiling),
		// A usable attachment proves the connect path works, so the
		// connect ramp is earned back here — where it means something —
		// rather than on a bare socket.
		nextConnectFailBackoff: p.connectFailFloor,
	}
}

// jitter spreads a delay by ±25% so that many clients knocked off the
// same peer at the same moment do not return in lockstep and flood it on
// recovery.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return time.Duration(float64(d) * (0.75 + rand.Float64()*0.5))
}
