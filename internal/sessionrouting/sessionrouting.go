// Package sessionrouting stores GUI-managed, per-session model/provider preferences.
package sessionrouting

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/provider"
)

type Mode string

const (
	Prefer Mode = "prefer"
	Strict Mode = "strict"
)

type Override struct {
	Agent    string `json:"agent"`
	Session  string `json:"session"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Mode     Mode   `json:"mode"`
}

var (
	mu          sync.RWMutex
	pathForTest string
	loaded      bool
	loadErr     error
	entries     []Override
	index       map[string]int
)

func Path() string {
	mu.RLock()
	p := pathForTest
	mu.RUnlock()
	return pathFor(p)
}

func pathFor(testPath string) string {
	p := testPath
	if p != "" {
		return p
	}
	return appdir.Join(appdir.Config(), "session-routing-overrides.json")
}

// CanonicalModel trims the model token used as the logical routing key.
// Callers should pass the stable routing-group or logical model ID, rather
// than a display name or a provider's concrete model ID.
func CanonicalModel(model string) string { return provider.CanonicalRef(strings.TrimSpace(model)) }

// ResolveModel returns the stable routing key used by an agent's model
// setting. provider.Resolve validates aliases, provider/model IDs and group
// references against the current provider catalog. A caller that already has
// a group reference can use the canonical reference even when its members are
// temporarily unavailable.
func ResolveModel(model string) (string, bool) {
	model = CanonicalModel(model)
	if g, _, ok := provider.FindGroup(model); ok {
		return provider.GroupPrefix + g.ID, true
	}
	if gid, ok := provider.GroupIDOf(model); ok {
		return provider.GroupPrefix + gid, true
	}
	if p, m, ok := provider.Resolve(model); ok {
		return p.ID + "/" + m, true
	}
	return model, false
}

func SetPathForTest(p string) {
	mu.Lock()
	pathForTest, loaded, loadErr, entries, index = p, false, nil, nil, nil
	mu.Unlock()
}

func validate(o Override) error {
	if o.Agent == "" || o.Session == "" || o.Model == "" || o.Provider == "" {
		return errors.New("agent, session, model and provider are required")
	}
	if o.Mode != Prefer && o.Mode != Strict {
		return fmt.Errorf("mode must be %q or %q", Prefer, Strict)
	}
	return nil
}

func normalize(o Override) Override {
	o.Agent, o.Session, o.Model, o.Provider = strings.TrimSpace(o.Agent), strings.TrimSpace(o.Session), strings.TrimSpace(o.Model), strings.TrimSpace(o.Provider)
	if o.Mode == "" {
		o.Mode = Prefer
	}
	return o
}

func loadLocked() error {
	if loaded {
		return loadErr
	}
	b, err := os.ReadFile(pathFor(pathForTest))
	if os.IsNotExist(err) {
		entries, index, loaded, loadErr = []Override{}, map[string]int{}, true, nil
		return nil
	}
	if err != nil {
		loadErr, loaded = err, true
		return loadErr
	}
	var v struct {
		Overrides []Override `json:"overrides"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		loadErr, loaded = fmt.Errorf("decode session routing overrides: %w", err), true
		return loadErr
	}
	for i := range v.Overrides {
		v.Overrides[i] = normalize(v.Overrides[i])
		if err := validate(v.Overrides[i]); err != nil {
			loadErr, loaded = fmt.Errorf("override %d: %w", i, err), true
			return loadErr
		}
	}
	entries, index, loaded, loadErr = v.Overrides, make(map[string]int, len(v.Overrides)), true, nil
	for i, o := range entries {
		index[key(o)] = i
	}
	return nil
}

func saveSnapshot(snapshot []Override) error {
	p := pathFor(pathForTest)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	sort.Slice(snapshot, func(i, j int) bool { return key(snapshot[i]) < key(snapshot[j]) })
	b, err := json.MarshalIndent(struct {
		Overrides []Override `json:"overrides"`
	}{snapshot}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(p), ".session-routing-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func publishLocked(snapshot []Override) {
	entries = snapshot
	index = make(map[string]int, len(snapshot))
	for i, o := range snapshot {
		index[key(o)] = i
	}
}

func key(o Override) string { return o.Agent + "\x00" + o.Session + "\x00" + o.Model }

func List(agent, session string) ([]Override, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := loadLocked(); err != nil {
		return nil, err
	}
	out := make([]Override, 0)
	for _, o := range entries {
		if (agent == "" || o.Agent == agent) && (session == "" || o.Session == session) {
			out = append(out, o)
		}
	}
	return out, nil
}

func All() ([]Override, error) { return List("", "") }

func Lookup(agent, session, model string) (Override, bool) {
	model = CanonicalModel(model)
	mu.RLock()
	if loaded {
		if i, ok := index[key(Override{Agent: agent, Session: session, Model: model})]; ok {
			o := entries[i]
			mu.RUnlock()
			return o, true
		}
		mu.RUnlock()
		return Override{}, false
	}
	mu.RUnlock()
	mu.Lock()
	defer mu.Unlock()
	if loadLocked() != nil {
		return Override{}, false
	}
	if i, ok := index[key(Override{Agent: agent, Session: session, Model: model})]; ok {
		return entries[i], true
	}
	return Override{}, false
}

func Put(o Override) error {
	o = normalize(o)
	if err := validate(o); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if err := loadLocked(); err != nil {
		return err
	}
	k := key(o)
	next := append([]Override(nil), entries...)
	if i, ok := index[k]; ok {
		next[i] = o
	} else {
		next = append(next, o)
	}
	if err := saveSnapshot(next); err != nil {
		return err
	}
	publishLocked(next)
	return nil
}

func Delete(agent, session, model string) error {
	model = CanonicalModel(model)
	mu.Lock()
	defer mu.Unlock()
	if err := loadLocked(); err != nil {
		return err
	}
	out := make([]Override, 0, len(entries))
	for _, o := range entries {
		if !(o.Agent == agent && o.Session == session && o.Model == model) {
			out = append(out, o)
		}
	}
	if err := saveSnapshot(out); err != nil {
		return err
	}
	publishLocked(out)
	return nil
}

func Clear(agent, session string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := loadLocked(); err != nil {
		return err
	}
	out := make([]Override, 0, len(entries))
	for _, o := range entries {
		if !(o.Agent == agent && o.Session == session) {
			out = append(out, o)
		}
	}
	if err := saveSnapshot(out); err != nil {
		return err
	}
	publishLocked(out)
	return nil
}
