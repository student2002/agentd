# Teammate Agentd

> **Read this in another language: [English](README.en.md) | 中文**

> **Teammate 的 AI 代理守护进程（Agent Daemon）** — 一个轻量级、常驻后台的自研代理运行时，负责自主认领、执行并上报任务节点。

`agentd` 是 [Teammate](https://github.com/teammate/teammate) 多代理协作平台的独立客户端子模块。它以常驻守护进程的方式运行在每台执行机上，通过 API Token 向 Server 注册 Runtime，实时接收任务节点事件，自主认领并调用本地编码工具完成节点执行，随后提交 Git 结果并上报执行摘要与 Token 用量。

本项目是 `teammate` 仓库中 `agentd/` 子模块的独立落地形态，与 Server 端解耦，可单独编译部署。

***

## 核心功能

- **自动认领** — 通过 SSE 实时接收新节点通知 + 默认每 60 秒轮询，发现可用节点后自动认领
- **接力执行** — 当前节点完成后自动激活下一节点，多个代理可在同一工作流中接力工作
- **Git 集成** — 每个节点自动克隆/切分支、打节点开始 tag，完成节点后 commit + push，带唯一 tag 标识节点与尝试次数
- **上下文注入** — 自动注入任务描述、技能、MCP 服务器配置、相关评论历史、工程级共享记忆到编码工具提示词
- **执行隔离** — 工作目录按 `{root}/{agentID}/{workspaceID}/{projectID}/{taskID}` 隔离，多代理互不干扰
- **多编码工具适配** — 统一的 `Tool` 接口，支持多种编码工具的适配（见下）
- **本地控制面** — 可选的回路（loopback）HTTP 控制 API，允许同机进程查看状态、接管/交还/人工完成节点
- **安全脱敏** — 对编码工具输出进行敏感信息过滤（API 密钥、Bearer 令牌、AWS 密钥等），防止凭据泄露到服务端
- **凭据加密** — Git 凭据经 RSA-OAEP 加密后从 Server 获取，仅本机使用

***

## 支持的编码工具

| 工具 | 状态 | 说明 |
| --- | --- | --- |
| **Claude Code** (claude) | ✅ 已支持 | Anthropic 官方 CLI 编码工具 |
| **OpenClaw** (openclaw) | ✅ 已支持 | 开源编码工具 |
| **OpenCode** (opencode) | ✅ 已支持 | 开源编码工具 |
| **AtomCode** (atomcode) | ✅ 已支持 | AtomGit 出品的 AI 编码工具 |
| **MimoCode** (mimocode) | ✅ 已支持 | 开源编码工具 |

均通过统一的 `Tool` 接口适配，支持执行、中断、`stream-json` 输出解析、Token 用量提取及会话恢复（`--resume`）。

***

## 快速开始

### 环境依赖

- Go 1.26+

### 1. 注册代理并获取 Token

在 Teammate Web 界面进入「代理管理」→「注册新代理」，填写名称和编码工具类型。注册成功后会弹出 **API Token**（仅展示一次，务必保存）。

### 2. 初始化配置文件

启动 agentd **前必须先有配置文件**（缺失时会报错 `config file not found ...; run 'teammate-agentd config init' to create one`）。

```bash
# 生成默认配置（默认路径 ~/.teammate/config.yaml）
go run ./cmd/teammate-agentd config init

# 填写必要字段（注册代理时获取的 Token / Agent UUID / 工作区 UUID）
go run ./cmd/teammate-agentd config set server.api_token <token>
go run ./cmd/teammate-agentd config set agent.id <uuid>
go run ./cmd/teammate-agentd config set workspace.id <uuid>
```

也可用 `--profile <name>` 管理多个代理：

```bash
go run ./cmd/teammate-agentd config init --profile claude
go run ./cmd/teammate-agentd config init --profile atomcode
```

### 3. 编译并启动守护进程

本机开发调试可直接运行（与 Server 的 `go run` 方式一致）：

```bash
go run ./cmd/teammate-agentd
```

作为常驻守护进程部署（通常部署到独立机器并后台运行），编译为独立二进制：

```bash
go build -o teammate-agentd ./cmd/teammate-agentd
./teammate-agentd
```

### 4. （可选）启用本地控制面

本地控制 API 默认关闭。如需让同机进程读取 agentd 状态或进行接管操作：

```bash
teammate-agentd config local enable
```

启用后会生成 `local_token` 与 `instance_id`，控制面默认监听 `127.0.0.1:17380`。

***

## 配置参考

配置文件路径：`~/.teammate/config.yaml`（可通过 `-c/--config` 指定，或通过 `--profile` 使用 `~/.teammate/config-{profile}.yaml`）。

### 配置文件示例

```yaml
server:
  url: "http://YOUR_SERVER:8080"
  api_token: "tm_xxxx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

agent:
  id: "YOUR_AGENT_ID"        # 注册代理时返回的 UUID
  name: "My Agent"
  context_window: 100000     # 上下文窗口大小（tokens）
  provider: "claude"         # claude / openclaw / opencode / atomcode / mimocode

workspace:
  id: "YOUR_WORKSPACE_ID"
  root: "~/.teammate/workspaces"

tools:
  claude:
    path: "claude"           # Claude Code CLI 路径

git:
  base_branch: "master"      # 可省略，默认 master

local:
  enabled: true
  bind_addr: "127.0.0.1:17380"
  local_token: "tm_local_xxxx"
  instance_id: "497c...-uuid"
```

初始化生成的默认配置仅含 server / agent / workspace / tools 基础字段，其余缺省项在运行时自动填充默认值。

### CLI 命令

| 命令 | 功能 |
| --- | --- |
| `teammate-agentd config init [--force] [--profile <name>]` | 生成默认配置文件（不覆盖已有文件，除非 `--force`） |
| `teammate-agentd config path` | 打印当前生效的配置文件路径 |
| `teammate-agentd config get [key]` | 显示配置；带 key 时输出单一配置项的值 |
| `teammate-agentd config set <key> <value>` | 设置某一配置项并写回 YAML |
| `teammate-agentd config list [--show-secrets]` | 列出全部配置，支持 `-o json/yaml` 输出，敏感字段默认打码 |
| `teammate-agentd config local enable` | 启用本地控制 API（自动生成 token / instance id） |
| `teammate-agentd config local disable` | 关闭本地控制 API（保留绑定凭据） |
| `teammate-agentd version` | 打印版本号 |

全局参数：`-c/--config <path>`（指定配置文件）、`--profile <name>`（多代理配置）、`-o/--output <table|json|yaml>`。

### 配置项 Key

| Key | 类型 | 说明 |
| --- | --- | --- |
| `server.url` | string | Server 地址 |
| `server.api_token` | string | 注册代理时获取的 API Token |
| `agent.id` | string | 代理 UUID |
| `agent.name` | string | 代理名称 |
| `agent.context_window` | int | 上下文窗口（tokens），正整数 |
| `agent.provider` | string | 编码工具提供商，同上 5 种 |
| `workspace.id` | string | 工作区 UUID |
| `workspace.root` | string | 工作目录根路径，默认 `~/.teammate/workspaces` |
| `git.base_branch` | string | Git 基线分支，默认 `master` |
| `local.enabled` | bool | 是否启用本地控制 API |
| `local.bind_addr` | string | 本地控制监听地址，默认 `127.0.0.1:17380` |
| `local.local_token` | string | 本地控制访问 token |
| `local.instance_id` | string | 本地控制实例 UUID |
| `debug` | bool | 开启调试日志 |
| `tools.<tool>.path` | string | 各编码工具 CLI 路径（如 `tools.claude.path`） |

***

## 本地控制面 API

启用本地控制后，以下端点仅监听 loopback 地址（默认 `127.0.0.1:17380`）。除健康检查外均需携带 `Authorization: Bearer <local_token>`。

| 方法 | 路径 | 功能 |
| --- | --- | --- |
| GET | `/api/local/health` | 健康检查 |
| GET | `/api/local/runtime/snapshot` | 运行时快照（运行状态、会话、当前节点） |
| GET | `/api/local/events` | 本地事件流（SSE） |
| GET | `/api/local/logs/recent` | 最近日志 |
| GET | `/api/local/logs/stream` | 实时日志流（SSE） |
| POST | `/api/local/control/pause` | 暂停认领 |
| POST | `/api/local/control/resume` | 恢复认领 |
| POST | `/api/local/control/soft-interrupt` | 接管（软中断）当前节点 |
| POST | `/api/local/control/intervene` | 向当前会话发送消息并执行一轮 |
| POST | `/api/local/control/handback` | 交还（继续由原代理执行） |
| POST | `/api/local/control/complete` | 人工完成当前节点 |
| GET | `/api/local/control/page` | 本地控制面 HTML（支持中英文切换） |

### 守护进程自动完成的操作

1. 用 API Token 向 Server 注册 Runtime，换取 Session Token
2. 每 30 秒发送心跳，维持 `online` 状态
3. 每 60 秒轮询可认领的 pending 节点
4. 认领后自动：
   - 克隆 Git 仓库并 checkout 正确基线
   - 创建特性分支：`teammate/task-{taskID}`
   - 打节点开始 tag：`teammate/task-{taskID}/node-{order}/attempt-{n}/start`
   - 注入任务上下文（任务描述、技能、MCP、记忆、评论历史）
   - 调用编码工具（Claude Code 等）执行任务
   - 实时上报执行日志（经脱敏处理）
5. 执行完成后：
   - 自动 commit + push
   - 打节点完成 tag
   - 上报 Token 用量
   - 上报执行摘要
6. 支持中断、回退、超时等异常流程处理

***

## 项目结构

```
agentd/
├── cmd/
│   └── teammate-agentd/       # 守护进程入口（cobra）
│       ├── main.go            # 入口点 + 根命令
│       └── config.go          # config 子命令（init/path/get/set/list/local）
├── internal/
│   ├── agent/                 # Agentd 客户端核心
│   │   ├── config.go          # 配置加载/校验/序列化（YAML）
│   │   ├── client.go          # Server HTTP 客户端（注册/心跳/认领/上报）
│   │   ├── daemon.go          # 守护进程主循环（SSE 事件分发 + 组件编排）
│   │   ├── watcher.go         # 节点轮询器（定时 + 事件触发）
│   │   ├── executor.go        # 任务执行器（Git 操作 + 工具调用 + 上下文构建）
│   │   ├── git.go             # GitManager（clone/checkout/branch/tag/push/credential）
│   │   ├── heartbeat.go       # 心跳发送器
│   │   ├── sse_client.go      # SSE 客户端（断线重连 + 指数退避）
│   │   ├── context.go         # 执行上下文构建（带优先级与可截断区段）
│   │   ├── crypto.go          # RSA 非对称加密（Git 凭据解密）
│   │   ├── desensitize.go     # 日志脱敏
│   │   ├── local_auth.go      # 本地控制 token 校验与生成
│   │   ├── local_events.go    # 本地事件发布/订阅
│   │   ├── local_server.go    # 本地控制 HTTP 服务端
│   │   ├── local_state.go     # 本地模式运行状态
│   │   ├── mcp_config.go      # MCP 配置文件生成与生命周期
│   │   ├── capability_materializer.go # 技能/MCP 本地配置物化
│   │   ├── version.go         # 版本常量
│   │   └── tool/
│   │       └── tool.go        # 编码工具适配器（Claude/OpenClaw/OpenCode/AtomCode/MimoCode）
│   └── clock/                 # 可测试的时间抽象层
├── test/
│   └── agent/                 # 集成测试（git / executor / context / token 估算 / 本地控制面等）
├── docs/                      # 设计文档（中文）
│   ├── Agentd架构与守护进程设计.md
│   ├── Agentd任务执行与Git设计.md
│   └── Agentd本地控制面设计.md
├── go.mod / go.sum            # Go 模块依赖
└── README.md
```

***

## 运行测试

所有测试文件仅存放在 `test/` 目录下，按模块划分子目录。禁止在 `internal/`、`cmd/` 或其他业务目录中存放测试文件。

```bash
# 全量测试
go test ./...

# Agent 集成测试
go test ./test/agent/ -v -count=1

# process 模块测试
go test ./test/agent/process/ -v -count=1
```

***

## 技术栈

| 层 | 技术 |
| --- | --- |
| 语言 | Go 1.26+ |
| CLI | cobra |
| 配置 | gopkg.in/yaml.v3 |
| 加密 | crypto/rsa (RSA-OAEP) |
| 实时通信 | SSE (Server-Sent Events) |
| 事件流解析 | stream-json |

***

## 相关文档

完整的架构、任务执行与 Git、本地控制面设计见 [`docs/`](docs/) 目录下的中文设计文档。

***

## 许可证

本项目采用 MIT 许可证。