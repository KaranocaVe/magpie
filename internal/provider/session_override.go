package provider

// Session-scoped manual routing is intentionally separate from Codex's
// model_provider. The latter is a Codex configuration identity; this is a
// Magpie upstream chosen for one Codex conversation. No entry means automatic
// routing. An entry never authorizes fallback to another provider.
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/yetone/magpie/internal/settings"
)

type SessionProviderOverride struct {
	Agent    string `json:"agent"`
	Session  string `json:"session"`
	Provider   string `json:"provider"`
	NoFallback bool   `json:"noFallback,omitempty"` // also applies with automatic selection
}

var sessionOverrideMu sync.Mutex

func sessionOverridePath() string {
	return filepath.Join(settings.Dir(), "session-provider-overrides.json")
}

func readSessionOverrides() ([]SessionProviderOverride, error) {
	b, err := os.ReadFile(sessionOverridePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("session provider override file exceeds 1 MiB")
	}
	var entries []SessionProviderOverride
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("read session provider overrides: %w", err)
	}
	return entries, nil
}

// SessionProviderFor returns the pinned provider, or "" for automatic routing.
// Read errors are returned rather than treating a corrupt configuration as
// permission to fall back to an arbitrary upstream.
func SessionProviderFor(agent, session string) (string, error) {
	entry, err := SessionRouteFor(agent, session)
	return entry.Provider, err
}

// SessionRouteFor also returns the no-fallback policy of an automatically
// routed session: the gateway picks its first candidate as usual, then
// refuses instead of asking another if that candidate fails.
func SessionRouteFor(agent, session string) (SessionProviderOverride, error) {
	if agent != "codex" || session == "" {
		return SessionProviderOverride{}, nil
	}
	sessionOverrideMu.Lock()
	defer sessionOverrideMu.Unlock()
	entries, err := readSessionOverrides()
	if err != nil {
		return SessionProviderOverride{}, err
	}
	for _, e := range entries {
		if e.Agent == agent && e.Session == session {
			return e, nil
		}
	}
	return SessionProviderOverride{}, nil
}

// SessionProvidersFor snapshots every override of one agent for the Sessions
// page. A bulk read avoids one disk read per session in a large list.
func SessionRoutesFor(agent string) (map[string]SessionProviderOverride, error) {
	sessionOverrideMu.Lock()
	defer sessionOverrideMu.Unlock()
	entries, err := readSessionOverrides()
	if err != nil {
		return nil, err
	}
	out := make(map[string]SessionProviderOverride)
	for _, entry := range entries {
		if entry.Agent == agent {
			out[entry.Session] = entry
		}
	}
	return out, nil
}

// SetSessionProviderFor sets a strict provider pin, or removes it when id is
// empty ("auto"). This modifies Magpie alone, never Codex rollout metadata.
func SetSessionProviderFor(agent, session, id string) error {
	return SetSessionProviderRouteFor(agent, session, id, id != "")
}

// SetSessionProviderRouteFor keeps strict-auto distinct from inherited auto:
// the latter is no entry; strict-auto records NoFallback without a provider.
func SetSessionProviderRouteFor(agent, session, id string, noFallback bool) error {
	if agent != "codex" || session == "" || len(session) > 128 ||
		strings.TrimSpace(session) != session ||
		strings.ContainsFunc(session, unicode.IsControl) {
		return errors.New("expected a valid Codex session id")
	}
	if id != "" {
		p, err := Find(id)
		if err != nil || p.ID != id {
			return fmt.Errorf("provider %q does not exist", id)
		}
		if !p.On() {
			return fmt.Errorf("provider %q is not enabled or ready", id)
		}
	}
	sessionOverrideMu.Lock()
	defer sessionOverrideMu.Unlock()
	entries, err := readSessionOverrides()
	if err != nil {
		return err
	}
	kept := make([]SessionProviderOverride, 0, len(entries)+1)
	for _, e := range entries {
		if e.Agent != agent || e.Session != session {
			kept = append(kept, e)
		}
	}
	if id != "" || noFallback {
		kept = append(kept, SessionProviderOverride{Agent: agent, Session: session, Provider: id, NoFallback: noFallback || id != ""})
	}
	dir := settings.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-provider-overrides-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), sessionOverridePath())
}
