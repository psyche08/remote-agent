# 锁屏自动解锁：真机调查记录（含重大结论更正）

本文记录在真机（macOS 26.5 / 25F71，M4 Pro，屏幕真实锁定）上，对"无人在场、
纯软件唤起一台已锁 Mac 并接管桌面"这条路径的完整调查。

> **2026-08-09 重大更正。** 本调查前半程反复断言"授权链已在真机完整验证、我们的
> mechanism 被 authd 调用→验签→放行、consumed 里的 nonce 是物证"。**这个结论是
> 错的**，且是本次任务最严重的一次误判。真相见下方"根因"一节：在 macOS 26.5 上，
> 第三方 authorization plug-in 被 `SecurityAgentHelper` 的 Library Validation 拒绝
> 加载，**我们的 plug-in 代码从未执行过**；`authd: running mechanism …` 那行是
> plug-in **加载失败之后**打出的，被我误读成了"成功执行"。GitHub 上的证据显示，
> 连 OpenAI 官方签名的 plug-in 在同一 macOS 版本上也被同样拒绝。

## 根因：Library Validation 在 macOS 26.5 拒绝第三方 SecurityAgent plug-in

真机统一日志，每一次 `running mechanism RemoteAgentLockedUse` 的前一毫秒，都有：

```
authorizationhosthelper.arm64: Error loading …/StagedPlugins/RemoteAgentLockedUse.bundle
  dlopen(...): code signature not valid for use in process:
  mapping process is a platform binary, but mapped file is not
authd: engine … running mechanism RemoteAgentLockedUse:invoke,privileged (1 of 1)
```

三次尝试全部如此。加载**失败**在前，`running mechanism` 在后——后者不是我们代码
执行的证据。决定性佐证：`AuthorizationPluginCreate called`（plug-in 真正被实例化
时才打印）在 12 小时日志中**只来自 `com.openai.sky.CUAService.AuthorizationPlugin`
（Codex 的）**，没有一条来自我们的 bundle。我们的 plug-in 从未被实例化。

`SecurityAgentHelper-arm64` / `authorizationhosthelper` 是 **平台二进制**
（platform: yes），macOS 26.5 的强化运行时 Library Validation 拒绝把非平台签名
（我们的 Developer ID，platform: no）的 bundle 映射进它。这不是 Accessibility /
Screen Recording 权限问题，也不是签名损坏——我们的 bundle `codesign --verify
--strict` 通过、Developer ID 链完整。是**平台策略**拒绝加载。

### GitHub 证据：OpenAI 官方在同一版本上也失败

openai/codex issue **#24013**（"Locked Computer Use authorization plug-in is
registered but rejected by macOS SecurityAgentHelper Library Validation"），环境
**macOS 26.5 (25F71)、Apple Silicon**，与本机逐字对应。报告者的 plug-in（OpenAI
Developer ID 签名、authorizationdb 注册与我们相同）被同样拒绝：

```
Library Validation failed: Rejecting '.../CodexComputerUseAuthorizationPlugin'
(Team ID: 2DC432GLL2, platform: no) for process 'SecurityAgentHelper-arm64'
(Team ID: N/A, platform: yes),
reason: mapping process is a platform binary, but mapped file is not
… Successfully removed staged plugin
```

相关联的后续 issue 印证这条路在 26.x 普遍不通：#24394（breaks unlock）、
#25788（fallback 把密码打进登录框）、#26319（解锁延迟 3-5s）、#29616（卡 17
分钟）、#32396（automatic unlock fails）。社区提出的 fix 是给 plug-in 加
`com.apple.security.cs.disable-library-validation` entitlement，但**未见在 26.5
上被确认有效**。

另一独立调查 scottjg/openaliro `docs/uwb-mac-login.md`（macOS 26.4.1 / 25E253）
补充了一个易混淆的关键事实：真正在解锁那一刻被求值的是 **`system.login.screensaver.unlock`**
（单一 mechanism `CryptoTokenKit:login`，其规则注释写明"不要修改"），而不是我们和
Codex 复刻者们都在改的 `system.login.screensaver`（它 resolve 到 `use-login-window-ui`）。
即便绕过 Library Validation，right 挂错这一点也需要重新审视。

## 结论

**在 macOS 26.5 上，通过第三方 authorization plug-in 自动免密解锁锁屏，是当前
不可达的**——不是本仓库的实现缺陷，是平台策略（Library Validation 拒绝非平台
SecurityAgent plug-in）。OpenAI 官方实现同样受阻。要打通需要其中之一：

1. plug-in 以带 `disable-library-validation` 的方式签名 —— 未经证实在 26.5 有效，
   且削弱强化运行时保护；
2. 苹果为该用途提供平台级授权 / entitlement —— 需向 Apple DTS 申请，非现有公开路径；
3. 等待苹果修复或明确这条 plug-in 路径的支持状态（多个官方 issue 悬而未决）。

**已交付且正确的部分**（与解锁那一跳无关，独立成立）：grant 契约、ECDSA 签名与
跨语言验签、nonce 账本、遮罩（覆盖全屏、多屏/镜像/扩展、窗口服务器几何确认）、
护栏状态机、grant 跨权限交接、以及 **Accessibility 操作通道（`ax_read`/`ax_press`/
`ax_setvalue`）——它在锁屏下直接操作应用元素树，不解锁、不注入、不依赖 plug-in，
对应 OpenAI 官方 window API**。AX 通道是"锁屏下操作"在当前平台策略下**唯一可行**
的路径，且已实现。

## 方法论教训

本调查中至少三次基于不充分证据做出被后续数据推翻的判断：(1) 早先把
`_authSuccessUsingPassword` 读成"靠密码"、又反转；(2) 把 `running mechanism` 读成
"我们的 mechanism 成功执行"——**这是最严重的一次，直到看到 GitHub issue 才发现
加载其实失败**；(3) 把 `StagedPlugins` 的加载失败误读成"正常首次暂存"。凡断言，
必须以当场、未过期、且能归因到具体进程的日志为据；`running mechanism` 不等于
plug-in 执行，必须由 `AuthorizationPluginCreate` 或 plug-in 自身输出佐证。
