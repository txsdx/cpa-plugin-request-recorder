# CPA Request Recorder · 请求记录插件

CLIProxyAPI 的原生插件：在模型请求结束后记录请求与响应，在 **CPAMC → 插件资源 → 请求记录** 中筛选、检查原文并回放对话。GUI 随动态库内嵌，不需要另行部署前端、Node 服务或数据库服务。

## 已实现

- 普通 JSON 与流式 SSE 正文采集；按顺序保留所有分片，不依赖宿主只有最近 64 块的 `HistoryChunks`。
- 成功、失败、拒绝、取消分别记录。达到大小/内存限制、没有完整响应时明确提示。
- 分别保存原请求、凭证选择后的请求、下游协议响应、CPA 日志里的全部上游尝试及其响应。
- 按 key 指纹、key 别名、模型、提供商、请求者 IP（宿主提供时）、会话、思考深度、状态、时间范围、最小耗时筛选。
- 查看请求时间、总耗时、宿主 TTFT、首分片耗时、输入/输出/思考/cache read/cache creation token 与缓存比例；缺失指标显示 `—`。
- 对话回放支持 OpenAI Chat Completions / Responses、Claude Messages、Gemini：系统指令、用户消息、模型输出、公开的思考内容/摘要、工具名、调用参数、调用 ID、工具返回值。
- **显示原始图片**：OpenAI `image_url` / `input_image`、Claude base64/url source、Gemini inlineData/fileData、OpenAI 图片生成 `b64_json` / Responses `image_generation_call`。内嵌图片直接展示，可放大和下载；远程 URL 手动加载。原始多模态字段保留在正文和 JSON 导出中。
- 本地 bbolt 持久化、摘要/时间/trace 索引、分页、历史别名修改、保留期限和条目上限、JSON 导出。无需外部数据库。

**重要边界：**纯插件钩子不能承诺记录所有 HTTP 入口。未进入请求拦截器的鉴权失败、路由前错误、非模型接口不在当前采集范围；WebSocket 专用响应事件和 multipart 二进制正文回放尚未实现。这里不会把“协议转换后的下游响应”标成“原始上游响应”。详见 [设计与数据边界](docs/design.md)。

## 构建和安装

需要 Go **1.26.10+**、目标平台 C 编译器和 `make`。Go module 校验和及 TLS 验证保持开启。

```bash
go mod download
make test
make build
```

Linux 产物为 `dist/request-recorder.so`。macOS 产物为 `.dylib`；Windows 为 `.dll`，须使用对应平台 C 编译器原生构建或配置交叉编译工具链。本轮已验证 Linux amd64，其他平台未做实际装载验证。

将库复制到 CPA 实际插件目录，例如：

```bash
mkdir -p /path/to/CLIProxyAPI/plugins/linux/amd64
cp dist/request-recorder.so /path/to/CLIProxyAPI/plugins/linux/amd64/
```

将 [config.example.yaml](config.example.yaml) 合并进现有 CPA 配置，保持你的上游和管理鉴权配置。**需要宿主支持 ABI v1 / JSON schema ≥ 6**，旧宿主会明确拒绝注册。

```yaml
request-log: true
usage-statistics-enabled: true
plugins:
  enabled: true
  dir: plugins
  configs:
    request-recorder:
      enabled: true
      priority: -1000
      db_path: data/request-recorder.db
      logs_dir: logs
```

以上旧式配置键由本轮验证的 CPA v8 兼容加载器支持。如果使用 v8 嵌套配置，请设置 `observability.logs.request-log: true` 和 `observability.usage.usage-statistics-enabled: true`；已有 v8 值优先于旧式值。

`logs_dir` 必须是 **CPA 实际写入请求日志的目录**，不是随便新建的路径。CPA 可能使用工作目录的 `logs/`、`WRITABLE_PATH/logs` 或 auth 目录下的 `logs/`；容器内必须能访问同一个目录。没有日志配置时仍可采集下游响应，但上游原文不可用。启用后，日志通常在请求结束数秒内补全，最长尝试 30 秒。

启动 / 重启 CPA 后，在 CPAMC 插件列表确认 `effective_enabled`，打开资源菜单“请求记录”。直接资源路径为：

```text
/v0/resource/plugins/request-recorder/index.html
```

输入 **CPA 管理密钥**连接。当前 CPAMC 资源 iframe 不提供管理密钥桥接，插件不读取 CPAMC 的本地存储；页面刷新后需重新输入。静态 GUI 资源公开，所有记录和别名 API 由 CPA 管理鉴权保护。

