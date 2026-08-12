# Computer Use / Locked Use — 状态与验收门禁

更新时间：2026-08-12。当前分支：`feature/computer-use-locked-use`。

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

- Claude 默认使用 `desktop_computer_use`，以 Claude Desktop 的原生会话作为问答和授权界面的
  唯一可变 owner；每个 logical session 持久绑定一个 session-sticky route，绑定后不能因单次
  发送失败在 Desktop 与 CLI 之间切换。
- Claude Desktop 的 prompt 输入、`AskUserQuestion` 回答和工具授权都在固定
  provider/logical-session/turn 的短时进程内 Computer Use transaction 内完成。问答支持单选、
  多选、`Other`、`Next` 和 `Submit`，并通过 transcript 严格核对 question→answer；授权只允许
  精确点击 `Allow once` 或 `Deny`，拒绝 `Always`、session/task 级等扩大权限的选项。
- 每个 UI/CLI 副作用前先持久创建独立的 attempted tombstone；Claude `/send_prompt` 还要求稳定
  `operation_id`。进程崩溃、PWA 重试或 API 重启只能恢复相同请求的已知/不确定结果，不能再次
  输入或提交。route、native session binding、operation tombstone 与 task/session outcome 都是
  durable state，不能靠 observer hook 的可改写输出决定是否重发。
- `stream_json_cli` 只是 fallback：仅全新、尚未 committed 的 logical session，且 Desktop
  capability preflight 在任何 UI mutation 之前失败时才可选择；一旦触及 Desktop UI 或投递结果
  不确定，必须返回 `needs_manual`，不得通过 CLI 补发。
- Codex 新建 thread 在 `thread/start` 注册 `computer_use` dynamic tools：
  `get_app_state`、`press`、`set_value`、`click`、`type_text`、`press_key`、`scroll`。
- 工具请求必须同时匹配当前 app-server generation、active thread、权威 turn ID 和 call ID；恢复的旧 thread、过期 generation、错误 thread/turn 均失败关闭。
- 每次 mutation 前必须成功调用一次新的 `get_app_state`；观察能力是单次的、原子消费，并把 AX path 绑定到同一 app，不能跨 app 或在 UI 已变化后复用。
- provider 终态、interrupt、error 或 app-server 丢失会撤销 lease、关闭窗口并重锁。
- 模型工具通过进程内 broker 调用 helper，不把 session/turn 参数当作模型提供的权限。
- 配置 Locked Use 后，裸 HTTP `window open`、`action` 和 `ax` 默认拒绝；只有设备本地显式设置 `computer_use.debug_http_actions=true` 才用于调试。HTTP close 始终保留，因为它只能收权和重锁。

