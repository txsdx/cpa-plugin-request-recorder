# 接口研究与开源参考

本项目使用本地最小 wire DTO 和 C ABI，不依赖整个 CPA 服务端 Go module，也不引入其他插件的计费或 provider 实现。

| 参考项目 / 文件 | 确认的事实 / 本项目取舍 |
| --- | --- |
| [CLIProxyAPI 官方插件示例](https://github.com/router-for-me/CLIProxyAPI/tree/307803510bce87f986fcf7b1a84190f105c7b4aa/examples/plugin)；request-lifecycle、response-interceptor、stream-interceptor、management-api 示例 | c-shared ABI v1、RPC envelope、注册与事件方法名；只观察，不改写、不终止、不丢弃请求或响应 |
| [sdk/pluginapi/types.go](https://github.com/router-for-me/CLIProxyAPI/blob/307803510bce87f986fcf7b1a84190f105c7b4aa/sdk/pluginapi/types.go) / [sdk/pluginabi/types.go](https://github.com/router-for-me/CLIProxyAPI/blob/307803510bce87f986fcf7b1a84190f105c7b4aa/sdk/pluginabi/types.go) | schema 2 引入 lifecycle；3 / 5 减少 stream payload 请求/历史拷贝；6 保留 management JSON 原文。使用 schema 6，不能假定旧版本具备接口 |
| [internal/pluginhost/management.go](https://github.com/router-for-me/CLIProxyAPI/blob/307803510bce87f986fcf7b1a84190f105c7b4aa/internal/pluginhost/management.go) | 管理路由相对 `/v0/management`，不是相对插件；资源路由相对插件资源前缀。资源**没有管理鉴权**，不能用于返回请求数据 |
| [internal/logging/requestid.go](https://github.com/router-for-me/CLIProxyAPI/blob/307803510bce87f986fcf7b1a84190f105c7b4aa/internal/logging/requestid.go) 与 request_logger_format.go、runtime/executor/helps/logging_helpers.go | 文件名使用完整 trace 的**末尾 8 位**；日志保留 API REQUEST / API RESPONSE 多次尝试，不能把最终下游 RESPONSE 当上游原文 |
| [CPAMC PluginResourcePage](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/714de0760a3ce86094b178fb46f834d34bc4a2a5/src/features/plugins/PluginResourcePage.tsx) / pluginResources.ts | 菜单资源在 iframe 加载，没有管理密钥 postMessage 桥接。v0.2.0 读取同源已保存的登录凭据并由 CPA 校验；读取 `apiKeyNames.ts` 的来源指纹备注，匹配后同步哈希 / 备注，不另存密钥 |
| [CPAMC encryption.ts](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/4e4aeff23f9f82b3ccdbe48b2e774d5dc918234e/src/utils/encryption.ts) / [useAuthStore.ts](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/4e4aeff23f9f82b3ccdbe48b2e774d5dc918234e/src/stores/useAuthStore.ts) | 当前凭据仍为 `cli-proxy-auth.state`，`enc::v2::` 的 XOR 字节来自 `cli-proxy-api-webui::secure-storage\|<host>`，不再依赖 UA；旧 v1 仍可读取非登录数据。v0.8.3 修正插件 v2 解码，同时增加用户自选的独立手动密钥保存；可逆混淆不是加密 |
| [CPA Key Billing](https://github.com/haowang02/cpa-plugin-key-billing/tree/e997191e04677b601329639108fca45e7e54d69c) 的 `internal/plugin/web/core.js`、`theme.js`、`keys.js` | 对照同源 CPAMC 记住的凭据读取、可逆存储格式与备注指纹算法；仍走 CPA 管理鉴权。本项目自动同步备注并保留手动别名优先级 |
| [CPA Key Billing usage.go](https://github.com/haowang02/cpa-plugin-key-billing/blob/91322c9f6bd617f0d81f0e4bae0ad5d33fddc260/internal/plugin/usage.go) 的 `executionMetadata`；[request-events.js](https://github.com/haowang02/cpa-plugin-key-billing/blob/91322c9f6bd617f0d81f0e4bae0ad5d33fddc260/internal/plugin/web/request-events.js) 的 `requestEventSession` | 会话列原样展示 CPA `UsageRecord.SessionID` / `ParentSessionID`，Billing 不负责推算会话或恢复正文；ParentSessionID 只表达父子关系，不包含分叉点。插件需要另外核对 previous_response_id / 共同上下文，不能据此复制父会话的最新全部历史 |
| [CPA session/info.go](https://github.com/router-for-me/CLIProxyAPI/blob/307803510bce87f986fcf7b1a84190f105c7b4aa/sdk/cliproxy/session/info.go)、[session/identity.go](https://github.com/router-for-me/CLIProxyAPI/blob/307803510bce87f986fcf7b1a84190f105c7b4aa/sdk/cliproxy/session/identity.go) 与 [handlers.go](https://github.com/router-for-me/CLIProxyAPI/blob/307803510bce87f986fcf7b1a84190f105c7b4aa/sdk/api/handlers/handlers.go) | `codex:` 来自客户端 Session / Thread 和 Agent 信息，`pck:` 来自 prompt_cache_key；宿主 canonical 身份还可能来自执行或上下文路由，不能一概当用户聊天 ID。只对有明确语义的原始会话字段或 protocol-prefixed SessionID 归并 |
| [Cherry promptCacheKey.ts](https://github.com/CherryHQ/cherry-studio/blob/92a945db3840d788d3d167570144e33ac358f225/src/main/features/apiGateway/utils/promptCacheKey.ts) 及其测试 | `cherry-agent:` 后 32 位小写十六进制是 `SHA256(agentSessionId)` 的前 32 位，稳定对应 Agent 会话；支持此精确格式，不将一般缓存 key 等同会话 |
| [Willxup/cpa-plugin-usage-keeper](https://github.com/Willxup/cpa-plugin-usage-keeper) | 参考资源菜单和 iframe 入口；Keeper 是外部应用嵌入，本项目改用二进制内嵌静态 GUI，减少部署组件 |
| [origin652/cpa-plugin-key-policy](https://github.com/origin652/cpa-plugin-key-policy) | 参考管理 API 与内嵌 UI、usage 接入；自定义认证插件的 usage.APIKey 可能是 Principal / key ID，不保证是客户端原始 key |
| [rheodev/cpa-plugin-privacyfilter](https://github.com/rheodev/cpa-plugin-privacyfilter) | 参考 before/after request hooks 的语义：after-auth 仍是协议转换前正文；不能把它标为真实 outbound request |
| [mmqz/cpa-multi-plugins](https://github.com/mmqz/cpa-multi-plugins) | 参考插件库打包形态和插件商店单库 zip + checksums.txt 约定 |

真实宿主测试发现当前 CPA 的 lifecycle RequestID 和 UsageReporter RequestID 可以不同。因此用量优先匹配 exact ID，再按**完整 TraceID**唯一关联，必要时以模型排除候选；存在歧义时保留待匹配状态，不将 A 的 key 或 token 写到 B 的记录。回归测试覆盖这两类 ID 差异、并发隔离及重试 usage 去重。


## 插件商店兼容性

发布 registry 使用 CPA `schema_version: 1` 的 GitHub Release 模式。核对了同一固定 CPA 提交中的 `internal/pluginstore/registry.go`、`github.go`、`install.go`：版本取自 `/releases/latest`，zip 名由 `ArchiveName(id,version,goos,goarch)` 计算，安装器校验 `checksums.txt`，并将根目录动态库写成带版本的安装文件。`scripts/check_store_contract.go` 调用官方公开 SDK，通过本地测试 HTTP 传输提供实际构建产物；生产插件本身不新增 CPA module 依赖。

私有仓库使用 `store-auth` 的 `github-token` 规则。官方客户端在已配置 artifact API 认证时优先使用 Release asset API，并在重定向时重新按目的地解析认证，因此凭证不跟随到无匹配规则的外部域名。
