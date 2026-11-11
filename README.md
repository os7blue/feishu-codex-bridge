# Feishu Codex Bridge

简体中文 | [English](README.en.md)

让 AI 完成开发，让 AI 持续迭代，让 AI 协助维护。Feishu Codex Bridge 把飞书变成 Codex 的协作入口：从提出需求、实现功能到测试、审阅和维护准备，在聊天中推进完整的开发流程。

本项目由 AI 编程代理完成实现，并持续通过 AI 进行功能迭代、问题排查、文档整理和维护工作。人负责方向、审阅与授权；AI 在明确边界内执行，可追溯的代码、验证结果和手动审批保留在流程中。

通过飞书私聊切换项目和会话、调整模型配置、审阅操作，并取回文件与完整成果。

桥接使用飞书官方 SDK 长连接与本机 Codex App Server。会话、任务、审批和文件逻辑独立于前端；当前提供飞书适配器，便于后续接入其他界面。

## 功能

- **会话话题**：主聊天保留当前完整回复，随后附飞书原生话题记录卡片；同一会话的对话、审批记录和成果集中留存。
- **多项目、多会话**：通过默认会话、引用消息或显式编号定位任务。
- **任务控制**：查看队列摘要和时间，取消单条等待任务或请求停止当前任务。
- **独立模型配置**：支持桥接全局默认与会话独立配置，区分当前轮和下一轮。
- **审批与回答**：保留操作详情，生成可编辑草稿，手动发送后才提交。
- **附件与成果**：接收文件、图片，取回项目文件和完整长结果。
- **交接接续**：在新会话中沿用项目、交接文档和完整模型配置。
- **恢复与维护**：持久化接收及发送记录，重启不自动重放任务；更新支持空闲检查、就绪核验和回退。

仅模型推理请求串行执行；消息收发、事件处理、本地控制和工具不要求单线程。

## 快速开始

### 1. 准备环境

- Go 1.24 或更新版本，以及 Node.js（用于项目运行器和维护脚本）。
- 已安装并完成所需认证或提供商配置的 Codex CLI，支持 `codex app-server --listen stdio://`。
- 已存在的项目目录，以及具备机器人能力的飞书应用。
- 飞书应用启用长连接，配置消息接收事件 `im.message.receive_v1`、卡片回调 `card.action.trigger`，并开通所需消息和文件资源权限。原生命令面板为可选功能。

当前维护流程面向 Linux。无需另建 HTTP 回调服务；运行环境需要能访问飞书及所配置的模型提供商。

### 2. 构建

在项目根目录执行：

```sh
node scripts/project.mjs go build -buildvcs=false -trimpath -o .cache/artifacts/feishu-bridge ./cmd/feishu-bridge
```

项目运行器将工具缓存、临时文件和构建产物放在本项目 `.cache/` 中。

### 3. 创建私有配置

复制公开示例，为新部署创建配置：

```sh
node scripts/project.mjs cp config.example.json .cache/config.json
node scripts/project.mjs chmod 600 .cache/config.json
```

自行编辑新文件，填写应用凭证和项目目录。不要覆盖已有部署的配置，也不要提交凭证、令牌、会话状态或缓存。

| 字段 | 用途 |
|---|---|
| `app_id` / `app_secret` | 飞书应用凭证；也支持 `FEISHU_APP_ID` / `FEISHU_APP_SECRET` 环境变量 |
| `project_dir` | 默认项目目录，必须已存在 |
| `state_dir` | 私有状态目录；相对路径以配置文件所在目录为基准，不能等于项目目录 |
| `allowed_open_ids` | 获准使用的飞书用户；也可通过显式配对授权 |
| `codex_binary` | Codex CLI 可执行文件，默认 `codex` |
| `codex_home` | 可选 Codex home；留空时沿用运行环境 |
| `catalog_homes` | 可选的其他客户端会话目录，原始记录只读 |
| `model` | 可选启动模型；留空时由模型目录解析默认值 |
| `approval_policy` | `on-request` 或 `untrusted` |
| `sandbox` | `workspace-write` 或 `read-only` |
| `writable_roots` | 可选额外授权目录，仅用于 `workspace-write` |
| `turn_timeout_seconds` | 单轮超时，默认 3600 秒 |

完整格式见 [config.example.json](config.example.json)，目录授权见 [AUTHORIZATION](docs/AUTHORIZATION.md)。

### 4. 检查并启动

```sh
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --check
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json
```

`--check` 验证配置与可执行文件，不证明登录、网络或交互正常。如果使用配对而非预先配置用户，将启动命令改为：

```sh
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --pair
```

按终端提示，在十分钟内向机器人发送 `/pair <code>`。以上是两种启动方式，不要为同一状态目录同时启动两个实例。授权后发送 `/start` 打开工作入口。

## 日常使用

### 会话路由

普通消息发送到默认会话；引用机器人消息接续对应会话；`#3 检查测试结果` 显式指定会话。优先级为 **显式编号 → 引用消息 → 默认会话**。无法识别的引用不会悄悄转给默认会话。

回复包含会话编号、项目短名、状态快照、模型和思考强度。`/status #3` 读取本地状态；`/show #3` 查看完整目录，并查询模型目录核验下一轮配置。

### 常用命令

编号、路径和参数占位符请替换为实际值。

