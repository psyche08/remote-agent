# AgentHalo Locked Use 真机安装与端到端验收

本文用于目标 Mac 的**设备所有者亲自执行**。安装 Authorization Plug-in、修改
`system.login.screensaver`、锁定/解锁机器和配置 Developer ID 都是安全敏感操作；
自动化 agent 不应在没有明确授权时替你完成。

当前实现**不存储、不读取、不提交 macOS 登录密码**。一次 Locked Use 解锁只由
Apple Authorization Plug-in 对短时、单次、签名 grant 作出 Allow；普通密码解锁分支
仍保留。当前 grant v2 还签名绑定 primary console user 的 UID/username，plug-in 会把两项
与本次 authorization transaction 的 username 及其 passwd UID 精确核对。

授权 proof 是 root-owned exact-nonce 的 `pending -> final -> complete`：`pending` 在 Allow
前，`final` 在 `SetResult(Allow)` 成功后，`complete` 仅由该成功 Allow 对应的
`MechanismDestroy` 写入。complete 前 loginwindow password field 必须保持同一 AX
element 且系统保持 locked；complete 后才能接受 field lifecycle completion + unlocked。

锁屏 wake 和 loginwindow UI ready 是两个时刻。helper 只调用一次 Apple 公开
`IOPMAssertionDeclareUserActivity`，固定传入长度小于 128 的 AgentHalo name 与
`kIOPMUserActiveRemote`；它不发送 wake CGEvent，不读 cursor/display geometry，不点击、不按键、
不重申也不切换 Local。只有 declare success 且返回非 null assertion ID 才继续；否则失败关闭且
0 grant。随后 helper 只从唯一的 exact
`com.apple.loginwindow` system bundle/executable 对应 PID 创建 AX application root；root、搜索种子和
字段都回验同一 PID，绝不信任 system-wide focused application，也不 activate/frontmost 进程。
搜索按 focused UI element、focused window、windows、application root 去重后做有界 BFS，不使用
role/title fallback，也不读取字段值。随后在最长 8 秒的不授权、pre-submission 阶段查找并 focus
exact `UserPasswordTextField`，读回 focus，并等待空 `AXValue` 可写；这期间 remote user-activity
lease 保持 active，磁盘上没有 grant。全部 ready 且 identity 回验后先记录
`authorization_field_ready`，再同步 release 同一 assertion ID；release 失败直接返回且 0 grant。
release 成功后 controller callback 重新核对 opening owner、真人输入、locked state 和 primary console user，
再按当前时间 mint/write 10 秒 grant，随后只执行一次空 `AXValue`，不发现或执行 secure-field AX
action；成功写入立即记录不含 nonce/secret 的 `authorization_empty_value_written`。callback 前取消、
真人输入或 discovery 失败不会发布 grant；callback 后 AX 失败不会重新 mint、rewrite 或 submit，
proof/lifecycle 不确定仍进入原有 fail-closed/quarantine 路径。

## 0. 前提与恢复路径

先准备：

- macOS 14 或更新版本；
- 一个可用的 `Developer ID Application` 身份；Go agent、Swift helper 和 plug-in
  必须由同一 Team 签名；
- 第二管理员账户或可用的 Recovery 入口；
- 从另一台设备可访问 AgentHalo 的远端通道；
- 已备份目标机配置。

在仓库根目录先跑只读 preflight：

```bash
cd /path/to/agenthalo
bash mac/preflight.sh
```

默认 preflight 不安装 plug-in、不改 authorizationdb、不锁屏。不要跳过失败项。

确认签名身份可见：

```bash
security find-identity -v -p codesigning
```

如果显示 `0 valid identities found`，先在 Keychain Access 中恢复/解锁对应证书及私钥；
不要继续安装一个无法以稳定身份运行的 helper。

### 历史开发机记录（2026-08-09，非 m4pro）

以下仅是早期本地开发机的历史记录，不是当前 m4pro 的部署真值；m4pro 当前正式签名、Plug-in、
authorizationdb 和 AgentHalo 版本以 [STATUS-and-TODO.md](STATUS-and-TODO.md) 为准：

- `security find-identity -v -p codesigning` 返回 `0 valid identities found`；
- 新旧 LaunchAgent label 均未加载；新旧 Authorization Plug-in bundle、Locked Use state
  目录和 authorizationdb 子 rule 均不存在；
- 因此本轮只完成了源码、构建、签名结构和非特权测试，没有执行正式签名安装、
  authorizationdb 写入或真实锁屏解锁 E2E。

