// probe.go runs the local coding-tool detection loop.
//
// The snapshot it produces is reported with every daemon registration: the
// server uses it to validate the provider of web-pushed agent instances.
// A background loop re-probes on an interval and signals the supervisor to
// re-register whenever the snapshot changes (provider added, removed, or its
// install state flipped).
package agent

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/teammate/agentd/internal/agent/tool"
)

// probeInterval is the re-detection interval of the tool snapshot.
const probeInterval = 5 * time.Minute

// ProbeManager probes the coding tools configured in the daemon config and
// maintains the latest snapshot.
type ProbeManager struct {
	tools   ToolsConfig
	factory func(provider, path string) tool.Tool

	mu       sync.Mutex
	snapshot []ProviderInfo
}

// NewProbeManager creates a probe manager over the daemon's tool paths.
func NewProbeManager(tools ToolsConfig) *ProbeManager {
	return &ProbeManager{
		tools:   tools,
		factory: tool.GetTool,
	}
}

// SetFactory replaces the tool adapter factory. Test-only seam.
func (p *ProbeManager) SetFactory(factory func(provider, path string) tool.Tool) {
	p.factory = factory
}

// ToolPathFor resolves a provider to its configured executable path.
func ToolPathFor(tools ToolsConfig, provider string) string {
	switch provider {
	case "claude":
		return tools.Claude.Path
	case "openclaw":
		return tools.OpenClaw.Path
	case "opencode":
		return tools.OpenCode.Path
	case "atomcode":
		return tools.AtomCode.Path
	case "mimocode":
		return tools.MiMoCode.Path
	default:
		return ""
	}
}

func (p *ProbeManager) toolPath(provider string) string {
	return ToolPathFor(p.tools, provider)
}

// Snapshot probes every supported provider once and stores the result.
// Version is left empty: the adapters expose install state only.
func (p *ProbeManager) Snapshot() []ProviderInfo {
	snapshot := make([]ProviderInfo, 0, len(SupportedProviders))
	for _, provider := range SupportedProviders {
		t := p.factory(provider, p.toolPath(provider))
		snapshot = append(snapshot, ProviderInfo{
			Provider:  provider,
			Installed: t.IsInstalled(),
		})
	}
	p.mu.Lock()
	p.snapshot = snapshot
	p.mu.Unlock()
	return snapshot
}

// LastSnapshot returns the most recent snapshot without re-probing.
func (p *ProbeManager) LastSnapshot() []ProviderInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProviderInfo(nil), p.snapshot...)
}

// Run re-probes on the fixed interval until ctx is cancelled. Whenever the
// snapshot differs from the previous one it invokes onChange so the caller
// can re-register with the fresh report.
func (p *ProbeManager) Run(ctx context.Context, onChange func(changed []ProviderInfo)) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			previous := p.LastSnapshot()
			current := p.Snapshot()
			if changed := diffProviderSnapshot(previous, current); len(changed) > 0 {
				log.Printf("[probe] tool snapshot changed: %s", formatProviderInfos(changed))
				if onChange != nil {
					onChange(changed)
				}
			}
		}
	}
}

// diffProviderSnapshot returns the entries whose provider set or install
// state differs between the two snapshots. Version changes are not tracked:
// adapters report no version.
func diffProviderSnapshot(oldSnapshot, newSnapshot []ProviderInfo) []ProviderInfo {
	oldByProvider := make(map[string]ProviderInfo, len(oldSnapshot))
	for _, info := range oldSnapshot {
		oldByProvider[info.Provider] = info
	}
	newByProvider := make(map[string]ProviderInfo, len(newSnapshot))
	for _, info := range newSnapshot {
		newByProvider[info.Provider] = info
	}

	var changed []ProviderInfo
	for provider, newInfo := range newByProvider {
		oldInfo, ok := oldByProvider[provider]
		if !ok || oldInfo.Installed != newInfo.Installed {
			changed = append(changed, newInfo)
		}
	}
	for provider, oldInfo := range oldByProvider {
		if _, ok := newByProvider[provider]; !ok {
			changed = append(changed, oldInfo)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Provider < changed[j].Provider })
	return changed
}

func formatProviderInfos(infos []ProviderInfo) string {
	parts := make([]string, 0, len(infos))
	for _, info := range infos {
		state := "removed"
		if info.Installed {
			state = "installed"
		} else {
			state = "uninstalled"
		}
		parts = append(parts, info.Provider+"="+state)
	}
	return joinStrings(parts, ", ")
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += sep
		}
		out += item
	}
	return out
}
