package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"ccw/internal/dns"
	"ccw/internal/pipeline"
	"ccw/internal/redact"
	"ccw/internal/sshexec"
)

// scriptRunner按命令内容匹配返回预设结果（前缀或子串）。
type scriptRunner struct {
	cmds  []string
	stdin [][]byte
	rules []rule
}

type rule struct {
	contains string
	res      sshexec.Result
}

func (s *scriptRunner) Run(_ context.Context, cmd string) (sshexec.Result, error) {
	s.cmds = append(s.cmds, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.contains) {
			return r.res, nil
		}
	}
	return sshexec.Result{}, nil
}

// RunCapturingSecret模拟生产行为：Result里的Stdout**已脱敏**，raw是原文。
// 这样如果init-projects退回去解析res.Stdout，CDK就会变成[REDACTED]，
// 下面那条"明文只经回调一次"的测试会立刻红——这正是它要守的东西。
func (s *scriptRunner) RunCapturingSecret(ctx context.Context, cmd string) (sshexec.Result, string, error) {
	res, err := s.Run(ctx, cmd)
	raw := res.Stdout
	res.Stdout = redact.String(raw)
	res.Stderr = redact.String(res.Stderr)
	return res, raw, err
}

// RunStdin记录命令与stdin内容（推送源码包用）。
func (s *scriptRunner) RunStdin(_ context.Context, cmd string, in io.Reader) (sshexec.Result, error) {
	s.cmds = append(s.cmds, cmd)
	b, _ := io.ReadAll(in)
	s.stdin = append(s.stdin, b)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.contains) {
			return r.res, nil
		}
	}
	return sshexec.Result{}, nil
}

func (s *scriptRunner) joined() string { return strings.Join(s.cmds, "\n===\n") }

// fakeDNS记录调用并可控制生效判定。
type fakeDNS struct {
	upserted   []string
	propagated bool
}

func (f *fakeDNS) UpsertA(_ context.Context, _ dns.Zone, name, ip string, _ int) (string, error) {
	f.upserted = append(f.upserted, name+"->"+ip)
	return "manual:" + name + " A " + ip + " (TTL 60)", nil
}
func (f *fakeDNS) DeleteA(context.Context, dns.Zone, string, string) error { return nil }
func (f *fakeDNS) WaitPropagated(context.Context, dns.Zone, string) error {
	if f.propagated {
		return nil
	}
	return dns.ErrNotPropagated
}
func (f *fakeDNS) CheckCAA(context.Context, dns.Zone) (bool, error) { return true, nil }

func baseDeps(r *scriptRunner, d *fakeDNS) Deps {
	return Deps{
		Exec: r, Sudo: "sudo -n ", DNS: d,
		Zone:               dns.Zone{ID: "z1", Domain: "example.com", SubdomainPrefix: "api"},
		FQDN:               "api-01.example.com",
		PublicIP:           "203.0.113.7",
		Slugs:              []string{"project-a"},
		RepoRoot:           "/srv/ccw",
		Artifacts:          map[string]string{"deploy/compose.yaml": "services: {}\n"},
		SourceTar:          func() ([]byte, error) { return []byte("FAKE-TARBALL"), nil },
		ComposeProjectName: "ccw",
	}
}

func stepByName(steps []pipeline.Step, name string) pipeline.Step {
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	panic("no step " + name)
}

func noLog(string, ...any) {}

// 步骤顺序是§5.3的硬约束：dns-allocate必须排在compose-up之前。
func TestStepOrderDNSBeforeComposeUp(t *testing.T) {
	steps := BootstrapSteps(baseDeps(&scriptRunner{}, &fakeDNS{}))
	var dnsIdx, upIdx = -1, -1
	for i, s := range steps {
		switch s.Name {
		case "dns-allocate":
			dnsIdx = i
		case "compose-up":
			upIdx = i
		}
	}
	if dnsIdx < 0 || upIdx < 0 {
		t.Fatal("缺少关键步骤")
	}
	if dnsIdx > upIdx {
		t.Error("dns-allocate必须排在compose-up之前（§5.3关键约束2：DNS没生效就起Caddy会烧掉LE失败限额）")
	}
	if steps[len(steps)-1].Name != "disk-guard" {
		t.Errorf("disk-guard应在最后（校验最终形态），got %s", steps[len(steps)-1].Name)
	}
}

