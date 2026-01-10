# CLIProxyAPI Plus

[English](README.md) | 中文

这是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 Plus 版本，在原有基础上增加了第三方供应商的支持。

> [!NOTE]
> 上游参考（用于了解基础行为/原始文档）：https://github.com/luispater/CLIProxyAPI/blob/main/README_CN.md

---

> [!IMPORTANT]
> ## 这是一个 fork（请先阅读）
> 当前仓库是 CLIProxyAPI 的 fork/衍生版本。
>
> **动机：** 让你能用“已经在付费的订阅/账号”（Claude Code、Codex、Gemini、Copilot 等）通过统一的 OpenAI 兼容接口，给各种 coding CLI / 客户端 / SDK 使用；既可本地跑，也可托管部署。
>
> **相对上游的主要差异（以及原因）：**
>
> - **更多 Provider 适配与登录方式（Copilot、Grok 等）** —— 方便把请求路由到你已有订阅/账号对应的服务，同时保持一致的 OpenAI 兼容 API 形态。
> - **面向 Railway 的部署路径**（`scripts/railway_start.sh`、`docs/RAILWAY_GUIDE.md`）—— 目标是让你几乎“零心智负担”搭一个个人常驻的 CLIProxyAPI 实例，随时随地调用。
>   - 本地先完成交互式登录，然后用 `scripts/auth_bundle.sh` 打包成 `AUTH_BUNDLE`，在远端环境恢复使用。
> - **更适合托管的凭据迁移**（`AUTH_BUNDLE`/`AUTH_ZIP_URL`）—— 避免手动搬运一堆文件/密钥，部署更顺滑。
> - **更强调兼容性（含 Responses 风格客户端）** —— 尽量让“只支持 OpenAI 兼容接口”的工具最小改动即可工作。
> - **一次配置，多处使用** —— 让 Claude Code / Codex CLI / Gemini 兼容客户端 / IDE 扩展都指向同一个 Base URL，由代理负责路由与账号管理。
>
> 如果你只需要上游项目的原始行为/功能，请对比原始上游仓库及其文档。

---

所有的第三方供应商支持都由第三方社区维护者提供，CLIProxyAPI 不提供技术支持。如需取得支持，请与对应的社区维护者联系。

现已支持通过 OAuth 登录接入 OpenAI Codex（GPT 系列）和 Claude Code。

该 Plus 版本的主线功能与主线功能强制同步。

## 与主线版本版本差异

