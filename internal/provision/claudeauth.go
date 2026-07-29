package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ccw/internal/sshexec"
)

// 在后台里给节点授权 Claude 账号（2026-07-29）。
//
// 此前只能 SSH 上机、手敲一串 docker exec + tmux 命令（DEPLOY.md 的 A7）。
// 这里把那套流程搬进后台，但**不去解析 Claude Code 的输出**：
// 它的登录提示、URL 形态、步骤数量都可能随版本变，写死解析规则等于
// 把后台绑死在某个客户端版本上。
//
// 做法是把终端本身当中转：
//
//	Start   → 在容器里起一个独立的 tmux 会话跑 claude，capture-pane 把画面取回来
//	Capture → 再取一次（管理员点"刷新"，或前端轮询）
//	SendCode→ 把粘贴的授权码送进那个会话，回车
//	Cancel  → kill-session
//
// 状态全在 tmux 里，HTTP 这边无状态，Console 重启也不影响正在进行的授权。
//
// **授权码经 stdin 送入，不进命令行**：命令行在节点上对所有用户可见（ps aux），
// 与推送源码包同款约束。

// authSession是授权用的tmux会话名。与终端会话（工作区键）、
// 管理员手动登录会话（main）都不同名，互不打扰。
const authSession = "ccw-auth"

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
%[1]sdocker exec -e LANG=C.UTF-8 -e LC_ALL=C.UTF-8 %[2]s tmux -L %[3]s new-session -d -s %[3]s -x 200 -y 50 claude
sleep 3
%[1]sdocker exec %[2]s tmux -L %[3]s capture-pane -p -t %[3]s`,
		sudo, shellQuote(container), authSession)

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

	res, err := cli.Run(ctx, fmt.Sprintf("%sdocker exec %s tmux -L %s capture-pane -p -t %s",
		sudo, shellQuote(container), authSession, authSession))
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
%[1]sdocker exec %[2]s tmux -L %[3]s capture-pane -p -t %[3]s`,
		sudo, shellQuote(container), authSession)
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