// probe：白名单外的发行版立即失败，不做猜测（§9.2）。
func TestProbeRejectsUnsupportedOS(t *testing.T) {
	r := &scriptRunner{rules: []rule{{contains: "os-release", res: sshexec.Result{
		Stdout: "centos 9\nx86_64\n200\n4\n8\n"}}}}
	step := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "probe")
	err := step.Run(context.Background(), noLog)
	if err == nil {
		t.Fatal("白名单外的发行版应失败")
	}
	if !strings.Contains(err.Error(), "9.2") {
		t.Errorf("错误应说明白名单来源: %v", err)
	}
}

func TestProbeAcceptsSupportedOSAndReportsFacts(t *testing.T) {
	r := &scriptRunner{rules: []rule{{contains: "os-release", res: sshexec.Result{
		Stdout: "ubuntu 24.04\naarch64\n120\n4\n8\n"}}}}
	d := baseDeps(r, &fakeDNS{})
	var gotOS, gotArch string
	d.OnHostFacts = func(os, arch string) { gotOS, gotArch = os, arch }
	if err := stepByName(BootstrapSteps(d), "probe").Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	if gotOS != "ubuntu 24.04" || gotArch != "aarch64" {
		t.Errorf("facts=%q %q", gotOS, gotArch)
	}
}

// 磁盘不足按§7.6的规格核算拒绝。
func TestProbeRejectsSmallDisk(t *testing.T) {
	r := &scriptRunner{rules: []rule{{contains: "os-release", res: sshexec.Result{
		Stdout: "ubuntu 24.04\nx86_64\n20\n4\n8\n"}}}}
	err := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "probe").Run(context.Background(), noLog)
	if err == nil || !strings.Contains(err.Error(), "磁盘") {
		t.Fatalf("磁盘不足应失败，got %v", err)
	}
}

// install-docker：已装则precheck跳过（幂等）。
func TestInstallDockerPrecheckSkips(t *testing.T) {
	r := &scriptRunner{rules: []rule{{contains: "docker --version", res: sshexec.Result{ExitCode: 0}}}}
	step := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "install-docker")
	ok, err := step.Precheck(context.Background(), noLog)
	if err != nil || !ok {
		t.Errorf("已装Docker应跳过: ok=%v err=%v", ok, err)
	}

	r2 := &scriptRunner{rules: []rule{{contains: "docker --version", res: sshexec.Result{ExitCode: 127}}}}
	step2 := stepByName(BootstrapSteps(baseDeps(r2, &fakeDNS{})), "install-docker")
	if ok, _ := step2.Precheck(context.Background(), noLog); ok {
		t.Error("未装Docker不应跳过")
	}
}

// dns-allocate：未生效即失败并提示要加的记录（manual模式的常态）。
func TestDNSAllocateBlocksUntilPropagated(t *testing.T) {
	fd := &fakeDNS{propagated: false}
	var logs []string
	step := stepByName(BootstrapSteps(baseDeps(&scriptRunner{}, fd)), "dns-allocate")
	err := step.Run(context.Background(), func(f string, a ...any) { logs = append(logs, f) })
	if !errors.Is(err, dns.ErrNotPropagated) {
		t.Fatalf("未生效应返回ErrNotPropagated，got %v", err)
	}
	if !strings.Contains(err.Error(), "重试") {
		t.Errorf("应提示加完记录后重试: %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "需要的记录") {
		t.Errorf("应把待添加的记录打给管理员: %v", logs)
	}

	fd.propagated = true
	if err := step.Run(context.Background(), noLog); err != nil {
		t.Errorf("生效后应通过: %v", err)
	}
	if len(fd.upserted) == 0 || !strings.Contains(fd.upserted[0], "api-01.example.com->203.0.113.7") {
		t.Errorf("UpsertA参数错误: %v", fd.upserted)
	}
}

// push-artifacts：sha256全匹配则跳过；否则用heredoc写入到<RepoRoot>/deploy/compose.yaml。
func TestPushArtifactsPrecheckAndWrite(t *testing.T) {
	content := "services: {}\n"
	want := sha256Hex(content)
	r := &scriptRunner{rules: []rule{{contains: "sha256sum", res: sshexec.Result{Stdout: want + "\n"}}}}
	step := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "push-artifacts")
	ok, err := step.Precheck(context.Background(), noLog)
	if err != nil || !ok {
		t.Errorf("sha匹配应跳过: ok=%v err=%v", ok, err)
	}

	r2 := &scriptRunner{rules: []rule{{contains: "sha256sum", res: sshexec.Result{Stdout: "deadbeef\n"}}}}
	step2 := stepByName(BootstrapSteps(baseDeps(r2, &fakeDNS{})), "push-artifacts")
	if ok, _ := step2.Precheck(context.Background(), noLog); ok {
		t.Error("sha不匹配不应跳过")
	}
	if err := step2.Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	cmds := r2.joined()
	if !strings.Contains(cmds, "<<'CCWEOF'") || !strings.Contains(cmds, content) {
		t.Errorf("应用带引号的heredoc写入（禁止变量展开）:\n%s", cmds)
	}
}

