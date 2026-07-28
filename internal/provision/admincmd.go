package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccw/internal/sshexec"
)

// 节点侧ccwadmin的远程调用（C17：后台在浏览器里签发/轮换CDK）。
//
// **为什么不是新写一套API**：CDK的签发、轮换、列举在节点上已经由`ccwadmin`
// 实现且有测试（§11.1.1的统一错误、24h宽限、立即撤销都在那里）。
// Console再实现一遍就是两份规则，迟早漂移。这里只是把同一条命令
// 从「管理员SSH上去手敲」变成「后台点一下」——**判定逻辑仍然只有节点那一份**。
//
// 安全边界：
//   - 只用托管密钥连接，**不接受密码**；没有托管密钥的节点一律拒绝
//   - 子命令与参数由调用方从固定集合里选，slug经shellQuote，不拼接用户自由文本
//   - 返回的stdout可能含CDK明文，**调用方必须只转发给浏览器一次，不落库不记日志**

// AdminCmdTimeout是单条ccwadmin命令的上限。它只是查库/写库，
// 比compose-up快得多；超过这个时间基本等于节点有问题。
const AdminCmdTimeout = 45 * time.Second

// RunAdmin在节点上执行一条ccwadmin子命令，返回其stdout。
//
// args里的每一项都会被shellQuote，调用方不必自己转义；但**调用方仍然要
// 保证子命令名来自固定集合**——这里不做白名单，因为Console侧的handler
// 才知道哪些操作是被允许的。
func (o *Orchestrator) RunAdmin(ctx context.Context, nodeID string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("ccwadmin: 缺少子命令")
	}
	node, err := o.Store.GetNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	// **必须已有托管密钥**：密码只在首次纳管时存在过，这里不接受、也没有可用的。
	if _, _, _, err := o.Store.NodeCredential(ctx, nodeID); err != nil {
		return "", errors.New("该节点尚未建立托管密钥，无法远程执行；请先完成纳管")
	}
	target := sshexec.Target{Host: node.Host, Port: node.SSHPort, User: node.SSHUser}
	if node.HostKeyFP != nil {
		target.KnownFingerprint = *node.HostKeyFP
	}
	cli, sudo, err := o.connect(ctx, node, target, "", "", func(string, ...any) {})
	if err != nil {
		if errors.Is(err, sshexec.ErrHostKeyChanged) {
			o.Store.SetNodeStatus(ctx, nodeID, "host_key_changed")
			return "", fmt.Errorf("host key与记录不符，已中止：%w", err)
		}
		return "", err
	}
	defer cli.Close()

	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	cmd := fmt.Sprintf("cd %s && %sdocker compose -p %s run --rm --entrypoint /ccwadmin control-api %s",
		shellQuote(o.RepoRoot+"/deploy"), sudo, shellQuote(o.ComposeProjectName), strings.Join(quoted, " "))

	cctx, cancel := context.WithTimeout(ctx, AdminCmdTimeout)
	defer cancel()
	res, err := cli.Run(cctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		// **不回显stderr原文**：ccwadmin的统一错误已经刻意不区分原因（§11.1.1），
		// 把它整段抛给页面反而可能带出目标信息。只给退出码与一句人话。
		return "", fmt.Errorf("节点上的 ccwadmin %s 执行失败（退出码%d）", args[0], res.ExitCode)
	}
	return res.Stdout, nil
}

// IssuedCDK是一次签发的结果。CDK字段是**明文**，只应活到浏览器渲染完那一刻。
type IssuedCDK struct {
	Slug     string `json:"slug"`
	PublicID string `json:"public_id"`
	CDK      string `json:"cdk"`
}

// IssueCDK为已有项目签发一个新CDK。
func (o *Orchestrator) IssueCDK(ctx context.Context, nodeID, slug string) (IssuedCDK, error) {
	out, err := o.RunAdmin(ctx, nodeID, "issue-cdk", "--slug", slug, "--json")
	if err != nil {
		return IssuedCDK{}, err
	}
	return parseIssued(out, slug)
}

// RotateCDK轮换项目的CDK：签发新的，旧的按宽限期过期或立即撤销（§11.1.1）。
//
// revokeNow=false时节点默认给24小时宽限——老客户端还能连，
// 换过去之后旧的自然失效。真出了泄露才用revokeNow。
func (o *Orchestrator) RotateCDK(ctx context.Context, nodeID, slug string, revokeNow bool) (IssuedCDK, error) {
	args := []string{"rotate-cdk", "--slug", slug, "--json"}
	if revokeNow {
		args = append(args, "--revoke-now")
	}
	out, err := o.RunAdmin(ctx, nodeID, args...)
	if err != nil {
		return IssuedCDK{}, err
	}
	return parseIssued(out, slug)
}

// NodeCDKState是节点侧对一个CDK的权威状态（Console的镜像只知道签发与撤销）。
type NodeCDKState struct {
	PublicID string `json:"public_id"`
	State    string `json:"state"` // active|expired|disabled
}

// ListCDKs读节点上某项目的CDK清单——**这是权威状态**，Console库里的是镜像。
func (o *Orchestrator) ListCDKs(ctx context.Context, nodeID, slug string) ([]NodeCDKState, error) {
	out, err := o.RunAdmin(ctx, nodeID, "list-cdks", "--slug", slug, "--json")
	if err != nil {
		return nil, err
	}
	i := strings.IndexByte(out, '[')
	j := strings.LastIndexByte(out, ']')
	if i < 0 || j <= i {
		return nil, nil
	}
	var rows []NodeCDKState
	if err := json.Unmarshal([]byte(out[i:j+1]), &rows); err != nil {
		return nil, fmt.Errorf("节点返回的CDK清单无法解析")
	}
	return rows, nil
}

// parseIssued从ccwadmin的JSON里取签发结果。与parseInitProjectJSON同款：
// 只截第一个{到最后一个}，扛住compose混进stdout的噪声。
func parseIssued(out, slug string) (IssuedCDK, error) {
	i := strings.IndexByte(out, '{')
	j := strings.LastIndexByte(out, '}')
	if i < 0 || j <= i {
		return IssuedCDK{}, errors.New("节点返回的内容不是预期的JSON")
	}
	var got IssuedCDK
	if err := json.Unmarshal([]byte(out[i:j+1]), &got); err != nil {
		return IssuedCDK{}, errors.New("节点返回的内容无法解析")
	}
	if got.PublicID == "" || got.CDK == "" {
		return IssuedCDK{}, errors.New("节点未返回可用的CDK")
	}
	got.Slug = slug
	return got, nil
}
