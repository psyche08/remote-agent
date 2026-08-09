# Locked Use / Computer Use — 状态与后续工作

分支 `claude/computer-use-locked-user-cj8o53`，29 个 commit（相对 main），全部已推送。
本文记录**已完成**、**已验证到什么程度**、**后续待办**，以及**关键设计事实**。

配套文档：
- [SETUP-locked-unlock.md](SETUP-locked-unlock.md) — 真机三步交接（用户执行）
- [../../docs/locked-unlock-investigation.md](../../docs/locked-unlock-investigation.md) — 锁屏解锁完整调查与方法论教训
- [../../docs/computer-use-locked-user.md](../../docs/computer-use-locked-user.md) — 功能与安装文档

---

## 一、架构（已完成）

整个功能重写为独立 macOS 组件，取代原先"Go 服务 shell 出 scripts"的模型：

| 组件 | 路径 | 职责 |
|---|---|---|
| 常驻 Swift helper | `mac/RemoteAgentDesktop/` | 遮罩、护栏、grant 契约、AX 通道、凭据提交；UDS + newline-JSON 服务 |
| Authorization plug-in | `mac/authorization-plugin/` | authd 内的 grant 验签门卫（ObjC，Team-ID 签名） |
| Go 客户端 | `internal/computeruse/client.go` | 仅转发到 helper socket，无策略 |
| 内嵌分发 | `internal/desktopasset/` | helper 随 agent 二进制分发，单产物 + sha256 |
| LaunchAgent | `mac/launchagent/` | helper 以用户 GUI 会话身份常驻（TCC 授权归属） |

设计要点（均有真机依据，见调查文档）：
- helper 必须是**用户会话 LaunchAgent**，不能由 agent 派生（遮罩需 Aqua 会话；TCC 归属责任进程）。
- 配置**永不经 socket 下发**——helper 自读设备 config.json；能自解锁的能力必须在设备授予。
- grant 签名用 **ECDSA P-256**（非 Ed25519：plug-in 经 SecKey 验签，Ed25519 常量是 SPI）。

---

## 二、已完成且已验证（无需密码 / 无需真解锁）

| 项 | 验证方式 | 结论 |
|---|---|---|
| grant 契约、ECDSA 签名、跨语言验签 | preflight 用 Go/helper 真实签出的 grant 跑 plug-in 验签器 | ✅ 接受合法、拒绝换钥 |
| plug-in 只用公开 Security API | 对照 SDK 头文件 | ✅ 无 SPI |
| Go 私钥与 helper 兼容 | 金标向量 | ✅ 相同公钥 |
| 遮罩覆盖（单屏 / 外接镜像） | 像素采样 + 窗口服务器几何 + **人工目视确认（用户："黑了，符合要求"）** | ✅ 全屏黑、无边缘漏光 |
| 一屏一遮罩、镜像/扩展 | NSScreen 计数 + 去重矩形匹配 | ✅ |
| 人在场判定（两方向） | agent 活动实测 idle 不重置；外部进程模拟真人 idle 归零；纯函数单测 | ✅ |
| pointer.move/scroll、screen.capture | 真机执行（idle 83722s→0.34s、写出真实 PNG） | ✅ 解锁态生效 |
| pointer.click | 真机执行（CatDesk 前台点击） | ✅ 解锁态生效 |
| AX 通道（ax_read/press/setvalue） | 真机锁屏下读到 CatDesk 菜单树（111 项可操作） | ✅ **应用级可达** |
| 凭据托管逻辑 | 86 个单测（3 个存取测试需 entitlement，正确 skip） | ✅ 契约成立 |
| helper 全套 | `swift test` 86 通过；Go build/vet/test 通过；双平台构建 | ✅ |

---

## 三、锁屏自动解锁 — 机制已查清，真机往返未验证

**真相（24h 日志确证，取代早期所有误判）**：参照实现 Codex 在本机 24h 内**成功解锁 5 次**，
机制是**程序化提交存储的凭据**，不是免密绕过：

```
loginwindow: checkAuthWithContinuityHints:username:password: | no activity semaphore
authd: mechanism <gate>:allow (1 of 1)              ← plug-in 门卫
authd: mechanism builtin:reset-password (2 of 3)
authd: mechanism builtin:authenticate (3 of 3)      ← 用注入凭据真正认证
loginwindow: Unlock succeeded, with password        ← password 是程序提交的
```

**我们缺的确切一环**：`builtin:authenticate` 需要的凭据提交。我们的 plug-in 门卫已就位
（rule 数组中与 Codex 分支并存），但没有凭据注入，authd 链停在 `1 of 1`。

**已实现**（`UnlockCredential.swift` + 接进 `LockedUseController`）：
- 凭据存**数据保护钥匙串**（`AfterFirstUnlockThisDeviceOnly` + `kSecUseDataProtectionKeychain`）
  ——锁屏时可访问、开机前不可、不同步。
- access group 绑定 **Team ID 前缀**（`89LGY6BD53.com.psyche08.remote-agent.locked-use`）
  ——把凭据绑到本签名 helper，别的进程读不到（"ACL 绑签名"硬化）。
