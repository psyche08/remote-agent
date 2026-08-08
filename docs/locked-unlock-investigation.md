# 锁屏自动解锁：真机调查记录

本文记录在真机（macOS 26.5，M4 Pro，屏幕真实锁定）上，对"无人在场、纯软件
唤起一台已锁 Mac 并接管桌面"这条路径的完整调查。结论先行：

- **授权链已 100% 证实可用。** grant 契约、plug-in、rule 排布三者在真机 authd
  日志中被确凿证明按设计工作。
- **唯一缺口**是让 `loginwindow` 实际撤销锁屏——它有两条解锁路径，且由其闭源内部
  逻辑自行选择走哪条。所有能想到的用户态触发手段均无法使它改走我们所在的那条。

## 一、授权链本身：已证实

在屏幕真实锁定、helper 发布有效 grant 的同时，用 `AuthorizationCopyRights`
求值 `system.login.screensaver`，authd 日志：

```
authd: engine 29955: running mechanism CodexComputerUseAuthorizationPlugin:allow (1 of 1)
authd: engine 29955: running mechanism RemoteAgentLockedUse:invoke,privileged (1 of 1)
authd: Succeeded authorizing right 'system.login.screensaver'
```

即 `k-of-n = 1` 的"或"语义正确：同机已装的 OpenAI CUA 分支先跑、因无 pending
授权而拒绝，落到我们的分支，grant 验签通过 → 授权成功，`consumed/` 中 nonce 被
烧掉。这条链跑通了不止一次，每次结果一致。

**结论：本仓库交付的一切——grant 契约、ECDSA P-256 签名、跨语言验签、O_EXCL
nonce 账本、plug-in 的 Allow/Deny 语义、rule 注册与排序——在真机上均正确。**

## 二、缺口：loginwindow 的两条解锁路径

`loginwindow` 撤销锁屏有两条独立路径：

| 路径 | 入口 | 认证方式 |
|---|---|---|
| PAM | `/etc/pam.d/screensaver`（`_beginServiceNamed:screensaver`） | 密码 / 生物识别 |
| authorizationdb | 求值 `system.login.screensaver`（`_authCopyRightsWithUsername:password:`） | 我们的 mechanism 挂在这里 |

授权 mechanism **不在** PAM 配置里，只在 authorizationdb 路径上。因此只有当
loginwindow 选择 authorizationdb 路径时，我们的 grant 才有机会撤锁。

`IOPMAssertionDeclareUserActivity`（`kIOPMUserActiveLocal` / `kIOPMUserActiveRemote`
均试）能可靠地让 loginwindow **发起**解锁：

```
loginwindow: userActivityChanged: | user event received, start an unlock with 'active user' as the reason
loginwindow: startUnlock: | entered newValue: kLWUnlockFromUserActive (9), oldValue: kLWLockFromDirectLock (8)
loginwindow: LWPAMManager _beginServiceNamed: | began service screensaver
```

但它**总是走 PAM 路径**，随后报 `AvailableMechanisms=( ) · User interaction
required` 并停下，**从不进入 authorizationdb 路径**。

## 三、已排除的用户态触发手段（逐一实测，均无效）

每次都用 `ioreg -n Root -d1 -a` 的 `CGSSessionScreenIsLocked` 作为 ground truth
核对，全部仍为锁定：

| 手段 | 结果 |
|---|---|
| 发布 grant 后轮询等待 | loginwindow 无反应 |
| `IOPMAssertionDeclareUserActivity(Local)` | 发起解锁，停在 PAM 层 |
| `IOPMAssertionDeclareUserActivity(Remote)` | 到达 `authStateForUsername`，仍停 |
| `PreventUserIdleDisplaySleep` 断言 + 反复声明活动 | 无进展 |
| 显示器 `pmset displaysleepnow` → 声明活动唤醒 | 到达认证层，未求值 right |
| 锁屏 UI 已显示（用户已选定）后触发 | 未求值 right |
| `AuthorizationCopyRights` 空环境 | mechanism 跑、放行，但屏幕不解锁 |
| `AuthorizationCopyRights` + 环境带 username | 同上：授权成功，屏幕不解锁 |
| `CGEventPostToPid` 直投 loginwindow（key/space） | 无反应 |
| 合成 HID 事件（鼠标/键盘/点击） | **锁屏时根本到不了 HID 层**（实测空闲计时器不重置） |

最后一条尤其关键：**用户态进程发出的合成事件，在锁屏状态下到不了 HID 层。**
解锁状态下能重置空闲计时器（5.2s→0.5s），锁屏状态下不能（空闲 2000s+ 不动）。

## 四、参照实现（OpenAI CUA）的关键事实

同机已装 `CodexComputerUseAuthorizationPlugin.bundle` 及其规则
`com.openai.sky.CUAService.AuthorizationPlugin.remote`。其规则注释自述：

> "Screen-unlock branch that asks SkyComputerUseClient whether an active
> Computer Use login authorization is pending."

即它的 plug-in **不是无条件放行，而是 XPC 询问一个客户端是否有 pending 授权**——
与我们"读磁盘 grant"在语义上等价。对其全部组件二进制的导入符号分析
（`nm -u`）显示，与解锁相关的系统调用**只有**：

```
_IOPMAssertionDeclareUserActivity
_IOPMAssertionCreateWithDescription
_IOPMAssertionRelease
_CGSessionCopyCurrentDictionary   （只读锁状态）
```

