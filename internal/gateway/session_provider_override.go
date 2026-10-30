package gateway

import (
	"fmt"

	"github.com/yetone/magpie/internal/provider"
)

// pinnedSessionRoute resolves one conversation's upstream without asking
// Codex to change its model_provider or rewriting its persistent history.
// Groups can spell a model differently at each member: take the matched
// member's vendor spelling, not an invented provider/model pair.
func pinnedSessionRoute(asked string, current provider.Provider, model, pinned string) (provider.Provider, string, string, error) {
	var p provider.Provider
	var m string
	var ok bool
	if _, members, group := provider.FindGroup(asked); group {
		for _, member := range members {
			if member.Provider.ID == pinned {
				p, m, ok = member.Provider, member.Model, true
				break
			}
		}
	} else if current.ID == pinned {
		p, m, ok = current, model, true
	} else {
		p, m, ok = provider.Resolve(pinned + "/" + model)
	}
	if !ok || p.ID != pinned || !p.On() {
		return provider.Provider{}, "", "", fmt.Errorf("session locked to %q: provider is unavailable or does not serve model %q (automatic fallback disabled)", pinned, asked)
	}
	// A provider may itself list fallback models or routing groups. A session
	// pin must not follow those on errors, exhaustion or timeouts.
	p.Fallback = nil
	return p, m, pinned + "/" + m, nil
}
