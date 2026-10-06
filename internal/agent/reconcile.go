// reconcile.go reconciles the owning connection's local catalog against the
// server's declared full instance set.
//
// The desired-agents set (returned by every register and heartbeat response)
// is the daemon's COMPLETE instance set — the declarative expectation. The
// reconciler performs a three-way pass against entry.Agents: new instances
// are materialized (name/provider/persona plus the delivered UUID as
// agent_id, persisted to the YAML, runtime created, identity bound); changed
// agent_id/persona_key values are overwritten (the server delivery is
// authoritative — a changed UUID means the instance was deleted web-side and
// re-created, a changed persona arrives after a backfill); instances present
// locally but absent from the full set (deleted web-side) are removed from
// the entry, persisted, and their runtime torn down (an in-flight execution
// is not interrupted — it finishes on the old executor).
//
// Delivery is guarded: items without an agent_id or persona_key, with a
// provider this build does not know, or with a provider not installed per
// the probe snapshot are skipped with a log line; the row stays in the
// server's set and is re-delivered on later heartbeats until the guard
// passes.
package agent

import (
	"log"
)

// ReconcilerHooks are the supervisor actions taken during reconciliation.
// The hooks are bound to one connection (the supervisor closes over the
// connection name), so an instance name addresses the (connection, instance)
// runtime.
type ReconcilerHooks struct {
	// ProviderInstalled reports whether the provider passes the local probe
	// snapshot. Uninstalled providers are not materialized.
	ProviderInstalled func(provider string) bool
	// EnsureRuntime creates (or fetches) the local runtime for a delivered
	// instance name on the owning connection.
	EnsureRuntime func(name string)
	// ResolveIdentity binds the delivered agent UUID as the instance's
	// identity in the owning connection and starts its watcher.
	ResolveIdentity func(name, agentID string)
	// RemoveRuntime tears down the local runtime of an instance pruned from
	// the full set (web-side deletion). Watchers stop; an in-flight execution
	// is left to finish.
	RemoveRuntime func(name string)
}

// Reconciler materializes desired agents into cfg and persists it through
// saveFn. entry is the connection's workspace entry in cfg: the reconciler
// mutates it in place. saveFn serializes every persist through the
// supervisor's single-point config lock.
type Reconciler struct {
	cfg    *GlobalConfig
	entry  *WorkspaceEntry
	saveFn func() error
	hooks  ReconcilerHooks
}

// NewReconciler creates a reconciler bound to one workspace connection.
func NewReconciler(cfg *GlobalConfig, entry *WorkspaceEntry, saveFn func() error, hooks ReconcilerHooks) *Reconciler {
	return &Reconciler{cfg: cfg, entry: entry, saveFn: saveFn, hooks: hooks}
}

// Reconcile consumes one desired-agents full set.
func (r *Reconciler) Reconcile(desired []DesiredAgent) {
	present := make(map[string]bool, len(desired))
	for _, d := range desired {
		if d.Name == "" {
			continue
		}
		if d.AgentID == "" {
			log.Printf("[reconcile] skipped desired agent %q: delivery carries no agent_id", d.Name)
			continue
		}
		if !isValidProvider(d.Provider) {
			log.Printf("[reconcile] skipped desired agent %q: unsupported provider %q", d.Name, d.Provider)
			continue
		}
		if r.hooks.ProviderInstalled != nil && !r.hooks.ProviderInstalled(d.Provider) {
			log.Printf("[reconcile] skipped desired agent %q: provider %q is not installed locally", d.Name, d.Provider)
			continue
		}
		if d.PersonaKey == "" {
			log.Printf("[reconcile] skipped desired agent %q: delivery carries no persona_key", d.Name)
			continue
		}
		present[d.Name] = true

		existing := r.entry.Agent(d.Name)
		appended := false
		previousID := ""
		previousPersona := ""
		changed := false
		if existing == nil {
			r.entry.Agents = append(r.entry.Agents, WorkspaceAgent{Name: d.Name, Provider: d.Provider, PersonaKey: d.PersonaKey, AgentID: d.AgentID})
			appended = true
			changed = true
		} else {
			if existing.AgentID != d.AgentID {
				// The server delivery is authoritative: a changed UUID means the
				// instance row was deleted web-side and re-created.
				previousID = existing.AgentID
				existing.AgentID = d.AgentID
				changed = true
			}
			if existing.PersonaKey != d.PersonaKey {
				previousPersona = existing.PersonaKey
				existing.PersonaKey = d.PersonaKey
				changed = true
			}
		}
		if changed {
			if err := r.saveFn(); err != nil {
				// Roll back the in-memory mutations so the next round retries cleanly.
				if appended {
					if idx := len(r.entry.Agents) - 1; idx >= 0 && r.entry.Agents[idx].Name == d.Name {
						r.entry.Agents = r.entry.Agents[:idx]
					}
				} else if existing := r.entry.Agent(d.Name); existing != nil {
					existing.AgentID = previousID
					existing.PersonaKey = previousPersona
				}
				log.Printf("[reconcile] ERROR: failed to persist delivered agent %q: %v", d.Name, err)
				continue
			}
			log.Printf("[reconcile] materialized agent name=%s provider=%s workspace=%s persona=%s agent_id=%s", d.Name, d.Provider, r.entry.Name, d.PersonaKey, d.AgentID)
		}
		// Runtime creation and identity binding run even without config
		// changes: after a restart the config is already materialized but the
		// identity must be rebound before the watcher can start.
		if r.hooks.EnsureRuntime != nil {
			r.hooks.EnsureRuntime(d.Name)
		}
		if r.hooks.ResolveIdentity != nil {
			r.hooks.ResolveIdentity(d.Name, d.AgentID)
		}
	}

	r.pruneAbsent(present)
}

// pruneAbsent removes local instances the full set no longer declares
// (deleted web-side): entry row dropped, persisted, runtime torn down.
func (r *Reconciler) pruneAbsent(present map[string]bool) {
	kept := r.entry.Agents[:0]
	pruned := make([]string, 0)
	for _, a := range r.entry.Agents {
		if present[a.Name] {
			kept = append(kept, a)
			continue
		}
		pruned = append(pruned, a.Name)
	}
	r.entry.Agents = kept
	if len(pruned) == 0 {
		return
	}
	if err := r.saveFn(); err != nil {
		log.Printf("[reconcile] ERROR: failed to persist pruned agents %v: %v", pruned, err)
		return
	}
	for _, name := range pruned {
		if r.hooks.RemoveRuntime != nil {
			r.hooks.RemoveRuntime(name)
		}
		log.Printf("[reconcile] pruned agent name=%s workspace=%s (absent from the delivered full set)", name, r.entry.Name)
	}
}
