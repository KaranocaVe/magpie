package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

func TestSessionProviderOverrideDefaultAndPersistence(t *testing.T) {
	// An unknown session is automatically routed, without creating a file.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	agent, session := "codex", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if got, err := SessionProviderFor(agent, session); err != nil || got != "" {
		t.Fatalf("default: %q %v", got, err)
	}
	// Set up a minimal local provider so the pin validator uses the real
	// provider catalog, not a hard-coded list.
	p := Provider{ID: "session-route-test", Name: "session-route-test", Key: "test", Responses: "https://example.test/v1"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if err := SetSessionProviderFor(agent, session, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := SessionProviderFor(agent, session); err != nil || got != p.ID {
		t.Fatalf("pinned: %q %v", got, err)
	}
	if got, err := SessionProviderFor(agent, "another"); err != nil || got != "" {
		t.Fatalf("other session changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(settings.Dir(), "session-provider-overrides.json")); err != nil {
		t.Fatal(err)
	}
	if err := SetSessionProviderFor(agent, session, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := SessionProviderFor(agent, session); err != nil || got != "" {
		t.Fatalf("reset: %q %v", got, err)
	}
}

func TestSessionProviderOverrideInvalidAndCorruptFailClosed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tc := range []struct{ agent, session, id string }{
		{"claude", "session", ""}, {"codex", "", ""}, {"codex", "a\nb", ""},
		{"codex", "valid", "nonexistent-provider"},
	} {
		if err := SetSessionProviderFor(tc.agent, tc.session, tc.id); err == nil {
			t.Fatalf("accepted invalid override %+v", tc)
		}
	}
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings.Dir(), "session-provider-overrides.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SessionProviderFor("codex", "valid"); err == nil {
		t.Fatal("corrupt pin storage must not silently permit automatic routing")
	}
}
