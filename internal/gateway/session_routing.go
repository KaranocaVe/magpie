package gateway

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessionrouting"
	"github.com/yetone/magpie/internal/usage"
)

// SessionOverrideTrace is the override snapshot taken when routing a request.
type SessionOverrideTrace struct {
	Session        string              `json:"session"`
	Model          string              `json:"model"`
	Provider       string              `json:"provider"`
	Mode           sessionrouting.Mode `json:"mode"`
	InheritedFrom  string              `json:"inheritedFrom,omitempty"`
	Result         string              `json:"result"`
	Reason         string              `json:"reason,omitempty"`
	ActualProvider string              `json:"actualProvider,omitempty"`
	writer         stick
}

func routingSessionID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 128 || strings.ContainsAny(id, "\r\n\t\x00") {
		return ""
	}
	return id
}

func codexOverrideChild(agent, kind string, meta sessionMetadata, h http.Header) bool {
	return (agent == "codex" || fromCodex(h)) && (usage.PurposeOf(kind) == "kind:collab_spawn" || meta.Source == "subagent")
}

func sessionOverrideFor(r *http.Request, agent, kind, model string, meta sessionMetadata, groupFormer ...string) *SessionOverrideTrace {
	if model == "" || agent == "" || r.Header.Get("User-Agent") == RouterAgent || isTitleKind(kind) {
		return nil
	}
	session, parent := sessionOf(r.Header), ""
	former := groupFormer
	if agent == "codex" || fromCodex(r.Header) {
		if codexOverrideChild(agent, kind, meta, r.Header) {
			session = routingSessionID(codexSubagentThread(r.Header, meta))
			parent = routingSessionID(cmp.Or(meta.Parent, r.Header.Get("x-codex-parent-thread-id")))
			if parent == session {
				parent = ""
			}
		} else {
			// Match the recorded Gateway session when the caller names it
			// explicitly. A fork with its own thread remains independent.
			if explicit := routingSessionID(r.Header.Get(SessionHeader)); explicit != "" && meta.Forked == "" {
				session = explicit
			} else if id := routingSessionID(meta.Thread); id != "" {
				session = id
			}
		}
	}
	for _, id := range []string{session, parent} {
		if id == "" {
			continue
		}
		o, ok := sessionrouting.Lookup(agent, id, model)
		for _, old := range former {
			if ok {
				break
			}
			o, ok = sessionrouting.Lookup(agent, id, provider.GroupPrefix+old)
		}
		if ok {
			trace := &SessionOverrideTrace{Session: cmp.Or(session, id), Model: model, Provider: o.Provider, Mode: o.Mode, Result: "pending"}
			if id == parent {
				trace.InheritedFrom = parent
			}
			return trace
		}
	}
	return nil
}

func sessionLogicalModel(asked string, p provider.Provider, model string, group provider.Group) string {
	if group.ID != "" {
		return provider.GroupPrefix + group.ID
	}
	if p.ID != "" && model != "" {
		return p.ID + "/" + model
	}
	return provider.CanonicalRef(asked)
}

func (o *SessionOverrideTrace) affinityKey(scope string, in http.Header, body []byte) string {
	if o != nil && o.Session != "" && o.Session != affinityID(in, body) {
		return scope + "|" + o.Session
	}
	return stickKey(scope, in, body)
}

func (o *SessionOverrideTrace) strict() bool {
	return o != nil && o.Mode == sessionrouting.Strict
}

// A request's session override can be skipped by a hard constraint. Keep
// that reason even when an ordinary candidate goes on to answer.
func (o *SessionOverrideTrace) skipped(reason string) {
	if o == nil || o.Reason != "" {
		return
	}
	o.Result, o.Reason = "skipped", reason
	if o.strict() {
		o.Result = "rejected"
	}
}

func (o *SessionOverrideTrace) unavailable(status int, reason string) {
	o.skipped(reason)
	if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
		o.Result = "fallback"
	}
}

func (o *SessionOverrideTrace) snapshot() *SessionOverrideTrace {
	if o == nil {
		return nil
	}
	out := *o
	out.writer = stick{}
	return &out
}