- 经公开 `LAContext.setCredential` + `evaluatePolicy` 提交，用后清零。
- 绝不经 socket；只能终端 `--set-unlock-credential`（stdin，不入 argv/历史）。
- 仅 armed turn + grant 验签后提交；无凭据则明确失败、屏幕保持锁定（安全方向）。

**未验证（真机往返）**——见 [SETUP-locked-unlock.md](SETUP-locked-unlock.md)，需用户执行：
1. `security unlock-keychain`（需登录密码；锁屏下 codesign 取不到私钥）
2. Developer ID 构建 + `--set-unlock-credential`（provision 真实登录密码）
3. `window_open` 往返（真实解锁 Mac）

**为什么未由 agent 执行**：这三步需要用户密码、会真实解锁机器。属用户亲手的安全决策，
非 agent 自主动作；安全分类器亦多次拦截同类操作。

---

## 四、后续工作（TODO）

### A. 完成锁屏解锁验证（用户 + agent 协作）
- [ ] 用户按 SETUP-locked-unlock.md 跑三步往返，贴回 `window_open` 返回 + `authd` 链日志。
- [ ] 若解锁成功：在 `LockedUseController` 补一条真机往返的集成验证记录。
- [ ] 若失败：按 `authd` 三-mechanism 链定位——门卫是否放行、`builtin:authenticate` 是否收到凭据。

### B. 凭据安全硬化（实现前需用户明确授权）
- [ ] 将 access group 前缀从字面 `89LGY6BD53` 改为构建期注入（避免硬编码 Team ID）。
- [ ] 评估把凭据从"密码明文存 Keychain"升级为 Secure Enclave 包裹，或改用一次性解锁票据。
- [ ] 明确凭据提交的触发边界：仅 relay mTLS + 已认证 turn 可触发，审计每次提交。
- [ ] `provision` 流程加入二次确认与到期/轮换策略。

### C. 遮罩"碰不了"的键盘缺口（需解锁基线验证）
- [ ] 当前遮罩吃鼠标（`ignoresMouseEvents=false`）但**不吃物理键盘**（`canBecomeKey=false`）。
      旁人物理打字仍会进到后面的应用。见 `DisplayShield.swift` 注释。
- [ ] 修复需让遮罩成为 key、吞掉键盘、同时不挡 agent 合成事件——改焦点路由，
      必须在**解锁会话**上实测（否则静默破坏 agent 自己的打字）。

### D. AX 窗口内容可达性（跨应用验证）
- [ ] 锁屏下仅应用级（菜单）AX 可达，窗口内容（web area/按钮/文本框）不可达——
      已在 CatDesk（Electron）+ 全应用扫描确认。openai/codex #24013 的锁屏失败正是
      "Failed: Get app state"。
- [ ] 待验证：原生应用（提醒事项/备忘录）锁屏下窗口内容是否可达（未逐一验证，不作推广）。
- [ ] `AXManualAccessibility` 已设在 app + 每个 window（Chromium web 内容开关），
      锁屏下未暴露——待解锁基线复验其在活跃窗口下是否生效。

### E. 遗留 flaky 测试（与本功能无关，已挂后台任务）
- [ ] `internal/provider` Codex daemon 测试在 macOS 上 flaky（首次 exec 新写二进制的
      代码签名评估耗时越过 1s 超时）。诊断与修法见后台任务 `relaxed-noyce-8571d9`
      的会话记录（该次改动未提交，需重做）。

### F. 部署与清理
- [ ] 本机当前保留了测试安装：plug-in 已装、解锁 right 已改、config 有 computer_use 块
      （备份 `config.json.backup-20260808-211523`）、LaunchAgent 未加载、无凭据。
      用户选择保留以自跑验证。验证后如需还原：`sudo mac/authorization-plugin/uninstall.sh`
      + `mac/launchagent/install.sh --uninstall`。
- [ ] 开 PR（未开）。
- [ ] `deploy/publish-release.sh` 已接入 helper 内嵌与签名；正式发布前需在目标机跑 preflight。

### G. right 注册的存疑点（低优先，需查证）
- [ ] scottjg/openaliro 调查指出解锁真正求值的可能涉及 `system.login.screensaver.unlock`
      （CryptoTokenKit 智能卡链）。但本机实测 Codex 也只注册在 `system.login.screensaver`
      且成功，故当前注册方式正确；此项仅作存疑记录，无需改动。

---

## 五、关键方法论教训（务必读，见调查文档详版）

调查锁屏解锁时，至少四次因"拿单一信号当铁证"而误判并被推翻：
1. 把 `_authSuccessUsingPassword` 读成"靠人敲密码"——实为程序化提交。
2. 把 `authd: running mechanism` 读成"我们 mechanism 成功执行"——需 nonce 消费佐证。
3. 把 `StagedPlugins` 加载失败读成"plug-in 从不运行、平台不支持"——实为无害暂存副本。
4. 把单个 GitHub issue 读成"整个功能不支持"——参照实现实际正常运行。

**规则**：任何断言需多源、当场、可归因到具体进程的证据。`running mechanism` ≠ plug-in 执行；
nonce 被消费 = plug-in 执行。过滤"排除 password"会漏掉程序化提交的成功案例。
