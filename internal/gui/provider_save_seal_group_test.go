package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestProviderSaveSealGroupPresence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/save", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	check := func(want string) {
		t.Helper()
		p, err := provider.Find("relay")
		if err != nil {
			t.Fatal(err)
		}
		if p.SealGroup != want {
			t.Fatalf("sealGroup = %q, want %q", p.SealGroup, want)
		}
	}

	const endpoint = "http://127.0.0.1:1/v1"
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Responses: endpoint, SealGroup: "group-a"}); err != nil {
		t.Fatal(err)
	}

	// The GUI omits this CLI-managed field. An omitted value keeps the saved one.
	post(`{"id":"relay","from":"relay","name":"Relay","responses":"` + endpoint + `","headers":{}}`)
	check("group-a")

	// The API can still change or explicitly clear it when a client sends the field.
	post(`{"id":"relay","from":"relay","name":"Relay","responses":"` + endpoint + `","headers":{},"sealGroup":"group-b"}`)
	check("group-b")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/providers", nil))
	var listed struct {
		Providers []struct {
			ID        string `json:"id"`
			SealGroup string `json:"sealGroup"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode provider list: %v", err)
	}
	found := false
	for _, p := range listed.Providers {
		if p.ID == "relay" {
			found = true
			if p.SealGroup != "group-b" {
				t.Errorf("listed sealGroup = %q, want %q", p.SealGroup, "group-b")
			}
		}
	}
	if !found {
		t.Fatal("relay not listed")
	}
	post(`{"id":"relay","from":"relay","name":"Relay","responses":"` + endpoint + `","headers":{},"sealGroup":""}`)
	check("")
}