正式部署仍必须在具有有效 Developer ID 的目标 Mac 上完成本文后续步骤和真机门禁。

### m4pro 当前复测点（2026-08-12）

`AgentHalo.7`（`f6941d2`）已经正式签名、公证并部署。其真机尝试已出现
`authorization_field_ready -> grant_published`，证明 exact loginwindow PID/root/field 路由生效；
但空值写入后追加 AXConfirm/AXPress 的组合路径中 field 消失，未出现可归因的 authd transaction，也没有
pending/final/complete proof。任务安全结束为 `needs_manual` / `delivery_outcome=unknown`，无 CLI
fallback 或重复输入，grant/window/shield 已收口且机器保持锁定。

`AgentHalo.8`（`320ceb0`）把 postgrant 触发面收窄为唯一一次空 `AXValue` 写入，已正式签名、公证并
部署。第一次 fresh locked 尝试被真实 USB 鼠标输入正确抑制；清除 suppression 后的两次 clean fresh
尝试均未出现 password field，0 次 field-ready/grant/proof。统一日志确认 helper 的 PostEvent 获 TCC
允许，但 loginwindow 没有收到 inactive→active user-activity transition；随后另一外部事件立即触发
active-user unlock UI。A8 固定向 `(1, 1)` 移动，首次后重复坐标会成为空间 no-op；这是由固定坐标源码
和重复无 transition 时序得出的高可信推断，不是统一日志直接记录的 cursor 坐标。

`AgentHalo.9`（`d5c2e92`）的 different-point single-move wake 已正式签名、公证、部署并
进行 fresh locked 复测。TCC 接受 PostEvent，但 powerd/loginwindow 仍无 active-user transition，
0 次 field-ready/grant/proof；任务以 `needs_manual` / `delivery_outcome=unknown` 安全结束，
没有 CLI fallback 或重复输入，机器保持 locked。

`AgentHalo.10` 源码已改为上述 public Remote user-activity lease；declare/release 任一失败都是
0 grant，任何 discovery/error/cancel/return 都对同一 ID 执行幂等清理。它**尚待正式签名、公证、
部署和真实锁屏复测**；本文不预判 wake 后的单次空写一定能触发目标 macOS，
旧 `remote-agent` 在全套 E2E 门禁通过前继续保留。

## 1. 配置设备能力

在 AgentHalo 的 `config.json` 中保留非空 `device_id`，并加入：

```json
{
  "computer_use": {
    "enabled": true,
    "debug_http_actions": false,
    "helper_socket": "~/Library/Application Support/AgentHalo/desktop.sock",
    "locked_use": {
      "enabled": true,
      "grant_ttl_seconds": 10,
      "window_ttl_seconds": 300,
      "input_relock_grace_ms": 250,
      "require_display_shield": true
    }
  }
}
```

`debug_http_actions` 必须保持 `false`。模型通过 provider 内部的权威 turn broker 操作；
打开这个调试开关会重新暴露可搭车的 HTTP 动作面。

## 2. 安装同 Team 签名的 agent 与 helper

正式发布应走 `deploy/publish-release.sh`，它会先构建并签名 helper，再把它嵌入
同 Team、identifier 为 `dev.linsheng.agenthalo` 的 Go agent。

### 旧产品必须先完整卸载

AgentHalo 是 fresh-only 安装：新 helper 只接受 `dev.linsheng.agenthalo`，grant key 只由
显式 provisioning 写入当前用户的 file-based login Keychain，LaunchAgent、socket、Support 目录和
authorizationdb rule 也都是全新的身份。它不会读取、迁移或兜底旧产品的任何运行时状态。

如果机器安装过旧产品，必须先切回对应旧版本源码/安装包，使用**旧版本自己的卸载器**
移除它，并确认旧 LaunchAgent、Authorization Plug-in、authorizationdb rule 和 state
目录都已不存在；然后才能按本文 fresh install。不要让新旧 rule/bundle/job 并存，也
不要用 `launchctl bootout` 或强杀来绕过运行中 helper 的安全收口。

安装/更新 agent 后，以登录用户执行：

```bash
/path/to/agenthalo desktop install
```

它会输出 helper 路径。验证两个运行产物，不要只检查源码构建目录：

