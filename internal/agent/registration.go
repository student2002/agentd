// registration.go builds and submits the daemon registration report.
//
// Registration is idempotent and re-entrant: the report carries machine
// information only, and the response delivers the pending instances waiting
// for local materialization. The retry loop lives in the Supervisor, which
// owns the daemon lifecycle.
package agent

import (
	"context"
	"os"
)

// Registrar submits the daemon registration report built by reportFn.
type Registrar struct {
	client  *Client
	reportFn func() RegisterDaemonReport
}

// NewRegistrar creates a registrar. reportFn must return the full current
// report (tool snapshot + daemon identity) on every call.
func NewRegistrar(client *Client, reportFn func() RegisterDaemonReport) *Registrar {
	return &Registrar{client: client, reportFn: reportFn}
}

// Register submits one registration and returns the desired set for
// materialization.
func (r *Registrar) Register(ctx context.Context) (*RegisterDaemonResponse, error) {
	return r.client.RegisterDaemon(ctx, r.reportFn())
}

// DeviceName resolves the reported machine name: the configured top-level
// name, falling back to the hostname.
func DeviceName(configured string) string {
	if configured != "" {
		return configured
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		return hostname
	}
	return "unknown-device"
}
