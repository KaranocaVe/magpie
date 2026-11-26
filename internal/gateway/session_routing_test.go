package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessionrouting"
)

type overrideFixture struct {
	t      *testing.T
	server *Server
	mu     sync.Mutex
	hits   []string
	reject string
}

func newOverrideFixture(t *testing.T) *overrideFixture {
	fresh(t)
	sessionrouting.SetPathForTest(filepath.Join(t.TempDir(), "overrides.json"))
	t.Cleanup(func() { sessionrouting.SetPathForTest("") })
	return &overrideFixture{t: t, server: New()}
}

func (f *overrideFixture) upstream(id string, extraKeys ...string) {
	serveOn(f.t, id, "key", []string{"sol", "terra"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			io.WriteString(w, "{\"data\":[]}")
			return
		}
		io.Copy(io.Discard, r.Body)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.hits = append(f.hits, id+"/"+key)
		refuse := f.reject == id+"/"+key
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse {
			w.WriteHeader(401)
			io.WriteString(w, "{\"error\":{\"message\":\"invalid api key\"}}")
			return
		}
		io.WriteString(w, chatOK)
	}), extraKeys...)
}

func (f *overrideFixture) group(g provider.Group) {
	f.t.Helper()
	if err := provider.SaveGroup(g); err != nil {
		f.t.Fatal(err)
	}
}

func (f *overrideFixture) set(session, model, pid string, mode sessionrouting.Mode) {
	f.t.Helper()
	if err := sessionrouting.Put(sessionrouting.Override{Agent: "codex", Session: session, Model: model, Provider: pid, Mode: mode}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *overrideFixture) request(path, session, body string, headers http.Header, status int, want ...string) Route {
	f.t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex/0.170.0")
	req.Header.Set("session_id", session)
	req.Header.Set("x-session-id", session)
	if req.Header.Get("Authorization") != "" {
		req.RemoteAddr = "192.168.1.20:5000"
	}
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, req)
	f.mu.Lock()
	hits := f.hits
	f.hits = nil
	f.mu.Unlock()
	if rec.Code != status || !slices.Equal(hits, want) {
		f.t.Fatalf("%s: got %d, hits %v, want %d %v\n%s", session, rec.Code, hits, status, want, rec.Body)
	}
	state := f.server.Trace(f.t.Context(), 0, 0)
	if len(state.Routes) == 0 {
		f.t.Fatal("no route was recorded")
	}
	return state.Routes[len(state.Routes)-1]
}

func (f *overrideFixture) chat(session, model string, headers http.Header, status int, want ...string) Route {
	f.t.Helper()
	return f.request("/v1/chat/completions", session,
		fmt.Sprintf("{\"model\":%q,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}", model), headers, status, want...)
}

func TestSessionRoutingOverrideModelsAndAffinity(t *testing.T) {
	f := newOverrideFixture(t)
	for _, id := range []string{"a", "b", "c"} {
		f.upstream(id)
	}
	for _, model := range []string{"sol", "terra"} {
		f.group(provider.Group{ID: model, Name: model, Members: []string{"a/" + model, "b/" + model, "c/" + model},
			Routing: provider.Ordered, Affinity: provider.AffinityStrict})
	}
	if route := f.chat("one", "group/sol", nil, 200, "a/key"); route.SessionOverride != nil {
		t.Fatalf("unexpected override %+v", route.SessionOverride)
	}
	f.set("one", "group/sol", "c", sessionrouting.Prefer)
	f.set("two", "group/sol", "b", sessionrouting.Strict)
	f.set("one", "group/terra", "b", sessionrouting.Strict)
	route := f.chat("one", "group/sol", nil, 200, "c/key")
	var order []string
	for _, w := range route.Order {
		order = append(order, w.Provider)
	}
	if !slices.Equal(order, []string{"c", "a", "b"}) || route.SessionOverride == nil ||
		route.SessionOverride.Result != "applied" || route.SessionOverride.ActualProvider != "c" {
		t.Fatalf("order %v, override %+v", order, route.SessionOverride)
	}
	f.chat("two", "group/sol", nil, 200, "b/key")
	f.chat("one", "group/terra", nil, 200, "b/key")
	f.chat("other", "group/terra", nil, 200, "a/key")
	f.set("one", "group/sol", "b", sessionrouting.Strict)
	f.chat("one", "group/sol", nil, 200, "b/key")
}