```bash
codesign --verify --strict --verbose=2 /path/to/agenthalo
codesign -d --verbose=4 /path/to/agenthalo 2>&1 \
  | grep -E 'Identifier=|TeamIdentifier='

codesign --verify --strict --verbose=2 \
  "$HOME/Library/Application Support/AgentHalo/bin/agenthalo-desktop"
codesign -d --verbose=4 \
  "$HOME/Library/Application Support/AgentHalo/bin/agenthalo-desktop" 2>&1 \
  | grep -E 'Identifier=|TeamIdentifier='
! codesign -d --entitlements - \
  "$HOME/Library/Application Support/AgentHalo/bin/agenthalo-desktop" 2>&1 \
  | grep -Eq 'keychain-access-groups|application-identifier'
```

期望 identifier 分别为：

- `dev.linsheng.agenthalo`
- `dev.linsheng.agenthalo.desktop`

两者 `TeamIdentifier` 必须完全相同。

## 3. 在 Keychain 中创建 grant key，并导出公钥

下面的 provisioning 模式不锁屏、不启动 socket，也不读取登录密码。必须直接运行最终
Developer ID 签名的已安装 helper，并在 login Keychain 已解锁时执行；首次创建的 item
由 Keychain 默认 creator ACL 绑定到该 helper 的 code-signing designated requirement，
不需要也不得携带 `keychain-access-groups` entitlement 或 provisioning profile：

```bash
HELPER="$HOME/Library/Application Support/AgentHalo/bin/agenthalo-desktop"
CONFIG=/absolute/path/to/config.json
KEY_JSON=/tmp/agenthalo-locked-use-public-key.json
KEY_FILE=/tmp/agenthalo-locked-use-public.key

"$HELPER" --provision-locked-use-key --config "$CONFIG" > "$KEY_JSON"
jq -er .public_key "$KEY_JSON" > "$KEY_FILE"
base64 -D < "$KEY_FILE" | wc -c
```

最后一条必须输出 `65`（P-256 X9.63 uncompressed public key）。如果 provisioning
报告签名身份或 Keychain 错误，停止；不能退回用户目录里的长期私钥文件，也不能删除
旧 item 后静默生成新 key。

file-based login Keychain 的锁定策略独立于屏幕锁。正常 runtime 只读取既有 item，禁止
认证 UI，也不会创建、删除或轮换 key；helper 会在启动时读取 key 并驻留内存。
如果用户手工锁定 login Keychain，或启用了 `Lock after` / `Lock when sleeping` 且 helper
随后在锁屏状态崩溃重启，它必须 fail closed 并报告 Locked Use unavailable，不会弹认证 UI、
绕过 Keychain 策略或轮换 key。恢复方式是在人工解锁会话和 login Keychain 后安全重启 helper。

## 4. 构建并安装 Authorization Plug-in

先构建稳定签名 bundle：

```bash
cd /path/to/agenthalo/mac/authorization-plugin
AGENTHALO_PLUGIN_SIGN_IDENTITY="Developer ID Application: ..." ./build.sh
codesign --verify --strict --verbose=2 build/AgentHaloLockedUse.bundle
TEAM_ID="$(codesign -d --verbose=4 build/AgentHaloLockedUse.bundle 2>&1 \
  | sed -n 's/^TeamIdentifier=//p' | head -1)"
test -n "$TEAM_ID"
```

读取配置中的 device ID，然后以管理员身份安装。安装器会先备份原始 right；
`AGENTHALO_LOCKED_USE_ACK=1` 表示你已经准备好恢复路径：

```bash
DEVICE_ID="$(jq -er .device_id "$CONFIG")"
AGENT_USER="$(id -un)"
sudo env \
  AGENTHALO_LOCKED_USE_ACK=1 \
  AGENTHALO_DEVICE_ID="$DEVICE_ID" \
  AGENTHALO_AGENT_USER="$AGENT_USER" \
  AGENTHALO_EXPECTED_TEAM_ID="$TEAM_ID" \
  ./install.sh "$KEY_FILE"
```

安装器会拒绝 ad-hoc 签名、空 TeamIdentifier、错误 bundle identifier，以及与
`AGENTHALO_EXPECTED_TEAM_ID` 不一致的签名；这里的 Team ID 必须也与第 2 步的 agent/helper
完全相同。`AGENTHALO_DEVICE_ID`、`AGENTHALO_AGENT_USER` 和 public key 都是强制项；grant hand-off
使用只授予该用户名 write 的 macOS ACL，而不是所有本地账户常共享的 `staff` group。

安装后只读确认 rule 结构：

```bash
security authorizationdb read dev.linsheng.agenthalo.locked-use
security authorizationdb read system.login.screensaver \
  | plutil -convert json -o - -- - | jq '{class, "k-of-n": .["k-of-n"], rule}'
```