// render-env：密钥在节点本地生成，且**已有值不被覆盖**（重跑不换数据库密码）。
func TestRenderEnvGeneratesLocallyAndPreserves(t *testing.T) {
	r := &scriptRunner{}
	step := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "render-env")
	if err := step.Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	cmd := r.joined()
	for _, want := range []string{"openssl rand -hex 32", "openssl rand -hex 16", "chmod 600"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("缺少%q:\n%s", want, cmd)
		}
	}
	// 幂等的关键：已有值时不重新生成
	if !strings.Contains(cmd, `[ -n "$(get POSTGRES_PASSWORD)" ] ||`) {
		t.Error("已有的数据库密码不得被重跑覆盖（会让已有数据连不上）")
	}
	// CCW_USAGE_WEIGHTS必须写入：worker-agent缺它直接拒绝启动
	if !strings.Contains(cmd, "CCW_USAGE_WEIGHTS") {
		t.Error("必须写入CCW_USAGE_WEIGHTS，否则worker-agent拒绝启动")
	}
	// 域名要写进.env（Caddy靠它签证书）
	if !strings.Contains(cmd, "CCW_DOMAIN") {
		t.Error("必须写入CCW_DOMAIN")
	}
}

// init-projects：CDK明文只经OnProject回调一次，**绝不进日志**。
func TestInitProjectsCDKNeverLogged(t *testing.T) {
	const plain = "ccw_a1b2c3d4e5f60718.SECRETSECRETSECRET"
	r := &scriptRunner{rules: []rule{{contains: "init-project", res: sshexec.Result{
		Stdout: `Creating network deploy_default` + "\n" + // compose的噪声，解析必须扛得住
			`{"slug": "project-a", "project_id": "p-1", "container": "project-a-claude",
			  "created": true, "disk_gib": 15, "five_hour": 1000, "seven_day": 5000,
			  "public_id": "a1b2c3d4e5f60718", "cdk": "` + plain + `"}`,
	}}}}
	d := baseDeps(r, &fakeDNS{})
	var got ProjectResult
	d.OnProject = func(pr ProjectResult) { got = pr }

	var logs []string
	err := stepByName(BootstrapSteps(d), "init-projects").Run(context.Background(),
		func(f string, a ...any) { logs = append(logs, f) })
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "project-a" || got.CDK != plain || got.PublicID != "a1b2c3d4e5f60718" {
		t.Errorf("回调参数错误: %+v", got)
	}
	// 镜像信息也要带回来，否则Console库里项目行是空壳、/connect解析不到
	if got.RemoteID != "p-1" || got.DiskGiB != 15 || got.FiveHour != 1000 || got.SevenDay != 5000 {
		t.Errorf("镜像字段没解析出来: %+v", got)
	}
	if strings.Contains(strings.Join(logs, "\n"), "SECRET") {
		t.Fatal("CDK明文绝不能进日志（它会写盘并经SSE推给浏览器）")
	}
}

