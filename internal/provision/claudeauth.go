package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ccw/internal/sshexec"
)

// 在后台里给节点授权 Claude 账号（2026-07-29；2026-07-30 按真机实测修正）。
//
// 此前只能 SSH 上机、手敲一串 docker exec + tmux 命令（DEPLOY.md 的 A7）。
// 这里把那套流程搬进后台，但**不去解析 Claude Code 的输出**：
// 它的登录提示、URL 形态、步骤数量都可能随版本变，写死解析规则等于
// 把后台绑死在某个客户端版本上。
//
// 做法是把终端本身当中转：
//
//	Start   → 在容器里起一个独立的 tmux 会话跑 claude，capture-pane 把画面取回来
//	Capture → 再取一次（管理员点"刷新"）
//	SendKeys→ 送方向键/回车（走完选单），或送粘贴的授权码
//	Cancel  → kill-session
//
// 状态全在 tmux 里，HTTP 这边无状态，Console 重启也不影响正在进行的授权。
//
// **授权码经 stdin 送入，不进命令行**：命令行在节点上对所有用户可见（ps aux），
// 与推送源码包同款约束。
//
// ---- 2026-07-30 在 ubuntu:24.04 + Claude Code v2.1.220 上实测到的三件事 ----
//
//  1. **第一屏不是登录**。首次运行依次是「主题选择」→「登录方式选择」→ 才到
//     带 URL 的粘贴码界面。前两步要方向键与回车——只给一个文本输入框的话，
//     管理员会卡在主题选择器上，且完全看不出该做什么。因此有了 SendKeys。
//  2. **pane 必须开得很宽**。URL 约 400 字符；`-x 200` 时 Claude 的 TUI 会**自己**
//     把它折成三行，而 `capture-pane -J` 只能合并终端折行、合不了应用自己折的。
//     实测 `-x 600` 时 URL 完整落在一行里。
//  3. `-J` 仍然要带：它解决的是另一层（终端级折行）。
const authCols = 600

// authSession是授权用的tmux会话名。与终端会话（工作区键）、
// 管理员手动登录会话（main）都不同名，互不打扰。
const authSession = "ccw-auth"

// capturePane构造抓取画面的命令。
//
// **-J 不能省**：登录 URL 实测 407 字符，在 200 列的 pane 里必然折行，
// 而 capture-pane 默认按显示行返回——管理员复制到的会是**断成两截的链接**。
// -J 把折行合并回一行。（2026-07-30 在 ubuntu:24.04 + tmux 3.4 上实测：
// 不加 -J 时 URL 落在 2 行里，加了之后是完整的 1 行。）
func capturePane(sudo, container string) string {
	return fmt.Sprintf("%sdocker exec %s tmux -L %s capture-pane -p -J -t %s",
		sudo, shellQuote(container), authSession, authSession)
}

// authKeys是允许送进授权会话的按键。**白名单而不是自由文本**：
// 这个通道直通一个正在跑的终端，放开等于把任意输入送进容器。
// 这几个键足够走完「主题选择 → 登录方式选择 → 粘贴码」的全部选单。
var authKeys = map[string]string{
	"up": "Up", "down": "Down", "enter": "Enter", "escape": "Escape",
	"1": "1", "2": "2", "3": "3",
}

// ClaudeAuthStart在节点上起一个跑claude的tmux会话，并返回当前画面。
//
// 已存在同名会话时**先杀掉重开**：授权流程卡在半路时，让管理员点一次"重新开始"
// 就能回到干净状态，比让他去猜"要不要先取消"好。
func (o *Orchestrator) ClaudeAuthStart(ctx context.Context, nodeID, container string) (string, error) {
	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	defer cli.Close()

	script := fmt.Sprintf(`%[1]sdocker exec %[2]s tmux -L %[3]s kill-session -t %[3]s 2>/dev/null
%[1]sdocker exec -e LANG=C.UTF-8 -e LC_ALL=C.UTF-8 %[2]s tmux -L %[3]s new-session -d -s %[3]s -x %[5]d -y 60 claude
sleep 3
%[4]s`,
		sudo, shellQuote(container), authSession, capturePane(sudo, container), authCols)

	res, err := cli.Run(ctx, script)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return "", errors.New("容器里没有输出：确认项目容器在运行、且镜像里装了 claude")
	}
	return res.Stdout, nil
}

// ClaudeAuthCapture取回授权会话的当前画面。会话不存在时返回明确错误。
func (o *Orchestrator) ClaudeAuthCapture(ctx context.Context, nodeID, container string) (string, error) {
	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	defer cli.Close()

	res, err := cli.Run(ctx, capturePane(sudo, container))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", errors.New("授权会话已结束或不存在，请重新开始")
	}
	return res.Stdout, nil
}

