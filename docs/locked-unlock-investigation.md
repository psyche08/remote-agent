# 锁屏自动解锁：真机调查记录

真机环境：macOS 26.5 / 25F71，M4 Pro，屏幕真实锁定。目标：让 agent 在锁屏、无人
在场时接管这台 Mac。

> **给读者的方法论警告（重要）。** 本调查过程曲折，作者多次因"拿单一信号当铁证"
> 而下错结论并反复更正。本文只保留经**多源、当场、可归因**证据支撑的结论。两条尤其
> 关键的教训写在文末，请先读。

## 一、已确证：我们的 plug-in 在 26.5 上确实加载并执行

**决定性物证是一个被消费的 nonce。** `system.login.screensaver` 被求值时（真机，
06:32:02），`/Library/Application Support/remote-agent/locked-use/consumed/` 下出现
一个 root 拥有的 nonce 文件。消费 nonce 的**唯一**途径是 plug-in 的验签逻辑：helper
以普通用户身份运行、写不进这个 root 目录；只有 plug-in 以 root 在 authd 内执行才能
写。**这个 nonce 证明 plug-in 真正运行了、验签通过、烧掉了 nonce。**

### 关于 "Library Validation failed / StagedPlugins" 的更正

日志中确有大量：

```
authorizationhosthelper: Error loading …/StagedPlugins/RemoteAgentLockedUse.bundle
  code signature not valid for use in process (platform binary vs non-platform)
```

作者一度据此断言"plug-in 从未加载、授权链从未生效"——**这是错的**。判据有二：

1. **这些失败全部来自 `StagedPlugins/`（暂存副本），不是主目录 `SecurityAgentPlugins/`。**
   暂存副本的校验失败是无害的；真正被 authd 加载运行的是主目录那份。
2. **参照实现（OpenAI Codex）在同一台机器上呈现完全相同的模式**：24h 内其
   `AuthorizationPluginCreate called`（真实实例化）出现 **7 次**，Library Validation
   拒绝也 **7 次**，两者并存。若拒绝是普适的，Codex 到处都无法运行——但它正常运行。
   可见 Staged 失败与主目录成功并存是**正常现象**，不代表 plug-in 不工作。

（我们的 plug-in 不像 Codex 那样打 `os_log`，所以"日志里看不到我们的
`AuthorizationPluginCreate`"不能作为任何判据——它本就静默。用 nonce 作证据。）

**结论：本仓库的 grant 契约、ECDSA 签名、跨语言验签、nonce 账本、plug-in、rule
注册——在真机 26.5 上均正确且已执行。** 早先"plug-in 被平台策略拒绝、从未运行"的
结论作废。

## 零、最终真相（由 24h 全量日志确证，取代本文以下所有早期结论）

**Codex 在这台机器上过去 24 小时多次成功完成锁屏操作——24h 内有 5 次"3-mechanism
完整解锁"。我此前"从无非密码解锁"的结论是错的，错因是一个致命的过滤失误。**

我用"排除 `_authSuccessUsingPassword`"去找"非密码解锁"，把成功案例全滤掉了——因为
**Codex 的成功解锁在日志里同样标记为 "with password"**。真相是那个 password 不是人
敲的，是**程序化注入**的。成功解锁（08-08 21:42、21:52 等）的实证序列：

```
loginwindow: _checkAuthWithContinuityHints:…username:password: | no activity semaphore, continuing auth
loginwindow: _authCopyRightsWithUsername:password: | The current user is trying to unlock the screen
authd: running mechanism CodexComputerUseAuthorizationPlugin:allow (1 of 1)
authd: running mechanism builtin:reset-password,privileged (2 of 3)
authd: running mechanism builtin:authenticate,privileged (3 of 3)
authd: UID 0 authenticated as user sheng (UID 501) for right 'system.login.screensaver'
loginwindow: _authSuccessUsingPassword | Unlock succeeded, with password
```

**真正的机制（可复制）**：

1. Codex 的 client **持有用户凭据**（用户在 Codex 中授权登录时提供），很可能经
   `com.apple.LocalAuthenticationRemoteService`（其 client 二进制引用了它）。
2. 远程 Computer Use 请求到达 → client 通过 loginwindow 的
   `checkAuthWithContinuityHints:username:password:` **程序化提交 username+password**
   （`no activity semaphore` = 不等人输入）。
3. authd 求值 `system.login.screensaver`：Codex 的 `:allow` 门卫确认"存在合法的
   Computer Use 待处理授权"，随后 **`builtin:authenticate` 用注入的凭据真正认证** →
   解锁成功。
4. 解锁后用 AX（window API）或坐标（desktop API）操作。

**结论修正——这不是"免密绕过"，是"程序化提交存储的凭据"。** 我们缺的**不是**触发、
**不是** right 挂错、**不是** plug-in 没执行（这些早期结论均作废或不完整）。我们缺的是
**第 1–2 步：一个持有用户凭据、并经 LocalAuthentication/authorization-context 程序化
提交 username+password 的机制。** 我们的 plug-in 作为门卫已就位（rule 数组中
`com.psyche08.remote-agent.locked-use` 与 Codex 分支并存），但没有凭据注入这一环，
authd 的链停在 `1 of 1`，永远走不到 `builtin:authenticate`。

