# sub-data-save

为 Sub2API 采集成功调用的上游原始 Request / Response，积累用于知识库和后训练的数据档案。

**当前源码版本：0.2.0；本地实现尚未发布或部署。旧宿主必须先更新一次，才支持采集能力。**

## 第一版目标

- 只覆盖 Claude/OpenAI 普通 HTTP 模型调用；完整结束的 HTTP 流式响应也保存。
- 成功调用按请求模型聚合为 UTC 日期 JSONL 分片，每片上限 10,000,000,000 字节；逐条下载档案达到配额后不再新增原始 JSON，但继续写 JSONL。
- 配置 S3 bucket 和 AWS region 后，封口分片上传到 `YYYY-MM-DD/model/` 下；使用 AWS 默认凭证链，凭证不得写入插件配置。
- 失败、断流和客户端断开不保存；逐条档案配额满后仅原始 JSON 停止新增，JSONL 继续记录。采集故障不阻断模型调用。

## 文档入口

- [已确认需求](docs/requirements.md)：用户已确定的目标和行为边界。
- [执行计划](docs/execution-plan.md)：两个仓库的分工、实施顺序、数据契约、下载方案与验收。

## 与网关的关系

[sub2api-r3-fix](https://github.com/jzg-lab/sub2api-r3-fix) 负责实际出站采集、完整性判定、
有界异步派发及管理员下载代理。本仓库负责接收并保存 JSON，以及提供按日期下载页面。

采集插件与 Cookie Pin 出站插件独立运行。0.2.0 新增 JSONL 聚合和可选 S3 上传。

## 安装前提

1. 必须使用包含 `data.capture.v1` UI Bridge `archive.groups`/`archive.models` 的宿主版本，并为采集记录提供 `group_id` 和原始 `requested_model`；不兼容宿主不可配置采集筛选。
2. 选择与宿主系统和架构匹配的签名 `.s2plugin`。宿主内置发布者公钥并限定插件 ID 为 `jzg-lab.sub-data-save`。
3. 在后台的采集设置中勾选分组和精确请求模型，可逐项勾选、全选或清空。两份名单必须都非空且同时命中才会保存记录。更换 0.2.x 前保留 `group_ids` 和 `models` 配置。
4. 默认保存在宿主持久化目录；可选配置 S3 bucket 与 region，运行环境需提供 AWS 默认凭证链权限。
5. 历史归档按 UTC 日期列出记录 ID、实例、单条大小和相对文件路径，显示当日条数与总大小，并支持逐条下载原始 JSON。文件路径相对于当前 JSON 归档根目录。JSONL 封口后异步上传到 bucket，宿主状态目录所在卷须持久化。
## 构建与本地验证

```sh
go test -race ./...
./build.sh                       # 默认 linux-amd64
GOOS=darwin GOARCH=arm64 ./build.sh
```

构建默认签名，私钥从 `S2PLUGIN_KEY` 或 `~/.config/sub-data-save/publisher-v1.private.pem` 读取；不自动生成或提交私钥。`S2PLUGIN_UNSIGNED=1` 仅用于明确允许未签名包的开发环境。

当前源码版本为 0.2.0。Linux amd64 和 macOS arm64 签名包位于 `dist/`，已验签并核对包内文件哈希，待生产进程验收。

## 当前成果来源

2026-11-09 根据用户指令，将此前讨论形成的需求和执行计划整理至本仓库。
来源为网关仓库 `codex/upstream-dataset-plan` 分支的提交
[`f881798d2925a06b249fbcbb7a6485e77066e166`](https://github.com/jzg-lab/sub2api-r3-fix/commit/f881798d2925a06b249fbcbb7a6485e77066e166)，
提取插件相关章节并更新仓库名称及文档链接，没有迁入网关源码和无关业务资料。

后续插件计划维护在本仓库，宿主产品规则与实际改动仍在网关仓库同步维护。
真实采集数据、凭据、签名私钥和导出包不提交 Git。
