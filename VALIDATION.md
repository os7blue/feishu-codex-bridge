# 验证方式与边界

从源码目录通过项目运行器执行：

```sh
node scripts/project.mjs go test -race -count=1 -timeout=60s ./...
node scripts/project.mjs go vet -tags integration ./...
node scripts/project.mjs node --test --test-isolation=none scripts/feishu-release.test.mjs scripts/feishu-update.test.mjs scripts/feishu-maintenance.test.mjs scripts/feishu-publication.test.mjs
node scripts/project.mjs node scripts/feishu-publication.mjs --check-tracked
```

最后一项用于 Git 检出目录，拒绝将内部交接、调查历史、私有配置、缓存、状态和构建产物混入源码提交。发布打包使用相同的严格公共源码选择规则。

## 自动化覆盖

- 会话归属、引用路由、持久化接收与恢复、去重和单模型任务队列。
- 审批详情、草稿手动提交、过期/替换、取消和停止。
- 全局默认、完整会话覆盖、模型目录分页及当前轮/下一轮配置。
- 附件范围与大小、成果完整取回、跨聊天拒绝和重试快照。
- 前端分层、纯文本降级、飞书 SDK 合成事件和原生命令定义。
- 公共清单、旧包兼容、空闲维护、就绪散列、失败回退和发布排除规则。

默认测试使用合成数据。`go vet -tags integration` 仅静态检查带标签的代码，不运行真实模型测试。不要为日常验证读取现有部署的私有配置、凭证、会话或原始日志。

## 真实环境验收

需另行验证飞书事件权限、客户端显示与操作、文件送达、登录状态和账号模型能力。若使用串行模型提供商，应独立安排真实模型复测，避免与其他客户端争用。

本地测试通过、部署 `ready` 与真实客户端验收是不同结果。公开仓库保留可复现的验证方法；本机 PID、维护回执、部署散列和内部调查记录不作为公共验证文档提交。
