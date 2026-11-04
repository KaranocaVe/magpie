package gateway

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// addSealedRelayAlias adds a second provider entry for the same Responses
// relay, optionally changing its credential and declaring an explicit seal group.
func addSealedRelayAlias(t *testing.T, from, to, key, sealGroup string, allowCrossProvider, sourceOfficial, aliasOfficial bool) {
	t.Helper()
	source, err := provider.Find(from)
	if err != nil {
		t.Fatal(err)
	}
	source.SealGroup = sealGroup
	source.AllowCrossProviderSealedTasks = allowCrossProvider
	source.RoutesToOfficialCodexBackend = sourceOfficial
	if err := provider.Save(*source); err != nil {
		t.Fatal(err)
	}
	source, err = provider.Find(from)
	if err != nil {
		t.Fatal(err)
	}
	alias := *source
	alias.ID = to
	alias.Name = to
	alias.Key = key
	alias.SealGroup = sealGroup
	alias.AllowCrossProviderSealedTasks = allowCrossProvider
	alias.RoutesToOfficialCodexBackend = aliasOfficial
	if err := provider.Save(alias); err != nil {
		t.Fatal(err)
	}
}

// Cross-provider routing is allowed when both provider entries explicitly opt in.
func TestSealedTaskAcceptedAcrossOptedInResponsesProviders(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	source, err := provider.Find("s2a")
	if err != nil {
		t.Fatal(err)
	}
	addSealedRelayAlias(t, "s2a", "s2b", source.Key, "", true, false, false)

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
	addSealedRelayAlias(t, "s2a", "s2b", "sk-different-tenant", "", false, false, false)

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

// Identical endpoints and credentials are not treated as proof by themselves.
func TestSealedTaskRefusedAcrossEquivalentResponsesProvidersWithoutOptIn(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	source, err := provider.Find("s2a")
	if err != nil {
		t.Fatal(err)
	}
	addSealedRelayAlias(t, "s2a", "s2b", source.Key, "", false, false, false)

	s := New()
	if rec := codexSubTurn(s, "s2a/gpt-6-astra", "lead-1", "", leadAsk); rec.Code != 200 {
		t.Fatalf("lead: %d %s", rec.Code, rec.Body.String())
	}
	rec := codexSubTurn(s, "s2b/gpt-6-astra", "worker-1", "lead-1", sealedHandoff)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "(s2a)") {
		t.Fatalf("subagent should require explicit opt-in: %d %s", rec.Code, rec.Body.String())
	}
	if got := relay.requests(); len(got) != 1 {
		t.Fatalf("shared relay was asked %d times; only the lead should have reached it", len(got))
	}
}

// The opt-in is an explicit assertion that the different credentials still
// reach a backend capable of opening the same sealed task.
func TestSealedTaskAcceptedAcrossOptedInResponsesProvidersWithDifferentCredentials(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	addSealedRelayAlias(t, "s2a", "s2b", "sk-different-tenant", "", true, false, false)

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
		t.Fatalf("the opted-in relay received %d requests; subagent task was not passed intact: %v", len(got), got)
	}
}

// Two shallow providers can exchange sealed tasks when both explicitly declare
// that their upstream chain ends at the official OpenAI Codex backend.
func TestSealedTaskAcceptedAcrossOfficialCodexBackendProviders(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	addSealedRelayAlias(t, "s2a", "s2b", "sk-different-tenant", "", false, true, true)

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
		t.Fatalf("official-backend relay received %d requests; subagent task was not passed intact: %v", len(got), got)
	}
}

// A single provider's declaration is not enough: both sides must terminate at
// the official Codex backend before the marker enables cross-provider handoff.
func TestSealedTaskRefusedWhenOnlyOneProviderRoutesToOfficialCodexBackend(t *testing.T) {
	fresh(t)
	relay := relayOn(t, "s2a")
	source, err := provider.Find("s2a")
	if err != nil {
		t.Fatal(err)
	}
	addSealedRelayAlias(t, "s2a", "s2b", source.Key, "", false, true, false)

	s := New()
	if rec := codexSubTurn(s, "s2a/gpt-6-astra", "lead-1", "", leadAsk); rec.Code != 200 {
		t.Fatalf("lead: %d %s", rec.Code, rec.Body.String())
	}
	rec := codexSubTurn(s, "s2b/gpt-6-astra", "worker-1", "lead-1", sealedHandoff)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "(s2a)") {
		t.Fatalf("subagent should be refused unless both providers are marked: %d %s", rec.Code, rec.Body.String())
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
	addSealedRelayAlias(t, "s2a", "s2b", "sk-different-tenant", "shared-relay", false, false, false)

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