[![bigmodel.cn](https://assets.router-for.me/chinese-4.7.png)](https://www.bigmodel.cn/claude-code?ic=RRVJPB5SII)

本项目由 Z智谱 提供赞助, 他们通过 GLM CODING PLAN 对本项目提供技术支持。

GLM CODING PLAN 是专为AI编码打造的订阅套餐，每月最低仅需20元，即可在十余款主流AI编码工具如 Claude Code、Cline、Roo Code 中畅享智谱旗舰模型GLM-4.7，为开发者提供顶尖的编码体验。

智谱AI为本软件提供了特别优惠，使用以下链接购买可以享受九折优惠：https://www.bigmodel.cn/claude-code?ic=RRVJPB5SII

---

<table>
<tbody>
<tr>
<td width="180"><a href="https://www.packyapi.com/register?aff=cliproxyapi"><img src="./assets/packycode.png" alt="PackyCode" width="150"></a></td>
<td>感谢 PackyCode 对本项目的赞助！PackyCode 是一家可靠高效的 API 中转服务商，提供 Claude Code、Codex、Gemini 等多种服务的中转。PackyCode 为本软件用户提供了特别优惠：使用<a href="https://www.packyapi.com/register?aff=cliproxyapi">此链接</a>注册，并在充值时输入 "cliproxyapi" 优惠码即可享受九折优惠。</td>
</tr>
<tr>
<td width="180"><a href="https://cubence.com/signup?code=CLIPROXYAPI&source=cpa"><img src="./assets/cubence.png" alt="Cubence" width="150"></a></td>
<td>感谢 Cubence 对本项目的赞助！Cubence 是一家可靠高效的 API 中转服务商，提供 Claude Code、Codex、Gemini 等多种服务的中转。Cubence 为本软件用户提供了特别优惠：使用<a href="https://cubence.com/signup?code=CLIPROXYAPI&source=cpa">此链接</a>注册，并在充值时输入 "CLIPROXYAPI" 优惠码即可享受九折优惠。</td>
</tr>
</tbody>
</table>


## Fork 版快速开始

本 fork 主要面向：

- 把 **OAuth/订阅账号登录**（Codex / Claude Code / Gemini / Copilot / Grok 等）统一代理成一个 OpenAI 兼容 Base URL
- **托管一个个人常驻实例**（尤其是 Railway），从任意设备/网络环境都能调用

关键文档：

- 面向最终用户的 Railway 部署指南：`docs/RAILWAY_GUIDE.md`
- Railway 脚本说明：`scripts/README_RAILWAY.md`
- SDK 使用（Go 内嵌）：`docs/sdk-usage_CN.md`
- SDK 高级：`docs/sdk-advanced_CN.md`
- SDK 认证/访问：`docs/sdk-access_CN.md`
- 凭据加载/更新：`docs/sdk-watcher_CN.md`

托管实例的核心目标：

- 本地登录一次，然后用 `scripts/auth_bundle.sh` 打包成 `AUTH_BUNDLE`（或用 `AUTH_ZIP_URL`），在托管环境恢复使用。
- 让各类工具只需要配置同一个 Base URL + API Key。

## 能力概览（Fork）

- OpenAI 兼容接口（chat + tools），支持 Provider 路由
- 多 Provider 的 OAuth/Cookie 登录与多账户轮询
- 流式/非流式，多模态输入（按 Provider 能力）
- 兼容目标：OpenAI 兼容客户端/SDK（含 Responses 风格）+ 各类 coding CLI

## 新手入门

CLIProxyAPI 用户手册： [https://help.router-for.me/](https://help.router-for.me/cn/)

## 管理 API 文档

请参见 [MANAGEMENT_API_CN.md](https://help.router-for.me/cn/management/api)

## 谁与我们在一起？

这些项目基于 CLIProxyAPI:

### [vibeproxy](https://github.com/automazeio/vibeproxy)

一个原生 macOS 菜单栏应用，让您可以使用 Claude Code & ChatGPT 订阅服务和 AI 编程工具，无需 API 密钥。

### [Subtitle Translator](https://github.com/VjayC/SRT-Subtitle-Translator-Validator)

一款基于浏览器的 SRT 字幕翻译工具，可通过 CLI 代理 API 使用您的 Gemini 订阅。内置自动验证与错误修正功能，无需 API 密钥。

### [CCS (Claude Code Switch)](https://github.com/kaitranntt/ccs)

CLI 封装器，用于通过 CLIProxyAPI OAuth 即时切换多个 Claude 账户和替代模型（Gemini, Codex, Antigravity），无需 API 密钥。

### [ProxyPal](https://github.com/heyhuynhgiabuu/proxypal)

基于 macOS 平台的原生 CLIProxyAPI GUI：配置供应商、模型映射以及OAuth端点，无需 API 密钥。

### [Quotio](https://github.com/nguyenphutrong/quotio)

原生 macOS 菜单栏应用，统一管理 Claude、Gemini、OpenAI、Qwen 和 Antigravity 订阅，提供实时配额追踪和智能自动故障转移，支持 Claude Code、OpenCode 和 Droid 等 AI 编程工具，无需 API 密钥。

> [!NOTE]
> 如果你开发了基于 CLIProxyAPI 的项目，请提交一个 PR（拉取请求）将其添加到此列表中。

- 新增 GitHub Copilot 支持（OAuth 登录），由[em4go](https://github.com/em4go/CLIProxyAPI/tree/feature/github-copilot-auth)提供
- 新增 Kiro (AWS CodeWhisperer) 支持 (OAuth 登录), 由[fuko2935](https://github.com/fuko2935/CLIProxyAPI/tree/feature/kiro-integration)、[Ravens2121](https://github.com/Ravens2121/CLIProxyAPIPlus/)提供

如果需要提交任何非第三方供应商支持的 Pull Request，请提交到主线版本。

## 贡献

欢迎贡献！请随时提交 Pull Request。

1. Fork 仓库
2. 创建您的功能分支（`git checkout -b feature/amazing-feature`）
3. 提交您的更改（`git commit -m 'Add some amazing feature'`）
4. 推送到分支（`git push origin feature/amazing-feature`）
5. 打开 Pull Request

## 许可证

此项目根据 MIT 许可证授权 - 有关详细信息，请参阅 [LICENSE](LICENSE) 文件。

## 写给所有中国网友的

QQ 群：188637136

或

Telegram 群：https://t.me/CLIProxyAPI

## 进阶内容（参考）

`README.md` 已以 fork 为主线做了精简；更完整的功能与集成说明请查看上游文档或本仓库的 `docs/`。

- 上游中文 README：https://github.com/luispater/CLIProxyAPI/blob/main/README_CN.md
- 本仓库 Railway 部署：`docs/RAILWAY_GUIDE.md`
- 本仓库 Railway 脚本说明：`scripts/README_RAILWAY.md`
