package gateway

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// addSealedRelayAlias adds a second provider entry for the same Responses
// relay, optionally changing its credential and declaring an explicit seal group.
func addSealedRelayAlias(t *testing.T, from, to, key, sealGroup string) {
	t.Helper()
	source, err := provider.Find(from)
	if err != nil {
		t.Fatal(err)
	}
	if sealGroup != "" {
		source.SealGroup = sealGroup
		if err := provider.Save(*source); err != nil {
			t.Fatal(err)
		}
		source, err = provider.Find(from)
		if err != nil {
			t.Fatal(err)
		}
	}
	alias := *source
	alias.ID = to
	alias.Name = to
	alias.Key = key
	alias.SealGroup = sealGroup
	if err := provider.Save(alias); err != nil {
		t.Fatal(err)
	}
}

// Distinct provider IDs do not prevent a sealed task from reaching the same
// Responses backend when the endpoint, credential and request routing match.
func TestSealedTaskAcceptedAcrossEquivalentResponsesProviders(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	source, err := provider.Find("s2a")
	if err != nil {
		t.Fatal(err)
	}
	addSealedRelayAlias(t, "s2a", "s2b", source.Key, "")

	s := New()
	if rec := codexSubTurn(s, "s2a/gpt-6-astra", "lead-1", "", leadAsk); rec.Code != 200 {
		t.Fatalf("lead: %d %s", rec.Code, rec.Body.String())
	}
	rec := codexSubTurn(s, "s2b/gpt-6-astra", "worker-1", "lead-1", sealedHandoff)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "on it") {
		t.Fatalf("subagent: %d %s", rec.Code, rec.Body.String())
	}
	got := relay.requests()
	if len(got) != 2 || !strings.Contains(got[1], `"encrypted_content":"gAAAAATest_ciphertext=="`) {
		t.Fatalf("the shared relay received %d requests; subagent task was not passed intact: %v", len(got), got)
	}
}

// A shared URL alone is not enough: a different credential may select another
// tenant or sealing backend, so the task must be refused before forwarding.
func TestSealedTaskRefusedAcrossSameEndpointDifferentCredentials(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	addSealedRelayAlias(t, "s2a", "s2b", "sk-different-tenant", "")

	s := New()
	if rec := codexSubTurn(s, "s2a/gpt-6-astra", "lead-1", "", leadAsk); rec.Code != 200 {
		t.Fatalf("lead: %d %s", rec.Code, rec.Body.String())
	}
	rec := codexSubTurn(s, "s2b/gpt-6-astra", "worker-1", "lead-1", sealedHandoff)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "(s2a)") || strings.Contains(rec.Body.String(), "gAAAAA") {
		t.Fatalf("subagent should be refused without exposing ciphertext: %d %s", rec.Code, rec.Body.String())
	}
	if got := relay.requests(); len(got) != 1 {
		t.Fatalf("shared relay was asked %d times; only the lead should have reached it", len(got))
	}
}

// Different credentials can be declared compatible explicitly when the
// operator knows both provider entries reach the same seal/decryption backend.
func TestSealedTaskAcceptedAcrossExplicitSealGroup(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	addSealedRelayAlias(t, "s2a", "s2b", "sk-different-tenant", "shared-relay")

	s := New()
	if rec := codexSubTurn(s, "s2a/gpt-6-astra", "lead-1", "", leadAsk); rec.Code != 200 {
		t.Fatalf("lead: %d %s", rec.Code, rec.Body.String())
	}
	rec := codexSubTurn(s, "s2b/gpt-6-astra", "worker-1", "lead-1", sealedHandoff)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "on it") {
		t.Fatalf("subagent: %d %s", rec.Code, rec.Body.String())
	}
	got := relay.requests()
	if len(got) != 2 || !strings.Contains(got[1], `"encrypted_content":"gAAAAATest_ciphertext=="`) {
		t.Fatalf("the explicitly shared relay received %d requests; subagent task was not passed intact: %v", len(got), got)
	}
}
