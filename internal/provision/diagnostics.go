package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 节点诊断与维护（2026-07-30）。
//
// 目标是让 docs/admin-login-runbook.md 与 docs/claude-auth-quickref.md 里
// 那些「登机敲一遍」的命令都能在后台点一下完成：容器在不在跑、每个项目登没登录、
// 凭据文件属主对不对、磁盘还剩多少、容器重建后凭据还在不在。
//
// **只读检查一次跑完**：它们本质上是同一次排查的几个侧面，分开点五次没有意义，
// 而且每次都要重新建一条 SSH 连接。
//
// **不解析输出**（除了登录状态取一个布尔用于染色）：`docker ps` 的列宽、
// `claude auth status` 的措辞都可能变，写死解析等于把诊断绑死在某个版本上。
// 原文照显，判断交给看的人。

// DiagSection是诊断结果的一段：一个标题 + 一段原始输出。
type DiagSection struct {
	Title  string
	Output string
	// LoggedIn仅对「登录状态」段有意义：true表示输出里明确出现了已登录标记。
	// 取不到时为nil——**不要把"没解析出来"显示成"未登录"**。
	LoggedIn *bool
}

// diagMarker用于在一次SSH里切分多段输出。用一串不可能出现在正常输出里的字符。
const diagMarker = "@@CCW-DIAG@@"

// Diagnose在节点上跑一组只读检查。containers是本节点的项目容器名。
func (o *Orchestrator) Diagnose(ctx context.Context, nodeID string, containers []string) ([]DiagSection, error) {
	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	var b strings.Builder
	sec := func(title, cmd string) {
		fmt.Fprintf(&b, "echo '%s%s'\n%s 2>&1\n", diagMarker, title, cmd)
	}

	sec("容器状态", sudo+`docker ps --format '{{.Names}}\t{{.Status}}\t{{.Image}}'`)
	for _, c := range containers {
		sec("登录状态 "+c, fmt.Sprintf("%sdocker exec %s claude auth status", sudo, shellQuote(c)))
	}
	if len(containers) > 0 {
		// 卷权限：凭据文件必须归 claude(1001)。归 root 就写不进去，
		// 表现是"登录完还是未登录、反复要求重新登录"——最常见的坑。
		sec("凭据文件与属主", fmt.Sprintf(
			"%sdocker exec %s ls -l /home/claude/.claude.json /home/claude/.claude/.credentials.json",
			sudo, shellQuote(containers[0])))
	}
	sec("磁盘与 data-root", sudo+`sh -c "df -h / | tail -1; docker info --format 'data-root: {{.DockerRootDir}}' 2>/dev/null"`)

	res, err := cli.Run(ctx, b.String())
	if err != nil {
		return nil, err
	}
	return splitDiag(res.Stdout), nil
}

// splitDiag按标记切分成段。
func splitDiag(out string) []DiagSection {
	parts := strings.Split(out, diagMarker)
	var secs []DiagSection
	for _, p := range parts[1:] { // parts[0]是第一个标记之前的内容（通常为空）
		nl := strings.IndexByte(p, '\n')
		if nl < 0 {
			continue
		}
		s := DiagSection{
			Title:  strings.TrimSpace(p[:nl]),
			Output: strings.TrimRight(p[nl+1:], "\n "),
		}
		if strings.HasPrefix(s.Title, "登录状态") {
			// 只认明确的已登录标记。取不到就留nil——
			// 把"没解析出来"显示成"未登录"会让人白折腾一轮重新授权。
			low := strings.ToLower(s.Output)
			switch {
			case strings.Contains(low, "loggedin: true"), strings.Contains(low, `"loggedin":true`):
				t := true
				s.LoggedIn = &t
			case strings.Contains(low, "loggedin: false"), strings.Contains(low, `"loggedin":false`),
				strings.Contains(low, "not logged in"):
				f := false
				s.LoggedIn = &f
			}
		}
		secs = append(secs, s)
	}
	return secs
}

// RecreateContainer重建一个项目容器。
//
// 用途是运行手册第2节那条验证：容器重建后凭据仍在（卷持久）。
// **数据都在命名卷里**，重建只换容器本身；但正在附着的终端会断，
// 所以页面上要说清楚。
func (o *Orchestrator) RecreateContainer(ctx context.Context, nodeID, service string) (string, error) {
	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	defer cli.Close()

	cmd := fmt.Sprintf("cd %s && %sdocker compose -p %s up -d --force-recreate %s 2>&1",
		shellQuote(o.RepoRoot+"/deploy"), sudo, shellQuote(o.ComposeProjectName), shellQuote(service))
	res, err := cli.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("重建 %s 失败（退出码%d）：%s", service, res.ExitCode, firstLine(res.Stdout))
	}
	return res.Stdout, nil
}

