package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestPinnedSessionRouteResolvesGroupMemberAndDropsFallback(t *testing.T) {
	fresh(t)
	one := provider.Provider{ID: "pin-one", Name: "One", Key: "k", Models: []string{"m"}, Chat: "https://one.example/v1", Fallback: []string{"pin-two/m"}}
	two := provider.Provider{ID: "pin-two", Name: "Two", Key: "k", Models: []string{"m"}, Chat: "https://two.example/v1"}
	for _, p := range []provider.Provider{one, two} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "session-pin-test", Name: "Session pin test", Members: []string{"pin-one/m", "pin-two/m"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	p, model, ok := provider.Resolve("group/session-pin-test")
	if !ok {
		t.Fatal("group did not resolve")
	}
	got, m, asked, err := pinnedSessionRoute("group/session-pin-test", p, model, "pin-two")
	if err != nil || got.ID != "pin-two" || m != "m" || asked != "pin-two/m" || len(got.Fallback) != 0 {
		t.Fatalf("group pin: provider=%q model=%q asked=%q err=%v", got.ID, m, asked, err)
	}
	got, m, asked, err = pinnedSessionRoute("pin-one/m", p, "m", "pin-one")
	if err != nil || got.ID != "pin-one" || m != "m" || asked != "pin-one/m" || len(got.Fallback) != 0 {
		t.Fatalf("direct pin: provider=%q model=%q asked=%q err=%v", got.ID, m, asked, err)
	}
	if _, _, _, err := pinnedSessionRoute("pin-one/missing", p, "missing", "pin-two"); err == nil {
		t.Fatal("missing destination model silently fell back")
	}
	if _, _, _, err := pinnedSessionRoute("group/session-pin-test", p, model, "missing"); err == nil {
		t.Fatal("missing destination provider silently fell back")
	}
}

func TestCodexSessionPinNeverFallsBackAndAutoSessionStillDoes(t *testing.T) {
	plan := &fake{t: t, ctype: "application/json", code: 429, reply: `{"error":{"message":"rate limited"}}`}
	spare := &fake{t: t, ctype: "application/json", reply: `{"id":"from-spare","choices":[]}`}
	twoProviders(t, plan, spare)
	t.Cleanup(noRests)
	p := "plan"
	if err := provider.SetSessionProviderFor("codex", "pinned-codex", p); err != nil {
		t.Fatal(err)
	}
	s := New()
	send := func(session string) (int, string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq))
		req.Header.Set("User-Agent", "codex_cli_rs/0.162.0")
		req.Header.Set(SessionHeader, session)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	code, body := send("pinned-codex")
	if code == 200 || spare.calls != 0 {
		t.Fatalf("pinned session must fail without using spare: status %d body %s spare calls %d", code, body, spare.calls)
	}
	code, body = send("different-codex")
	if code != 200 || !strings.Contains(body, "from-spare") || spare.calls == 0 {
		t.Fatalf("automatic session must retain fallback: status %d body %s spare calls %d", code, body, spare.calls)
	}
	if err := provider.SetSessionProviderFor("codex", "pinned-codex", ""); err != nil {
		t.Fatal(err)
	}
	code, body = send("pinned-codex")
	if code != 200 || !strings.Contains(body, "from-spare") {
		t.Fatalf("clearing pin must restore automatic routing: status %d body %s", code, body)
	}
}
