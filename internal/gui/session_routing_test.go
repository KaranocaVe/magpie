package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessionrouting"
)

func TestSessionRoutingManagement(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	sessionrouting.SetPathForTest(filepath.Join(root, "session-routing-overrides.json"))
	t.Cleanup(func() { sessionrouting.SetPathForTest("") })
	for _, id := range []string{"member", "outsider"} {
		if err := provider.Save(provider.Provider{ID: id, Name: id, Key: "fake-key", Chat: "http://127.0.0.1:1/v1", Models: []string{"m"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "test-route", Name: "Test route", Members: []string{"member/m"}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	sessionRoutingRoutes(mux)
	const path = "/api/session-routing"
	request := func(h http.Handler, method, query, body string, code int) string {
		t.Helper()
		req := httptest.NewRequest(method, path+query, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != code {
			t.Fatalf("%s %s: %d %s, want %d", method, query, rec.Code, rec.Body, code)
		}
		return rec.Body.String()
	}
	const prefix = `{"agent":"codex","session":"thread","model":"group/test-route",`
	set := func(pid, mode string) string {
		return prefix + `"action":"set","provider":` + string(mustOverrideJSON(t, pid)) + `,"mode":` + string(mustOverrideJSON(t, mode)) + "}"
	}
	gw := gateway.New()
	request(gw.Handler(), http.MethodPost, "", set("member", "strict"), http.StatusNotFound)
	request(mux, http.MethodPost, "", set("outsider", "strict"), http.StatusBadRequest)
	request(mux, http.MethodPost, "", set("member", ""), http.StatusOK)
	if saved, ok := sessionrouting.Lookup("codex", "thread", "group/test-route"); !ok || saved.Mode != sessionrouting.Prefer {
		t.Fatalf("saved override: %+v, present %v", saved, ok)
	}
	if err := provider.SetOff("member", true); err != nil {
		t.Fatal(err)
	}
	list := request(mux, http.MethodGet, "?agent=codex&session=thread", "", http.StatusOK)
	if !strings.Contains(list, `"valid":false`) || !strings.Contains(list, `"provider":"member"`) {
		t.Fatalf("invalid override missing from list: %s", list)
	}
	request(mux, http.MethodPost, "", set("member", "strict"), http.StatusOK)
	request(mux, http.MethodPost, "", prefix+`"action":"delete"}`, http.StatusOK)
	if _, ok := sessionrouting.Lookup("codex", "thread", "group/test-route"); ok {
		t.Fatal("override still present after deletion")
	}
}

func mustOverrideJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
