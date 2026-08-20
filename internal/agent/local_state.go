// local_state.go 管理本地模式的运行状态。
package agent

import (
	"fmt"
	"sync"
	"time"
)

const (
	LocalRuntimeOffline = "offline"
	LocalRuntimeOnline  = "online"
	LocalRuntimeError   = "error"

	LocalAgentOnline = "online"
	LocalAgentBusy   = "busy"
	LocalAgentPaused = "paused"

	LocalExecutionIdle        = "idle"
	LocalExecutionRunning     = "running"
	LocalExecutionCompleted   = "completed"
	LocalExecutionInterrupted = "interrupted"
	LocalExecutionFailed      = "failed"
	LocalExecutionIntervening = "intervening"

	LocalToolDisconnected = "disconnected"
	LocalToolConnected    = "connected"
	LocalToolConnecting   = "connecting"
)

type LocalStateConfig struct {
	InstanceID  string
	Profile     string
	ServerURL   string
	WorkspaceID string
	AgentID     string
	AgentName   string
	Provider    string
}

type LocalSnapshot struct {
	InstanceID       string                `json:"instance_id"`
	CapturedAt       time.Time             `json:"captured_at"`
	Config           LocalSnapshotConfig   `json:"config"`
	Runtime          LocalRuntimeStatus    `json:"runtime"`
	Agent            LocalAgentStatus      `json:"agent"`
	ExecutionSession LocalExecutionSession `json:"execution_session"`
	Tool             LocalToolStatus       `json:"tool"`
	LastError        LocalError            `json:"last_error"`
}

type LocalSnapshotConfig struct {
	Profile     string `json:"profile"`
	ServerURL   string `json:"server_url"`
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	Provider    string `json:"provider"`
}

type LocalRuntimeStatus struct {
	ID                 string    `json:"id"`
	DaemonID           string    `json:"daemon_id"`
	Status             string    `json:"status"`
	LastHeartbeatAt    time.Time `json:"last_heartbeat_at"`
	LastHeartbeatError string    `json:"last_heartbeat_error"`
	SSEConnected       bool      `json:"sse_connected"`
	SSELastEventID     string    `json:"sse_last_event_id"`
}

type LocalAgentStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Paused bool   `json:"paused"`
}

type LocalExecutionSession struct {
	Status               string    `json:"status"`
	TaskID               int32     `json:"task_id"`
	NodeID               string    `json:"node_id"`
	NodeName             string    `json:"node_name"`
	StartedAt            time.Time `json:"started_at"`
	Tool                 string    `json:"tool"`
	ToolSessionIDPresent bool      `json:"tool_session_id_present"`
	WorkDir              string    `json:"workdir"`
}

type LocalToolStatus struct {
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	LastError string `json:"last_error"`
}

type LocalError struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	At      *time.Time `json:"at"`
}

type LocalStateStore struct {
	mu       sync.RWMutex
	snapshot LocalSnapshot
}

func NewLocalStateStore(cfg LocalStateConfig) *LocalStateStore {
	return &LocalStateStore{
		snapshot: LocalSnapshot{
			InstanceID: cfg.InstanceID,
			CapturedAt: time.Now().UTC(),
			Config: LocalSnapshotConfig{
				Profile:     cfg.Profile,
				ServerURL:   cfg.ServerURL,
				WorkspaceID: cfg.WorkspaceID,
				AgentID:     cfg.AgentID,
				AgentName:   cfg.AgentName,
				Provider:    cfg.Provider,
			},
			Runtime: LocalRuntimeStatus{
				Status: LocalRuntimeOffline,
			},
			Agent: LocalAgentStatus{
				ID:     cfg.AgentID,
				Status: LocalAgentOnline,
			},
			ExecutionSession: LocalExecutionSession{
				Status: LocalExecutionIdle,
			},
			Tool: LocalToolStatus{
				Provider: cfg.Provider,
				Status:   LocalToolDisconnected,
			},
		},
	}
}

func (s *LocalStateStore) Snapshot() LocalSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := s.snapshot
	if s.snapshot.LastError.At != nil {
		at := *s.snapshot.LastError.At
		snapshot.LastError.At = &at
	}
	return snapshot
}

func (s *LocalStateStore) SetRuntimeRegistered(runtimeID string, daemonID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Runtime.ID = runtimeID
	s.snapshot.Runtime.DaemonID = daemonID
	s.snapshot.Runtime.Status = LocalRuntimeOnline
	s.touchLocked()
}

func (s *LocalStateStore) SetSSEConnected(lastEventID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Runtime.SSEConnected = true
	s.snapshot.Runtime.SSELastEventID = lastEventID
	s.touchLocked()
}

