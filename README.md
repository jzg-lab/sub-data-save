# sub-data-save

为 Sub2API 按上游调用保存 Request / Response，供按日期下载和 JSONL 聚合。

**源码版本 0.3.1。依赖支持 Rawdata v2 的网关，已使用 0.3.0 的部署只需更新插件。**

## 采集范围

- 支持 Claude/OpenAI 上游业务 HTTP、SSE 和 OpenAI WebSocket。每次尝试独立归档，包含收到的错误响应。`response_complete` 标记响应是否完整。
- HTTP 与 SSE 正文为字符串。WebSocket 的请求为原消息字符串，响应为按接收顺序排列的消息字符串数组。
- 已知 API key 和可识别的凭据值在网关内存中遮盖。保留文本中的 `ciyuan.fast`、`ciyuan`（忽略大小写）及 `词元` 替换为 `中转`。对话、代码、附件与业务内容仍属客户私有数据。
- 新记录保存到配置目录的 `rawdata-v2/`。每次调用先保存独立 JSON，再追加 JSONL。单条配额、磁盘预留、可用内存或队列不足时会漏存，转发继续。
- S3 对象位于 `rawdata-v2/YYYY-MM-DD/model/`。升级前停用旧采集与上传，更新后再启用。旧包、对象和分片需要单独管理。

## 每日取包

插件在北京时间每天中午 12:00 生成 `<配置目录>/rawdata-v2/daily/YYYY-MM-DD.zip`，文件名为截止日期。直接复制这个 ZIP 即可。包内保留每次调用的独立 JSON 和原相对路径，可以用 `unzip -t 文件名.zip` 检查完整性。日包保存在服务器本地，已有 S3 上传流程继续传送 JSONL 分片。

首次运行和停机恢复时，会将最近一次中午截止前已落盘、尚未入包的记录收进一个包。边界按文件修改时间计算，迟到记录由后续包补收。打包失败会在一分钟后重试。请复制并保留服务器上的 ZIP，插件用已有包的目录核对哪些记录已收录。源 JSON 和 ZIP 都占用磁盘，现有采集配额继续生效。

## 文档入口

- [已确认需求](docs/requirements.md)：用户已确定的目标和行为边界。
- [执行计划](docs/execution-plan.md)：两个仓库的分工、实施顺序、数据契约、下载方案与验收。

## 与网关的关系

[sub2api-r3-fix](https://github.com/jzg-lab/sub2api-r3-fix) 负责实际出站采集、完整性判定、
有界异步派发及管理员下载代理。本仓库负责接收并保存 JSON，以及提供按日期查看归档概况的页面。

采集插件与 Cookie Pin 出站插件独立运行。0.2.0 新增 JSONL 聚合和可选 S3 上传。

## 安装前提

1. 宿主须支持 `data.capture.v1`、UI Bridge `archive.groups`/`archive.models`、v2 记录与原请求模型的筛选摘要。旧网关送来的记录会被拒收。
2. 选择与宿主系统和架构匹配的签名 `.s2plugin`。宿主内置发布者公钥并限定插件 ID 为 `jzg-lab.sub-data-save`。
3. 选择分组和精确请求模型，可逐项勾选、全选或清空。两份名单必须都非空且同时命中才会保存记录。升级时保留 `group_ids` 和 `models` 配置。
4. 默认保存在宿主持久化目录；可选配置 S3 bucket 与 region，运行环境需提供 AWS 默认凭证链权限。
5. 历史归档按 UTC 日期列出记录 ID、实例、单条大小和相对文件路径，显示当日条数、JSON 文件总大小和每条平均大小。文件大小按十进制显示为 MB、GB 或 TB。JSONL 封口后异步上传到 bucket，宿主状态目录所在卷须持久化。
## 构建与本地验证

```sh
go test -race ./...
./build.sh                       # 默认 linux-amd64
GOOS=darwin GOARCH=arm64 ./build.sh
```

构建默认签名，私钥从 `S2PLUGIN_KEY` 或 `~/.config/sub-data-save/publisher-v1.private.pem` 读取；不自动生成或提交私钥。`S2PLUGIN_UNSIGNED=1` 仅用于明确允许未签名包的开发环境。

首次配置构建机时，在已设置 `S2PLUGIN_KEY` 的终端运行 `./build.sh --install-signing-key`。也可在命令末尾传入现有私钥的文件路径。脚本核对宿主信任的发布者公钥后，将私钥保存到当前用户的固定位置，目录权限为 `0700`，文件权限为 `0600`。以后同一用户可直接运行 `./build.sh` 签名，整个过程在构建机上完成。

部署新版本前需从当前源码构建签名包。

## 当前成果来源

2026-11-09 根据用户指令，将此前讨论形成的需求和执行计划整理至本仓库。
来源为网关仓库 `codex/upstream-dataset-plan` 分支的提交
[`f881798d2925a06b249fbcbb7a6485e77066e166`](https://github.com/jzg-lab/sub2api-r3-fix/commit/f881798d2925a06b249fbcbb7a6485e77066e166)，
提取插件相关章节并更新仓库名称及文档链接，没有迁入网关源码和无关业务资料。

后续插件计划维护在本仓库，宿主产品规则与实际改动仍在网关仓库同步维护。
真实采集数据、凭据、签名私钥和导出包不提交 Git。
