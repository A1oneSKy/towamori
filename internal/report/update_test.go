package report

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/towamori/agent/protocol"
)

type fakeUpdater struct {
	mu        sync.Mutex
	confirmed int
	applied   []string
	err       error
	done      chan struct{}
}

func (f *fakeUpdater) Confirm() { f.mu.Lock(); f.confirmed++; f.mu.Unlock() }
func (f *fakeUpdater) Apply(_ context.Context, version, base string) error {
	f.mu.Lock()
	f.applied = append(f.applied, version+"@"+base)
	f.mu.Unlock()
	if f.done != nil {
		defer close(f.done)
	}
	return f.err
}

func TestOfferIsAppliedAndFailureReportedOnce(t *testing.T) {
	h := newHarness(t)
	f := &fakeUpdater{err: errors.New("the signature is not valid"), done: make(chan struct{})}
	h.r.Updater = f
	h.r.Agent.SelfUpdate = true
	h.p.reply = protocol.ReportResponse{Update: &protocol.UpdateOffer{Version: "0.3.0", URL: "https://app.example/dl"}}

	h.tick(0)
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the offer was not acted on")
	}
	// Wait for the goroutine to record its failure.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.r.updMu.Lock()
		failed := h.r.updFail != nil
		h.r.updMu.Unlock()
		if failed || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.applied) != 1 || f.applied[0] != "0.3.0@https://app.example/dl" || f.confirmed != 1 {
		t.Fatalf("applied=%v confirmed=%d", f.applied, f.confirmed)
	}
	h.p.reply = protocol.ReportResponse{}
	h.tick(10 * time.Second)
	h.tick(10 * time.Second)
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	var got []*protocol.UpdateStatus
	for _, u := range h.p.updates {
		if u != nil {
			got = append(got, u)
		}
	}
	if len(got) != 1 || got[0].Version != "0.3.0" || got[0].Error != "the signature is not valid" {
		t.Fatalf("reported failures = %+v", got)
	}
}

func TestOfferIgnoredWhenAgentCannotUpdate(t *testing.T) {
	h := newHarness(t)
	f := &fakeUpdater{}
	h.r.Updater = f
	h.r.Agent.SelfUpdate = false
	h.p.reply = protocol.ReportResponse{Update: &protocol.UpdateOffer{Version: "0.3.0", URL: "https://app.example/dl"}}
	h.tick(0)
	time.Sleep(50 * time.Millisecond)
	if len(f.applied) != 0 {
		t.Fatalf("applied %v although updates are off", f.applied)
	}
}

func TestFailureFromBeforeStartIsReportedOnce(t *testing.T) {
	h := newHarness(t)
	h.r.UpdateFailure = &protocol.UpdateStatus{Version: "0.3.0", Error: "did not report in 3 tries"}
	h.tick(0)
	h.tick(10 * time.Second)
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	if len(h.p.updates) < 2 || h.p.updates[0] == nil || h.p.updates[0].Version != "0.3.0" {
		t.Fatalf("updates = %+v", h.p.updates)
	}
	if h.p.updates[1] != nil {
		t.Error("reported twice")
	}
}