`system.login.screensaver.rule` 中应同时存在：

- `dev.linsheng.agenthalo.locked-use`
- `use-login-window-ui`

前者必须唯一且位于整个数组的 index 0；正常密码分支必须继续存在。安装器不得删除 OpenAI
Codex 或任何其他插件规则，并保持所有非 AgentHalo 规则原有的相对顺序。
子规则还必须显示 `shared = false` 和 `timeout = 0`；否则一次成功可能进入 Authorization
Services credential cache，破坏 one-grant/one-unlock 约束。

## 5. 注册 LaunchAgent 与 TCC

仍以登录用户运行（不要 sudo）：

```bash
cd /path/to/agenthalo
mac/launchagent/install.sh --config "$CONFIG" --helper "$HELPER"
```

在「系统设置 → 隐私与安全性」中给**该 helper 二进制**授予：

- Accessibility
- Screen & System Audio Recording（Screen Recording）

helper 的 shielding-level 黑窗使用 `sharingType=.readOnly`，所以普通截图、后台录屏和会议
应用应保留黑罩；helper 自己的 ScreenCaptureKit filter 才排除自身 application/windows，
让绑定模型 turn 得到下层 UI。不要把“模型帧非黑”误当作普通录屏也可绕过黑罩。

然后重启 helper：

```bash
launchctl kickstart -k "gui/$(id -u)/dev.linsheng.agenthalo.desktop"
launchctl print "gui/$(id -u)/dev.linsheng.agenthalo.desktop"
```

确认 socket 和状态：

```bash
SOCKET="$HOME/Library/Application Support/AgentHalo/desktop.sock"
printf '{"op":"status"}\n' | nc -U "$SOCKET" | jq
```

必须看到 `enabled=true`、`available=true`、`locked_use.enabled=true`、
`locked_use.armed=true`。任何 `error` 都是硬失败。

## 6. 先验证人工解锁与卸载路径

在测试 Locked Use 前，先按 Ctrl-Cmd-Q 锁屏，再用正常密码人工解锁一次。若人工解锁
异常，立即从第二管理员账户或 Recovery 执行：

```bash
cd /path/to/agenthalo/mac/authorization-plugin
sudo ./uninstall.sh
```

确认普通解锁无回归后再继续。

## 7. 无人值守模型闭环

Claude 是主验收路径：

1. 在远端创建一个 **Claude fresh logical session**，确认 session-sticky route 为
   `desktop_computer_use`，并绑定新的 Claude Desktop native session；不要 resume 旧 session。
2. 让 Claude Desktop 和目标应用保持已启动，例如 TextEdit，并准备一个可辨识但不敏感的测试文档。
3. 锁定目标 Mac，现场不要触碰键鼠。
4. 发送一个新 `operation_id` 的明确 prompt：先截图/读取目标界面，完成一次文本输入和一次键鼠
   操作，再报告最终界面；同一 native session 还要触发并回答 `AskUserQuestion`。
5. 同一轮分别验证一次工具 `Allow once` 和一次 `Deny`；不得选择 `Always`、session/task 级授权。
   Desktop UI 一旦发生 mutation，任何失败都不得切到 `stream_json_cli` 补发。

验收证据必须同时包含：

- Claude route 始终为 `desktop_computer_use`，native session/turn/operation tombstone 保持相同，
  没有 CLI fallback 或重复 prompt；
- Computer Use 返回的 PNG 不是黑图，并且带目标应用 Accessibility 树；
- `click`/`scroll` 使用该 composite PNG 的左上角原点坐标，不能把未经转换的 CG global
  坐标或旧截图坐标混入当前观察；
- 模型至少成功执行一次 `press`/`set_value` 和一次键鼠动作；
- Claude transcript 精确显示 question→answer，并分别完成 exact `Allow once` 与 `Deny`；
- helper audit 出现同一 turn 的 `grant_published`、`authorization_empty_value_written`、
  `grant_consumed`、`window_opened`、`window_closed`；
- `grant_published` 必须晚于 exact field ready 与 remote user-activity assertion release 成功；较慢的 loginwindow UI discovery 不得占用
  grant TTL；同一 turn 必须满足 `authorization_field_ready -> grant_published ->
  authorization_empty_value_written`，且 empty-write marker 只能出现一次；
  `authorization_request_returned` 失败项必须带不含 nonce/secret 的诊断 `reason`；
