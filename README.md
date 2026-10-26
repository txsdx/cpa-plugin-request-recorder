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

以 Linux amd64 的 `v0.1.0` 为例（先确认该版本已经在 Releases 生成）：

1. 下载 `request-recorder_0.1.0_linux_amd64.zip` 和 `checksums.txt`，放在同一目录。
2. 校验并解压到 CPA 的插件目录：

   ```bash
   sha256sum --ignore-missing --check checksums.txt
   mkdir -p /path/to/CLIProxyAPI/plugins/linux/amd64
   unzip request-recorder_0.1.0_linux_amd64.zip -d /path/to/CLIProxyAPI/plugins/linux/amd64
   ```

3. 合并下面的插件配置，设置数据库与真实日志目录，然后重启 CPA。
4. CPAMC 中确认插件 `effective_enabled`，打开“插件资源 → 请求记录”，输入 CPA **管理密钥**。
5. 发一条测试模型请求。请求结束后刷新列表，检查“上游原文”是否已采集。若不可用，在详情中查看补全原因。

不需要部署独立网站，GUI 已包含在动态库中。不必注册进官方商店：添加本仓库的 `registry.json` 自定义源即可在 CPA 插件商店安装和更新，详见下节。

### Docker / Compose 路径示例

在现有 CPA 服务中追加目录挂载（不要覆盖原有配置、auth 等挂载）：

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins
  - ./recorder-data:/CLIProxyAPI/recorder-data
  - ./logs:/CLIProxyAPI/logs
```

将库放在宿主机 `./plugins/linux/amd64/request-recorder.so`（ARM64 则用 `arm64` 子目录）。插件配置使用容器内路径：

```yaml
request-log: true
usage-statistics-enabled: true
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    request-recorder:
      enabled: true
      priority: -1000
      db_path: /CLIProxyAPI/recorder-data/request-recorder.db
      logs_dir: /CLIProxyAPI/logs
```

示例假设 CPA 的工作目录和实际请求日志位于 `/CLIProxyAPI`。如果你的镜像使用其他工作目录或 `WRITABLE_PATH`，按实际路径修改。目录要允许容器内 CPA 用户写入。数据库挂载用于保留记录，重建容器后也需要保留。

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

刷新商店，选择 `CPA Request Recorder · 请求记录`，点击安装。以后刷新检查更新，再点击更新即可。CPA 从仓库的**最新正式 Release**解析版本，自动选择 OS / CPU 对应 zip，校验 `checksums.txt`，并写入带版本的库文件（例如 `plugins/linux/amd64/request-recorder-v0.1.0.so`）。不要为了匹配版本再手动重命名 Release 的 zip 或库。

安装源解决的是下载安装和版本更新。仍需按本文配置 `request-log`、真实 `logs_dir` 和数据库持久化；GUI 通过 CPAMC 插件资源打开。更新是否热加载或要求重启由 CPA 版本及运行平台决定。预发布版本不会被 GitHub latest 接口当作正式更新推送。

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
- Action 在**实际测试的提交**上创建标签，所有构建、CPA 安装器兼容测试完成后才公开 Release；不会用未验证的新 main 提交打包。

也可手动推标签：

```bash
git fetch origin
git tag v0.1.0 origin/main
git push origin v0.1.0
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

根目录 `registry.json` 省略可选展示版本，长期作为稳定的商店源；Release 附件中的 registry 由同一元数据自动生成，包含该 Release 版本。CPA 以 GitHub 最新正式 Release 为版本真相，因此更新不需要 Action 回写 main。正式发布版本不会被覆盖，失败只留下可重试的 draft；发布使用仓库自带的 `GITHUB_TOKEN`，不需额外 PAT。

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
