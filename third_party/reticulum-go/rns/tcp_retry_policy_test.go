package rns

import (
	"testing"
	"time"
)

// fastRetryPolicy is defaultRetryPolicy's shape with the ramps scaled
// into milliseconds, so reconnect tests exercise the real ramp without
// waiting minutes for it.
//
// usableAfter is NOT scaled down proportionally, and that is deliberate.
// It measures the gap between a refusal and a real session, and in
// production it has three orders of magnitude to work with: a refusal is
// shut in under a second, the threshold sits at 3s. In a test the
// measured lifetime also carries however long the runner took to
// schedule the pump goroutine and notice the close — tens of ms on a
// loaded machine. At 30ms that noise was larger than the signal and a
// refusal intermittently classified as a live session; 500ms restores
// the margin while keeping the ramps fast.
func fastRetryPolicy() retryPolicy {
	return retryPolicy{
		usableAfter:        500 * time.Millisecond,
		readFailFloor:      5 * time.Millisecond,
		readFailCeiling:    60 * time.Millisecond,
		connectFailFloor:   20 * time.Millisecond,
		connectFailCeiling: 500 * time.Millisecond,
		healthyAfter:       1 * time.Second,
	}
}

func dur(d time.Duration) *time.Duration { return &d }

// The case that drove this policy into its own file: rns.michmesh.net
// had denylisted an operator's address, which presents as a completed
// TCP handshake followed immediately by a close. Classifying that as a
// dropped session gave it the fast ramp — and because the old supervisor
// also re-initialised its backoff every cycle, "fast" meant a redial per
// second, indefinitely.

func TestRetryPolicyNeverConnectedIsAConnectFailure(t *testing.T) {
	p := defaultRetryPolicy()
	d := p.decide(nil, p.readFailFloor, p.connectFailFloor)
	if d.wasReadFailure {
		t.Fatal("a dial that never landed must not be a read failure")
	}
	if d.delayBase != p.connectFailFloor {
		t.Fatalf("delayBase = %v, want %v", d.delayBase, p.connectFailFloor)
	}
}

// The denylist signature: accepted, then closed in well under a second.
func TestRetryPolicyAcceptedThenClosedIsARefusal(t *testing.T) {
	p := defaultRetryPolicy()
	d := p.decide(dur(40*time.Millisecond), p.readFailFloor, p.connectFailFloor)
	if d.wasReadFailure {
		t.Fatal("accepted-then-closed must be a refusal, not a read failure")
	}
	if d.delayBase != p.connectFailFloor {
		t.Fatalf("delayBase = %v, want the connect floor %v", d.delayBase, p.connectFailFloor)
	}
	if d.nextReadFailBackoff != p.readFailFloor {
		t.Fatalf("a refusal must not disturb the read ramp: got %v", d.nextReadFailBackoff)
	}
}

// The regression that made the naive fix worse than the bug: reset the
// connect ramp on a bare established socket and a refusing peer resets it
// on every attempt, pinning it at the floor. A refusal must leave the
// ramp climbing.
func TestRetryPolicyRefusalNeverResetsTheConnectRamp(t *testing.T) {
	p := defaultRetryPolicy()
	connect := p.connectFailFloor
	var waits []time.Duration
	for range 8 {
		d := p.decide(dur(40*time.Millisecond), p.readFailFloor, connect)
		waits = append(waits, d.delayBase)
		connect = d.nextConnectFailBackoff
	}
	want := []time.Duration{
		15 * time.Second, 30 * time.Second, 1 * time.Minute, 2 * time.Minute,
		4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("wait[%d] = %v, want %v (full ramp %v)", i, waits[i], want[i], waits)
		}
	}
}

func TestRetryPolicyRealSessionTakesTheReadRamp(t *testing.T) {
	p := defaultRetryPolicy()
	d := p.decide(dur(30*time.Second), p.readFailFloor, p.connectFailFloor)
	if !d.wasReadFailure {
		t.Fatal("a session that lived 30s then died is a read failure")
	}
	if d.delayBase != p.readFailFloor {
		t.Fatalf("delayBase = %v, want %v", d.delayBase, p.readFailFloor)
	}
	if d.nextReadFailBackoff != 10*time.Second {
		t.Fatalf("nextReadFailBackoff = %v, want 10s", d.nextReadFailBackoff)
	}
}

func TestRetryPolicyRampsAreCapped(t *testing.T) {
	p := defaultRetryPolicy()
	if got := p.decide(nil, p.readFailFloor, p.connectFailCeiling).nextConnectFailBackoff; got != p.connectFailCeiling {
		t.Fatalf("connect ramp uncapped: %v", got)
	}
	if got := p.decide(dur(30*time.Second), p.readFailCeiling, p.connectFailFloor).nextReadFailBackoff; got != p.readFailCeiling {
		t.Fatalf("read ramp uncapped: %v", got)
	}
}

// A long-lived attachment dying is a NAT idle timeout — come back fast.
func TestRetryPolicyHealthySessionRestartsTheReadRampAtTheFloor(t *testing.T) {
	p := defaultRetryPolicy()
	d := p.decide(dur(90*time.Second), p.readFailCeiling, p.connectFailFloor)
	if !d.wasReadFailure || d.delayBase != p.readFailFloor {
		t.Fatalf("healthy session should restart at the floor: %+v", d)
	}
}

// Having been usable proves the connect path works; earn the ramp back.
func TestRetryPolicyReadFailureResetsTheConnectRamp(t *testing.T) {
	p := defaultRetryPolicy()
	d := p.decide(dur(30*time.Second), p.readFailFloor, p.connectFailCeiling)
	if d.nextConnectFailBackoff != p.connectFailFloor {
		t.Fatalf("nextConnectFailBackoff = %v, want %v", d.nextConnectFailBackoff, p.connectFailFloor)
	}
}

func TestRetryPolicyUsableThresholdIsTheBoundary(t *testing.T) {
	p := defaultRetryPolicy()
	if p.decide(dur(p.usableAfter-1), p.readFailFloor, p.connectFailFloor).wasReadFailure {
		t.Fatal("just under the threshold must be a refusal")
	}
	if !p.decide(dur(p.usableAfter), p.readFailFloor, p.connectFailFloor).wasReadFailure {
		t.Fatal("at the threshold must be a read failure")
	}
}

// The point of the whole change, stated as a budget: a full day against a
// peer that refuses us must cost hundreds of attempts, not tens of
// thousands. The old supervisor's ramp reset every cycle, so a refusal
// held it at a 1s redial — the third figure below.
func TestRetryPolicyADayOfRefusalsStaysUnderThreeHundredAttempts(t *testing.T) {
	p := defaultRetryPolicy()
	const day = 24 * time.Hour

	connect := p.connectFailFloor
	var elapsed time.Duration
	attempts := 0
	for elapsed < day {
		d := p.decide(dur(40*time.Millisecond), p.readFailFloor, connect)
		elapsed += d.delayBase
		connect = d.nextConnectFailBackoff
		attempts++
	}
	if attempts >= 300 {
		t.Fatalf("%d attempts/day against a refusing peer, want < 300", attempts)
	}
	if old := day / time.Second; old < 80_000 {
		t.Fatalf("sanity: the old 1s redial was %d/day", old)
	}
}

func TestJitterStaysWithinTwentyFivePercent(t *testing.T) {
	const base = time.Second
	for range 500 {
		got := jitter(base)
		if got < 750*time.Millisecond || got > 1250*time.Millisecond {
			t.Fatalf("jitter(%v) = %v, outside ±25%%", base, got)
		}
	}
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %v, want 0", got)
	}
}
