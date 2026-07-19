package main

import (
	"testing"
	"time"
)

func TestChainRouteStateSkipsCoolingRoutesThenRetriesAfterCooldown(t *testing.T) {
	state := newChainRouteState()
	routes := []APIRoute{{ID: 1, Name: "first"}, {ID: 2, Name: "second"}, {ID: 3, Name: "third"}}

	state.MarkFailure(10, "session-a", 1)
	state.MarkSuccess(10, "session-a", 2)

	got := state.OrderedRoutes(10, "session-a", routes)
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("cooling route should be skipped and last success tried first, got %#v", got)
	}

	key := chainSessionKey(10, "session-a")
	state.mu.Lock()
	state.sessions[key].FailedUntil[1] = time.Now().Add(-time.Second)
	state.mu.Unlock()

	got = state.OrderedRoutes(10, "session-a", routes)
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Fatalf("expired route should be retried before last success, got %#v", got)
	}
}

func TestChainRouteStateClearFailuresStartsFromFirstRoute(t *testing.T) {
	state := newChainRouteState()
	routes := []APIRoute{{ID: 1, Name: "first"}, {ID: 2, Name: "second"}, {ID: 3, Name: "third"}}

	state.MarkFailure(10, "session-a", 1)
	state.MarkFailure(10, "session-a", 2)
	state.MarkFailure(10, "session-a", 3)
	state.ClearFailures(10, "session-a")

	got := state.OrderedRoutes(10, "session-a", routes)
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Fatalf("cleared failures should restart from first route, got %#v", got)
	}
}