- root-owned `pending`、`final`、`complete` 均匹配同一 nonce，且日志/时序显示
  complete 之前 exact field 保持同一且系统保持 locked；
- 目标机实测顺序为 `complete -> field lifecycle completion -> unlocked`；
- authd/loginwindow 日志显示 AgentHalo mechanism 被求值；
- turn completed 后，独立系统读数确认屏幕已锁。

Codex 只做独立兼容验收：另开**新 Codex task**（不要 resume 旧 task），确认 `thread/start`
注入 `computer_use` dynamic tools，并单独验证 `get_app_state → one mutation → get_app_state`；它不能
替代上述 Claude Desktop-first 的问答和授权门禁。

查看授权日志：

```bash
/usr/bin/log show --last 5m --style compact --info --debug \
  --predicate 'process == "authd" OR process == "loginwindow"' \
  | grep -iE 'AgentHaloLockedUse|system.login.screensaver|Unlock succeeded|running mechanism'
```

独立读取锁态：

```bash
ioreg -n Root -d1 -a | python3 -c '
import plistlib,sys
d=plistlib.loads(sys.stdin.buffer.read())
for u in d.get("IOConsoleUsers") or []:
    if "CGSSessionScreenIsLocked" in u:
        print("locked=", bool(u["CGSSessionScreenIsLocked"]))
'
```

## 8. 安全回归

在第二次新 turn 打开窗口后，逐项验证：

1. 按一次物理键并移动物理鼠标：目标应用不得收到事件；约 40 ms 监控应开始关窗并重锁。
2. 不人工解锁，直接发起下一次 Locked Use：应因 local-input suppression 被拒绝。
3. 人工解锁一次后再手动锁屏：suppression 才能恢复。
4. 测试 turn completed、interrupted 和 provider error：三种终态都必须重锁。
5. 启动 QuickTime 或普通 ScreenCaptureKit 录屏后重复测试：录屏必须只保留黑罩，同时模型
   截图仍能看到目标 UI。
6. 接入扩展显示器重复测试：每块物理屏都保持全黑；把显示器分别放到主屏左侧、上方和
   下方，按 composite PNG 坐标 click/scroll 必须命中对应显示器，显示器间空洞坐标必须拒绝。
   Retina、旋转屏和实际 Y 轴方向仍需在目标机校准记录。
7. 在没有 open window 时重启 agent/helper：必须保持/恢复 locked baseline。
8. 在测试构建中把 AgentHalo Allow 的应用点卡在 final/complete 边界，同时用
   Apple Watch 或另一授权分支尝试解锁：exact field 提前消失或 complete 前观察到
   unlocked 时必须不 open、保持 shield、重锁并进入 quarantine。随后释放迟到的
   AgentHalo transition，仍不得在 window/shield 已清理后留下 unlocked 桌面；状态必须
   显示 `requires_manual_recovery=true`，且在受控重启/人工恢复前不得自行撤下 shield。
9. 另做一次 **post-terminal** 竞态：先让本 nonce 的 `receipt.complete` 落盘、但故意
   延迟 AgentHalo transaction 的可见撤锁，再由 Apple Watch/另一分支先造成 field 消失
   和 unlocked。若随后释放原 transaction 会在 close/relock 之后再次解锁，该 macOS
   版本不得部署本功能；需先禁用 alternate unlock 或增加独立 guardian/客户端完成信号。

Apple 公开 Authorization Plug-in API 未保证 `MechanismDestroy` 一定晚于 loginwindow
实际应用解锁副作用，也没有把可见撤锁绑定到本 nonce 的 transaction ID。
因此第 8、9 项和上述 complete → unlock 时序必须在每个
目标 macOS 版本上真机通过；单元测试或静态 receipt fixture 不能代替。

不要用 `kill -9` 作为“已安全支持”的验收。当前没有独立 root deadman；SIGKILL 会同时
移除 helper 内的遮罩和监控，只有 launchd 重启后的 startup scrub 能补救。这是正式
部署前仍需解决或接受的风险。

## 9. 回退

先停止模型 turn，然后：

```bash
cd /path/to/agenthalo
mac/launchagent/install.sh --uninstall

cd mac/authorization-plugin
sudo ./uninstall.sh
```

最后把 `computer_use.enabled` 和 `computer_use.locked_use.enabled` 设回 `false`，重启
AgentHalo，并删除 `/tmp/agenthalo-locked-use-public-key.json` 与
`/tmp/agenthalo-locked-use-public.key`。公钥本身不是秘密，但不应留下混淆部署状态的
临时文件。