func TestSessionRoutingOverrideFailureAndPermissions(t *testing.T) {
	f := newOverrideFixture(t)
	f.reject = "c/key"
	f.upstream("a")
	f.upstream("c")
	f.group(provider.Group{ID: "nested", Name: "Nested", Members: []string{"a/sol", "c/sol"},
		Routing: provider.Ordered, Rules: []provider.Rule{{Agents: []string{"codex"}, Use: "a/sol"}}})
	f.group(provider.Group{ID: "top", Name: "Top", Members: []string{"group/nested"}, Routing: provider.Ordered})
	f.set("prefer", "group/top", "c", sessionrouting.Prefer)
	route := f.chat("prefer", "group/top", nil, 200, "c/key", "a/key")
	if route.SessionOverride == nil || route.SessionOverride.Result != "fallback" ||
		route.SessionOverride.ActualProvider != "a" || len(route.Nested) != 1 ||
		route.Nested[0].Rule == nil || route.Nested[0].Rule.Use != "a/sol" {
		t.Fatalf("nested %+v override %+v", route.Nested, route.SessionOverride)
	}
	noRests()
	f.set("strict", "group/top", "c", sessionrouting.Strict)
	route = f.chat("strict", "group/top", nil, 401, "c/key")
	if route.SessionOverride == nil || route.SessionOverride.ActualProvider != "c" || len(route.Order) != 1 {
		t.Fatalf("order %+v override %+v", route.Order, route.SessionOverride)
	}
	noRests()
	keys, secrets := newCaller(t, "Only A")
	if _, err := access.Update("accounts-key", access.Change{Key: keys[0].ID, Accounts: []string{"a/" + provider.KeyID("key")}}); err != nil {
		t.Fatal(err)
	}
	auth := http.Header{"Authorization": {"Bearer " + secrets[0]}}
	route = f.chat("strict", "group/top", auth, 403)
	if route.SessionOverride == nil || route.SessionOverride.Result != "rejected" {
		t.Fatalf("override %+v", route.SessionOverride)
	}
	f.set("strict", "group/top", "gone", sessionrouting.Strict)
	route = f.chat("strict", "group/top", nil, 503)
	if route.SessionOverride == nil || !strings.Contains(route.SessionOverride.Reason, "removed") {
		t.Fatalf("override %+v", route.SessionOverride)
	}
}

func TestSessionRoutingOverrideStrictRetriesProviderKeys(t *testing.T) {
	f := newOverrideFixture(t)
	f.reject = "target/key"
	f.upstream("base")
	f.upstream("target", "next")
	f.group(provider.Group{ID: "keys", Name: "Keys", Members: []string{"base/sol", "target/sol"}, Routing: provider.Ordered})
	f.set("strict", "group/keys", "target", sessionrouting.Strict)
	f.chat("strict", "group/keys", nil, 200, "target/key", "target/next")
}

func TestSessionRoutingOverrideCodexChildAndFork(t *testing.T) {
	f := newOverrideFixture(t)
	f.upstream("base")
	f.upstream("target")
	f.group(provider.Group{ID: "sol", Name: "sol", Members: []string{"base/sol", "target/sol"}, Routing: provider.Ordered})
	f.group(provider.Group{ID: "terra", Name: "terra", Members: []string{"base/terra", "target/terra"}, Routing: provider.Ordered})
	f.set("root", "group/terra", "target", sessionrouting.Strict)
	f.set("root", "group/sol", "base", sessionrouting.Strict)
	headers := func(thread, source, parent, fork string) http.Header {
		h := make(http.Header)
		h.Set("x-codex-turn-metadata", fmt.Sprintf(`{"thread_id":%q,"thread_source":%q,"parent_thread_id":%q,"forked_from_thread_id":%q}`, thread, source, parent, fork))
		return h
	}
	child := headers("child", "subagent", "root", "")
	r := f.chat("root", "terra", child, 200, "target/key")
	if o := r.SessionOverride; o == nil || o.InheritedFrom != "root" || o.Session != "child" || o.Model != "group/terra" {
		t.Fatalf("child route override: %+v", o)
	}
	f.set("child", "group/terra", "base", sessionrouting.Strict)
	r = f.chat("root", "group/terra", child, 200, "base/key")
	if o := r.SessionOverride; o == nil || o.InheritedFrom != "" || o.Session != "child" {
		t.Fatalf("child route override: %+v", o)
	}
	r = f.chat("root", "group/terra", headers("unlinked", "subagent", "", ""), 200, "base/key")
	if r.SessionOverride != nil || r.SessionOverrideNote == "" {
		t.Fatalf("unlinked route override: %+v, note %q", r.SessionOverride, r.SessionOverrideNote)
	}
	r = f.chat("root", "group/terra", headers("forked", "user", "", "root"), 200, "base/key")
	if r.SessionOverride != nil {
		t.Fatalf("fork route override: %+v", r.SessionOverride)
	}
}