// 已存在的项目不重新签发CDK（幂等，避免重跑产生多余CDK），
// 但**仍要回调**——配额与remote id的镜像需要刷新。
func TestInitProjectsIdempotentNoNewCDK(t *testing.T) {
	r := &scriptRunner{rules: []rule{{contains: "init-project", res: sshexec.Result{
		Stdout: `{"slug": "project-a", "created": false, "project_id": "x", "disk_gib": 15}`,
	}}}}
	d := baseDeps(r, &fakeDNS{})
	var got ProjectResult
	d.OnProject = func(pr ProjectResult) { got = pr }
	if err := stepByName(BootstrapSteps(d), "init-projects").Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	if got.Slug != "project-a" || got.RemoteID != "x" {
		t.Errorf("已存在的项目也应回调镜像信息: %+v", got)
	}
	if got.PublicID != "" || got.CDK != "" {
		t.Error("已存在的项目不应带回新CDK")
	}
}

// healthcheck：容器未全running即失败。
func TestHealthcheckDetectsDownContainer(t *testing.T) {
	r := &scriptRunner{rules: []rule{
		{contains: "compose -p", res: sshexec.Result{Stdout: "postgres running\ncontrol-api exited\n"}},
	}}
	err := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "healthcheck").Run(context.Background(), noLog)
	if err == nil || !strings.Contains(err.Error(), "control-api") {
		t.Fatalf("应指出未运行的容器，got %v", err)
	}
}

// disk-guard只报告不失败：N4的取舍是明示接受，不能表述为"已隔离"。
func TestDiskGuardReportsWithoutFailing(t *testing.T) {
	r := &scriptRunner{rules: []rule{
		{contains: "DockerRootDir", res: sshexec.Result{Stdout: "/var/lib/docker\n/ 41%\n"}},
	}}
	var logs []string
	err := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "disk-guard").Run(context.Background(),
		func(f string, a ...any) { logs = append(logs, f) })
	if err != nil {
		t.Fatalf("disk-guard不应让部署失败: %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "取舍") {
		t.Errorf("data-root在根分区时应如实提示已接受的取舍: %v", logs)
	}
	// 不得出现"已隔离/保证"这类越界表述
	for _, banned := range []string{"已隔离", "保证不超过"} {
		if strings.Contains(joined, banned) {
			t.Errorf("越界表述: %q", banned)
		}
	}
}

func TestParseInitProjectJSON(t *testing.T) {
	pr := parseInitProjectJSON(
		`{"slug": "a", "created": true, "public_id": "abc123", "cdk": "ccw_abc123.secret"}`)
	if pr.CDK != "ccw_abc123.secret" || pr.PublicID != "abc123" || !pr.Created {
		t.Errorf("got %+v", pr)
	}
	if parseInitProjectJSON(`{"created": false}`).Created {
		t.Error("created=false应解析为false")
	}
	// compose的噪声包在JSON前后都要能扛住
	noisy := parseInitProjectJSON("Creating network x_default\n" +
		`{"slug": "b", "disk_gib": 15}` + "\nDone\n")
	if noisy.Slug != "b" || noisy.DiskGiB != 15 {
		t.Errorf("噪声中的JSON应能取出: %+v", noisy)
	}
	// 完全不是JSON时返回零值，而不是panic或半个结构体
	if got := parseInitProjectJSON("command not found"); got.Slug != "" || got.Created {
		t.Errorf("非JSON输入应返回零值: %+v", got)
	}
}

// push-source是compose-up能成立的前提：节点必须有完整源码树才能构建镜像。
// 这条测试守住那个曾经缺失的前提——只推几个编排文件时compose-up必然失败。
func TestPushSourceUploadsTarballViaStdin(t *testing.T) {
	r := &scriptRunner{}
	d := baseDeps(r, &fakeDNS{})
	step := stepByName(BootstrapSteps(d), "push-source")
	if err := step.Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	if len(r.stdin) != 1 || string(r.stdin[0]) != "FAKE-TARBALL" {
		t.Fatalf("源码包应经stdin推送（命令行在节点上人人可见），got %v", r.stdin)
	}
	cmds := r.joined()
	if !strings.Contains(cmds, "tar xzf - -C '/srv/ccw'") {
		t.Errorf("应解包到RepoRoot:\n%s", cmds)
	}
	// 内容绝不能出现在命令行里
	if strings.Contains(cmds, "FAKE-TARBALL") {
		t.Error("源码包内容不得拼进命令行（ps aux对所有用户可见）")
	}
}