func (o *SessionOverrideTrace) completed(actual, reason string, tried bool) *SessionOverrideTrace {
	out := o.snapshot()
	if out == nil {
		return nil
	}
	out.ActualProvider = actual
	if out.Reason != "" {
		return out
	}
	switch {
	case actual == out.Provider:
		out.Result = "applied"
	case actual != "":
		out.Result, out.Reason = "fallback", "preferred provider unavailable"
		if tried {
			out.Reason = "preferred provider did not complete the request"
		}
	default:
		out.skipped(cmp.Or(reason, "no upstream attempt"))
	}
	return out
}

func (o *SessionOverrideTrace) refusal(reason string) string {
	return fmt.Sprintf("strict session routing override for %q requires provider %q: %s", o.Model, o.Provider, reason)
}

func (o *SessionOverrideTrace) matches(c candidate) bool {
	return c.p.ID == o.Provider && (o.writer.who == "" || c.sameVendor(o.writer.who, o.writer.model))
}

func (o *SessionOverrideTrace) has(cs []candidate) bool {
	return slices.ContainsFunc(cs, o.matches)
}

// restrict applies strict mode to each pool the retry loop or a reset can
// bring back into the plan. Candidate and trace rows must stay aligned.
func (o *SessionOverrideTrace) restrict(cs []candidate, pl planned) ([]candidate, planned) {
	keep := func(cs []candidate, rows []Weighed) ([]candidate, []Weighed) {
		var out []candidate
		var kept []Weighed
		for i, c := range cs {
			if o.matches(c) {
				out, kept = append(out, c), append(kept, rows[i])
			}
		}
		return out, kept
	}
	other := func(c candidate) bool { return !o.matches(c) }
	cs, pl.order = keep(cs, pl.order)
	pl.unlisted, pl.unlistedW = keep(pl.unlisted, pl.unlistedW)
	pl.held = slices.DeleteFunc(slices.Clone(pl.held), other)
	pl.capped = slices.DeleteFunc(slices.Clone(pl.capped), other)
	pl.left = slices.DeleteFunc(slices.Clone(pl.left), func(w Weighed) bool { return w.Provider != o.Provider })
	return cs, pl
}

// promote is a stable partition of the existing plan. Resting and timed
// candidates still go through the ordinary probe and quota checks.
func (o *SessionOverrideTrace) promote(cs []candidate, pl planned) ([]candidate, planned) {
	if len(cs) < 2 || !o.has(cs) {
		return cs, pl
	}
	out := make([]candidate, 0, len(cs))
	order := make([]Weighed, 0, len(cs))
	for _, preferred := range []bool{true, false} {
		for i, c := range cs {
			if o.matches(c) == preferred {
				out, order = append(out, c), append(order, pl.order[i])
			}
		}
	}
	pl.order = order
	return out, pl
}

// Reasoning is opaque. The last account known to have answered the
// conversation is the only evidence available here about ordinary Responses
// reasoning. Sealed subagent messages and native checkpoints have separate
// checks before the override is applied.
func (o *SessionOverrideTrace) compatibleReasoning(scope string, in http.Header, from provider.Protocol, plain []byte, checkpoint *checkpointWriter, cs []candidate) bool {
	if from != provider.Responses || !bodyHas(plain, "encrypted_content") {
		return true
	}
	if checkpoint != nil {
		// Checkpoint readers have already been checked. Reasoning can be
		// present alongside the checkpoint and needs its own check.
		if b, ok := withoutCompaction(plain); ok {
			plain = b
		}
	}
	if len(seals(plain)) == 0 {
		return true
	}
	key := o.affinityKey(scope, in, plain)
	sticks.Lock()
	st, ok := stickOf(key)
	sticks.Unlock()
	if !ok || time.Since(st.at) > stickKeep {
		return false
	}
	o.writer = st
	return o.has(cs)
}

func sessionOverrideUnavailable(o *SessionOverrideTrace, pl planned, reason string) (int, string) {
	for _, row := range pl.left {
		if row.Provider != o.Provider {
			continue
		}
		switch {
		case row.Capped > 0 || row.NoCredits:
			return http.StatusTooManyRequests, "provider is at its usage limit"
		case row.Held || row.Barred:
			return http.StatusForbidden, "provider's accounts or keys are not allowed"
		}
	}
	if reason != "" {
		return http.StatusBadRequest, reason
	}
	p, err := provider.Find(o.Provider)
	if err != nil || p.ID != o.Provider {
		return http.StatusServiceUnavailable, "provider was removed"
	}
	if !p.On() {
		return http.StatusServiceUnavailable, "provider is disabled"
	}
	return http.StatusServiceUnavailable, "provider is not an available candidate for this request"
}