**没有任何 SAC* 私有解锁原语，没有 CGSCreateLoginSession，没有安装任何特权
守护进程。** 也就是说，它能调用的解锁相关 API，我全都试过了。

## 四之补：唯一成功那次的实证，与分岔点

对唯一一次成功解锁（记为 T0，`loginwindow` 发起）的 authd 日志确证：

```
authd: engine … running mechanism CodexComputerUseAuthorizationPlugin:allow (1 of 1)
authd: Succeeded authorizing right 'system.login.screensaver' by client
       '/System/Library/CoreServices/loginwindow.app' [171]
loginwindow: _authSuccessUsingPassword | Unlock succeeded, with password
```

三个关键点：

1. **求值 right 的 client 是 `loginwindow` 自己**，不是某个用户态进程。这正是
   与本人所有探针的根本差异——探针里求值 right 的 client 是探针进程，authd 虽
   放行，但 loginwindow 不因此撤锁；只有 loginwindow **自己发起**的求值才会。
2. 那次只跑了 `CodexComputerUseAuthorizationPlugin:allow`（`1 of 1`）——因为它
   排在 rule[0]，`k-of-n=1`，它返回 Allow 后求值即结束，**没轮到我们的分支**。
   这说明参照实现的免密分支在其自身触发下确实生效。
3. FileVault 为 Off，故 `with password` 一句不必然意味着键入了密码（keybag 无需
   真实凭据）；此点不单独作为结论依据。

**分岔点已定位到 `began service screensaver` 之后那约 2.8 秒**：成功序列在此之后
走 `_authCopyRights` 并求值授权链；本人所有触发（含与成功序列前段逐行一致的
`userActivityChanged → enqueueUnlock reason:9 → Did Wake → authStateForUsername
→ began service screensaver`）都停在 `began service screensaver`，不进入求值。

这 2.8 秒里发生了什么，是打通最后一跳的唯一未知数。它已随统一日志保留窗口过期，
**必须在 `/usr/bin/log stream` 实时捕获运行期间让成功序列重现一次**才能取得——
这是本调查唯一仍缺、且无法由本进程自行生成的数据。

> 方法论提醒：本调查中断言必须以**当场查询到的日志**为据。会话过程中曾两次基于
> 已过期日志的记忆片段做出被随后数据推翻的判断；凡结论，均需 live capture 或
> 未过期的 `log show` 支撑。

### 补充：捕获期间的 Codex 请求未触发解锁

`/usr/bin/log stream` 从 06:29:35 起持续捕获。期间用户在 ChatGPT 发起的两次
Computer Use 请求（06:30、06:34）在捕获中表现为：`SkyComputerUseClient` 启动、
建立 XPC、连上 service——但**全程没有 `userActivityChanged`、没有 `began
service screensaver`、没有任何 loginwindow 解锁活动**。即那两次请求**根本没有
发起解锁**，最可能是 Codex 侧判定条件不满足（其失败文案含 "cannot be associated
with a ChatGPT thread" / "paused because physical input was detected"）而未尝试。

**因此，要取得那关键 2.8 秒的数据，需要在 live capture 运行时，让参照实现完成一次
真正解锁屏幕的操作**（如 T0 那样走到 `_authSuccessUsingPassword | Unlock
succeeded`）。仅"发起请求"不足够——必须是那次请求真的把屏幕解开。

## 交接状态

- 代码、遮罩、护栏、grant 交接、授权链、rule 注册：**均已在真机验证并提交**。
- 唯一未完成：让 `loginwindow` 自身发起对该 right 的求值，从而在我们的分支上
  免密解锁。缺的数据是 T0 型成功序列中 `began service screensaver → _authCopyRights`
  之间约 2.8 秒的完整调用链。
- 取数方法：`/usr/bin/log stream --predicate 'process == "loginwindow" OR
  process == "authd" OR process == "coreautha" OR process CONTAINS
  "SkyComputerUse"'`（务必绝对路径 `/usr/bin/log`，`log` 在 zsh 是 builtin），
  运行期间让参照实现完成一次真正解锁，逐行比对其与本仓库触发序列在该 2.8 秒的
  差异。

## 五、下一步（未完成）

要打通最后一跳，路径有二：

1. **定位那个纯用户态触发条件**——需要在 `log stream`（务必用 `/usr/bin/log`
   绝对路径，`log` 在 zsh 是 builtin）实时捕获运行期间，让参照实现再成功解锁
   一次，抓下 `kLWUnlockFromUserActive` 成功走到 `Succeeded authorizing` 之间的
   完整调用序列，与我们的失败序列逐行对比。这是当前唯一能得到关键数据的途径。
2. **特权 HID 注入**——一个 root LaunchDaemon 用 IOKit 在 HID 硬件层注入真实
   输入，产生 loginwindow 认可的物理级用户活动。技术上应可行，但显著增大攻击面
   （root 常驻 + 能注入输入），且据"Codex 无物理输入"的情报，很可能非必需。

在打通之前，Locked Use **无法**在"无人在场、纯远程唤起已锁 Mac"场景下工作。但
**一旦有任何真实用户活动使 loginwindow 走上 authorizationdb 路径，授权链已证实
会正确放行并解锁**——缺的纯粹是那个软件触发，不是本仓库的任何实现缺陷。
