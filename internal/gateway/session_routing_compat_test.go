package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessionrouting"
)

func sessionCompatibilityTurn(t *testing.T, s *Server, path, session, parent, body string, status int, contains string) Route {
	t.Helper()
	before := s.Trace(t.Context(), 0, 0).Seq
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("User-Agent", "codex/0.170.0")
	req.Header.Set("session_id", session)
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, CodexPath) {
		req.Header.Set("Authorization", "Bearer chatgpt-token")
		req.Header.Set("chatgpt-account-id", "acct-1")
	}
	if parent != "" {
		req.Header.Set("x-openai-subagent", "collab_spawn")
		req.Header.Set("x-codex-parent-thread-id", parent)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != status || !strings.Contains(rec.Body.String(), contains) {
		t.Fatalf("got %d %s, want %d and %q", rec.Code, rec.Body.String(), status, contains)
	}
	routes := s.Trace(t.Context(), before, 0).Routes
	if len(routes) == 0 {
		t.Fatal("no route was traced")
	}
	return routes[len(routes)-1]
}

func assertCompatibilityTrace(t *testing.T, route Route, result, reason, actual string) *SessionOverrideTrace {
	t.Helper()
	o := route.SessionOverride
	if o == nil || o.Result != result || !strings.Contains(o.Reason, reason) || o.ActualProvider != actual {
		t.Fatalf("override = %+v, want result %q, reason containing %q, actual provider %q", o, result, reason, actual)
	}
	return o
}

func compatibilityResponsesProvider(t *testing.T, id, model string, handler http.Handler) {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: id, Name: id, Key: "key-" + id,
		Models: []string{model}, Responses: up.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRoutingOverrideEncryptedReasoning(t *testing.T) {
	f := newOverrideFixture(t)
	writer, target := &sealer{name: "writer"}, &sealer{name: "target"}
	compatibilityResponsesProvider(t, "writer", "grok-4", writer)
	compatibilityResponsesProvider(t, "target", "grok-4", target)
	f.group(provider.Group{ID: "reasoning", Name: "Reasoning", Members: []string{"writer/grok-4", "target/grok-4"},
		Routing: provider.Ordered, Affinity: provider.AffinityStrict})
	const model = "group/reasoning"
	const seal = "sealed-by-writer"
	turn := func(t *testing.T, session, body, text string) Route {
		t.Helper()
		return sessionCompatibilityTurn(t, f.server, "/v1/responses", session, "", body, 200, text)
	}
	first := `{"model":"group/reasoning","stream":true,"input":[` + leadAsk + `]}`
	next := `{"model":"group/reasoning","stream":true,"input":[` + leadAsk +
		`,{"type":"reasoning","id":"rs_writer","summary":[],"encrypted_content":"` + seal +
		`"},{"type":"function_call","call_id":"tool_1","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"tool_1","output":"ok"}]}`

	t.Run("no saved affinity", func(t *testing.T) {
		const session = "resumed-thread"
		f.set(session, model, "target", sessionrouting.Strict)
		sameRelaySeal := strings.Replace(next, seal, "sealed-by-target", 1)
		assertCompatibilityTrace(t, turn(t, session, sameRelaySeal, "from target"), "applied", "", "target")
		if got := target.sent(); len(got) != 1 || !slices.Equal(got[0], []string{"sealed-by-target"}) {
			t.Fatalf("target received %q, want one intact request", got)
		}
	})

	for _, mode := range []sessionrouting.Mode{sessionrouting.Strict, sessionrouting.Prefer} {
		t.Run(string(mode)+" with foreign reasoning", func(t *testing.T) {
			session := string(mode) + "-thread"
			turn(t, session, first, seal)
			writer.sent()
			f.set(session, model, "target", mode)
			route := turn(t, session, next, "from target")
			assertCompatibilityTrace(t, route, "applied", "", "target")
			if len(route.Tries) != 2 || route.Tries[0].Fail != failForeign {
				t.Fatalf("ordinary encrypted reasoning retry missing: %+v", route.Tries)
			}
			if got := target.sent(); len(got) != 2 || !slices.Equal(got[0], []string{seal}) || len(got[1]) != 0 {
				t.Fatalf("target received %q, want intact request and existing retry without refused reasoning", got)
			}
			if got := writer.sent(); len(got) != 0 {
				t.Fatalf("original account received %q", got)
			}
		})
	}
}

func TestSessionRoutingOverrideForkSealedTask(t *testing.T) {
	f := newOverrideFixture(t)
	relay := relayOn(t, "sealer")
	p, err := provider.Find("sealer")
	if err != nil {
		t.Fatal(err)
	}
	// The fork requires explicit compatibility even when two saved
	// providers have the same endpoint and credentials.
	addSealedRelayAlias(t, "sealer", "alias", p.Key, "", false, false, false)
	f.group(provider.Group{ID: "handoff", Name: "Handoff", Members: []string{"sealer/gpt-6-astra", "alias/gpt-6-astra"}, Routing: provider.Ordered})
	const lead = "sealed-task-lead"
	const worker = "sealed-task-worker"
	const model = "group/handoff"
	leadBody := fmt.Sprintf(`{"model":%q,"stream":true,"input":[%s]}`, model, leadAsk)
	sessionCompatibilityTurn(t, f.server, CodexPath+"/responses", lead, "", leadBody, 200, "on it")
	workerBody := strings.Replace(forkNoneTask, "ag/gemini-3.8-flash", model, 1)
	turn := func(status int, text string) Route {
		t.Helper()
		return sessionCompatibilityTurn(t, f.server, CodexPath+"/responses", worker, lead, workerBody, status, text)
	}

	f.set(lead, model, "alias", sessionrouting.Prefer)
	o := assertCompatibilityTrace(t, turn(200, "on it"), "skipped", "sealed subagent message", "sealer")
	if o.Session != worker || o.InheritedFrom != lead {
		t.Fatalf("override did not inherit from the lead: %+v", o)
	}
	f.set(worker, model, "alias", sessionrouting.Strict)
	o = assertCompatibilityTrace(t, turn(400, "sealed subagent message"), "rejected", "sealed subagent message", "")
	if o.Session != worker || o.InheritedFrom != "" {
		t.Fatalf("worker override did not take precedence: %+v", o)
	}
	if got := relay.requests(); len(got) != 2 || !strings.Contains(got[1], "gAAAAABo_sealedTaskFromTheLead-Q3x9_aZ==") {
		t.Fatalf("want only the lead turn and the intact task on the original relay, got %q", got)
	}
}

func TestSessionRoutingOverrideNativeCheckpoint(t *testing.T) {
	f := newOverrideFixture(t)
	const seal = "gAAAAB-session-override-checkpoint"
	var writerBodies, otherBodies []string
	var mu sync.Mutex
	output := sse(`data: {"type":"response.output_text.delta","delta":"continuing"}`)
	done := sse(`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`)
	upstream := func(bodies *[]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			*bodies = append(*bodies, string(body))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(string(body), "compaction_trigger") {
				io.WriteString(w, output+nativeSealedReply(seal))
				return
			}
			io.WriteString(w, output+done)
		}
	}
	compatibilityResponsesProvider(t, "writer", "m1", upstream(&writerBodies))
	compatibilityResponsesProvider(t, "other", "m1", upstream(&otherBodies))
	nativeOn(t, "writer")
	nativeOn(t, "other")
	f.group(provider.Group{ID: "checkpoint", Name: "Checkpoint", Members: []string{"writer/m1", "other/m1"}, Routing: provider.Ordered})
	const model = "group/checkpoint"
	const session = "native-checkpoint-thread"
	turn := func(body string, status int, text string) Route {
		t.Helper()
		return sessionCompatibilityTurn(t, f.server, CodexPath+"/responses", session, "", body, status, text)
	}
	first := turn(fmt.Sprintf(compactTurn, model), 200, seal)
	next := fmt.Sprintf(`{"model":%q,"stream":true,"input":[{"type":"compaction","encrypted_content":%q},%s]}`, model, seal, leadAsk)

	if checkpoint, ok := checkpointOf([]byte(next)); !ok || checkpoint.Provider != "writer" {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("compaction attempts: %+v, checkpoint: %+v, writer bodies: %q, other bodies: %q", first.Tries, checkpoint, writerBodies, otherBodies)
	}

	f.set(session, model, "other", sessionrouting.Prefer)
	assertCompatibilityTrace(t, turn(next, 200, "response.completed"), "skipped", "native compaction", "writer")
	f.set(session, model, "other", sessionrouting.Strict)
	assertCompatibilityTrace(t, turn(next, 400, "native compaction"), "rejected", "native compaction", "")

	mu.Lock()
	defer mu.Unlock()
	if len(writerBodies) != 2 || !strings.Contains(writerBodies[0], "compaction_trigger") || !strings.Contains(writerBodies[1], seal) {
		t.Fatalf("want native compaction followed by its intact checkpoint on the writer, got %q", writerBodies)
	}
	if len(otherBodies) != 0 {
		t.Fatalf("checkpoint reached another provider: %q", otherBodies)
	}
}

