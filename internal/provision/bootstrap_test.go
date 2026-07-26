package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ccw/internal/dns"
	"ccw/internal/pipeline"
	"ccw/internal/sshexec"
)

// scriptRunner按命令内容匹配返回预设结果（前缀或子串）。
type scriptRunner struct {
	cmds  []string
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
		ArtifactDir:        "/srv/ccw",
		Artifacts:          map[string]string{"compose.yaml": "services: {}\n"},
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

// push-artifacts：sha256全匹配则跳过；否则用heredoc写入。
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

// init-projects：CDK明文只经OnCDK回调一次，**绝不进日志**。
func TestInitProjectsCDKNeverLogged(t *testing.T) {
	const plain = "ccw_a1b2c3d4e5f60718.SECRETSECRETSECRET"
	r := &scriptRunner{rules: []rule{{contains: "init-project", res: sshexec.Result{
		Stdout: `{"slug": "project-a", "created": true, "public_id": "a1b2c3d4e5f60718", "cdk": "` + plain + `"}`,
	}}}}
	d := baseDeps(r, &fakeDNS{})
	var gotSlug, gotCDK, gotPub string
	d.OnCDK = func(slug, cdk, pub string) { gotSlug, gotCDK, gotPub = slug, cdk, pub }

	var logs []string
	err := stepByName(BootstrapSteps(d), "init-projects").Run(context.Background(),
		func(f string, a ...any) { logs = append(logs, f) })
	if err != nil {
		t.Fatal(err)
	}
	if gotSlug != "project-a" || gotCDK != plain || gotPub != "a1b2c3d4e5f60718" {
		t.Errorf("回调参数错误: %s %s %s", gotSlug, gotCDK, gotPub)
	}
	if strings.Contains(strings.Join(logs, "\n"), "SECRET") {
		t.Fatal("CDK明文绝不能进日志（它会写盘并经SSE推给浏览器）")
	}
}

// 已存在的项目不重新签发CDK（幂等，避免重跑产生多余CDK）。
func TestInitProjectsIdempotentNoNewCDK(t *testing.T) {
	r := &scriptRunner{rules: []rule{{contains: "init-project", res: sshexec.Result{
		Stdout: `{"slug": "project-a", "created": false, "project_id": "x"}`,
	}}}}
	d := baseDeps(r, &fakeDNS{})
	called := false
	d.OnCDK = func(string, string, string) { called = true }
	if err := stepByName(BootstrapSteps(d), "init-projects").Run(context.Background(), noLog); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("已存在的项目不应触发CDK回调")
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
	cdk, pub, created := parseInitProjectJSON(
		`{"slug": "a", "created": true, "public_id": "abc123", "cdk": "ccw_abc123.secret"}`)
	if cdk != "ccw_abc123.secret" || pub != "abc123" || !created {
		t.Errorf("got %q %q %v", cdk, pub, created)
	}
	if _, _, created := parseInitProjectJSON(`{"created": false}`); created {
		t.Error("created=false应解析为false")
	}
}