func sessionOverrideNote(h http.Header, agent, kind string, meta sessionMetadata) string {
	if codexOverrideChild(agent, kind, meta, h) && routingSessionID(meta.Parent) == "" &&
		routingSessionID(h.Get("x-codex-parent-thread-id")) == "" {
		return "subagent request does not identify its parent session"
	}
	return ""
}

func (s *Server) traceSessionRejection(r *http.Request, o *SessionOverrideTrace, call *Call, start time.Time) {
	if o == nil {
		return
	}
	o.skipped(call.Error)
	if o.Reason == "" {
		o.Reason = http.StatusText(call.Status)
	}
	tr := s.trace.beginFor(r, Route{Time: start, Agent: call.Agent, Session: sessionOf(r.Header),
		Kind: call.Kind, Model: call.Model, Provider: call.Provider, SessionOverride: o.snapshot(), Order: []Weighed{}})
	call.Route = tr.ID
	s.trace.update(tr, func(rt *Route) {
		rt.Done, rt.Status, rt.Error, rt.Millis = true, call.Status, call.Error, time.Since(start).Milliseconds()
	})
}

// recordNativeSessionRefusal links preflight failures on the direct Codex
// route to the Requests ledger when they carry an override decision.
func (s *Server) recordNativeSessionRefusal(r *http.Request, override *SessionOverrideTrace, call Call) {
	if override != nil {
		call.Millis = time.Since(call.Time).Milliseconds()
		s.traceSessionRejection(r, override, &call, call.Time)
		rec := usage.Record{RouteID: call.Route, Time: call.Time, Agent: call.Agent, Kind: call.Kind, Model: call.Model,
			Requested: call.Model, Millis: call.Millis, Status: call.Status, NativeSession: nativeSessionOf(r.Header), Rejected: true}
		failedWith(&rec, call.Status, call.Error, "")
		appendUsage(r, rec)
	}
	s.record(call)
}

// mediaCandidates applies the same capability checks to an override
// preflight and to the final plan. Its result keeps the plan rows aligned.
func (s *Server) mediaCandidates(cands []candidate, pl planned, from provider.Protocol, body []byte, described bool) ([]candidate, planned, string) {
	if !described {
		if _, currentImage := textOnlyBody(from, body); currentImage {
			var sees, unknown []candidate
			var seesAt, unknownAt []Weighed
			for i, c := range cands {
				in := membersImageInput([]provider.Member{{Provider: c.p, Model: c.model}})
				switch {
				case !s.blindTo(c.p.ID, c.model, in):
					sees, seesAt = append(sees, c), append(seesAt, pl.order[i])
				case in == nil:
					unknown, unknownAt = append(unknown, c), append(unknownAt, pl.order[i])
				}
			}
			cands, pl.order = sees, seesAt
			if len(sees) == 0 {
				cands, pl.order = unknown, unknownAt
			}
			if len(cands) == 0 {
				return cands, pl, "image"
			}
		}
	}
	if hasCurrentVideo(from, body) {
		var takes, unknown []candidate
		var takesAt, unknownAt []Weighed
		for i, c := range cands {
			switch v := videoInput(c.p.ID, c.model); {
			case v == nil:
				unknown, unknownAt = append(unknown, c), append(unknownAt, pl.order[i])
			case *v:
				takes, takesAt = append(takes, c), append(takesAt, pl.order[i])
			}
		}
		cands, pl.order = takes, takesAt
		if len(takes) == 0 {
			cands, pl.order = unknown, unknownAt
		}
		if len(cands) == 0 {
			return cands, pl, "video"
		}
	}
	return cands, pl, ""
}

func (s *Server) overrideEligibility(o *SessionOverrideTrace, cands []candidate, pl planned, isGroup bool, from provider.Protocol, body []byte, described bool, pin string) string {
	if isGroup {
		cands, pl, _ = s.mediaCandidates(cands, pl, from, body, described)
		if !o.has(cands) {
			return "image or video requirements exclude the provider"
		}
	}
	if pin != "" {
		cands, pl, _, _ = pinTo(pin, cands, pl)
		if !o.has(cands) {
			return "account pin excludes the provider"
		}
	}
	cands, _, _, _ = roomy(cands, pl, from, body, true)
	if !o.has(cands) {
		return "context-window requirements exclude the provider"
	}
	return ""
}
