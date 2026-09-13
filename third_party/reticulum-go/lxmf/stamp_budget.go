package lxmf

import (
	"encoding/hex"
	"sync"
	"time"
)

// Per-sender budget for the work an inbound stamp can make us do.
//
// THE EXPOSURE. Validating a §5.7.2 stamp costs a 768 KiB workblock and
// 3000 HKDF rounds, and it runs INLINE on the transport's per-interface
// dispatcher goroutine (rns.LocalDestination.OnPacket is documented as
// dispatcher-called). So it is not merely attacker-triggered work — it
// is attacker-triggered work that stalls all inbound traffic on that
// interface while it runs.
//
// A missing or wrong-length stamp costs nothing: ValidateStamp returns
// before building the workblock. The expensive case is a stamp of the
// RIGHT length that fails — 32 arbitrary bytes. That is the flood to
// bound, and it is free for the sender to produce.
//
// MaxConcurrentStampValidations does not bound it. That cap limits
// CONCURRENCY, and with one dispatcher per interface a single-interface
// node never reaches it; the cost accumulates serially instead, which
// the cap cannot see. It also sheds fail-OPEN, so on the rare occasion
// it does engage it becomes a second way past EnforceStamps.
//
// WHAT THIS DOES. Each sender gets an allowance of FAILED validations.
// Successes are never charged, so a legitimate peer — whose stamps
// clear, however chatty they are — is never throttled and never
// dropped. A flooder's stamps do not clear by construction (not doing
// the work is the entire point), so they exhaust their allowance in
// StampFailureBurstPerSender messages and every message after that
// costs us nothing: we refuse before building the workblock.
//
// Because exhaustion is ATTRIBUTABLE — the sender spent their own
// allowance and cannot touch anyone else's — failing closed on it is
// safe in a way the global cap's shedding is not. Under EnforceStamps
// an out-of-allowance message is dropped, which is what the operator
// asked for; without it, delivered unvalidated as before.
const (
	// StampFailureBurstPerSender is how many failed validations one
	// sender may cost us before we stop paying. Four is enough slack
	// for a peer with a genuinely stale ticket or a clock skew issue
	// to recover without being cut off.
	StampFailureBurstPerSender = 4

	// StampFailureRefillInterval restores one unit of allowance. A
	// flooder is therefore worth at most one workblock per 30s
	// sustained, down from one per message.
	StampFailureRefillInterval = 30 * time.Second

	// MaxTrackedStampFailureSenders bounds the table. Only senders
	// currently IN debt are tracked — a bucket is dropped once it
	// refills — so this is exceeded only under a many-identity flood.
	MaxTrackedStampFailureSenders = 4096
)

type stampFailureBucket struct {
	spent      int       // allowance consumed, 0..StampFailureBurstPerSender
	lastRefill time.Time // when spent was last decremented
}

// stampBudget tracks per-sender failed-validation allowance.
//
// Held by POINTER on Delivery, allocated in NewDelivery. It carries a
// mutex, and embedding it by value would make Delivery non-copyable —
// which go vet flags and which would be an API break for anyone holding
// Delivery values. Every method tolerates a nil receiver, so a Delivery
// built without the constructor simply has no budget.
type stampBudget struct {
	mu      sync.Mutex
	senders map[string]*stampFailureBucket
}

// allow reports whether we are willing to spend a workblock on this
// sender right now. It does not charge — only a validation that
// actually fails does, via charge.
func (b *stampBudget) allow(sourceHash []byte, now time.Time) bool {
	if b == nil {
		return true
	}
	key := hex.EncodeToString(sourceHash)
	b.mu.Lock()
	defer b.mu.Unlock()
	bucket, ok := b.senders[key]
	if !ok {
		return true // never failed, or already refilled: full allowance
	}
	refill(bucket, now)
	if bucket.spent <= 0 {
		delete(b.senders, key)
		return true
	}
	return bucket.spent < StampFailureBurstPerSender
}

// charge records one failed validation against this sender.
func (b *stampBudget) charge(sourceHash []byte, now time.Time) {
	if b == nil {
		return
	}
	key := hex.EncodeToString(sourceHash)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.senders == nil {
		b.senders = map[string]*stampFailureBucket{}
	}
	bucket, ok := b.senders[key]
	if !ok {
		if len(b.senders) >= MaxTrackedStampFailureSenders {
			b.sweepLocked(now)
			// Table still full: stop admitting new debtors rather than
			// grow without bound, and rather than evict an existing
			// one — eviction would let a many-identity flood clear the
			// debt of whichever sender it targeted.
			if len(b.senders) >= MaxTrackedStampFailureSenders {
				return
			}
		}
		bucket = &stampFailureBucket{lastRefill: now}
		b.senders[key] = bucket
	}
	refill(bucket, now)
	if bucket.spent < StampFailureBurstPerSender {
		bucket.spent++
	}
}

// refill restores allowance for elapsed whole intervals. It advances
// lastRefill by exactly the credit granted, so partial time carries
// over instead of being discarded on every charge.
func refill(bucket *stampFailureBucket, now time.Time) {
	elapsed := now.Sub(bucket.lastRefill)
	if elapsed < StampFailureRefillInterval {
		return
	}
	restored := int(elapsed / StampFailureRefillInterval)
	bucket.spent -= restored
	bucket.lastRefill = bucket.lastRefill.Add(time.Duration(restored) * StampFailureRefillInterval)
	if bucket.spent < 0 {
		bucket.spent = 0
	}
}

// sweepLocked drops every bucket that has refilled out of debt, so the
// table holds only senders currently being throttled.
func (b *stampBudget) sweepLocked(now time.Time) {
	for k, bucket := range b.senders {
		refill(bucket, now)
		if bucket.spent <= 0 {
			delete(b.senders, k)
		}
	}
}

// trackedSenders reports how many senders are currently in debt, for
// tests and observability.
func (b *stampBudget) trackedSenders() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.senders)
}