func TestSessionRoutingOverrideNativeSignInStrict(t *testing.T) {
	f := newOverrideFixture(t)
	b := &spentBackend{refuse: codexSpent[0].refuse}
	fallback := spentAccount(t, b, true)
	const model = "codex/gpt-5.5"
	const session = "native-sign-in-thread"
	f.set(session, model, "codex", sessionrouting.Strict)
	route := sessionCompatibilityTurn(t, f.server, CodexPath+"/responses", session, "", oneAccountTurn, 429, "usage_limit_reached")
	assertCompatibilityTrace(t, route, "applied", "", "codex")
	if b.n.Load() != 1 || fallback.n != 0 {
		t.Fatalf("ChatGPT was asked %d times and fallback %d times", b.n.Load(), fallback.n)
	}
}

func TestSessionRoutingOverrideStrictQuota(t *testing.T) {
	f := newOverrideFixture(t)
	codexSignedIn(t)
	capUsage(t, map[string]float64{"me@example.com": 75})
	if err := provider.SetAccountCap("codex", "me@example.com", 70); err != nil {
		t.Fatal(err)
	}
	f.upstream("spare")
	f.group(provider.Group{ID: "quota", Name: "Quota", Members: []string{"codex/gpt-5.5", "spare/sol"}, Routing: provider.Ordered})
	f.set("limited", "group/quota", "codex", sessionrouting.Strict)
	body := strings.Replace(oneAccountTurn, "gpt-5.5", "group/quota", 1)
	r := sessionCompatibilityTurn(t, f.server, CodexPath+"/responses", "limited", "", body, 429, "usage limit")
	assertCompatibilityTrace(t, r, "rejected", "usage limit", "")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hits) != 0 {
		t.Fatalf("capped override fell back: %v", f.hits)
	}
}