// ClaudeAuthSendCode把授权码送进会话并回车，然后返回新的画面。
//
// **码经stdin进tmux的load-buffer**，不出现在命令行上——命令行在节点上
// 人人可见（ps aux）。paste-buffer 之后单独发一次 Enter。
func (o *Orchestrator) ClaudeAuthSendCode(ctx context.Context, nodeID, container, code string) (string, error) {
	code = strings.TrimSpace(code)
	if err := checkAuthCode(code); err != nil {
		return "", err
	}

	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	defer cli.Close()

	load := fmt.Sprintf("%sdocker exec -i %s tmux -L %s load-buffer -",
		sudo, shellQuote(container), authSession)
	if res, err := cli.RunStdin(ctx, load, strings.NewReader(code)); err != nil {
		return "", err
	} else if res.ExitCode != 0 {
		return "", errors.New("授权会话已结束或不存在，请重新开始")
	}

	paste := fmt.Sprintf(`%[1]sdocker exec %[2]s tmux -L %[3]s paste-buffer -t %[3]s
%[1]sdocker exec %[2]s tmux -L %[3]s send-keys -t %[3]s Enter
sleep 3
%[4]s`,
		sudo, shellQuote(container), authSession, capturePane(sudo, container))
	res, err := cli.Run(ctx, paste)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// checkAuthCode校验粘贴进来的授权码。
//
// **拒绝控制字符与空白，而不是给出一份字母表**。真正的风险是换行：
// 粘进 tmux buffer 后会被当成多次提交，把后面的内容送给会话里的下一个提示。
// 至于码本身用了哪些字符——base64url 有 `-_`，有的实现带 `=`、`+`、`%`、`?`——
// 猜错就会把一个合法的码挡在门外，而管理员在后台里没有任何绕过的办法。
// 宁可放宽：风险已由"无控制字符、无空白"覆盖。
func checkAuthCode(code string) error {
	if code == "" {
		return errors.New("授权码为空")
	}
	for _, r := range code {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\t' {
			return errors.New("授权码里有换行或空白；请只粘贴那一串码本身")
		}
	}
	return nil
}

// ClaudeAuthSendKey送一个按键进授权会话，然后返回新画面。
//
// 首次运行的前两屏（主题、登录方式）是选单，只能用方向键与回车走。
// key必须来自authKeys白名单——这个通道直通一个正在跑的终端。
func (o *Orchestrator) ClaudeAuthSendKey(ctx context.Context, nodeID, container, key string) (string, error) {
	tk, ok := authKeys[key]
	if !ok {
		return "", errors.New("不支持的按键")
	}
	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	defer cli.Close()

	cmd := fmt.Sprintf(`%[1]sdocker exec %[2]s tmux -L %[3]s send-keys -t %[3]s %[4]s
sleep 3
%[5]s`, sudo, shellQuote(container), authSession, tk, capturePane(sudo, container))
	res, err := cli.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", errors.New("授权会话已结束或不存在，请重新开始")
	}
	return res.Stdout, nil
}

// ClaudeAuthCancel结束授权会话。
func (o *Orchestrator) ClaudeAuthCancel(ctx context.Context, nodeID, container string) error {
	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.Run(ctx, fmt.Sprintf("%sdocker exec %s tmux -L %s kill-session -t %s 2>/dev/null; true",
		sudo, shellQuote(container), authSession, authSession))
	return err
}

// dialNode是这几个操作共用的连接过程：只用托管密钥，不接受密码。
func (o *Orchestrator) dialNode(ctx context.Context, nodeID string) (*sshexec.Client, string, error) {
	node, err := o.Store.GetNode(ctx, nodeID)
	if err != nil {
		return nil, "", err
	}
	if _, _, _, err := o.Store.NodeCredential(ctx, nodeID); err != nil {
		return nil, "", errors.New("该节点尚未建立托管密钥，无法远程执行；请先完成纳管")
	}
	target := sshexec.Target{Host: node.Host, Port: node.SSHPort, User: node.SSHUser}
	if node.HostKeyFP != nil {
		target.KnownFingerprint = *node.HostKeyFP
	}
	cli, sudo, err := o.connect(ctx, node, target, "", "", func(string, ...any) {})
	if err != nil {
		if errors.Is(err, sshexec.ErrHostKeyChanged) {
			o.Store.SetNodeStatus(ctx, nodeID, "host_key_changed")
			return nil, "", fmt.Errorf("host key与记录不符，已中止：%w", err)
		}
		return nil, "", err
	}
	return cli, sudo, nil
}
