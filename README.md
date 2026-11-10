# sub-data-save

为 Sub2API 采集成功调用的上游原始 Request / Response，积累用于知识库和后训练的数据档案。

**当前状态：0.1.1 签名包已构建，本地宿主接入与真实插件进程验证通过；尚未部署。旧宿主必须先更新一次，才支持此采集能力。**

## 第一版目标

- 只覆盖 Claude/OpenAI 普通 HTTP 模型调用；完整的 HTTP 流式响应也保存。
- 每次成功调用一个 JSON，按 UTC 日期分目录，插件页面按日期逐条下载。
- 失败、断流、客户端断开和超限记录不保存；采集故障不阻断模型调用。
- WebSocket、图片/视频素材、异步任务、ZIP 和统计告警暂不做。

## 文档入口

- [已确认需求](docs/requirements.md)：用户已确定的目标和行为边界。
- [执行计划](docs/execution-plan.md)：两个仓库的分工、实施顺序、数据契约、下载方案与验收。

## 与网关的关系

[sub2api-r3-fix](https://github.com/jzg-lab/sub2api-r3-fix) 负责实际出站采集、完整性判定、
有界异步派发及管理员下载代理。本仓库负责接收并保存 JSON，以及提供按日期下载页面。

采集插件与 Cookie Pin 出站插件独立运行。本机已验证签名校验、空配置启用、保存、列表、下载和重新启用后数据保留；没有运行生产 Cookie Pin 二进制或真实收费模型调用。

## 安装前提

1. 网关后端和管理前端必须先部署本任务的采集接入改动。版本号满足清单范围不代表已有采集能力；出现“初期仅支持能力 openai.oauth.outbound_transport.v1”说明仍在运行旧宿主。
2. 使用对应平台的签名 `.s2plugin`。本次宿主内置本插件发布者公钥，并限定插件 ID 为 `jzg-lab.sub-data-save`，无需手改信任配置或允许未签名包。
3. 在后台上传并启用，配置留空即可。档案默认保存在宿主提供的持久化状态目录下的 `archive/`；仅需自定义时展开高级设置填写绝对路径。版本范围内但未列为已测试的宿主，仍沿用宿主的兼容性确认提示。
4. 成功调用完成后，打开插件页选择 UTC 日期并逐条下载 JSON。宿主状态目录所在卷须持久化；容器自身的临时文件层不保证重建后保留。

## 构建与本地验证

```sh
go test -race ./...
./build.sh                       # 默认 linux-amd64
GOOS=darwin GOARCH=arm64 ./build.sh
```

构建默认签名，私钥从 `S2PLUGIN_KEY` 或 `~/.config/sub-data-save/publisher-v1.private.pem` 读取；不自动生成或提交私钥。`S2PLUGIN_UNSIGNED=1` 仅用于明确允许未签名包的开发环境。

本次产物位于 `dist/sub-data-save-0.1.1-{linux-amd64,darwin-arm64}.s2plugin`。宿主的真实进程验证使用本机架构包和隔离仓储 fixture，未连接生产数据库。宿主测试入口及局限见[实现与限制](docs/execution-plan.md)。

## 当前成果来源

2026-11-09 根据用户指令，将此前讨论形成的需求和执行计划整理至本仓库。
来源为网关仓库 `codex/upstream-dataset-plan` 分支的提交
[`f881798d2925a06b249fbcbb7a6485e77066e166`](https://github.com/jzg-lab/sub2api-r3-fix/commit/f881798d2925a06b249fbcbb7a6485e77066e166)，
提取插件相关章节并更新仓库名称及文档链接，没有迁入网关源码和无关业务资料。

后续插件计划维护在本仓库，宿主产品规则与实际改动仍在网关仓库同步维护。
真实采集数据、凭据、签名私钥和导出包不提交 Git。
