# Teammate Agentd

> **Read this in another language: [English](README.en.md) | 中文**

> **Teammate 的 AI 代理守护进程（Agent Daemon）** — 一个轻量级、常驻后台的自研代理运行时，负责自主认领、执行并上报任务节点。

`agentd` 是 [Teammate](https://github.com/teammate/teammate) 多代理协作平台的独立客户端。它以常驻守护进程的方式运行在每台执行机上：以工作区的 daemon token 向 Server 注册机器，实时接收任务节点事件，物化 Server 下发的代理实例，自主认领并调用本地编码工具完成节点执行，随后提交 Git 结果并上报执行摘要与 Token 用量。

***

## 核心功能

- **多工作区连接** — 一台机器可同时接入多个工作区（每个工作区一条 `td_` daemon token），实例由 Server 下发、本地自动物化
- **自动认领** — 通过 SSE 实时接收新节点通知 + 默认每 60 秒轮询兜底，发现可用节点后自动认领
- **接力执行** — 当前节点完成后自动激活下一节点，多个代理可在同一工作流中接力工作
- **Git 集成** — 每个节点自动克隆/切分支、打节点开始 tag，完成节点后 commit + push，带唯一 tag 标识节点与尝试次数
- **上下文注入** — 自动注入任务描述、技能、MCP 服务器配置、相关评论历史到编码工具提示词
- **实例记忆** — 经本地 MCP server 向编码工具提供 `search/read/write_instance_memory` 与 `search_workspace_memory` 工具，按 persona 跨工作区共享，写入提示要求先查后写
- **执行隔离** — 工作目录按 `{root}/{workspaceID}/{agentID}/{projectID}/{taskID}` 隔离，多代理互不干扰
- **多编码工具适配** — 统一的 `Tool` 接口，支持多种编码工具的适配（见下）
- **本地控制面** — 默认启用的回路（loopback）HTTP 控制 API，允许同机进程查看状态、接管/交还/介入/人工完成节点
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

### 1. 在 Server Web 界面准备 daemon token 与代理实例

在 Teammate Web 界面进入工作区的「daemon 分组」创建 daemon，获得 **daemon token**（`td_` 前缀，仅展示一次，务必保存）；随后在同一分组下创建代理实例（选择 provider）。实例经 Server 下发（desired）由 agentd 自动物化，本地无需也无法手工创建实例。

### 2. 初始化配置文件

启动 agentd **前必须先有配置文件**（缺失时会报错 `config file not found ...; run 'teammate-agentd config init' to create one`）。

```bash
# 生成默认配置（默认路径 ~/.teammate/config.yaml）
go run ./cmd/teammate-agentd config init

# 指向 Teammate Server
go run ./cmd/teammate-agentd config set server.url http://YOUR_SERVER:8080

# 接入工作区（td_ 前缀的 daemon token）
go run ./cmd/teammate-agentd workspace add <td_xxxx...> --name team-a
```

`workspace_id` / `daemon_id` / 实例清单在注册成功后由 agentd 自动写回配置，**不要手工编辑**。一台机器可 `workspace add` 多个工作区。

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

### 4. 本地控制面

本地控制 API 默认启用（`local.enabled` 缺省为 true），监听 `127.0.0.1:17380`，凭据（`local_token` / `instance_id`）在首次启动时自动生成并写回配置。控制台页面：

```
http://127.0.0.1:17380/api/local/control/page?token=<local_token>
```

***

## 配置参考

配置文件路径：`~/.teammate/config.yaml`（可通过 `-c/--config` 指定其他路径）。

### 配置文件示例

```yaml
server:
  url: "http://YOUR_SERVER:8080"
name: "my-dev-machine"            # 可省略，缺省取主机名
workspace_root: "~/.teammate/workspaces"
workspaces:
  - name: team-a                  # 本地连接别名
    token: "td_xxxx_xxxxxxxxxxxx" # 该工作区的 daemon token
    # 以下三项注册成功后由 agentd 自动写回，勿手工编辑
    # workspace_id: "<uuid>"
    # daemon_id: "<uuid>"
    # agents:
    #   - name: claude-01
    #     provider: claude
    #     agent_id: "<uuid>"
tools:
  claude:
    path: "claude"                # Claude Code CLI 路径
git:
  base_branch: "master"           # 可省略，默认 master
local:
  enabled: true
  bind_addr: "127.0.0.1:17380"
  local_token: "lt_xxxx"          # 首次启动自动生成
  instance_id: "inst-xxxx"        # 首次启动自动生成
debug: false
```

### CLI 命令

| 命令 | 功能 |
| --- | --- |
| `teammate-agentd config init [--force]` | 生成默认配置文件（不覆盖已有文件，除非 `--force`） |
| `teammate-agentd config path` | 打印当前生效的配置文件路径 |
| `teammate-agentd config get [key]` | 显示配置；带 key 时输出单一配置项的值 |
| `teammate-agentd config set <key> <value>` | 设置某一配置项并写回 YAML |
| `teammate-agentd config list [--show-secrets]` | 列出全部配置，支持 `-o json/yaml` 输出，敏感字段默认打码 |
| `teammate-agentd config local enable` | 启用本地控制 API（自动生成 token / instance id） |
| `teammate-agentd config local disable` | 关闭本地控制 API |
| `teammate-agentd workspace add <td_token> --name <alias>` | 接入一个工作区 |
| `teammate-agentd workspace list` | 列出已接入的工作区连接 |
| `teammate-agentd workspace remove <name>` | 移除一个工作区连接 |
| `teammate-agentd agent list` | 列出已物化的代理实例 |
| `teammate-agentd version` | 打印版本号 |

全局参数：`-c/--config <path>`（指定配置文件）、`-o/--output <table|json|yaml>`。

### 配置项 Key

| Key | 类型 | 说明 |
| --- | --- | --- |
| `server.url` | string | Server 地址 |
| `name` | string | 机器名（注册时上报的 device_name 基础） |
| `workspace_root` | string | 工作目录根路径，默认 `~/.teammate/workspaces` |
| `git.base_branch` | string | Git 基线分支，默认 `master` |
| `local.enabled` | bool | 是否启用本地控制 API，默认 true |
| `local.bind_addr` | string | 本地控制监听地址，默认 `127.0.0.1:17380` |
| `local.local_token` | string | 本地控制访问 token（自动生成） |
| `local.instance_id` | string | 本地控制实例 ID（自动生成） |
| `debug` | bool | 开启调试日志 |
| `tools.<tool>.path` | string | 各编码工具 CLI 路径（如 `tools.claude.path`） |

工作区连接（`workspaces[]`）由 `workspace add/remove` 管理；`workspace_id` / `daemon_id` / `agents` 由 Server 下发后自动写回，勿手工编辑。

***

## 本地控制面 API

本地控制面默认监听 loopback 地址（`127.0.0.1:17380`）。除健康检查与控制台页面外均需携带 `X-Local-Token: <local_token>`（或 `Authorization: Bearer <local_token>`）。实例级端点按 `(connection, name)` 寻址；实例目录由 Server 拥有，本地只读/只操作，不能创建或删除实例。

**机器级端点**

| 方法 | 路径 | 功能 |
| --- | --- | --- |
| GET | `/api/local/health` | 健康检查 |
| GET | `/api/local/agents` | 全部已物化实例列表 |
| GET | `/api/local/connections` | 工作区连接列表（注册/SSE 状态） |
| DELETE | `/api/local/connections/{name}` | 移除一个连接 |
| GET | `/api/local/memory/{persona}` | 按 persona 查看实例记忆 |
| GET | `/api/local/chat/*` | 直聊会话（tools/sessions/session/state/send/stop/reset/logs） |
| GET | `/api/local/control/page` | 本地控制台 HTML（支持中英文切换） |

**实例级端点**（前缀 `/api/local/connections/{conn}/agents/{name}`）

| 方法 | 路径后缀 | 功能 |
| --- | --- | --- |
| GET | `/runtime/snapshot` | 实例运行时快照（运行状态、会话、当前节点） |
| GET | `/runtime/events` | 实例事件流（SSE） |
| GET | `/logs/recent` · `/logs/stream` | 最近日志 · 实时日志流（SSE） |
| POST | `/control/pause` · `/control/resume` | 暂停 / 恢复认领 |
| POST | `/control/soft-interrupt` | 接管（软中断，Server 无感知） |
| POST | `/control/intervene` | 向当前会话发送消息并执行一轮 |
| POST | `/control/handback` | 交还（触发恢复续会话执行） |
| POST | `/control/complete` | 人工完成当前节点 |

### 守护进程自动完成的操作

1. 启动时生成机器级 RSA 密钥对，探测本机已安装的编码工具（每 5 分钟重探测，变化即重新注册）
2. 用 daemon token 向 Server 注册（上报公钥与工具快照），换取会话 token；Server 下发的实例清单自动物化为本地运行时
3. 每 30 秒发送心跳，维持实例 `online` 状态；SSE 断线指数退避重连
4. 认领节点后自动：
   - 克隆 Git 仓库并 checkout 正确基线（凭据经 RSA 解密）
   - 创建特性分支：`teammate/task-{taskID}`
   - 打节点开始 tag：`teammate/task-{taskID}/node-{order}/attempt-{n}/start`
   - 注入任务上下文（任务描述、技能、MCP、评论历史；记忆经 MCP 工具按需查询）
   - 调用编码工具（Claude Code 等）执行任务，实时上报执行日志（经脱敏处理）
5. 执行完成后：自动 commit + push、打节点完成 tag、上报 Token 用量与执行摘要
6. 支持中断（Server `task:interrupt`）、驳回回滚（git reset 到目标节点基线）、超时、接管/交还等异常流程处理
7. 重启后自动恢复此前认领未完成的 in_progress 节点（续用持久化的工具会话）

***

## 项目结构

```
agentd/
├── cmd/
│   └── teammate-agentd/       # 守护进程入口（cobra）
│       ├── main.go            # 入口点 + 根命令
│       ├── config.go          # config 子命令（init/path/get/set/list/local）
│       ├── workspace.go       # workspace 子命令（add/list/remove）
│       ├── agent.go           # agent 子命令（list）
│       └── mcp.go             # mcp 子命令（实例记忆 MCP server）
├── internal/
│   ├── agent/                 # Agentd 客户端核心
│   │   ├── config.go          # 配置加载/校验/序列化（YAML）
│   │   ├── client.go          # Server HTTP 客户端（注册/心跳/认领/上报）
│   │   ├── supervisor.go      # 机器级编排（连接池/密钥/探测/本地控制面）
│   │   ├── connection.go      # 单工作区连接（注册循环/SSE/心跳接线）
│   │   ├── registration.go    # 注册报告组装
│   │   ├── reconcile.go       # Server 下发实例的物化
│   │   ├── runtime.go         # 实例运行时（SSE 事件分发/中断/回滚）
│   │   ├── watcher.go         # 节点轮询器（定时 + 事件触发 + 恢复）
│   │   ├── executor.go        # 任务执行器（Git 操作 + 工具调用 + 上下文构建 + 接管/介入）
│   │   ├── git.go             # GitManager（clone/checkout/branch/tag/push/credential）
│   │   ├── heartbeat.go       # 心跳发送器
│   │   ├── sse_client.go      # SSE 客户端（断线重连 + 指数退避）
│   │   ├── context.go         # 执行上下文构建（带优先级与可截断区段）
│   │   ├── memory.go          # 实例记忆存储（persona 跨工作区共享）
│   │   ├── mcp_server.go      # 实例记忆 MCP server（stdio）
│   │   ├── probe.go           # 编码工具探测循环
│   │   ├── chat.go            # 本地直聊管理器
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
├── web/                       # 本地控制台静态前端（与 Go 代码分离）
│   └── control/               # 控制台页面（index.html + assets/control.css|js）
├── test/
│   └── agent/                 # 集成测试（git / executor / context / 本地控制面 / 注册与 SSE 等）
├── docs/                      # 设计文档（中文，本地保留，不入库）
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