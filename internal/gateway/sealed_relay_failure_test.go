package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

type sealedRelayRequest struct {
	path string
	body string
}

type sealedRelayAPIFailure struct {
	mu       sync.Mutex
	requests []sealedRelayRequest
}

func (f *sealedRelayAPIFailure) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, sealedRelayRequest{path: r.URL.Path, body: string(b)})
	n := len(f.requests)
	f.mu.Unlock()

	if n == 2 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"model gpt-6-astra is not supported via Responses API.","code":"unsupported_api_for_model"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, sse(`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`))
}

func (f *sealedRelayAPIFailure) got() []sealedRelayRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sealedRelayRequest(nil), f.requests...)
}

func postSealedRelayTurn(s *Server, session, parent, input string) *httptest.ResponseRecorder {
	body := `{"model":"s2a/gpt-6-astra","stream":true,"input":[` + input + `]}`
	req := httptest.NewRequest("POST", CodexPath+"/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("chatgpt-account-id", "acct-1")
	req.Header.Set("session_id", session)
	if parent != "" {
		req.Header.Set("x-codex-parent-thread-id", parent)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSealedTaskRelayReturnsUpstreamAPIFailure(t *testing.T) {
	fresh(t)
	relay := &sealedRelayAPIFailure{}
	up := httptest.NewServer(relay)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{
		ID: "s2a", Name: "S2A", Key: "sk-s2a", Models: []string{"gpt-6-astra"},
		Responses: up.URL + "/v1", Chat: up.URL + "/v1",
	}); err != nil {
		t.Fatal(err)
	}

	s := New()
	if rec := postSealedRelayTurn(s, "lead-1", "", leadAsk); rec.Code != http.StatusOK {
		t.Fatalf("lead: %d %s", rec.Code, rec.Body.String())
	}
	rec := postSealedRelayTurn(s, "worker-1", "lead-1", sealedHandoff)
	got := relay.got()
	if len(got) != 2 {
		t.Fatalf("vendor received %d requests, want lead and one native refusal: %+v", len(got), got)
	}
	if got[0].path != "/v1/responses" || got[1].path != "/v1/responses" {
		t.Fatalf("vendor paths: %+v", got)
	}
	if !strings.Contains(got[1].body, `"type":"agent_message"`) ||
		!strings.Contains(got[1].body, `"encrypted_content":"gAAAAATest_ciphertext=="`) {
		t.Fatalf("sealed task was not relayed intact: %+v", got[1])
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not supported via Responses API") {
		t.Fatalf("sealed task refusal: %d %s", rec.Code, rec.Body.String())
	}
}