// precheck靠sha标记文件：未变则跳过整个上传。
func TestPushSourcePrecheckSkipsWhenUnchanged(t *testing.T) {
	tarball := []byte("FAKE-TARBALL")
	r := &scriptRunner{rules: []rule{
		{contains: ".ccw-src-sha256", res: sshexec.Result{Stdout: sha256HexBytes(tarball) + "\n"}},
	}}
	step := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "push-source")
	ok, err := step.Precheck(context.Background(), noLog)
	if err != nil || !ok {
		t.Errorf("sha一致应跳过: ok=%v err=%v", ok, err)
	}

	r2 := &scriptRunner{rules: []rule{
		{contains: ".ccw-src-sha256", res: sshexec.Result{Stdout: "stale\n"}},
	}}
	step2 := stepByName(BootstrapSteps(baseDeps(r2, &fakeDNS{})), "push-source")
	if ok, _ := step2.Precheck(context.Background(), noLog); ok {
		t.Error("sha不一致不应跳过")
	}
}

// 没有源码包时明确失败：这是compose-up的前提，不能沉默跳过。
func TestPushSourceFailsWithoutTarball(t *testing.T) {
	d := baseDeps(&scriptRunner{}, &fakeDNS{})
	d.SourceTar = nil
	err := stepByName(BootstrapSteps(d), "push-source").Run(context.Background(), noLog)
	if err == nil || !strings.Contains(err.Error(), "源码包") {
		t.Fatalf("缺源码包应明确失败，got %v", err)
	}
}

// compose-up必须在<RepoRoot>/deploy下执行——渲染出的compose.yaml用
// `context: ..`，工作目录错了build context就指向错的地方。
func TestComposeUpRunsInDeployDir(t *testing.T) {
	r := &scriptRunner{}
	if err := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "compose-up").Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.joined(), "cd '/srv/ccw/deploy' &&") {
		t.Errorf("compose-up应在deploy目录执行:\n%s", r.joined())
	}
}

// push-source必须排在compose-up之前，且在dns-allocate之后
// （DNS阻断时不该已经把源码推上去——那没意义，但更重要的是顺序别乱）。
func TestPushSourceBeforeComposeUp(t *testing.T) {
	steps := BootstrapSteps(baseDeps(&scriptRunner{}, &fakeDNS{}))
	idx := map[string]int{}
	for i, s := range steps {
		idx[s.Name] = i
	}
	if idx["push-source"] > idx["compose-up"] {
		t.Error("push-source必须在compose-up之前——否则节点上没有源码可构建")
	}
	if idx["push-source"] > idx["push-artifacts"] {
		t.Error("push-source必须在push-artifacts之前——compose.yaml要覆盖解包出来的那份")
	}
}

// harden现在是流水线步骤：凭据交接失败会被记进provision_steps，
// 而不是只留一行日志（此前它跑在流水线之外）。
func TestHardenIsAPipelineStep(t *testing.T) {
	r := &scriptRunner{}
	d := baseDeps(r, &fakeDNS{})
	called := false
	d.Harden = func(context.Context, pipeline.Logf) error { called = true; return nil }
	if err := stepByName(BootstrapSteps(d), "harden").Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("harden步骤应执行凭据交接")
	}

	d.Harden = func(context.Context, pipeline.Logf) error { return errors.New("公钥注入失败") }
	err := stepByName(BootstrapSteps(d), "harden").Run(context.Background(), noLog)
	if err == nil || !strings.Contains(err.Error(), "公钥注入失败") {
		t.Fatalf("凭据交接失败应让harden步骤失败（从而被记账），got %v", err)
	}
}

