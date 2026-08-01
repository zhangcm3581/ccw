package terminal

// 终端会话按**工作区**而不是按项目区分（2026-07-29）。
//
// 原来一个项目只有一个名为main的会话、固定跑在/workspace。同步改成按本地目录
// 分层之后，若终端还停在/workspace，Claude 看到的是全部工作区的父目录，
// 与客户端正在同步的那个目录对不上——用户在 ~/code 里跑，Claude 却在
// 一个装着 code/ 与 work/ 两个子目录的地方工作。
//
// 现在：socket 仍是 project id（同项目共用一个 tmux server），
// **会话名与工作目录都取工作区键**，于是每个本地目录各有一个互不打扰的会话，
// 断线重连仍回到自己那个。
//
// **不接受空工作区**：调用方（terminal/ws.go）在建立连接前就已用
// ValidWorkspace 拒掉了缺失与非法的键，这里再留一条"空则退回 /workspace"
// 的分支只会是死代码，而且读起来像还有那么一条受支持的路径。
//
// 管理员授权 Claude 账号用的 `-t main` 会话不走这里——那是手敲 docker exec
// 建的另一个会话（见 DEPLOY.md 的 A7），与本文件无关。

// DefaultTerm是客户端没报 TERM 时用的值。
//
// **不能不设**：`docker exec -it` 会写死 `TERM=xterm`（实测，与调用方的环境无关），
// 而 xterm 只宣告 8 色。tmux 据此决定能对外层终端发哪些序列，Claude Code 这种
// 重绘频繁的 TUI 在能力被低估时会留下重绘残影。
const DefaultTerm = "xterm-256color"

// termEnv把校验过的 TERM 拼成 docker exec 的 -e 参数。
func termEnv(term string) []string {
	if !ValidTerm(term) {
		term = DefaultTerm
	}
	return []string{"-e", "TERM=" + term}
}

// ValidTerm校验 TERM 值。它会成为 docker exec 的一个 argv 元素（不经 shell），
// 但仍然只放行 terminfo 名字里合法的字符——宁可退回默认值，也不把任意字符串
// 塞进容器环境。
func ValidTerm(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '+'
		if !ok {
			return false
		}
	}
	return true
}

// Names：同一 project + 工作区永远得到同一 tmux socket 与会话名，保证重连回到原会话。
func Names(projectID, ws string) (socket, session string) {
	return projectID, ws
}

// workdir返回容器内的工作目录。与同步的落盘位置必须一致：
// worker 把工作区写在 <workspace-root>/<slug>/<ws>，容器里挂成 /workspace/<ws>。
func workdir(ws string) string {
	return "/workspace/" + ws
}

// BypassFlag是 Claude Code 跳过权限确认的参数。
//
// 等价于 `--permission-mode bypassPermissions`（官方 CLI 文档）。官方明确说这个
// 模式"只应在受限网络、可随时恢复的沙箱容器/VM 里用"——**本项目的容器正好是
// 那种环境**：每个项目独立容器、独立卷、独立磁盘配额，坏了重建即可。
//
// **只在建会话时生效**。会话是持久的（断线不中断正是它的意义），已经在跑的
// claude 进程改不了权限模式；调用方要如实告诉用户这一点，而不是让人以为
// 加了参数就一定生效。
const BypassFlag = "--dangerously-skip-permissions"

// PermMode是允许的权限模式。**白名单而不是透传字符串**：这个值会成为容器里
// 命令行的一部分，放开等于让客户端往 claude 的参数里塞任意东西。
type PermMode string

const (
	PermDefault PermMode = ""
	PermBypass  PermMode = "bypass"
)

// ParsePermMode把客户端报上来的值收敛到白名单。认不出的一律当默认——
// **不报错**：权限模式认不出就退回更严格的那个，比拒绝连接更合适。
func ParsePermMode(s string) PermMode {
	if s == string(PermBypass) {
		return PermBypass
	}
	return PermDefault
}

// claudeArgs返回容器里要执行的 claude 命令。
func claudeArgs(mode PermMode) []string {
	if mode == PermBypass {
		return []string{"claude", BypassFlag}
	}
	return []string{"claude"}
}

// EnsureSessionCmds：附着前必须依次尝试的命令（第一条失败时才执行第二条）。
// 不使用new-session -A的前台形式：容器PID 1不是tmux，会话一律detached创建。
func EnsureSessionCmds(containerName, projectID, ws, term string, mode PermMode) [][]string {
	socket, session := Names(projectID, ws)
	base := append([]string{"docker", "exec", "-e", "LANG=C.UTF-8", "-e", "LC_ALL=C.UTF-8"}, termEnv(term)...)
	with := func(rest ...string) []string {
		return append(append([]string{}, base...), append([]string{containerName}, rest...)...)
	}
	return [][]string{
		with("tmux", "-L", socket, "has-session", "-t", session),
		// -c 指定的目录必须先存在，否则 tmux 会在 $HOME 起会话而不报错——
		// 表现是"终端里看不到任何同步过来的文件"。mkdir -p 是幂等的。
		with("sh", "-c", "mkdir -p "+shellQuote(workdir(ws))),
		with(append([]string{"tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", workdir(ws)},
			claudeArgs(mode)...)...),
	}
}

// BypassIneffectiveCmd构造一条 tmux 提示：请求了 bypass 模式但会话已存在。
//
// **为什么不由客户端打印**：客户端写完 stderr 之后终端立刻进 raw mode，
// Claude 的 fullscreen 渲染进 alt screen 把整屏清掉——那句提示活不过一秒，
// 等于没说（2026-08-01 review 发现）。tmux 的 display-message 显示在状态行上，
// 由 tmux 自己维持，不会被应用清掉。
func BypassIneffectiveCmd(containerName, projectID, ws string) []string {
	socket, session := Names(projectID, ws)
	return []string{"docker", "exec", containerName,
		"tmux", "-L", socket, "display-message", "-t", session, "-d", "6000",
		"已有会话在运行，本次仍是原来的权限模式；要用 -d 请先 kill-session 再重连"}
}

// AttachCmd必须带-t（审查§2.1）：容器内不分配TTY时tmux attach会直接失败；
// 宿主机侧的TTY由creack/pty提供给docker CLI进程，两者缺一不可。
func AttachCmd(containerName, projectID, ws, term string) []string {
	socket, session := Names(projectID, ws)
	args := append([]string{"docker", "exec", "-it", "-e", "LANG=C.UTF-8", "-e", "LC_ALL=C.UTF-8"}, termEnv(term)...)
	return append(args, containerName, "tmux", "-L", socket, "attach-session", "-t", session)
}

// shellQuote用单引号包裹参数。工作区键已经过ValidWorkspace校验（只有小写字母、
// 数字与连字符），这里再包一层是纵深防御，不是唯一防线。
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}
