# 安装与维护

源码目录、安装目录与被管理的项目目录可以分开。下列路径均为示例，请替换为自己的绝对路径；配置和状态不随源码发布。

## 构建与打包

```sh
node scripts/project.mjs go build -buildvcs=false -trimpath -o .cache/artifacts/feishu-bridge ./cmd/feishu-bridge
node scripts/project.mjs node scripts/stage-feishu.mjs --binary .cache/artifacts/feishu-bridge
```

打包器输出候选目录、归档及校验值。只复制严格公共源码清单中的文件和显式提供的二进制，不包含开发交接、历史调查、凭证、配置或会话状态。旧包验证器保留兼容规则，不代表旧包里的每份文档都应继续公开。

## 新安装

按 README 创建自己的私有配置，以交互方式完成首次配对，再选择前台运行或自行配置进程管理。`deploy/feishu-codex-bridge.service.example` 是可编辑的用户服务示例；复制前替换安装路径，并确保 Codex 可执行文件及认证环境可用。示例不会自动安装服务。

## 已有安装更新

先审阅候选清单和维护影响，取得对目标安装目录的授权，再执行：

```sh
node scripts/project.mjs node scripts/update-feishu.mjs --target /opt/feishu-codex-bridge --config /absolute/private/config.json --release .cache/artifacts/feishu-release-REPLACE_ME --delay-seconds 60
```

`--target` 必须显式指定绝对路径；`--config` 仅由正常桥接 CLI 内部加载。更新器不迁移或复制私有配置与状态。现有程序必须支持维护控制接口；活动任务、待审请求、排队、文件传输、未确认接收或待发消息会使维护推迟，状态未知时不会强行停止。

更新使用公共清单、进程身份校验、空闲原子确认、替换期独占状态锁、就绪与运行散列核验。失败时尝试回退公共文件；遇到无法确认的存活进程时停止恢复操作，避免重复启动。

## 已正常停止的实例

只有先通过原启动方式正常停止实例，才能使用离线更新：

```sh
node scripts/project.mjs node scripts/update-feishu.mjs --offline --target /opt/feishu-codex-bridge --config /absolute/private/config.json --release .cache/artifacts/feishu-release-REPLACE_ME --delay-seconds 60
```

离线模式不会替你停止旧程序。它要求状态锁可用且没有运行实例，替换和回退时持有该锁。旧版本的特殊首次升级入口保留为兼容代码，绑定已审阅程序散列和显式中断授权，不是通用升级方式，也不是控制接口失败后的自动降级。

## 核验结果

`scheduled` 表示已安排，`deferred` 表示未执行切换；只有 `ready` 与匹配的运行散列才证明服务恢复。恢复通知、飞书客户端卡片、文件送达和实际模型能力分别验收。重启不自动重放任务、审批或旧卡片授权。

可通过 `--maintenance status --maintenance-pid PID` 只读查询指定实例；`stop` 会请求空闲停止，不能当作检查命令。不要删除状态文件或跳过维护保护来处理异常。
