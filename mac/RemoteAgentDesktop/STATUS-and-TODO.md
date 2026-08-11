# Computer Use / Locked Use — 状态与验收门禁

更新时间：2026-08-09。当前分支：`feature/computer-use-locked-use`。

## 目标

在 Mac 已锁屏、现场无人时，由**当前受信任的大模型 turn**完成一个闭环：

1. macOS 原生解锁流程求值 AgentHalo 的 Apple Authorization Plug-in；
2. Plug-in 只凭一次性签名 grant 放行，不读取、保存或注入登录密码；
3. 临时解锁前先遮黑全部物理显示器，并屏蔽真人键鼠；
4. 模型得到未被黑色遮罩污染的界面 PNG 与 Accessibility 树；
5. 模型执行 AX、鼠标和键盘操作；
6. turn 结束、TTL 到期、真人输入、遮罩失效或任何状态不确定时，撤销 grant、重新锁屏并读回确认。

这与 [ChatGPT Locked Use](https://learn.chatgpt.com/docs/computer-use#locked-use) 的产品语义对齐；底层采用 Apple 的 [Authorization Plug-in](https://developer.apple.com/documentation/security/extending-authorization-services-with-plug-ins) 机制。

配套文档：

- [SETUP-locked-unlock.md](SETUP-locked-unlock.md)：目标机安装和真实锁屏验收
- [../../docs/computer-use-locked-user.md](../../docs/computer-use-locked-user.md)：架构、安全边界和协议
- [../../docs/locked-unlock-investigation.md](../../docs/locked-unlock-investigation.md)：历史调查；顶部勘误优先

## 当前实现

### 1. 模型工具与权威 turn

- Codex 新建 thread 在 `thread/start` 注册 `computer_use` dynamic tools：
  `get_app_state`、`press`、`set_value`、`click`、`type_text`、`press_key`、`scroll`。
- 工具请求必须同时匹配当前 app-server generation、active thread、权威 turn ID 和 call ID；恢复的旧 thread、过期 generation、错误 thread/turn 均失败关闭。
- 每次 mutation 前必须成功调用一次新的 `get_app_state`；观察能力是单次的、原子消费，并把 AX path 绑定到同一 app，不能跨 app 或在 UI 已变化后复用。
- provider 终态、interrupt、error 或 app-server 丢失会撤销 lease、关闭窗口并重锁。
- 模型工具通过进程内 broker 调用 helper，不把 session/turn 参数当作模型提供的权限。
- 配置 Locked Use 后，裸 HTTP `window open`、`action` 和 `ax` 默认拒绝；只有设备本地显式设置 `computer_use.debug_http_actions=true` 才用于调试。HTTP close 始终保留，因为它只能收权和重锁。

当前只把上述**模型原生闭环**接到 Codex 新建 thread。Codex resume 不能安全补发 thread-only dynamic tools，因此失败关闭；Claude 的受管 CLI/MCP 父进程证明尚未实现，不能声称支持。

### 2. Apple Authorization Plug-in 解锁链

- `system.login.screensaver` 是 `k-of-n=1` 的 rule 列表；安装器新增独立的 `evaluate-mechanisms` 子规则，并放在 `use-login-window-ui` 正常密码分支之前。子规则固定 `shared=false`、`timeout=0`，不让一次 Allow 进入可复用 credential cache。
- Plug-in 对无效、缺失、过期、重放或无法消费的 grant 返回 Deny；该分支不获授权后，正常密码分支仍可继续。
- 合法 grant v2 使用 ECDSA P-256，绑定 `purpose`、设备、turn、nonce、primary console user 的
  `console_uid`/`console_username` 和 15 秒以内的有效期；helper 不是当前 console user 时不发布，
  plug-in 再把两项与本次 authorization transaction username 及 passwd UID 精确核对。
- nonce 以 `O_CREAT|O_EXCL` 原子消费。Plug-in 依次写本 nonce 的 root-owned
  `receipt.pending`（Allow 前）、`receipt`（final，`SetResult(Allow)` 成功后）和
  `receipt.complete`（仅该成功 Allow 的 `MechanismDestroy`）；三者都是精确 32 字节 proof。
- plug-in 从读取 grant 到 final proof 完成持有 shared fd lock；controller 撤 grant 取得
  exclusive lock 后再从磁盘复核 pending/final/complete，关闭 grant-expiry 与迟到
  verifier 之间的竞态。
- `receipt.complete` 前，exact `UserPasswordTextField` 必须持续是同一 AX element，
  且系统必须一直 locked；只有 complete 后的 field lifecycle completion + unlocked +
  shield coverage 才能把窗口置为 open。complete 前的真人、Apple Watch 或其他分支
  解锁一律 fail closed、重锁并进入 quarantine。
- 解锁触发走 loginwindow 自身的 Accessibility 控件 `UserPasswordTextField` + confirm/press，不提交任何 credential。Apple public API 未保证 `MechanismDestroy` 晚于 loginwindow
  的可见解锁副作用，所以 complete → unlock 顺序仍是目标机 E2E 门禁。

旧的 `LAContext.setCredential(.applicationPassword)` / 登录密码托管实现已删除。LocalAuthentication policy evaluation 是独立认证上下文，不能证明它参与 loginwindow 的当前撤锁事务。

### 3. helper、截图与操作

- Swift helper 是用户 Aqua 会话里的常驻 LaunchAgent，持有 AppKit 遮罩、Screen Recording、Accessibility 和输入事件 tap。
- helper UDS 为 `0600`，启用 computer use 时还校验 kernel audit token 对应的 peer code signature：Go agent 必须是同一 Team、精确 identifier `dev.linsheng.agenthalo`；helper 自身必须是 `dev.linsheng.agenthalo.desktop`。
- 截图使用 ScreenCaptureKit，排除 helper/遮罩窗口，按 `CGDisplayBounds` 在内存中合成左上角原点的多显示器 PNG；不会返回可被同 UID 进程替换的临时文件路径。
- 模型 `click`/`scroll` 显式使用 composite PNG 坐标；Swift 将其映射到 CG global coordinates，
  覆盖负原点和上下排列显示器，并拒绝 union 外或显示器空洞坐标。纯函数已覆盖，真实 Retina、
  旋转和多屏布局仍是目标机验收项。
- helper 只返回 `image_base64` + `media_type=image/png`。Go broker 严格校验 base64、PNG magic 和 25 MiB 上限，再以 `data:image/png;base64,...` 交给模型。
- AX 路径限制为非负、深度不超过 40；`set_value` 可以写空字符串；bundle ID 优先于应用名，避免选错应用。
- 普通未锁屏 computer use 仍可在没有 Locked Use window 时运行；helper 会再次读取真实锁态，锁屏且无窗口时拒绝。

### 4. 物理隐私与故障关闭

- 每块 `NSScreen` 都有一个 shielding-level、全黑、`sharingType=.readOnly` 的窗口；普通录屏保留
  黑罩，helper 自己的 ScreenCaptureKit filter 才排除该 application/windows。窗口忽略鼠标，
  让经过标记的 agent 事件落到目标应用。
- session event tap 丢弃所有未同时满足随机 marker 与 Core Graphics 报告的 source PID=`getpid()` 的键盘、鼠标、滚轮和 tablet 事件。它可靠屏蔽物理/未标记输入和普通 synthetic input，但 CGEvent 字段没有被 Apple 文档承诺为不可伪造，不能把它表述成对持有 Accessibility/Event Injection 权限的恶意同 UID 进程的密码学认证。
- 未标记输入另有 sticky latch。即便事件已被成功丢弃、HID idle 计数没有变化，controller 仍会在约 40 ms 轮询中发现真人到场、停止窗口并重锁。
- opening/open/closing 是显式状态；same-turn 并发 open 等待原结果，close 会取消 opening、等待在途授权和操作结束，再重锁。
- open 后持续监控系统锁态；只要重新 locked 或锁态不可读，该 turn 永久关窗，外部再次解锁也不能复活旧 lease。
- grant 撤销失败、pending/final/complete 缺失或乱序、alternate unlock、解锁状态
  迟迟不确定，或重锁无法确认会进入 quarantine：disarm、保持遮罩、持续撤
  grant 和重锁。proof 已出现但 ordered UI lifecycle 无法确认时会设置
  `requires_manual_recovery=true`；后续任意 unlocked 快照不能修复归因歧义，只有受控重启/人工恢复
  才能结束该永久 fail-closed 状态。优雅退出只有在确认安全后才退出。
- 启动时先 scrub grant 并强制建立锁定基线；配置文件存在但缺少/关闭 `computer_use` 时也安装 disabled controller，绝不退化成无守卫桌面路径。
- Locked Use 开启时 `require_display_shield=false` 会被强制规范化为 true，因为同一生命周期也承载 physical-input guard。

### 5. 密钥与发布

- grant 私钥保存在当前用户的 file-based login Keychain；首次显式 provisioning 使用默认 creator ACL，把 item 绑定到已安装 helper 的 code-signing designated requirement，不使用 restricted keychain access-group entitlement。
- `--provision-locked-use-key --config <path>` 是唯一允许创建缺失私钥的入口，也可读取既有私钥并输出 public-key JSON；不锁屏、不启动 socket、不要求登录密码。AgentHalo 没有 plaintext key 或旧产品 key 导入/迁移路径。
- helper 正常运行只读取既有 item，并禁止 Keychain 认证 UI；缺失、ACL 不匹配，或 helper 启动/重启时 login Keychain 被手工或策略锁定，都必须 fail closed，不能删除、轮换或静默新建 key。
- helper 与 Go agent 使用同一 Developer ID Team 和固定 identifier 签名；发布脚本临时嵌入 helper，完成后总会清除 ignored asset，避免后续构建误带陈旧二进制。
- agent 每次启动都会刷新已加载 helper，即使二进制字节未变化；因此 config 的 true→false、socket、TTL 等变更不会留在旧 helper 进程中。

## 自动化验证

最终交付前必须重新执行并记录：

```bash
cd mac/RemoteAgentDesktop
CLANG_MODULE_CACHE_PATH=/tmp/remote-agent-clang-cache \
SWIFT_MODULECACHE_PATH=/tmp/remote-agent-swift-cache \
swift test --disable-sandbox

cd ../..
GOCACHE=/tmp/remote-agent-go-cache go test ./...
GOCACHE=/tmp/remote-agent-go-cache go vet ./...
bash -n deploy/install.sh deploy/publish-release.sh mac/preflight.sh
(cd mac/authorization-plugin && ./build.sh --adhoc)
git diff --check
```

2026-08-09 当前工作树的实际结果：

- Swift 全量测试 `150/150` 通过；
- `go test ./...` 与 `go vet ./...` 通过；
- computeruse/API/provider 定向 race tests 通过；
- Authorization Plug-in ad-hoc build/sign/verify 通过（仅编译验证，禁止部署）；
- 非破坏 preflight 的所有 attempted checks 通过；3 项按设计 skip：真实 plug-in rule
  未安装、display-shield 扰动检查未运行、screen-lock 扰动检查未运行；
- 相关 shell 脚本 `bash -n` 和 `git diff --check` 通过。

覆盖重点包括：grant/receipt 的 owner、mode、symlink、精确字节；open/close 并发与 late unlock；物理输入 latch；屏幕 PNG 内存编码；AX 越界；signed peer；provider generation/thread/turn；inspect-before-mutate；provider 终态重锁；HTTP 搭车拒绝；helper mode/restart/config reload。

## 真实设备状态与硬门禁

截至 2026-08-09，本机只读检查结果：

- macOS 26.5.2 (25F84), arm64；
- 系统已有 OpenAI 的 Authorization Plug-in 分支和正常 `use-login-window-ui` 分支；
- AgentHalo plug-in、helper、socket 和 LaunchAgent **均未安装**；
- 当前 shell 的 `security find-identity -v -p codesigning` 显示 `0 valid identities found`。

因此本轮代码可以完成，但**不能诚实地把真实“锁定 → 无人值守授权解锁 → 模型截图/操作 → 重锁”标记为已通过**。必须先由设备所有者提供/解锁 Developer ID 签名身份，并明确授权安装 plug-in、修改 authorizationdb 和实际锁屏。完整步骤见 SETUP 文档。

真实验收必须同时满足：

- [ ] helper、agent、plug-in 的签名/Team/identifier 验证通过；显式 provisioning 能创建/读取 creator-ACL-bound Keychain item，正常 runtime 只读，login Keychain 锁定时重启会 fail closed；
- [ ] 安装后人工密码解锁仍正常，卸载/Recovery 路径已准备；
- [ ] 锁屏且现场无人时，远端启动一个**新 Codex turn**；
- [ ] `get_app_state` 返回非黑 PNG 和目标应用 AX 树；
- [ ] 模型至少完成一次 AX mutation 和一次键鼠 mutation；
- [ ] authd/loginwindow 日志、root pending/final/complete proofs、helper audit 能归因到同一 nonce/turn；
- [ ] 真机记录 `complete -> field lifecycle completion -> unlocked` 顺序；在 final/complete 竞态中同时发起 Apple Watch/其他 alternate unlock 时必须不 open、保持遮罩并重锁；
- [ ] turn completed/interrupted/error 后系统读回 `locked=true`；
- [ ] 物理键盘和鼠标事件被屏蔽，并立即触发重锁与 automatic-unlock suppression；
- [ ] 外接扩展显示器下无遮挡泄露；普通后台录屏保留黑罩；在左侧/上方/下方显示器按 composite PNG 坐标点击或滚动命中正确目标；
- [ ] helper 正常退出/更新时重锁；崩溃恢复行为被实测并记录。

## 仍需明确保留的生产边界

1. **没有独立 root deadman。** helper 遭 `SIGKILL`、内核崩溃或 WindowServer 故障时，进程内遮罩和监控会一起消失；launchd 重启后的 startup scrub 会重锁，但中间仍可能有暴露窗口。面向不可信本地环境的正式版本应增加独立特权 guardian/heartbeat。
2. **Codex resume 与 Claude 尚未接入模型工具闭环。** 当前只保证 AgentHalo 新建的 Codex thread；其他 provider 必须先实现不可伪造的父进程/turn 绑定。
3. **ScreenCaptureKit、loginwindow AX 与 authorizationdb 都是系统版本敏感边界。** 单元测试不能替代目标 macOS 版本上的真实锁屏验收。
4. **Authorization Plug-in 分发仍是管理员操作。** 自动更新不能在没有明确管理员授权时静默改 authorizationdb。
5. **code signature 不是实例身份。** helper UDS 的 Team/identifier pin 不能区分受管 agent 与同 UID 另起的、字节相同的已签名副本；后者可自配 provider/agent 形成 living-off-the-land 路径。hostile same-UID 场景需要 root-owned launchd broker/Mach service，把 capability 绑定到唯一受管 audit token/PID；仅 pin path、签名或 creator-ACL-bound Keychain secret 不足以区分满足同一 designated requirement 的副本。
6. **系统级 TCC 能力不受 UDS pin 管理。** 恶意同登录用户进程若已持有 Screen Recording，可显式排除 shield；持有 Accessibility/Event Injection 时也可直接操作或伪造事件。普通录屏与普通 synthetic input 必须被黑罩/guard 拦住，但对敌对 TCC 客户端的隔离需要独立 agent GUI session 或 privileged broker。
7. **post-terminal 解锁归因没有公开的因果回调。** 代码已对 `receipt.complete` 前的 field 变化/提前解锁永久失败关闭；但 complete 后的 Apple Watch/另一 authorization transaction 可以产生与本 transaction 相同的 `field disappeared + unlocked` 快照。Apple Authorization Plug-in 公开 API 不提供把 loginwindow 可见撤锁绑定到 nonce 的 transaction ID。在每个目标 macOS 版本的真机竞态验收通过，或实现独立 guardian/client completion 之前，无人值守环境必须确保 alternate unlock 不可用/不在场；当前不声称已对该竞态提供生产保证。
8. **锁状态 generation 只对已观测边沿 sticky。** 已开窗口和普通 unlocked 操作都在入口/出口比对 generation，watcher 观测到重锁后即使又 unlocked 也会撤权并丢弃结果。但 macOS 没有向该用户会话 helper 提供文档化、无损的 lock-transition notification；完全落在两次 `CGSession` probe 之间的极短 locked→unlocked 边沿仍不可观测。真机验收必须覆盖这类竞态；不能把当前轮询表述为与 WindowServer 原子同步。

在上述真实 E2E 门禁打勾前，状态应写为：**实现已落地；自动化结果按最终交付记录逐项陈述；真机 Locked Use 待设备所有者验收**，而不是“生产完成”。