插件配置热变更会被拒绝并保留旧实例，需重启 CPA 应用；这是为了避免切换数据库时丢失正在进行的采集。

## 配置

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `db_path` | `data/request-recorder.db` | 相对 CPA 工作目录；一个数据库只能由一个插件实例写入 |
| `logs_dir` | 空 | 不配置则不尝试上游原文补全 |
| `retention_days` | `30` | 记录保留天数；`0` 不按时间删除 |
| `max_records` | `100000` | 每分钟清理超额记录；`0` 不限制数量 |
| `max_body_bytes` | `67108864` | 每份请求/响应采集上限 64 MiB；超过明确标记截断 |
| `max_memory_bytes` | `268435456` | 正在采集正文的总预算 256 MiB；不是 Go 进程总 RSS 上限 |
| `max_inflight` | `256` | 同时采集请求上限，超过时计入 `dropped_requests` |

数据库以 `0600` 权限创建，父目录新建时为 `0700`。正文不进行内容脱敏，可能包含业务敏感数据和原始图片；鉴权头脱敏，key 用 SHA-256 指纹关联，不单独保存 key 明文。保留期限只清理插件数据库，不清理 CPA 自己的日志。bbolt 删除会复用页，不自动缩小数据库文件或擦除旧磁盘页。

远程图片只保存 URL，不自动抓取到磁盘，URL 失效或需要额外鉴权时不能保证日后仍能加载。隐藏思考、服务端 `previous_response_id` 关联但未发送的历史、未返回的工具结果不会被补造。

## API

以下全部需要 `Authorization: Bearer <CPA management key>`，路径前缀为 `/v0/management/plugins/request-recorder`：

| 方法 / 路径 | 作用 |
| --- | --- |
| `GET /records` | 摘要列表、筛选、分页 |
| `GET /record?id=…` | 记录详情、下游对话与最后一次上游响应对话 |
| `GET /status` | 采集中请求、待补全日志、未匹配用量、丢弃数量、最近存储错误 |
| `POST /aliases` | `{"key_id":"sha256…","alias":"开发机"}` 或 `{"key":"临时提交原 key","alias":"开发机"}`；空别名删除映射 |

列表参数：`q`、`key_id`、`alias`、`ip`、`model`、`provider`、`outcome`、`session`、`reasoning`、`from` / `to`（RFC3339）、`min_duration`（毫秒）、`offset`、`limit`（默认 50，上限 200）。结果为 `{items,total,offset,limit}`。`q` 搜索摘要字段，不扫描完整正文。

## 开发与验证

默认使用已有 checkout；每个云任务已有隔离环境，不需要新建 Git worktree。

```bash
make test                         # Go race tests + JS 语法检查
go vet ./...
make package                      # 动态库、单库 zip、checksums.txt
```

有真实 CPA 可执行文件时，可以运行 **真实宿主 + 本地模拟上游**集成测试，不需要上游账号，不产生模型调用费用：

```bash
CPA_BIN=/absolute/path/to/cli-proxy-api python3 scripts/integration.py
```

本轮实际验证 CPA 源码 `307803510bce87f986fcf7b1a84190f105c7b4aa`：动态库装载、CPAMC 菜单资源、管理鉴权、普通/160 分片流式请求、usage 关联、上游日志补全、请求/响应原图、别名筛选均通过。没有连接真实用户的 CPA 实例；线上模型和各版本宿主的行为需要部署后验证。

独立 GUI 开发服务器与插件共用实现，只用于开发，不是生产部署所必需的服务：

```bash
# 安全地在进程环境中设置 RECORDER_MANAGEMENT_KEY，再运行：
go run ./cmd/recorder -listen 127.0.0.1:8787 -db /path/to/development.db
```

浏览器 smoke test 需要 Chromium 和包含集成测试两条记录的数据库。对测试实例设置 `RECORDER_MANAGEMENT_KEY` 后：

```bash
npm ci
CHROMIUM_PATH=/usr/bin/chromium npm run test:browser
```

已验证登录、筛选、详情页签、请求/下游/上游三处原图实际解码、别名修改、移动宽度及没有 JS 错误。前端运行和插件构建均不需要安装 npm 依赖，npm 仅用于浏览器测试。

## 参考资料

开发前检查了官方 CPA 模板、SDK、CPAMC 资源页面和现有开源插件，具体文件、观察和兼容性取舍见 [references.md](docs/references.md)。
