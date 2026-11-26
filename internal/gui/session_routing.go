package gui

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessionrouting"
	"github.com/yetone/magpie/internal/sessions"
)

type overrideModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Providers []string `json:"providers"`
}

func overrideProviders(model string) []string {
	var ids []string
	add := func(p provider.Provider) {
		if p.On() && !slices.Contains(ids, p.ID) {
			ids = append(ids, p.ID)
		}
	}
	if g, members, ok := provider.FindGroup(model); ok {
		if !g.Decides() {
			for _, m := range members {
				add(m.Provider)
			}
		}
	} else if p, m, ok := provider.Resolve(model); ok && !provider.IsGroupRef(model) && !p.OnlyDecides(m) {
		add(p)
		for _, f := range provider.FallbackModels(p) {
			add(f.Provider)
		}
	}
	return ids
}

func sessionRoutingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/session-routing/trace", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "a valid route id is required", http.StatusBadRequest)
			return
		}
		route, _ := ctxRouteOf(r, id)
		writeJSON(w, struct {
			Override *gateway.SessionOverrideTrace `json:"override,omitempty"`
			Note     string                        `json:"note,omitempty"`
		}{route.SessionOverride, route.SessionOverrideNote})
	})
	mux.HandleFunc("GET /api/session-routing", func(w http.ResponseWriter, r *http.Request) {
		agent, session := r.URL.Query().Get("agent"), r.URL.Query().Get("session")
		overrides, err := sessionrouting.List(agent, session)
		if err != nil {
			fail(w, err)
			return
		}
		type view struct {
			sessionrouting.Override
			Title  string `json:"title,omitempty"`
			Valid  bool   `json:"valid"`
			Reason string `json:"reason,omitempty"`
		}
		out := make([]view, 0, len(overrides))
		titles := make(map[[2]string]string)
		gatewayTitle := sync.OnceValue(sessions.GatewayTitler)

		for _, o := range overrides {
			v := view{Override: o, Valid: true}
			if agent == "" {
				k := [2]string{o.Agent, o.Session}
				if _, seen := titles[k]; !seen {
					if s, ok := sessions.Find(o.Agent, o.Session); ok {
						titles[k] = s.Title
					}
					if titles[k] == "" {
						titles[k] = gatewayTitle()(o.Agent, o.Session)
					}
				}
				v.Title = titles[k]
			}
			switch p, err := provider.Find(o.Provider); {
			case err != nil || p.ID != o.Provider:
				v.Valid, v.Reason = false, "Provider removed"
			case !p.On():
				v.Valid, v.Reason = false, "Provider disabled or unavailable"
			case !slices.Contains(overrideProviders(o.Model), o.Provider):
				v.Valid, v.Reason = false, "Provider is outside this model's route"
			}
			out = append(out, v)
		}
		providers := make([]map[string]string, 0)
		models := make([]overrideModel, 0)
		if r.URL.Query().Get("options") == "1" {
			for _, p := range provider.All() {
				if p.On() {
					providers = append(providers, map[string]string{"id": p.ID, "name": cmp.Or(p.Name, p.ID)})
				}
			}
			seen := make(map[string]bool)
			for _, e := range provider.Served() {
				model, ok := sessionrouting.ResolveModel(e.ID)
				if !ok || seen[model] {
					continue
				}
				if ps := overrideProviders(model); len(ps) > 0 {
					models = append(models, overrideModel{model, cmp.Or(e.Name, e.ID), ps})
					seen[model] = true
				}
			}
		}
		writeJSON(w, struct {
			Overrides []view              `json:"overrides"`
			Models    []overrideModel     `json:"models"`
			Providers []map[string]string `json:"providers"`
		}{out, models, providers})
	})
	mux.HandleFunc("POST /api/session-routing", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Action string `json:"action"`
			sessionrouting.Override
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		in.Agent, in.Session, in.Model, in.Provider = strings.TrimSpace(in.Agent), strings.TrimSpace(in.Session), strings.TrimSpace(in.Model), strings.TrimSpace(in.Provider)
		if in.Agent == "" || len(in.Agent) > 64 || in.Session == "" || len(in.Session) > 128 ||
			strings.ContainsAny(in.Agent+in.Session+in.Model+in.Provider, "\x00\r\n\t") {
			http.Error(w, "a valid agent and session are required", http.StatusBadRequest)
			return
		}
		var err error
		switch in.Action {
		case "set":
			stored, exists := sessionrouting.Lookup(in.Agent, in.Session, in.Model)
			if !exists || stored.Provider != in.Provider {
				model, ok := sessionrouting.ResolveModel(in.Model)
				if !ok || !slices.Contains(overrideProviders(model), in.Provider) {
					http.Error(w, "the provider is outside this model's route", http.StatusBadRequest)
					return
				}
				// Keep an existing key so editing a former group ID does
				// not leave a second override behind.
				if !exists {
					in.Model = model
				}
			}
			if in.Mode != "" && in.Mode != sessionrouting.Prefer && in.Mode != sessionrouting.Strict {
				http.Error(w, "mode must be prefer or strict", http.StatusBadRequest)
				return
			}
			err = sessionrouting.Put(in.Override)
		case "delete":
			if in.Model == "" {
				http.Error(w, "a model is required", http.StatusBadRequest)
				return
			}
			err = sessionrouting.Delete(in.Agent, in.Session, in.Model)
		case "clear":
			err = sessionrouting.Clear(in.Agent, in.Session)
		default:
			http.Error(w, "action must be set, delete or clear", http.StatusBadRequest)
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]bool{"saved": true})
	})
}
