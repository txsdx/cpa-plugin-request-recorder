# CPA Request Recorder · 请求记录插件

CLIProxyAPI 的原生插件：在模型请求结束后更新完整会话，在 **CPAMC → 插件资源 → 请求记录** 中筛选、检查原文并回放对话。GUI 随动态库内嵌，不需要另行部署前端、Node 服务或数据库服务。

## 已实现

- 普通 JSON 直接保存；Responses 流式优先直接保存 `response.completed.response` 完整结束快照，收到后释放中间事件。缺少完整快照时才使用完整输出项或增量兜底，不落盘 SSE 中间事件。
- 成功、失败、拒绝、取消分别记录。达到大小/内存限制、没有完整响应时明确提示。
- 每个会话只保留一条最新完整历史：文字、可见思考、工具调用 / 返回、原图；后续轮次覆盖旧快照，保留完整对话。原请求和耗时 / 用量元数据对应最新一轮。
- 只依据用户请求与 CPA 给用户的响应还原会话，不读取 CPA 请求日志；已删除“服务商侧结果”、日志扫描 / 补全队列及相关配置。
- 按实际 API key、CPAMC 备注 / key 别名、模型、提供商、会话、思考深度、状态、时间范围、最小耗时筛选；自动读取当前 key 列表，无需输入或管理指纹。
- 查看请求时间、总耗时、宿主 TTFT、首分片耗时、输入/输出/思考/cache read/cache creation token 与缓存比例；缺失指标显示 `—`。
- 对话回放支持 OpenAI Chat Completions / Responses、Claude Messages、Gemini：系统指令、用户消息、模型输出、公开的思考内容/摘要、工具名、调用参数、调用 ID、工具返回值。
- **显示原始图片**：OpenAI `image_url` / `input_image`、Claude base64/url source、Gemini inlineData/fileData、OpenAI 图片生成 `b64_json` / Responses `image_generation_call`。内嵌图片和公网 HTTPS 图片由 CPA 服务端保存到插件图片目录，通过管理鉴权接口展示 WebP 压缩预览，点击下载时才传输原图；浏览器不访问图片源站。原始多模态字段保留在正文和 JSON 导出中。
- 会话详情支持一键导出 Markdown：完整多轮消息、可见思考、工具参数 / 返回与内嵌原图；同时保留 JSON 导出。
- 本地 bbolt 持久化、摘要/时间/trace 索引、分页、历史别名修改、保留期限和条目上限。无需外部数据库。

**重要边界：**纯插件钩子不能承诺记录所有 HTTP 入口。未进入请求拦截器的鉴权失败、路由前错误、非模型接口不在当前采集范围；WebSocket 专用响应事件和 multipart 二进制正文回放尚未实现。模型最终结果来自 CPA 响应回调。详见 [设计与数据边界](docs/design.md)。

## 下载 Release 安装（无需 Go / Node）