func (s *LocalStateStore) SetSSEDisconnected(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Runtime.SSEConnected = false
	if err != nil {
		s.snapshot.Runtime.SSELastEventID = ""
	}
	s.touchLocked()
}

func (s *LocalStateStore) SetHeartbeatSuccess(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Runtime.LastHeartbeatAt = at.UTC()
	s.snapshot.Runtime.LastHeartbeatError = ""
	if s.snapshot.Runtime.Status != LocalRuntimeError {
		s.snapshot.Runtime.Status = LocalRuntimeOnline
	}
	s.touchLocked()
}

func (s *LocalStateStore) SetHeartbeatError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.snapshot.Runtime.LastHeartbeatError = err.Error()
	} else {
		s.snapshot.Runtime.LastHeartbeatError = ""
	}
	s.touchLocked()
}

func (s *LocalStateStore) SetExecutionStarted(session LocalExecutionSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.StartedAt.IsZero() {
		session.StartedAt = time.Now().UTC()
	}
	session.Status = LocalExecutionRunning
	s.snapshot.Agent.Status = LocalAgentBusy
	s.snapshot.ExecutionSession = session
	s.touchLocked()
}

func (s *LocalStateStore) SetExecutionCompleted(taskID int32, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.ExecutionSession.TaskID == taskID && s.snapshot.ExecutionSession.NodeID == nodeID {
		s.snapshot.ExecutionSession.Status = LocalExecutionCompleted
	}
	s.snapshot.Agent.Status = LocalAgentOnline
	s.touchLocked()
}

func (s *LocalStateStore) SetExecutionInterrupted(taskID int32, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.ExecutionSession.TaskID == taskID && s.snapshot.ExecutionSession.NodeID == nodeID {
		s.snapshot.ExecutionSession.Status = LocalExecutionInterrupted
	}
	s.snapshot.Agent.Status = LocalAgentOnline
	s.touchLocked()
}

// SetExecutionIntervening marks the node as locally taken over by a human;
// the server still sees in_progress.
func (s *LocalStateStore) SetExecutionIntervening(taskID int32, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.ExecutionSession.TaskID == taskID && s.snapshot.ExecutionSession.NodeID == nodeID {
		s.snapshot.ExecutionSession.Status = LocalExecutionIntervening
	}
	s.touchLocked()
}

// SetExecutionHandback flips the session back to running after a human hands
// control back to the agent.
func (s *LocalStateStore) SetExecutionHandback(taskID int32, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.ExecutionSession.TaskID == taskID && s.snapshot.ExecutionSession.NodeID == nodeID {
		s.snapshot.ExecutionSession.Status = LocalExecutionRunning
	}
	s.touchLocked()
}

// SetPaused records the watcher's paused flag in the snapshot so the local
// control UI can render the pause state.
func (s *LocalStateStore) SetPaused(paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Agent.Paused = paused
	s.touchLocked()
}

func (s *LocalStateStore) SetExecutionFailed(taskID int32, nodeID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.ExecutionSession.TaskID == taskID && s.snapshot.ExecutionSession.NodeID == nodeID {
		s.snapshot.ExecutionSession.Status = LocalExecutionFailed
	}
	s.snapshot.Agent.Status = LocalAgentOnline
	now := time.Now().UTC()
	message := ""
	if err != nil {
		message = err.Error()
	}
	s.snapshot.LastError = LocalError{
		Code:    "tool_execution_failed",
		Message: message,
		At:      &now,
	}
	s.touchLocked()
}

func (s *LocalStateStore) SetToolStatus(provider string, status string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Tool.Provider = provider
	s.snapshot.Tool.Status = status
	if err != nil {
		s.snapshot.Tool.LastError = err.Error()
	} else {
		s.snapshot.Tool.LastError = ""
	}
	s.touchLocked()
}

func (s *LocalStateStore) SetRuntimeError(code string, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.snapshot.Runtime.Status = LocalRuntimeError
	s.snapshot.LastError = LocalError{
		Code:    code,
		Message: message,
		At:      &now,
	}
	s.touchLocked()
}

func (s *LocalStateStore) SetError(code string, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if message == "" && code != "" {
		message = fmt.Sprintf("%s occurred", code)
	}
	s.SetRuntimeError(code, message)
}

func (s *LocalStateStore) SetLastError(code string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	message := ""
	if err != nil {
		message = err.Error()
	}
	s.snapshot.LastError = LocalError{
		Code:    code,
		Message: message,
		At:      &now,
	}
	s.touchLocked()
}

func (s *LocalStateStore) touchLocked() {
	s.snapshot.CapturedAt = time.Now().UTC()
}
