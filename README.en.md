# Teammate Agentd

> **Read this in another language: [English](README.en.md) | [中文](README.md)**

> **The AI Agent Daemon of Teammate** — a lightweight, resident background agent runtime that autonomously claims, executes, and reports task nodes.

`agentd` is the standalone client sub-module of the [Teammate](https://github.com/teammate/teammate) multi-agent collaboration platform. It runs as a resident daemon on each execution machine, registers the Runtime with the Server using an API Token, receives real-time task node events, autonomously claims nodes and invokes the local coding tool to complete execution, then commits the Git result and reports the execution summary and token usage.

This project is the self-contained realization of the `agentd/` sub-module in the `teammate` repository: it is decoupled from the Server and can be compiled and deployed independently.

***

## Core Features

- **Autonomous claiming** — Receives new node notifications in real time via SSE, plus bounds pending nodes by default every 60 seconds, and claims available nodes automatically.
- **Relay execution** — Automatically activates the next node after the current one finishes; multiple agents can work in relay within a single workflow.
- **Git integration** — Clones/checks out branches automatically per node, tags the node start, then commits + pushes on completion, with unique tags identifying both the node and attempt count.
- **Context injection** — Automatically injects task description, skills, MCP server config, relevant comment history, and project-level shared memory into the coding tool prompt.
- **Execution isolation** — Workspaces are isolated by `{root}/{agentID}/{workspaceID}/{projectID}/{taskID}`, so multiple agents never interfere with each other.
- **Multiple coding tool adapters** — A unified `Tool` interface supports several coding tools (see below).
- **Local control surface** — An optional loopback HTTP control API lets same-machine processes inspect state and take over / hand back / manually complete nodes.
- **Secure desensitization** — Filters sensitive information (API keys, Bearer tokens, AWS keys, etc.) from coding tool output so credentials never leak to the Server.
- **Credential encryption** — Git credentials are fetched from the Server after RSA-OAEP encryption and used only on this machine.

***

## Supported Coding Tools

| Tool | Status | Description |
| --- | --- | --- |
| **Claude Code** (claude) | ✅ Supported | Anthropic's official CLI coding tool |
| **OpenClaw** (openclaw) | ✅ Supported | Open-source coding tool |
| **OpenCode** (opencode) | ✅ Supported | Open-source coding tool |
| **AtomCode** (atomcode) | ✅ Supported | AI coding tool by AtomGit |
| **MimoCode** (mimocode) | ✅ Supported | Open-source coding tool |

All are adapted through the unified `Tool` interface, supporting execution, interrupt, `stream-json` output parsing, token usage extraction, and session recovery (`--resume`).

***

## Quick Start

### Prerequisites

- Go 1.26+

### 1. Register an agent and obtain a token

In the Teammate web UI go to **Agents → Register a new agent**, enter a name and coding tool type. An **API Token** is shown once after registration (be sure to save it).

### 2. Initialize the config file

`agentd` **must have a config file before starting** (otherwise it errors with `config file not found ...; run 'teammate-agentd config init' to create one`).

```bash
# Generate the default config (default path ~/.teammate/config.yaml)
go run ./cmd/teammate-agentd config init

# Fill in the required fields (API token / agent UUID / workspace UUID obtained at registration)
go run ./cmd/teammate-agentd config set server.api_token <token>
go run ./cmd/teammate-agentd config set agent.id <uuid>
go run ./cmd/teammate-agentd config set workspace.id <uuid>
```

You can also manage multiple agents with `--profile <name>`:

```bash
go run ./cmd/teammate-agentd config init --profile claude
go run ./cmd/teammate-agentd config init --profile atomcode
```

### 3. Build and start the daemon

For local development, run directly (same as the Server's `go run` approach):

```bash
go run ./cmd/teammate-agentd
```

To deploy it as a resident daemon (typically on a separate machine in the background), compile it into a standalone binary:

```bash
go build -o teammate-agentd ./cmd/teammate-agentd
./teammate-agentd
```

### 4. (Optional) Enable the local control surface

The local control API is disabled by default. To let a same-machine process read `agentd` state or take control actions:

```bash
teammate-agentd config local enable
```

This generates a `local_token` and `instance_id`, and the control surface listens on `127.0.0.1:17380` by default.

***

## Configuration Reference

Config file path: `~/.teammate/config.yaml` (override with `-c/--config`, or use `~/.teammate/config-{profile}.yaml` with `--profile`).

### Example Config File

```yaml
server:
  url: "http://YOUR_SERVER:8080"
  api_token: "tm_xxxx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

agent:
  id: "YOUR_AGENT_ID"        # UUID returned at agent registration
  name: "My Agent"
  context_window: 100000     # context window size (tokens)
  provider: "claude"         # claude / openclaw / opencode / atomcode / mimocode

workspace:
  id: "YOUR_WORKSPACE_ID"
  root: "~/.teammate/workspaces"

tools:
  claude:
    path: "claude"           # Claude Code CLI path

git:
  base_branch: "master"      # optional, default master

local:
  enabled: true
  bind_addr: "127.0.0.1:17380"
  local_token: "tm_local_xxxx"
  instance_id: "497c...-uuid"
```

The generated default config contains only the basic server / agent / workspace / tools fields; omitted fields are filled with defaults automatically at runtime.

### CLI Commands

| Command | Description |
| --- | --- |
| `teammate-agentd config init [--force] [--profile <name>]` | Generate a default config file (does not overwrite an existing file unless `--force`) |
| `teammate-agentd config path` | Print the active config file path |
| `teammate-agentd config get [key]` | Show config; with a key, print the value of that single key |
| `teammate-agentd config set <key> <value>` | Set a config key and write it back to YAML |
| `teammate-agentd config list [--show-secrets]` | List all config values; supports `-o json/yaml`; secrets masked by default |
| `teammate-agentd config local enable` | Enable the local control API (auto-generates token / instance id) |
| `teammate-agentd config local disable` | Disable the local control API (keeps binding credentials) |
| `teammate-agentd version` | Print the version |

Global flags: `-c/--config <path>` (specify config file), `--profile <name>` (multi-agent config), `-o/--output <table|json|yaml>`.

### Config Keys

| Key | Type | Description |
| --- | --- | --- |
| `server.url` | string | Server address |
| `server.api_token` | string | API Token obtained at agent registration |
| `agent.id` | string | Agent UUID |
| `agent.name` | string | Agent name |
| `agent.context_window` | int | Context window (tokens), positive integer |
| `agent.provider` | string | Coding tool provider, one of the 5 listed above |
| `workspace.id` | string | Workspace UUID |
| `workspace.root` | string | Workspace root path, default `~/.teammate/workspaces` |
| `git.base_branch` | string | Git base branch, default `master` |
| `local.enabled` | bool | Whether the local control API is enabled |
| `local.bind_addr` | string | Local control listen address, default `127.0.0.1:17380` |
| `local.local_token` | string | Local control access token |
| `local.instance_id` | string | Local control instance UUID |
| `debug` | bool | Enable debug logging |
| `tools.<tool>.path` | string | CLI path of each coding tool (e.g. `tools.claude.path`) |

***

## Local Control API

Once local control is enabled, the following endpoints listen only on the loopback address (default `127.0.0.1:17380`). Except for health check, all require `Authorization: Bearer <local_token>`.

| Method | Path | Description |
| --- | --- | --- |
| GET | `/api/local/health` | Health check |
| GET | `/api/local/runtime/snapshot` | Runtime snapshot (status, session, current node) |
| GET | `/api/local/events` | Local event stream (SSE) |
| GET | `/api/local/logs/recent` | Recent logs |
| GET | `/api/local/logs/stream` | Real-time log stream (SSE) |
| POST | `/api/local/control/pause` | Pause claiming |
| POST | `/api/local/control/resume` | Resume claiming |
| POST | `/api/local/control/soft-interrupt` | Take over (soft interrupt) the current node |
| POST | `/api/local/control/intervene` | Send a message to the current session and execute one round |
| POST | `/api/local/control/handback` | Hand back (let the original agent continue execution) |
| POST | `/api/local/control/complete` | Manually complete the current node |
| GET | `/api/local/control/page` | Local control HTML page (supports Chinese/English switching) |

### Operations the Daemon Performs Automatically

1. Registers the Runtime with the Server using the API Token and obtains a Session Token
2. Sends a heartbeat every 30 seconds to stay `online`
3. Polls claimable pending nodes every 60 seconds
4. On claim, automatically:
   - Clones the Git repository and checks out the correct base
   - Creates a feature branch: `teammate/task-{taskID}`
   - Tags the node start: `teammate/task-{taskID}/node-{order}/attempt-{n}/start`
   - Injects task context (task description, skills, MCP, memory, comment history)
   - Invokes the coding tool (Claude Code, etc.) to execute the task
   - Reports execution logs in real time (desensitized)
5. On completion:
   - Automatically commits + pushes
   - Tags the node completion
   - Reports token usage
   - Reports the execution summary
6. Supports interrupt, rollback, timeout, and other exceptional flows

***

## Project Structure

```
agentd/
├── cmd/
│   └── teammate-agentd/       # daemon entry point (cobra)
│       ├── main.go            # entry point + root command
│       └── config.go          # config sub-commands (init/path/get/set/list/local)
├── internal/
│   ├── agent/                 # Agentd client core
│   │   ├── config.go          # config loading/validation/serialization (YAML)
│   │   ├── client.go          # Server HTTP client (register/heartbeat/claim/report)
│   │   ├── daemon.go          # daemon main loop (SSE event dispatch + component orchestration)
│   │   ├── watcher.go         # node poller (timer + event triggered)
│   │   ├── executor.go        # task executor (Git ops + tool invocation + context building)
│   │   ├── git.go             # GitManager (clone/checkout/branch/tag/push/credential)
│   │   ├── heartbeat.go       # heartbeat sender
│   │   ├── sse_client.go      # SSE client (reconnect with exponential backoff)
│   │   ├── context.go         # execution context building (priority + truncatable sections)
│   │   ├── crypto.go          # RSA asymmetric encryption (Git credential decryption)
│   │   ├── desensitize.go     # log desensitization
│   │   ├── local_auth.go      # local control token validation and generation
│   │   ├── local_events.go    # local event publish/subscribe
│   │   ├── local_server.go    # local control HTTP server
│   │   ├── local_state.go     # local mode runtime state
│   │   ├── mcp_config.go      # MCP config file generation and lifecycle
│   │   ├── capability_materializer.go # skill/MCP local config materialization
│   │   ├── version.go         # version constant
│   │   └── tool/
│   │       └── tool.go        # coding tool adapters (Claude/OpenClaw/OpenCode/AtomCode/MimoCode)
│   └── clock/                 # testable time abstraction
├── test/
│   └── agent/                 # integration tests (git / executor / context / token estimation / local control, etc.)
├── docs/                      # design docs (Chinese)
│   ├── Agentd架构与守护进程设计.md
│   ├── Agentd任务执行与Git设计.md
│   └── Agentd本地控制面设计.md
├── go.mod / go.sum            # Go module dependencies
└── README.md
```

***

## Running Tests

All test files are kept in the `test/` directory only, organized into subdirectories by module. Do not place test files in `internal/`, `cmd/`, or other business directories.

```bash
# Run all tests
go test ./...

# Agent integration tests
go test ./test/agent/ -v -count=1

# process module tests
go test ./test/agent/process/ -v -count=1
```

***

## Tech Stack

| Layer | Technology |
| --- | --- |
| Language | Go 1.26+ |
| CLI | cobra |
| Config | gopkg.in/yaml.v3 |
| Crypto | crypto/rsa (RSA-OAEP) |
| Real-time comms | SSE (Server-Sent Events) |
| Event stream parsing | stream-json |

***

## Related Documentation

See the Chinese design documents under [`docs/`](docs/) for full architecture, task execution & Git, and local control surface design.

***

## License

This project is licensed under the MIT License.