| 操作 | 命令 |
|---|---|
| 工作入口 / 完整帮助 | `/start` / `/help` |
| 选择已知项目 / 浏览会话 | `/projects` / `/sessions` |
| 新建会话 / 另选授权目录 | `/new 名称` / `/new --cwd "/path/to/project" 名称` |
| 切换默认会话 | `/use #3` |
| 查看状态 / 最近消息 | `/status #3` / `/history #3` |
| 查看队列 / 仅取消一条 | `/queue` / `/cancelone <task-id>` |
| 取消该会话全部排队 / 请求停止 | `/cancel #3` / `/stop #3` |
| 可用模型 / 设置选项翻页 | `/models` / `/model #3 page 2` |
| 全局模型 / 强度默认 | `/model global <model-id>` / `/effort global <effort>` |
| 会话模型 / 强度 | `/model #3 <model-id>` / `/effort #3 <effort>` |
| 会话整套恢复跟随全局 | `/model #3 default` 或 `/effort #3 default` |
| 附件与成果 / 取回完整成果 | `/files #3` / `/result <result-id>` |
| 取回项目文件 | `/file #3 docs/report.md` |
| 生成交接 / 按原项目和配置接续 | `/handoff #3` / `/continue #3` |
| 打开该会话操作草稿 | `/commands #3` |

`/new` 沿用引用或默认会话的项目目录，使用全局模型默认。`/continue` 检查原会话已空闲、交接文档可读取后，创建新会话，固定原会话的完整配置，并开始核对交接。新建不会扩大目录授权。

### 会话话题与历史

每个账号、聊天和 Codex 会话绑定独立飞书话题。主聊天继续显示本次完整回复，随后另发一张飞书原生话题转发记录卡片，可点击展开；不向正文拼接深链。话题内保存用户输入、回复、进度、审批说明和成果，重启及切换默认会话不改变绑定。在话题内回复直接接续对应会话。

从主聊天发出的会话输入会同步一份，明确标注来源，不再次提交模型；原本就在话题内的输入不重复复制。审批仍在有效期内由原用户手动提交。正文、话题副本和原生记录卡片分别保存送达回执，先将本次回复加入话题再转发；卡片失败时只重试尚未成功的步骤，避免重复正文或话题副本。

已有散落消息不会自动迁移成原生楼层；`/history #3` 查看已有模型历史。转发卡片的展开方式、手机/桌面显示及旧卡片是否刷新后续内容仍需在实际飞书客户端核验，不保证历史转发卡片自动更新。当前只为飞书适配器提供此能力，其他前端保持原发送方式。

### 模型与思考强度

“全局”指本桥接的默认配置，不是机器全局 Codex 设置。会话首次自定义任一项，会把模型和强度一起固定；之后修改全局不会影响该会话。

会话任一项写 `default` 会让**两项一起**恢复跟随全局；`global` 的 `default` 只清除对应项。设置持久保存，从下一轮生效；当前任务保持已捕获配置，排队任务在启动时解析。可用模型和强度以 `/models` 返回的能力为准。

### 审批与文件

卡片中的高亮入口只生成草稿，可以修改或重新选择，**手动发送后才提交**。卡片内输入框不是飞书原生聊天输入框。直接发送文字命令则是提交操作。审批/回答绑定原账号、聊天、会话与任务，十分钟有效；普通草稿通常三十分钟有效。

直接发送文件或图片，会保存到对应项目的 `.cache/feishu-files/`。接收本身不运行模型；后续任务获得附件元数据，不自动附加文件全文。单文件为 1 字节至 20 MiB，最多并行下载 4 个。`/files` 最多列 20 项最近收件和已登记成果；只生成但尚未发送的项目文件可用 `/file` 登记并取回。

长结果可通过 `/result` 取回完整附件。发送采用快照，重试不读取后来改动的原文件；成果快照保留以便重取。私有配置、凭证、会话目录与越界文件不允许通过文件接口导出。

## 原生命令与维护

文字命令和卡片不依赖原生命令面板已注册。面板可单独检查、同步：

```sh
node scripts/project.mjs .cache/artifacts/feishu-bridge --slash-commands plan
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --slash-commands check
node scripts/project.mjs .cache/artifacts/feishu-bridge --config .cache/config.json --slash-commands sync
```

`plan` 离线列出命令；`check` 只读检查；`sync` 向飞书创建缺少的定义，需要相应权限。

安装、打包、延迟更新与回退见 [LAUNCH](LAUNCH.md)。`scheduled` 仅代表已调度，`ready` 及运行散列核验代表服务恢复；卡片点击、文件送达和模型切换仍需单独验收。重启不自动重放任务或审批，旧卡片可能失效。

## 开发与验证

```sh
node scripts/project.mjs go test -race -count=1 -timeout=60s ./...
node scripts/project.mjs go vet -tags integration ./...
node scripts/project.mjs node --test --test-isolation=none scripts/feishu-release.test.mjs scripts/feishu-update.test.mjs scripts/feishu-maintenance.test.mjs scripts/feishu-publication.test.mjs
node scripts/project.mjs node scripts/stage-feishu.mjs --binary .cache/artifacts/feishu-bridge
```

先完成快速开始中的构建，再打包。默认测试使用合成数据；上述 `integration` 标签仅用于静态检查，不自动运行真实模型或接入现有私有配置。

- [架构与前端接入](docs/ARCHITECTURE.md)
- [验证方式与边界](VALIDATION.md)
- [贡献与 AI 协作](CONTRIBUTING.md)

当前仅装配飞书适配器。本地自动化通过、部署就绪与真实客户端验收分别记录。