Codex resume 仍不能安全补发 thread-only dynamic tools，因此失败关闭。Claude Desktop-first 闭环
已经实现并进入 m4pro 真机验收；AgentHalo.6 已验证 pregrant 两阶段 readiness 不会提前发布 grant，
但也暴露出 system-wide focused application 不是 loginwindow 的可靠 AX root。AgentHalo.7 的 exact
loginwindow process/root 修复尚待签名部署和 E2E，不能把 Claude Locked Use
写成已验收完成。

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
- 解锁触发走 loginwindow 自身的 Accessibility 控件：精确 focus `UserPasswordTextField`，确认
  public `kAXValueAttribute` 可写后显式写入空字符串，再执行 confirm/press；全程不读取字段值，
  不写入 sentinel 或 credential；空值虽不改变凭据状态，赋值事件本身可能启动授权，因此
  wake 后先用最长 8 秒、完全不发布 grant 的 discovery/focus/readiness 阶段等待 exact field、
  空 `AXValue` 可写性和 exact AXConfirm/AXPress action 同时 ready；字段
  ready 后 controller 再次核对 opening owner、真人输入、locked state 和 primary console identity，
  才以当时 `Date()` mint/write 10 秒 grant。由此 wake/discovery 不消耗 grant TTL，也不在字段未
  ready 时暴露短时授权；
  helper 不再信任 system-wide focused application，也不会 activate/frontmost loginwindow；它只接受
  唯一、未 terminated、PID>0 且 bundle ID=`com.apple.loginwindow`、bundle URL=
  `/System/Library/CoreServices/loginwindow.app`、executable URL=
  `/System/Library/CoreServices/loginwindow.app/Contents/MacOS/loginwindow` 全部精确匹配的
  `NSRunningApplication`。AX root 只由该 PID 的 `AXUIElementCreateApplication` 创建并用
  `AXUIElementGetPid` 回验；搜索种子固定为 focused UI element、focused window、windows、app root，
  去重后做深度/节点有界 BFS，每个候选和最终字段都必须仍归属同一 PID；不使用 role/title fallback；
  从首次空值尝试起使用新的短 AX deadline，
  从空值写入尝试起任一步 AX 失败都直接返回，不会在同一 request 内重写或再次 submit。该顺序是
  AgentHalo 当前的真机诊断实现，不代表对其他产品内部调用序列的声明；是否仍需 confirm/press
  且其行为无害，以目标 macOS 的真实锁屏 E2E 为准。Apple public API 未保证 `MechanismDestroy` 晚于 loginwindow
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
GOCACHE=/tmp/remote-agent-go-cache go test -race -count=1 ./...
GOCACHE=/tmp/remote-agent-go-cache go vet ./...
bash -n deploy/install.sh deploy/publish-release.sh mac/preflight.sh
(cd mac/authorization-plugin && ./build.sh --adhoc)
git diff --check
```

2026-08-12 当前工作树的实际结果：

- Swift 全量测试 `178/178` 通过；
- Go 全量 `go test -count=1 ./...`、全量 race `go test -race -count=1 ./...` 与
  `go vet ./...` 通过；
- Authorization Plug-in ad-hoc build/sign/verify 通过（仅编译验证，禁止部署）；
- 默认非破坏 preflight 的所有 attempted checks 通过；已安装的 plug-in/rule 检查通过，2 项
  破坏性检查按设计 skip：display-shield 扰动和 screen-lock 扰动未运行；
- 相关 shell 脚本 `bash -n` 和 `git diff --check` 通过。

覆盖重点包括：grant/receipt 的 owner、mode、symlink、精确字节；open/close 并发与 late unlock；物理输入 latch；屏幕 PNG 内存编码；AX 越界；signed peer；provider generation/thread/turn；inspect-before-mutate；provider 终态重锁；HTTP 搭车拒绝；helper mode/restart/config reload。

## 真实设备状态与硬门禁

截至 2026-08-12，m4pro 的实际状态：

- Developer ID 正式构建已完成签名和 Apple 公证；当前 `AgentHalo.6` main/helper（commit
  `83369bc`）已部署，main identifier=`dev.linsheng.agenthalo`、helper
  identifier=`dev.linsheng.agenthalo.desktop`、Team=`89LGY6BD53`，签名校验通过；
- 正式签名的 `AgentHaloLockedUse` Plug-in 已安装并通过签名校验；
  `system.login.screensaver` 已链接独立的 `dev.linsheng.agenthalo.locked-use` 子 rule，正常
  `use-login-window-ui` 密码分支仍保留；
- Locked Use key 已在登录 Keychain 中完成 provisioning，配置、运行目录、socket 和 hook 权限已
  收紧；helper 的 Screen Recording/Accessibility TCC 已配置。AgentHalo 默认
  `computer_use.enabled=true`、`locked_use.enabled=true`，部署后的 startup gate 曾读回
  `armed=true`、`active=true`、`quarantined=false`；
- 首个真实 locked Claude prompt 使用 `desktop_computer_use`，在 grant TTL 内没有收到
  authorization receipt，任务安全结束为 `needs_manual` / `delivery_outcome=unknown`；没有 CLI
  fallback、没有重复输入，窗口关闭且机器保持锁定。审计确认当次 loginwindow 交互只做了
  focus + confirm/press，没有显式执行空 `kAXValueAttribute` 写入，因此没有启动
  `system.login.screensaver` transaction，Plug-in 未被调用；
- m4pro 后续时序证据显示 mouse wake 后 loginwindow UI 约 4 秒才 ready；旧实现把 1.5 秒
  discovery 放在已经发布的 grant 内，导致 `AgentHalo.5` 的第二次真机 locked E2E 在字段出现前
  失败、没有 authorization receipt。现已为待发布的 `AgentHalo.6` 拆成“wake + 不授权的
  pre-submission exact-field discovery/focus/value+action readiness → `authorization_field_ready`
  audit → callback 内重验并现 mint grant → 单次空 AXValue + preselected action”两阶段；callback
  前的取消、真人输入或 readiness 失败不会发布 grant，callback 后任一 AX 失败也不会重新
  mint/write；
- AgentHalo.6 的 fresh Claude logical session `aa212ed9c44b` 固定 route=
  `desktop_computer_use`；operation
  `prompt-agenthalo6-locked-20260812-100033` 安全结束为 `needs_manual` /
  `delivery_outcome=unknown`，没有 input、CLI fallback 或重复提交。helper audit 在
  `02:00:56.844` 记录 `authorization_request_returned`，原因是 exact password field not found，
  随后 `open_failed`、`window_closed`；全程 0 次 `authorization_field_ready`、0 次
  `grant_published`、0 个 receipt。最终 helper `armed=true`、`active=true`、未 quarantine/manual/
  suppressed、窗口和 shield 均关闭。机器最初 `IOConsoleLocked=Yes`，10:01:51 由另一/人工路径解锁；
  此结果不是 AgentHalo 授权成功；
- 根因是旧代码从 system-wide focused application 开始 AX 搜索；锁屏显示已 ready、TCC auth=2 时，
  focused app 仍不保证是 loginwindow。待发布的 `AgentHalo.7` 改为上述唯一 exact loginwindow PID、
  root/field PID 回验和有界 multi-seed BFS，同时保留 AgentHalo.6 的 pregrant readiness 与单次提交；
  **AgentHalo.7 尚待正式签名、公证、部署到 m4pro 并完成真实锁屏 E2E**；
- 旧 `remote-agent` 仍保留运行，只有下列真实 E2E 全部通过后才执行最终删除/切流。

因此安装、签名、公证和基础启动门禁已经落地，但**不能诚实地把“锁定 → 无人值守授权解锁 →
Claude 截图/问答/授权/操作 → 重锁”标记为已通过**。完整操作步骤见 SETUP 文档。

真实验收必须同时满足：

- [x] helper、agent、plug-in 的签名/Team/identifier 验证通过；显式 provisioning 已创建并可读取 creator-ACL-bound Keychain item；
- [ ] 修复后的 main/helper 重新正式签名、公证并部署；正常 runtime 只读，login Keychain 锁定时重启会 fail closed；
- [ ] 安装后人工密码解锁仍正常，卸载/Recovery 路径已准备；
- [ ] 锁屏且现场无人时，远端创建 Claude fresh logical session；确认 session-sticky route 为
  `desktop_computer_use`，并完成一次 prompt input/response；
- [ ] 在同一 Claude Desktop native session 完成 `AskUserQuestion` 单选、多选、`Other`、`Next`、
  `Submit`，远端答案与最终 transcript 的 question→answer 精确一致；
- [ ] 分别完成一次 Claude 工具 `Allow once` 和一次 `Deny`；不点击或接受任何 `Always`、
  session/task 级授权；
- [ ] 对 prompt、问答和授权使用相同 `operation_id`/request 重试，并覆盖 agent/PWA 重启；
  attempted tombstone 必须保证每项副作用最多发生一次，未知结果不得 CLI fallback；
- [ ] `get_app_state` 返回非黑 PNG 和目标应用 AX 树；
- [ ] 模型至少完成一次 AX mutation 和一次键鼠 mutation；
- [ ] authd/loginwindow 日志、root pending/final/complete proofs、helper audit 能归因到同一 nonce/turn；
- [ ] 真机记录 `complete -> field lifecycle completion -> unlocked` 顺序；在 final/complete 竞态中同时发起 Apple Watch/其他 alternate unlock 时必须不 open、保持遮罩并重锁；
- [ ] turn completed/interrupted/error 后系统读回 `locked=true`；
- [ ] 物理键盘和鼠标事件被屏蔽，并立即触发重锁与 automatic-unlock suppression；
- [ ] 外接扩展显示器下无遮挡泄露；普通后台录屏保留黑罩；在左侧/上方/下方显示器按 composite PNG 坐标点击或滚动命中正确目标；
- [ ] helper 正常退出/更新时重锁；崩溃恢复行为被实测并记录。
- [ ] 上述门禁全部通过后才停用并删除 m4pro 上的旧 `remote-agent`，并复核 AgentHalo ingress、
  provider/session、Locked Use 和回滚状态。

## 仍需明确保留的生产边界

1. **没有独立 root deadman。** helper 遭 `SIGKILL`、内核崩溃或 WindowServer 故障时，进程内遮罩和监控会一起消失；launchd 重启后的 startup scrub 会重锁，但中间仍可能有暴露窗口。面向不可信本地环境的正式版本应增加独立特权 guardian/heartbeat。
2. **Codex resume 仍不支持 dynamic-tool 闭环；Claude 尚未完成真机 E2E。** Claude Desktop-first
   route、durable operation tombstone、问答和一次性授权操作已经实现，但 AgentHalo.7 的 exact
   loginwindow PID/root 修复还需重签部署和锁屏验证。任何 Desktop mutation 之后都禁止切到
   CLI；不能把 `needs_manual` 当作可重试发送。
3. **ScreenCaptureKit、loginwindow AX 与 authorizationdb 都是系统版本敏感边界。** 单元测试不能替代目标 macOS 版本上的真实锁屏验收。
4. **Authorization Plug-in 分发仍是管理员操作。** 自动更新不能在没有明确管理员授权时静默改 authorizationdb。
5. **code signature 不是实例身份。** helper UDS 的 Team/identifier pin 不能区分受管 agent 与同 UID 另起的、字节相同的已签名副本；后者可自配 provider/agent 形成 living-off-the-land 路径。hostile same-UID 场景需要 root-owned launchd broker/Mach service，把 capability 绑定到唯一受管 audit token/PID；仅 pin path、签名或 creator-ACL-bound Keychain secret 不足以区分满足同一 designated requirement 的副本。
6. **系统级 TCC 能力不受 UDS pin 管理。** 恶意同登录用户进程若已持有 Screen Recording，可显式排除 shield；持有 Accessibility/Event Injection 时也可直接操作或伪造事件。普通录屏与普通 synthetic input 必须被黑罩/guard 拦住，但对敌对 TCC 客户端的隔离需要独立 agent GUI session 或 privileged broker。
7. **post-terminal 解锁归因没有公开的因果回调。** 代码已对 `receipt.complete` 前的 field 变化/提前解锁永久失败关闭；但 complete 后的 Apple Watch/另一 authorization transaction 可以产生与本 transaction 相同的 `field disappeared + unlocked` 快照。Apple Authorization Plug-in 公开 API 不提供把 loginwindow 可见撤锁绑定到 nonce 的 transaction ID。在每个目标 macOS 版本的真机竞态验收通过，或实现独立 guardian/client completion 之前，无人值守环境必须确保 alternate unlock 不可用/不在场；当前不声称已对该竞态提供生产保证。
8. **锁状态 generation 只对已观测边沿 sticky。** 已开窗口和普通 unlocked 操作都在入口/出口比对 generation，watcher 观测到重锁后即使又 unlocked 也会撤权并丢弃结果。但 macOS 没有向该用户会话 helper 提供文档化、无损的 lock-transition notification；完全落在两次 `CGSession` probe 之间的极短 locked→unlocked 边沿仍不可观测。真机验收必须覆盖这类竞态；不能把当前轮询表述为与 WindowServer 原子同步。

在上述真实 E2E 门禁打勾前，状态应写为：**AgentHalo.6（83369bc）已正式部署；Claude Locked Use
E2E 因错误信任 system-wide focused application 而在 grant 前安全失败，AgentHalo.7 exact
loginwindow PID/root 修复待签名部署和复测；旧 remote-agent 继续保留**，而不是“生产完成”。