**这也回答了"屏幕必须锁"的矛盾**：Codex 的方式下屏幕**确实经历真实的系统级解锁**
（setScreenIsLocked→0），操作完再锁回。它不是"遮罩伪装"，是持凭据的真解锁。安全模型
是"用户预先把凭据托付给 Codex，Codex 在收到经认证的远程请求时代为提交"——凭据的
保管与提交授权由 Codex 账户体系背书。

**要在本仓库复刻，需实现凭据托管 + 程序化提交**：安全地保存用户解锁凭据（Keychain /
Secure Enclave），在收到经 mTLS/账户认证的远程 turn 时，经
`checkAuthWithContinuityHints:username:password:` 或 LocalAuthentication remote service
提交。这是**明确的、有官方先例的实现路径**，非平台不可达——但它涉及在设备上保管用户
解锁凭据，是重大安全设计决策，需用户明确授权与专门的凭据保护设计后再实现。

---

## 二之前置：（历史）曾疑为根因的 "screenLock delay is immediate"

（下文为发现最终真相前的调查过程，保留以记录方法论。`screenLock immediate` 影响的是
Apple Watch 式 auto-unlock，与上述"程序化提交凭据"路径是不同机制；后者才是 Codex 实际
使用并成功的路径。）

### （原）根因假设 —— "screenLock delay is immediate" 禁用了 auto-unlock

（本节是整个调查的真正答案，晚于下面各节被发现，置顶。）

唯一一次 loginwindow 自己发起求值的解锁（T0）日志里有决定性一行：

```
loginwindow: -[LWAuthServiceManager activateAppropriateServicesAllowingAutoUnlock…] |
             lock mode doesn't allow enabling auto unlock
```

而 `sysadminctl -screenLock status` 返回：

```
screenLock delay is immediate
```

**这台机器设为"锁屏后立即要求密码"。macOS 的 auto-unlock（涵盖 Apple Watch 与
authorization-plugin 这类免密路径）只在有宽限期时才被允许启用；"立即"这个设置直接
让 loginwindow 在 `activateAppropriateServicesAllowingAutoUnlock` 一步判定
"lock mode doesn't allow enabling auto unlock"，从而不据 plug-in 的授权撤锁，退回
密码。**

这解释了全部现象且完全自洽：
- plug-in 确实执行、授权成功（nonce 被消费）；
- 但 `screenLock delay = immediate` → auto-unlock 被系统禁用 → 授权不转化为撤锁；
- 每一次解锁都走密码（24h 内 4 次 `_authSuccessUsingPassword`，8/8 起全量日志中
  **从无一次非密码解锁**）；
- 参照实现 Codex 在同机同样被挡——这不是我们或 Codex 的实现缺陷，是**这台机器的
  锁屏策略**。

**可验证的下一步（需用户决定）**：将 screenLock delay 从 immediate 改为一个宽限期
（如 5 秒 / 1 分钟），auto-unlock 才可能被允许，plug-in 的授权才可能真正解锁。
命令示例（**会降低这台机器的锁屏安全性，属安全权衡，未擅自执行**）：

```
sysadminctl -screenLock 60 -password <你的密码>   # 60 秒宽限
# 或在 系统设置 > 锁定屏幕 > 「在屏幕关闭或开始屏保后要求输入密码」改为非"立即"
```

FileVault 为 Off（已确认），不是此处的阻断因素。是否存在 MDM/配置描述文件强制该
策略需 `sudo profiles` 确认。

## 二、（历史）曾误判的方向：解锁那一刻求值的是另一个 right

plug-in 在跑、`AuthorizationCopyRights(system.login.screensaver)` 返回成功、nonce
被消费——但**屏幕没有解锁**。多次真机尝试，`ioreg` ground truth 全程 locked。

第二独立调查 scottjg/openaliro `docs/uwb-mac-login.md`（macOS 26.4.1 / 25E253，
证据分级严谨）指出一个易混淆的关键事实：

```
system.login.screensaver         →  resolve 到 use-login-window-ui
system.login.screensaver.unlock  →  单一 mechanism CryptoTokenKit:login，
                                     规则注释写明"这是 screensaver-unlock 规则、不要修改"
```

**loginwindow 实际撤锁时求值的是 `system.login.screensaver.unlock`，而我们（以及
所有照 Codex 复刻的项目）都把 mechanism 注册在了 `system.login.screensaver` 上。**
我们的 plug-in 因此会在"某人主动求值 screensaver right"时运行（如探针、如 authd
的其它触发），却**不在 loginwindow 的真实解锁链上**——这正好解释了"授权成功却不
解锁"这个贯穿始终的矛盾。