// NodeProjectInfo是节点上一个项目的权威信息（ccwadmin list-projects --json）。
type NodeProjectInfo struct {
	Slug      string `json:"slug"`
	ProjectID string `json:"project_id"`
	Container string `json:"container"`
	DiskGiB   int64  `json:"disk_gib"`
	FiveHour  int64  `json:"five_hour"`
	SevenDay  int64  `json:"seven_day"`
}

// ListProjectsOnNode读节点上的项目清单。
//
// Console库里的node_projects是镜像，只在纳管跑init-projects时写入。
// 早于该功能上线的节点、或镜像写失败的那次，镜像会是空的——那时
// /connect解析不到、CDK页也是空的。有了这个就能从节点补齐，
// 不必为了补一行记录去重跑一遍部署。
func (o *Orchestrator) ListProjectsOnNode(ctx context.Context, nodeID string) ([]NodeProjectInfo, error) {
	out, err := o.RunAdmin(ctx, nodeID, "list-projects", "--json")
	if err != nil {
		return nil, err
	}
	i := strings.IndexByte(out, '[')
	j := strings.LastIndexByte(out, ']')
	if i < 0 || j <= i {
		return nil, nil
	}
	var rows []NodeProjectInfo
	if err := json.Unmarshal([]byte(out[i:j+1]), &rows); err != nil {
		return nil, fmt.Errorf("节点返回的项目清单无法解析")
	}
	return rows, nil
}

// ResetNode把节点擦回「装了Docker的干净机器」，供反复重来的测试循环用。
//
// **销毁什么**：ccw 这个 compose 项目的全部容器与**命名卷**，以及源码树
// /srv/ccw。也就是说 workspace 里的文件、Claude 授权凭据、节点自己的 Postgres
// （项目与 CDK）全部消失。这是不可撤销的。
//
// **刻意保留什么，以及为什么**：
//   - Docker 本身：重装要好几分钟，而 install-docker 那一步有 precheck 会跳过，
//     留着能让每轮测试快很多。
//   - authorized_keys 里的托管密钥：删了 Console 就再也连不上这台机器，
//     一旦擦除中途失败就彻底失联，连重试都做不到。
//   - Console 库里的节点、域名分配与托管密钥：留着才能直接点「继续 / 重新部署」
//     重跑一遍，而不是重新填一次 IP、再等一次 DNS 生效。
//     要连 Console 的账一起清，用「解除纳管」。
//
// 擦完之后点「继续 / 重新部署」即为一次全新部署：新 run 没有任何已完成步骤，
// 12 步会重跑，harden 与 dns-allocate 因为凭据/域名还在而快速跳过。
func (o *Orchestrator) ResetNode(ctx context.Context, nodeID string) (string, error) {
	cli, sudo, err := o.dialNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	defer cli.Close()

	// 不 `cd` 进 deploy 再执行：源码树可能已经被上一次擦除删掉了，那样 cd 会失败、
	// 后面的清理全都跑不到。compose v2 能只凭项目名（靠标签）拆掉资源。
	//
	// 标签过滤是精确的：只碰 compose 为 ccw 这个项目建的东西，
	// 同机上别人的容器与卷不受影响。
	script := fmt.Sprintf(`set -u
if [ -f %[2]s/compose.yaml ]; then
  cd %[2]s && %[1]sdocker compose -p %[3]s down -v --remove-orphans 2>&1 || true
else
  %[1]sdocker compose -p %[3]s down -v --remove-orphans 2>&1 || true
fi
left_c=$(%[1]sdocker ps -aq --filter label=com.docker.compose.project=%[3]s)
[ -n "$left_c" ] && %[1]sdocker rm -f $left_c 2>&1 || true
left_v=$(%[1]sdocker volume ls -q --filter label=com.docker.compose.project=%[3]s)
[ -n "$left_v" ] && %[1]sdocker volume rm -f $left_v 2>&1 || true
%[1]srm -rf %[4]s
echo "---- 擦除后剩余 ----"
echo "容器: $(%[1]sdocker ps -aq --filter label=com.docker.compose.project=%[3]s | wc -l)"
echo "卷:   $(%[1]sdocker volume ls -q --filter label=com.docker.compose.project=%[3]s | wc -l)"
echo "源码树: $([ -e %[4]s ] && echo 仍存在 || echo 已删除)"
echo "Docker: $(%[1]sdocker --version 2>/dev/null || echo 不可用)"`,
		sudo, shellQuote(o.RepoRoot+"/deploy"), shellQuote(o.ComposeProjectName), shellQuote(o.RepoRoot))

	res, err := cli.Run(ctx, script)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("擦除未完成（退出码%d）：%s", res.ExitCode, firstLine(res.Stdout+res.Stderr))
	}
	return res.Stdout, nil
}