仓库已配置 [Build and release](.github/workflows/release.yml)：在 Actions 点击 **Run workflow** 就会测试、编译五个平台并发布 [GitHub Release](https://github.com/txsdx/cpa-plugin-request-recorder/releases)。版本可手填；留空则首版为 `0.1.0`，以后按已有正式版本标签自动递增 patch。推送 `vX.Y.Z` 标签也会发布该版本。仅推送 main 不自动发版。仓库私有时，请用有仓库权限的 GitHub 账号下载，CPA 自身也必须配置读取权限。

| CPA 运行平台 | 选择的附件后缀 | 解压后的库 |
| --- | --- | --- |
| Linux / Docker，Intel 或 AMD 64 位 | `_linux_amd64.zip` | `request-recorder.so` |
| Linux / Docker，ARM64（如树莓派 64 位） | `_linux_arm64.zip` | `request-recorder.so` |
| macOS Intel | `_darwin_amd64.zip` | `request-recorder.dylib` |
| macOS Apple Silicon | `_darwin_arm64.zip` | `request-recorder.dylib` |
| Windows x64 | `_windows_amd64.zip` | `request-recorder.dll` |

Linux 包在 Debian 12 / glibc 环境原生编译，匹配所检查 CPA Dockerfile 的系统基线；Alpine / musl 和 Windows ARM64 不包含在这五种发布目标中。Docker 部署应按 **容器内部** 的 OS / 架构选择，不是按浏览器或宿主桌面系统选择。

以 Linux amd64 `v0.6.0` 为例：

1. 下载 `request-recorder_0.6.0_linux_amd64.zip` 和 `checksums.txt`，放在同一目录。
2. 校验并解压到 CPA 的插件目录：

   ```bash
   sha256sum --ignore-missing --check checksums.txt
   mkdir -p /path/to/CLIProxyAPI/plugins/linux/amd64
   unzip request-recorder_0.6.0_linux_amd64.zip -d /path/to/CLIProxyAPI/plugins/linux/amd64
   ```

3. 合并下面的插件配置，设置数据库持久化，然后重启 CPA。无需配置日志目录。
4. CPAMC 中确认插件 `effective_enabled`，打开“插件资源 → 请求记录”，同源 CPAMC 已保存登录时会自动连接，否则输入 CPA **管理密钥**。
5. 发一条测试模型请求。请求结束后刷新列表，检查“模型最终结果”和会话回放；无需先开启请求日志。

不需要部署独立网站，GUI 已包含在动态库中。不必注册进官方商店：添加本仓库的 `registry.json` 自定义源即可在 CPA 插件商店安装和更新，详见下节。

### Docker / Compose 路径示例

在现有 CPA 服务中追加目录挂载（不要覆盖原有配置、auth 等挂载）：

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins
  - ./recorder-data:/CLIProxyAPI/recorder-data
```

将库放在宿主机 `./plugins/linux/amd64/request-recorder.so`（ARM64 则用 `arm64` 子目录）。插件配置使用容器内路径：

```yaml
request-log: false
usage-statistics-enabled: true
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    request-recorder:
      enabled: true
      priority: -1000
      db_path: /CLIProxyAPI/recorder-data/request-recorder.db
```

示例假设 CPA 的工作目录位于 `/CLIProxyAPI`。如果你的镜像使用其他工作目录，按实际路径修改。目录要允许容器内 CPA 用户写入。这里的 `recorder-data` 挂载同时保留数据库和默认图片目录 `/CLIProxyAPI/recorder-data/request-recorder-images/`；重建容器后需要保留整个目录。

### 从 CPA 插件商店自动安装 / 一键更新

在 CPAMC 的插件商店来源中添加这个 **registry 文件 URL**（不是仓库主页）：

```text
https://raw.githubusercontent.com/txsdx/cpa-plugin-request-recorder/main/registry.json
```

或合并到 CPA 配置：

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - "https://raw.githubusercontent.com/txsdx/cpa-plugin-request-recorder/main/registry.json"
```

刷新商店，选择 `CPA Request Recorder · 请求记录`，点击安装。以后刷新检查更新，再点击更新即可。CPA 从仓库的**最新正式 Release**解析版本，自动选择 OS / CPU 对应 zip，校验 `checksums.txt`，并写入带版本的库文件（例如 `plugins/linux/amd64/request-recorder-v0.6.0.so`）。不要为了匹配版本再手动重命名 Release 的 zip 或库。

安装源解决的是下载安装和版本更新。仍需按本文配置数据库持久化；GUI 通过 CPAMC 插件资源打开。更新是否热加载或要求重启由 CPA 版本及运行平台决定。预发布版本不会被 GitHub latest 接口当作正式更新推送。

**当前仓库是私有仓库。** 仅在浏览器中登录 GitHub 不会赋予 CPA 服务访问权限；需在 **CPA 服务的进程环境**中安全设置 `CPA_PLUGIN_GITHUB_TOKEN`，使用对这个仓库有 Contents 读取权限的凭证，然后合并以下规则。不要把 token 写进 registry URL、配置文件、仓库或聊天。

```yaml
plugins:
  store-auth:
    - match: "https://raw.githubusercontent.com/txsdx/cpa-plugin-request-recorder/"
      apply-to: [registry]
      type: github-token
      token-env: CPA_PLUGIN_GITHUB_TOKEN
    - match: "https://api.github.com/repos/txsdx/cpa-plugin-request-recorder/"
      apply-to: [metadata, artifact]
      type: github-token
      token-env: CPA_PLUGIN_GITHUB_TOKEN
```

两条规则分别覆盖 registry 与 Release 元数据 / 资产 API，不把凭证发送到任意下载域名。Docker 需把该环境变量传入 CPA 容器。若以后将仓库设为公开，可省略认证规则，并相应把 registry 的 `auth_required` 改为 false。

### 维护者发布新版本

推荐打开 [Build and release](https://github.com/txsdx/cpa-plugin-request-recorder/actions/workflows/release.yml)，点击 **Run workflow**：

- `version` 留空：首版 `0.1.0`；已有 `v0.1.0` 时自动发布 `v0.1.1`。
- 手填 `0.2.0`：发布指定版本；`0.2.0-rc.1` 会标为 prerelease。
- Action 在**实际测试的提交**上创建标签，所有构建、真实 CPA 的无日志完整快照采集及安装器兼容测试完成后才公开 Release；不会用未验证的新 main 提交打包。

也可手动推标签：

```bash
git fetch origin
git tag v0.2.0 origin/main
git push origin v0.2.0
```

每个正式 Release 必须包含：

```text
request-recorder_<version>_linux_amd64.zip
request-recorder_<version>_linux_arm64.zip
request-recorder_<version>_darwin_amd64.zip
request-recorder_<version>_darwin_arm64.zip
request-recorder_<version>_windows_amd64.zip
checksums.txt
registry.json
```

版本不带 `v` 写进附件名，Git 标签带 `v`。ZIP 根目录只有 `request-recorder.so` / `.dylib` / `.dll`，不包含目录、`.h` 文件或其他动态库。发布前会使用固定 CPA 源码中的真实 registry 解析器和安装器校验全部五个平台的实际产物、重复安装、版本判断及校验和拒绝行为。

Release 流程包含五个平台构建与发布。额外验证了从线上私有 registry 读取最新 Release、使用文档中的 scoped GitHub 认证、通过 CPA 官方安装器真实下载并校验 Linux amd64 插件；公开 Release 中的五份 ZIP 及其校验和也已逐份复核。

根目录 `registry.json` 省略可选展示版本，长期作为稳定的商店源；Release 附件中的 registry 由同一元数据自动生成，包含该 Release 版本。CPA 以 GitHub 最新正式 Release 为版本真相，因此更新不需要 Action 回写 main。正式发布版本不会被覆盖，失败只留下可重试的 draft；发布使用仓库自带的 `GITHUB_TOKEN`，不需额外 PAT。

## 构建和安装

需要 Go **1.26.10+**、目标平台 C 编译器和 `make`。Go module 校验和及 TLS 验证保持开启。

```bash
go mod download
make test
make build
```

Linux 产物为 `dist/request-recorder.so`。macOS 产物为 `.dylib`；Windows 为 `.dll`，须使用对应平台 C 编译器原生构建或配置交叉编译工具链。Linux amd64 已验证实际装载，其他平台由 Action 原生编译和测试。

将库复制到 CPA 实际插件目录，例如：

```bash
mkdir -p /path/to/CLIProxyAPI/plugins/linux/amd64
cp dist/request-recorder.so /path/to/CLIProxyAPI/plugins/linux/amd64/
```

将 [config.example.yaml](config.example.yaml) 合并进现有 CPA 配置，保持你的上游和管理鉴权配置。**需要宿主支持 ABI v1 / JSON schema ≥ 6**，旧宿主会明确拒绝注册。

```yaml
request-log: false
usage-statistics-enabled: true
plugins:
  enabled: true
  dir: plugins
  configs:
    request-recorder:
      enabled: true
      priority: -1000
      db_path: data/request-recorder.db
```

以上旧式配置键由本轮验证的 CPA v8 兼容加载器支持。如果使用 v8 嵌套配置，请设置 `observability.logs.request-log: false` 和 `observability.usage.usage-statistics-enabled: true`；已有 v8 值优先于旧式值。

插件不读取 CPA 请求日志，`request-log` 可以保持关闭；`logs_dir` 已删除。普通 JSON 直接保存原始响应，Responses 流式优先保存完整结束快照；快照缺少 output 时优先使用 output_item.done 的完整输出项，仅在接口没有完整结果时使用分片兜底。旧配置中可移除 logs_dir。

启动 / 重启 CPA 后，在 CPAMC 插件列表确认 `effective_enabled`，打开资源菜单“请求记录”。直接资源路径为：

```text
/v0/resource/plugins/request-recorder/index.html
```

**后台记录不依赖 WebUI 登录。** 插件启用后持续采集，管理密钥仅用于记录、图片和别名等管理 API 的访问鉴权。

自 v0.2.0 起，同源 CPAMC 已选择记住登录凭据时，插件自动读取它已有的登录状态，通过 CPA 校验后连接，无需再输入；不会另存一份密钥。这里复用已有凭据，服务端鉴权始终保留。参考了 CPA Key Billing 的同源凭据读取方式。不同域名 / 端口 / 协议、CPAMC 未记住密钥、浏览器禁用存储或存储格式不兼容时，显示手动连接入口。CPAMC 不提供通用 postMessage 凭据桥接，不能仅凭 iframe / Referer 判定已授权。

静态 GUI 资源公开，所有记录、原图和别名 API 由 CPA 管理鉴权保护。CPAMC 移除保存的登录凭据时，使用自动登录的插件视图清空并停止读取；这不会停止后台采集。

### 自动读取 CPAMC 的 API key 备注

图中“接入与认证 → 客户端 API 密钥”的备注由 CPAMC 存在**当前浏览器、当前来源**的 `api-key-names:v1:<CPA地址>` 中，不在 CPA 的 `access.api-keys` 配置里。插件连接后会自动读取同源备注，使用 CPA 鉴权接口获取当前 key 列表，在浏览器内匹配 CPAMC 的来源指纹，再将 **SHA-256 key 指纹和备注**同步到插件数据库。同步请求不传 key 明文，也不把获取到的 key 列表另存进浏览器或插件数据库。

已同步备注可用于历史记录显示和别名筛选，后台采集不需要 WebUI 持续打开。修改 CPAMC 备注后，下次打开插件会同步；同源浏览器存储事件也会触发更新。插件中手动设置的别名优先；清空手动别名后恢复使用 CPAMC 备注。删除 CPAMC 备注会从后续同步快照中移除自动别名。插件不修改 CPAMC 的存储。

换浏览器、换域名，或外部 CPAMC 连接另一个来源的 CPA 时，浏览器隔离使插件无法读取那边的本地备注，需要先在同源 CPAMC 中配置 / 迁移备注。自定义认证插件若把 API key 改成不等于原 key 的主体 ID，也无法可靠按原 key 指纹匹配，仍需手动绑定。

插件配置热变更会被拒绝并保留旧实例，需重启 CPA 应用；这是为了避免切换数据库时丢失正在进行的采集。

## 每个会话只保留最新完整历史

这里的方向是：**下游客户端（例如 Codex / Cherry Studio）→ CPA → 上游模型服务商**。模型回答从服务商经 CPA 返回客户端。详情中的“模型最终结果”就是这份模型回答在 CPA 回调中的完整结果；插件仅记录 CPA 返回给客户端的结果，不另行采集服务商协议转换前正文。

同一 API key 下，优先使用客户端明确传入的会话 / 线程 ID（例如 `X-Session-ID`、`X-Conversation-ID`、`Session_id` 或正文中的 `conversation_id` / `thread_id` / `session_id`）；`previous_response_id` 可关联插件已经记录过的 Responses 历史。没有明确 ID 时，按请求重发的完整上下文和已记录回答精确接续，系统指令与工具配置也参与区分；不会只按 key / 模型或相似提示词合并，也不把宿主缓存 / 路由 / 执行 ID 当会话 ID。

新一轮原子覆盖同一条会话，旧请求行、旧分支正文和无引用块随之删除；保留当前分支完整消息，旧思考和工具结果即使客户端不重发也尽可能保留。重新生成覆盖对应回答及之后的旧分支。按请求开始时间保护较新的轮次，延迟完成的旧请求不能覆盖新快照。独立会话和不同 API key 分开保存；稳定会话 ID 与最新请求 ID 分别为 `id` / `request_id`。

每条会话的列表耗时、token、状态、原请求与原响应对应**最新一次模型请求**，不是整段会话累计账单。`conversation_turns` 是观察到的更新次数，包含重新生成。聊天历史保存在数据库，不另外落一份逐请求历史。没有明确会话标识且上下文被客户端裁剪时，不能保证归并；两个没有 ID 且上下文完全相同的聊天也无法可靠区分，建议客户端传会话 ID。

## 配置

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `max_database_bytes` | `1073741824` | 活动数据库文件上限 1 GiB，最小 8 MiB；自动淘汰最旧条目并整理。图片、CPA 日志、整理临时文件另计 |
| `refresh_interval_seconds` | `1` | 列表自动刷新间隔，1–3600 秒；GUI 可修改并持久化覆盖此默认值 |
| `db_path` | `data/request-recorder.db` | 相对 CPA 工作目录；一个数据库只能由一个插件实例写入 |
| `images_dir` | 数据库旁的 `<数据库名>-images/` | 默认 `data/request-recorder-images/`，可用绝对路径指定 |
| `archive_remote_images` | `true` | 后台保存公网 HTTPS 图片；关闭后仍保存内嵌图片 |
| `preview_quality` | `75` | 在线 WebP 预览画质，1–100；原图不改动 |
| `preview_max_dimension` | `1600` | 预览最长边像素，128–4096；保持比例，不放大小图 |
| `max_image_bytes` | `20971520` | 单张图片最大 20 MiB，允许 1 KiB 至 64 MiB |
| `max_image_storage_bytes` | `2147483648` | 图片文件总配额 2 GiB；不包括数据库和 CPA 日志 |
| `max_image_files` | `100000` | 去重后的图片文件数上限，防止大量小图耗尽 inode |
| `max_images_per_record` | `64` | 每个完整会话最多归档的不同图片数，允许 1 至 128；原始图片字段仍保存在会话 / 正文中 |
| `image_workers` | `2` | 图片后台下载并发数，允许 1 至 8 |
| `image_download_timeout_seconds` | `20` | 每张图片总下载超时，允许 1 至 120 秒 |
| `retention_days` | `30` | 记录保留天数；`0` 不按时间删除 |
| `max_records` | `100000` | 会话数量上限，每分钟清理；`0` 不限制 |
| `max_body_bytes` | `67108864` | 每份用户请求 / CPA 响应及完整会话上限 64 MiB；采集超限标记截断，合并历史超限则保留前一快照并报告错误 |
| `max_memory_bytes` | `268435456` | 正在采集正文的总预算 256 MiB；不是 Go 进程总 RSS 上限 |
| `max_inflight` | `256` | 同时采集请求上限，超过时计入 `dropped_requests` |

数据库以 `0600` 权限创建，父目录新建时为 `0700`。正文不进行内容脱敏，可能包含业务敏感数据和原始图片；鉴权头脱敏，key 用 SHA-256 指纹关联，不单独保存 key 明文。保留期限只清理插件数据库，不清理 CPA 自己的日志。bbolt 删除会复用页，不自动缩小数据库文件或擦除旧磁盘页。

### 最终结果存储、容量限制与自动整理

v0.4.0 **只保存完整最终结果**：非流式结果保留完整 JSON；Responses 优先使用最终 `response` 快照，其他支持的流式协议汇总为一份消息快照，包含文字、可见思考、工具调用和原图。SSE 中间事件、重复完成快照和进度事件不会持久化；不读取或复制 CPA 请求日志。失败或截断只保存能恢复的部分，并标记原因，不补造内容。

汇总快照是可检查的**最终结果**，不是逐事件原文或网络抓包。原请求仍完整保留；工具返回值如果在请求上下文中，仍在聊天历史中显示。usage_attempts 保留重试用量和耗时，因此不会因为只保留最终正文而少算消耗。

正文按内容边界分块，用 SHA-256 去重并按块压缩，重复历史 / base64 片段可以跨字段、跨请求共用。读取详情和导出会校验后还原保存的最终正文；修改用量、别名等元数据不重复写入正文。删除记录按引用回收块，不影响其他记录。索引中的提示词 / 响应预览最多 800 个字符，完整内容仍在详情。

`max_database_bytes` 默认 **1 GiB**。后台每秒检查容量，预留页分配和下一次写入的空间；接近容量阈值时淘汰最旧条目，保留较新的记录，再按空闲空间比例整理。单条记录如果也无法容纳，可能被淘汰，GUI 会显示容量淘汰累计数。原图目录和 CPA 自身日志按各自规则清理，不包含在数据库配额里。

整理复制一致快照，使用有界事务和不超过 32 MiB 的写入重放预算；复制期间继续处理正常写入，切换前重放变更并检查数据库结构，再替换工作文件。写入过多会取消本轮并重试，不无限积压内存。最终切换有短暂写入屏障。切换使用临时回滚文件，成功后清理，失败保留可恢复的原库；需要额外磁盘空间。容量阈值是自动维护目标，**正在写入及后台整理之间，活动文件可能短暂超过设置值**；不是文件系统硬配额。若需要绝对磁盘限制，请给持久化目录设置文件系统 / 容器卷配额。

GUI 的 **存储占用** 区分活动文件、空闲页、正文合计、去重压缩后正文、容量淘汰数与整理状态。`GET /status` 提供轻量统计，`GET /storage` 显式遍历清单 / 块头进行详细统计，避免每秒扫描大库。

**v0.5.0 可继续读取 v0.4.0 会话数据库。** 已经只保存了错误占位文本的响应无法从该占位符自行补回；原始请求日志仍在时可离线恢复。更早的旧格式不兼容。

**v0.4.0 会话结构不兼容 v0.3.x 及更早数据库。** 按本次升级约定，请先停止 CPA 并删除或移走旧 `.db`，再启用 v0.4.0；插件不迁移旧库，也不会悄悄把旧库当空库打开。旧图片目录中不再被记录引用的自有图片文件会由常规清理回收。

### 列表与刷新

顶部控制按钮置于左侧下方，避开 CPAMC 右上角浮层；手动刷新保留在“管理 key 别名”所在的列表工具栏。自动刷新默认开启，每秒更新；工具栏可以修改 1–3600 秒的间隔，设置保存在插件数据库。关闭自动刷新时仍可手动刷新。后台采集不受这些 UI 设置影响。

列表区域宽度为视口的 90%，最后两列为**最新用户提示词**与**模型最新响应**，共用剩余宽度。元数据列使用紧凑宽度，预览最多显示六行；点击条目查看全文。列表保留 8 列，移除服务商侧结果列，为提示词和回答提供更多宽度。打开的会话在新轮次到达时随列表刷新。窄屏在列表内部横向滚动。自动刷新跳过正在进行的刷新，不重复创建请求；数据未变化时不重建条目。

### 导出完整会话到 Markdown

打开会话详情，点击标题旁的 **↓ 导出 Markdown**，下载 `conversation-<会话ID>.md`。导出使用数据库中的完整会话快照，包含各轮系统 / 开发者提示、用户提问、模型回答、已公开的思考摘要、工具名 / 调用 ID / 参数和返回值，按消息顺序排列；不是只导出最新一轮。文本保留 Markdown，工具数据使用代码块。

已归档原图或正文中有效的内嵌原图使用 `data:` URI 嵌入同一个文件，重复图片只写入一份，不需要图片服务或管理密钥即可离线查看。Markdown 阅读器需要支持 data URI 图片；GitHub 等会过滤这类图片的渲染器可能不显示，但原始图片字节仍在文件里。原图尚未归档或不可用时，会在对应位置注明原因，导出操作不会访问远程源站。原有 **↓ 导出 JSON** 按钮保留。

下载走 CPA 管理鉴权接口，沿用 CPA 的 TLS。导出文件总大小（含原图 base64）受 `max_body_bytes` 限制，默认 64 MiB；超过限制会提示错误，不下载截断文件。采集时已有的警告和错误会写入文件。

### 对话、图片和日志存在哪里

默认路径相对于 **CPA 进程工作目录**，不相对于 CPAMC 浏览器或配置文件目录：

| 内容 | 默认存储位置 |
| --- | --- |
| 最新原请求、模型最终结果、完整会话与最新用量元数据、图片索引 | `data/request-recorder.db`（bbolt） |
| 已归档原图 | `data/request-recorder-images/<图片内容 SHA-256>.png` 等 |
| CPA 原始请求日志 | 由 CPA 自身管理，插件不读取 |
| 插件运行错误日志 | CPA 进程日志，由 CPA 的日志配置决定 |

聊天视图读取数据库中唯一的最新完整消息历史，没有额外聊天历史文件。base64 图片仍保留在原始正文中，另外解码为原图文件；远程图片保留原 URL，并把下载成功的原始字节保存为文件。文件扩展名按实际图片字节识别为 `.png` / `.jpg` / `.gif` / `.webp` / `.bmp` / `.avif`，不依赖 URL 后缀或错误的 Content-Type，不转码、不压缩。已有插件目录的 `.img` 自有文件在启动时按内容改名；查看和下载也使用对应类型。内容相同的图片在插件目录内去重。

请求结束后异步归档图片，不等待下载才返回模型响应。任务随正文持久化，内存队列满或重启后继续；旧记录在第一次打开时补归档。已有记录的远程链接若已过期，无法补回；需要额外鉴权的图片也不会擅自复用管理密钥或上游凭证。失败在 GUI 显示原因，可点击“重试服务端保存”，没有浏览器直连源站的回退。

查看时，浏览器仅向同一个 CPA 的管理 API 发送带管理密钥的图片请求，再用临时 Blob URL 展示；详情关闭后释放 Blob URL。列表详情与放大框只读取 WebP 压缩预览（默认质量 75、最长边 1600，保持比例、小图不放大）；只有点击“下载原图”才读取原始文件。预览采用有损压缩，静态展示动画的第一帧，不用于替换归档原图。预览解码上限 3200 万像素，失败时仍可下载原图；不会自动回退传输大原图。聊天详情按需加载：初次打开不传输藏在原文 / 历史字段里的 base64 原图；只有切换原文页签或导出 JSON 时再读取完整正文。服务器预览缓存仅在内存中，LRU 上限 32 MiB / 64 张、编码并发 1，不写入数据库或图片目录；重启后按需重建。图片服务看到的是 **CPA 服务器或其出口代理的 IP**。GUI 的 CSP 只允许 Blob 图片，不加载远程图片。插件不另开图片服务端口，浏览器到插件的传输沿用 CPA 的 TLS 设置。

远程归档仅接受公网 **HTTPS 443**，验证证书；每次 DNS 解析和重定向都拒绝内网、环回、链路本地、云元数据等非公网地址，并把连接固定到已验证的 IP，防止 DNS 重绑定。不会转发调用者的 Authorization、Cookie、Referer 或原请求头。支持继承 HTTP(S) 出口代理，代理必须允许向已验证 IP 建立 CONNECT；不支持的代理或 TLS 错误会使图片归档失败。只保存 PNG/JPEG/GIF/WebP/BMP/AVIF，拒绝 HTML/SVG；有单图、条目数量、总容量、文件数、并发和超时限制。对外 HTTP 图片或内网图床的链接仍保留在原文中，但不会下载。

配额满时保留已归档图片，新图显示失败，不自动驱逐仍有记录引用的图片。记录过期后，未被其他记录引用的图片经过至少 5 分钟宽限，由每分钟运行的清理任务删除。`GET /status` 包含实际 `images_dir`、已用字节、配额和待处理任务数。

数据库、图片与 CPA 日志均为本地明文文件，TLS 只保护传输。备份或迁移时应暂停 CPA，并同时复制 **数据库和图片目录**；CPA 源日志按需另备份。JSON 导出保留原始内嵌数据和远程 URL / 图片索引，不打包远程图片文件。改动 `db_path` 后若要继续使用原图片目录，需要明确设置 `images_dir`。图片目录应由插件独占；目录 `0700`、新文件 `0600`（具体权限语义随操作系统而定）。

未公开思考、插件未观察过的 `previous_response_id` 历史、未返回的工具结果不会被补造。

## API

以下全部需要 `Authorization: Bearer <CPA management key>`，路径前缀为 `/v0/management/plugins/request-recorder`：

| 方法 / 路径 | 作用 |
| --- | --- |
| `GET /records` | 摘要列表、筛选、分页 |
| `GET /record?id=…` | 最新会话原文与完整对话；GUI 初次打开用 `view=chat` 省略大正文，原文页签 / 导出再加载 |
| `GET /export/markdown?id=…` | 下载完整会话 Markdown，内嵌可用原图；超出 max_body_bytes 返回 413，图片读取繁忙返回 429 |
| `GET /storage` | 活动文件、空闲页、容量淘汰、压缩去重后正文与整理状态 |
| `POST /settings` | `{"refresh_interval_seconds":1}`，设置列表刷新间隔 |
| `GET /status` | 采集、用量、存储错误，以及图片目录 / 配额 / 待处理数 |
| `GET /images?record_id=…` | 图片归档状态；轮询不重新读取巨大正文 |
| `GET /image?record_id=…&id=…&variant=preview` | 返回该会话原图的 WebP 压缩预览，同样需要管理鉴权 |
| `GET /image?record_id=…&id=…&variant=original` | 下载归档原图，原始字节与实际类型不变；省略 variant 也返回原图，不接受任意 URL |
| `POST /images/retry?record_id=…` | 重试该记录失败的服务端图片保存 |
| `POST /aliases/sync` | 自动备注快照 `{"aliases":{"<SHA-256 key_id>":"CPAMC 备注"}}`；原子替换自动备注，保留手动别名 |
| `POST /aliases` | `{"key_id":"sha256…","alias":"开发机"}` 或 `{"key":"临时提交原 key","alias":"开发机"}`；空别名删除映射 |

列表参数：`q`、`key_id`、`alias`、`model`、`provider`、`outcome`、`session`、`reasoning`、`from` / `to`（RFC3339）、`min_duration`（毫秒）、`offset`、`limit`（默认 50，上限 200）。结果为 `{items,total,offset,limit}`。`q` 搜索摘要字段，不扫描完整正文。

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

实际验证 CPA 源码 `307803510bce87f986fcf7b1a84190f105c7b4aa`：动态库装载、CPAMC 菜单资源、管理鉴权、普通/160 分片流式请求、usage 关联、完整 Responses 结束快照直接保存、请求/响应原图文件落盘与鉴权读取、别名筛选均通过。没有连接真实用户的 CPA 实例；线上模型和各版本宿主的行为需要部署后验证。

独立 GUI 开发服务器与插件共用实现，只用于开发，不是生产部署所必需的服务：

```bash
# 安全地在进程环境中设置 RECORDER_MANAGEMENT_KEY，再运行：
go run -tags=nodynamic ./cmd/recorder -listen 127.0.0.1:8787 -db /path/to/development.db
```

浏览器 smoke test 需要 Chromium 和包含集成测试两条记录的数据库。对测试实例设置 `RECORDER_MANAGEMENT_KEY` 后：

```bash
npm ci
CHROMIUM_PATH=/usr/bin/chromium npm run test:browser
CHROMIUM_PATH=/usr/bin/chromium npm run test:panel
```

已验证登录、筛选、详情页签、会话原图的 WebP 预览与原图下载、别名修改、移动宽度及没有 JS 错误；图片读取携带管理鉴权，详情关闭后释放 Blob URL，注入外部图片 URL 后浏览器仍无外部请求。CPAMC 同源存储兼容测试覆盖自动登录、中文备注、历史筛选、动态更新、手动优先、退出清空和跨服务凭据拒绝。下载器测试覆盖内网 / 元数据拒绝、混合 DNS、重定向、TLS 验证、CONNECT 出口代理、容量、重启与队列溢出恢复、损坏修复和保留清理。前端运行和插件构建均不需要安装 npm 依赖，npm 仅用于浏览器测试。

## 参考资料

开发前检查了官方 CPA 模板、SDK、CPAMC 资源页面和现有开源插件，具体文件、观察和兼容性取舍见 [references.md](docs/references.md)。