// healthcheck 失败时，探测现场必须进日志。
//
// 老版本用 `curl -s`，失败原因被吞掉，错误只剩一个 `000`——连不上、TLS 校验失败、
// 超时全是 000，管理员无从下手（2026-07-30 真机上就卡在这里）。
// 现在探测脚本自己把现场打在 stdout 上，而 run() 出错时只带得走第一行，
// 所以步骤必须**先把 stdout 写进日志再返回错误**。
func TestHealthcheckLogsProbeEvidenceOnFailure(t *testing.T) {
	evidence := "探测30次仍未就绪：code=000 curl_exit=60 curl: (60) SSL certificate problem\n" +
		"--- 域名解析 ---\n203.0.113.9\n--- control-api 近期日志 ---\nmigrate: connection refused\n"
	r := &scriptRunner{rules: []rule{
		// 探测脚本（唯一带 auth/exchange 的那条）退出1，并把现场打在stdout
		{contains: "auth/exchange", res: sshexec.Result{Stdout: evidence, ExitCode: 1}},
		{contains: "compose -p", res: sshexec.Result{Stdout: "caddy running\ncontrol-api running\n"}},
	}}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	err := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "healthcheck").Run(context.Background(), logf)
	if err == nil {
		t.Fatal("探测失败时步骤应失败")
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"curl_exit=60", "control-api 近期日志", "connection refused"} {
		if !strings.Contains(joined, want) {
			t.Errorf("现场未进日志，缺 %q。日志：\n%s", want, joined)
		}
	}
}

// 探测脚本必须重试：`docker compose ps` 说 running 不等于在听端口，
// control-api 启动时要跑迁移（重置后是空库，更久）。一次性探测会在
// "正在迁移"这个正常状态上判失败。
func TestHealthcheckProbeRetries(t *testing.T) {
	r := &scriptRunner{rules: []rule{
		{contains: "auth/exchange", res: sshexec.Result{Stdout: "OK code=401\n"}},
		{contains: "compose -p", res: sshexec.Result{Stdout: "caddy running\n"}},
	}}
	if err := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "healthcheck").Run(context.Background(), noLog); err != nil {
		t.Fatalf("401应算就绪: %v", err)
	}
	var probe string
	for _, c := range r.cmds {
		if strings.Contains(c, "auth/exchange") {
			probe = c
		}
	}
	if !strings.Contains(probe, "seq 1 30") {
		t.Error("探测应重试而不是一次定生死")
	}
	if strings.Contains(probe, "curl -s ") {
		t.Error("不能用 curl -s：它把失败原因一起吞掉，错误就只剩一个 000")
	}
	// 日志命令不能依赖 `cd` 进源码树——重置之后那棵树可能已经不在了
	if strings.Contains(probe, "cd ") {
		t.Error("现场收集不应依赖 cd 进源码树")
	}
}

// 探测必须经**回环**，不能依赖节点访问自己的公网地址。
//
// 2026-07-30 真机：客户端从公网拿到 401（说明 Caddy 与 control-api 都是好的），
// 而节点自己 curl 同一个域名得到 000。这一步要验的是"Caddy → control-api 接对了"，
// 不是"云厂商的 hairpin NAT 通不通"——后者与栈的健康无关，却能让部署整体失败。
func TestHealthcheckProbesViaLoopback(t *testing.T) {
	r := &scriptRunner{rules: []rule{
		{contains: "auth/exchange", res: sshexec.Result{Stdout: "OK code=401\n"}},
		{contains: "compose -p", res: sshexec.Result{Stdout: "caddy running\n"}},
	}}
	if err := stepByName(BootstrapSteps(baseDeps(r, &fakeDNS{})), "healthcheck").Run(context.Background(), noLog); err != nil {
		t.Fatalf("401应算就绪: %v", err)
	}
	var probe string
	for _, c := range r.cmds {
		if strings.Contains(c, "auth/exchange") {
			probe = c
		}
	}
	if !strings.Contains(probe, `--resolve "$fq:443:127.0.0.1"`) {
		t.Error("重试循环里的探测应经回环，不该绕公网")
	}
	// 失败时仍要走一次公网，用来佐证"是栈坏了还是只是绕不回来"
	if !strings.Contains(probe, "公网路径") {
		t.Error("失败现场应包含一次公网探测作为对照")
	}
}