这是当前最有希望的、尚未验证的方向：将 mechanism 注册进
`system.login.screensaver.unlock`。但该 right 的注释明写"不要修改"，且其既有
mechanism 是 `CryptoTokenKit:login`（智能卡登录），改动它的风险与可支持性都需在
备用机上先验证，openaliro 亦将"替换该 Apple 拥有的 right 是否可作为产品契约"列为
需向 Apple DTS 确认的未决项。

## 三、不依赖解锁的可行路径：Accessibility 通道（已实现并真机验证）

无论解锁那跳何时打通，还有一条**不需要解锁**就满足"锁屏下操作"的路径，且已经跑通：

**Accessibility API 在锁屏下直接触达应用的元素树,不经过被锁屏隔断的 HID /
window-server 层。** 合成 HID / CGEvent 事件在锁屏下到不了桌面（实测空闲计时器
不动），AX 走的是应用进程内的 UI 层。真机实测（屏幕 `locked=true`）确认了它的
**可达范围与边界**——详见第五节:**应用级结构（菜单，111 项）锁屏下可达可操作;
窗口内容（web area / 按钮 / 文本框）不可达。**

这与 OpenAI 官方文档一致：Codex 有两套 API——desktop API（坐标+合成事件，解锁态用）
与 window API（`get_app_state`/`click by index`/`set_value`，全 Accessibility 驱动）。
我们新增的 `ax_read`/`ax_press`/`ax_setvalue` 对应后者。它不解锁、不注入、不依赖
plug-in。

（实现中修过一个真 bug：AX 树含循环引用——Electron 应用的 child 会指回 application
元素——未去重的遍历会把节点预算耗在自引用上（早期一次读到 1794 个"元素"实为同一
自引用链的重复）。已按 CFEqual 身份去重修复,修复后得到 141 个干净节点。）

## 四、方法论教训（务必读）

本调查至少四次因证据不足而误判、随后被推翻：

1. 把 `_authSuccessUsingPassword` 读成"靠密码解锁"，忽略同期 `passwordRequried:0`。
2. 把 `authd: running mechanism` 读成"我们的 mechanism 成功执行"——应以 nonce 消费
   或 plug-in 自身输出佐证。
3. 把 `StagedPlugins` 的加载失败读成"plug-in 从未运行、平台不支持"——**最严重**，
   直到对照 Codex 同样的失败+成功并存模式才纠正。
4. 把单个 GitHub issue（#24013）读成"整个功能在 26.5 不支持"——而参照实现在本机
   实际正常运行。

统一规则：**任何断言必须有多源、当场、可归因到具体进程的证据。单一日志行、单个
issue、已过期的记忆片段，都不足以支撑结论。** nonce 被消费 = plug-in 执行；
`running mechanism` ≠ plug-in 执行。


## 五、锁屏下 AX 的可达性边界（真机实测）

将 AX 通道在锁屏下对 CatDesk（Electron 应用）实测：

- **应用级元素可达**：菜单栏 7 项、111 个菜单项全部可读可操作（`ax_read` 返回，
  `lock_state = locked`）。菜单树是应用级的，锁屏下始终存在，因此**菜单级操作在
  锁屏下确定可用**。
- **窗口内容不可达**：`kAXWindowsAttribute` 只返回应用自引用，web area / 按钮 /
  文本框不在树里。已尝试在 application 与每个 window 上设 `AXManualAccessibility`
  / `AXEnhancedUserInterface`（Chromium 展开 web 内容的文档开关），锁屏下仍未暴露
  窗口内容。

这与参照实现一致，非本仓库缺陷：openai/codex issue #24013 在锁屏下的失败正是
**"Failed: Get app state"**——`get_app_state` 抓的就是窗口 accessibility 内容。
Codex 的 `SkyComputerUseService` 日志显示它锁屏下大量使用 **ReplayKit（屏幕录制）**
而非纯 AX，而截屏在锁屏下亦受限。

**最一致的解释**：应用在锁屏时其窗口 UI 被系统降级/不渲染，故窗口级 AX 内容不可达；
只有应用级结构（菜单）保持可达。是否所有应用皆如此、原生应用是否不同，未在本机
逐一验证——此处不作过度推广。

## 六、总结论

"人不在、屏幕锁定、agent 操作应用**窗口内容**"，在 macOS 26.5 上，经本调查覆盖
Codex 的全部技术手段，均受平台限制：

| 手段 | 锁屏下状态 |
|---|---|
| plug-in 免密解锁 | plug-in 执行、授权成功（nonce 消费），但 loginwindow 不据此撤锁（解锁全走密码），Codex 同机同样 |
| AX 操作窗口内容 | 窗口内容不可达（仅菜单可达），Codex `get_app_state` 同样失败（#24013） |
| 合成事件 / 截屏 | 合成事件到不了锁屏 HID 层；截屏受限 |

**确定可用**：解锁态的 pointer/keyboard/capture；**锁屏下的应用菜单级 AX 操作**。
后者是当前平台策略下，"锁屏 + agent 操作"唯一无争议可用的通道，且已实现。