func TestSessionRoutingOverrideRecordedCodexSession(t *testing.T) {
	f := newOverrideFixture(t)
	f.upstream("base")
	f.upstream("target")
	f.group(provider.Group{ID: "sol", Name: "Sol", Members: []string{"base/sol", "target/sol"}, Routing: provider.Ordered})
	f.set("recorded-session", "group/sol", "target", sessionrouting.Strict)
	headers := make(http.Header)
	headers.Set(SessionHeader, "recorded-session")
	headers.Set("x-codex-turn-metadata", `{"thread_id":"native-thread","thread_source":"user"}`)
	route := f.chat("native-thread", "group/sol", headers, 200, "target/key")
	if route.SessionOverride == nil || route.SessionOverride.Session != "recorded-session" {
		t.Fatalf("recorded session override: %+v", route.SessionOverride)
	}
	headers.Set("x-codex-turn-metadata", `{"thread_id":"fork","thread_source":"user","forked_from_thread_id":"native-thread"}`)
	route = f.chat("fork", "group/sol", headers, 200, "base/key")
	if route.SessionOverride != nil {
		t.Fatalf("fork inherited a recorded session override: %+v", route.SessionOverride)
	}
}

func TestSessionRoutingOverrideWindowAndPin(t *testing.T) {
	f := newOverrideFixture(t)
	for _, id := range []string{"first", "held", "target"} {
		f.upstream(id)
	}
	p, err := provider.Find("target")
	if err != nil {
		t.Fatal(err)
	}
	p.Contexts = map[string]int{"sol": 2000}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	f.group(provider.Group{ID: "windows", Name: "Windows", Members: []string{"first/sol", "held/sol", "target/sol"}, Routing: provider.Ordered, Affinity: provider.AffinityStrict})
	f.set("session", "group/windows", "held", sessionrouting.Strict)
	f.chat("session", "group/windows", nil, 200, "held/key")
	f.set("session", "group/windows", "target", sessionrouting.Prefer)
	body := fmt.Sprintf(`{"model":"group/windows","messages":[{"role":"user","content":%q}]}`, strings.Repeat("word ", 2200))
	r := f.request("/v1/chat/completions", "session", body, nil, 200, "held/key")
	if o := r.SessionOverride; o == nil || o.Result != "skipped" || !strings.Contains(o.Reason, "context-window") {
		t.Fatalf("prefer override: %+v", o)
	}
	f.set("session", "group/windows", "target", sessionrouting.Strict)
	f.request("/v1/chat/completions", "session", body, nil, 400)
	h := make(http.Header)
	h.Set(AccountHeader, "first/"+provider.KeyID("key"))
	r = f.chat("session", "group/windows", h, 400)
	if o := r.SessionOverride; o == nil || o.Result != "rejected" || !strings.Contains(o.Reason, "account pin") {
		t.Fatalf("pinned override: %+v", o)
	}
}

func TestSessionRoutingOverrideStrictReplan(t *testing.T) {
	f := newOverrideFixture(t)
	overloaded := reply{529, "", `{"error":{"message":"Our servers are currently overloaded. Please try again later.","code":"server_is_overloaded"}}`}
	target := &scripted{replies: []reply{overloaded, overloaded, {200, "", chatOK}}}
	base := &scripted{replies: []reply{{200, "", chatOK}}}
	serveOn(t, "target", "k1", []string{"m"}, target, "k2")
	scriptedOn(t, "base", provider.Chat, base)
	f.group(provider.Group{ID: "retry", Name: "Retry", Members: []string{"base/m", "target/m"}, Routing: provider.Ordered, Patience: 5})
	f.set("strict", "group/retry", "target", sessionrouting.Strict)
	r := f.chat("strict", "group/retry", nil, 200)
	if target.n != 3 || base.n != 0 || len(r.Tries) != 3 || r.Tries[1].Replan != 2 {
		t.Fatalf("target called %d times, base called %d times, trace %+v", target.n, base.n, r.Tries)
	}
}
