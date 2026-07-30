package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ccw/internal/dns"
	"ccw/internal/pipeline"
)

// C11 bootstrap流水线（console-fleet-design §5.3的12步）。
//
// 每一步都满足三条性质：幂等、可precheck、可断点续跑。步骤本身只负责"做什么"，
// 记账、跳过与恢复由pipeline引擎统一处理。
//
// 两条硬顺序约束（§5.3）：
//   - dns-allocate必须**排在compose-up之前且失败即阻断**：DNS没生效就起Caddy，
//     会连续触发Let's Encrypt验证失败，撞上「每标识符每小时5次失败」后
//     即使DNS修好也要等一小时（§6.5）
//   - render-env的密钥**在节点本地生成**，明文从不经过Console（§5.3关键约束1）

// Deps是bootstrap需要的外部能力，全部可注入以便单测。
type Deps struct {
	// Exec在节点上执行命令（已由sshexec完成源头脱敏）。
	Exec Runner
	// Sudo是特权命令前缀（root为空、免密sudo为"sudo -n "、带密码为管道形式）。
	Sudo string
	// DNS是provider（默认manual）。
	DNS dns.Provider
	// Zone与FQDN是本节点分配到的域名。
	Zone dns.Zone
	FQDN string
	// PublicIP是节点公网IP（A记录的目标）。
	PublicIP string
	// Slugs是初始项目列表（最多3个，产品规则由ccwadmin再次强制）。
	Slugs []string

	// RepoRoot是节点上解开源码包的根目录（＝仓库根的等价物）。
	// 编排文件在<RepoRoot>/deploy下，与仓库布局一致——**这是硬要求**：
	// 渲染出的compose.yaml用`context: ..` + `dockerfile: deploy/Dockerfile.X`，
	// 且各Dockerfile都`COPY . .`后从Go源码构建，节点上必须有完整源码树。
	RepoRoot string
	// SourceTar是仓库源码的tar.gz。由Console镜像内置（见deploy/Dockerfile.console），
	// 经SSH的stdin推送——不走命令行，那在节点上对所有用户可见（ps aux）。
	SourceTar func() ([]byte, error)
	// Artifacts是解包后要覆盖写入的文件：相对RepoRoot的路径 → 内容。
	// 目前只有deploy/compose.yaml（按本节点的项目列表渲染）。
	Artifacts map[string]string
	// ComposeProjectName供docker compose -p使用，保持与已有部署一致。
	ComposeProjectName string
	// Harden执行凭据交接（生成/注入/验证托管密钥）。为nil表示已完成或无需执行。
	// 放进Deps是为了让它成为**流水线的一个步骤**、进provision_steps记账，
	// 而不是在流水线之外悄悄跑（失败时步骤表里看不到任何记录）。
	Harden func(ctx context.Context, log pipeline.Logf) error
	// OnProject在init-projects处理完一个slug后回调，带回节点侧的项目信息
	// 与（新建时的）CDK明文。**明文只经过这里一次**，由调用方转发到浏览器后
	// 即丢弃，绝不入Console库（§8.4）；其余字段是可以落库的镜像信息。
	OnProject func(ProjectResult)
	// OnHostFacts在probe采集完成后回调（写nodes表）。
	OnHostFacts func(osRelease, arch string)
	// DiskGiB是probe校验可用磁盘的门槛（默认按§7.6的80 GB规格）。
	MinDiskGiB int
}

// 发行版白名单（§9.2）：Docker安装、防火墙工具、init系统在各发行版差异巨大，
// 宣称"通用"而只在两三个发行版测过属于CLAUDE.md禁止的「未验证的功能不写成已完成」。
var supportedOS = []string{"ubuntu 22.04", "ubuntu 24.04", "debian 12"}

// BootstrapSteps构造12步流水线。
//
// 注意步骤顺序即§5.3表格的顺序，改动前先读那一节——尤其是dns-allocate与
// compose-up的相对位置，以及disk-guard必须在最后（它校验的是最终形态）。
func BootstrapSteps(d Deps) []pipeline.Step {
	return []pipeline.Step{
		stepProbe(d),
		stepHarden(d),
		stepInstallDocker(d),
		stepDNSAllocate(d),
		stepPushSource(d),
		stepPushArtifacts(d),
		stepRenderEnv(d),
		stepComposeUp(d),
		stepCertWait(d),
		stepHealthcheck(d),
		stepInitProjects(d),
		stepDiskGuard(d),
	}
}

// deployDir是节点上编排文件所在目录（＝仓库的deploy/）。
func (d Deps) deployDir() string { return d.RepoRoot + "/deploy" }

// run是步骤内执行命令的简写：非0退出码转成带退出码的错误。
func run(ctx context.Context, d Deps, cmd string) (string, error) {
	res, err := d.Exec.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, &pipeline.ExitError{
			Code: res.ExitCode,
			Err:  fmt.Errorf("命令失败: %s", firstLine(res.Stderr+res.Stdout)),
		}
	}
	return res.Stdout, nil
}

// ok执行命令并只看退出码（precheck用）。
func ok(ctx context.Context, d Deps, cmd string) bool {
	res, err := d.Exec.Run(ctx, cmd)
	return err == nil && res.ExitCode == 0
}

// 1) probe：采集系统信息、校验发行版白名单与磁盘。
//
// 白名单外立即失败（§9.2）——不做任何"试试看能不能装上"的猜测。
func stepProbe(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "probe",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			out, err := run(ctx, d, `. /etc/os-release 2>/dev/null; echo "$ID $VERSION_ID"; uname -m; `+
				`df -BG --output=avail / | tail -1 | tr -dc '0-9'; echo; nproc; free -g | awk '/Mem:/{print $2}'`)
			if err != nil {
				return err
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) < 3 {
				return fmt.Errorf("probe输出异常，无法识别系统")
			}
			osID := strings.ToLower(strings.TrimSpace(lines[0]))
			arch := strings.TrimSpace(lines[1])
			log("系统=%s 架构=%s", osID, arch)

			supported := false
			for _, s := range supportedOS {
				if osID == s {
					supported = true
				}
			}
			if !supported {
				return fmt.Errorf("不支持的发行版%q：仅支持%s（设计§9.2的白名单）",
					osID, strings.Join(supportedOS, "、"))
			}
			if d.OnHostFacts != nil {
				d.OnHostFacts(osID, arch)
			}

			minDisk := d.MinDiskGiB
			if minDisk == 0 {
				minDisk = 60 // 3×15 GiB workspace + 13–17 GiB非workspace开销（§7.6）
			}
			var availGiB int
			fmt.Sscanf(strings.TrimSpace(lines[2]), "%d", &availGiB)
			if availGiB > 0 && availGiB < minDisk {
				return fmt.Errorf("可用磁盘%d GiB低于要求的%d GiB（§7.6：3×15 GiB workspace + 13–17 GiB开销）",
					availGiB, minDisk)
			}
			log("可用磁盘=%d GiB", availGiB)
			return nil
		},
	}
}

// 2) harden：凭据交接（生成/注入/验证托管密钥）+ 防火墙放行。
//
// 凭据交接的实现在credentials.go的Harden里（它需要拨号能力与信封加密，属于编排层），
// 经Deps.Harden注入。**放在流水线内而不是流水线外**：失败时步骤表里能看到
// 是harden挂的，而不是只留一行日志；成功后也进provision_steps，续跑时可跳过。
func stepHarden(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "harden",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			if d.Harden != nil {
				if err := d.Harden(ctx, log); err != nil {
					return err
				}
			}
			// 防火墙：只在ufw已启用时放行必要端口，不主动启用防火墙——
			// 在远程机器上启用防火墙有把自己关在门外的风险。
			if ok(ctx, d, "command -v ufw >/dev/null 2>&1") {
				if ok(ctx, d, d.Sudo+"ufw status | grep -q 'Status: active'") {
					for _, p := range []string{"22", "80", "443"} {
						if _, err := run(ctx, d, d.Sudo+"ufw allow "+p+"/tcp"); err != nil {
							return err
						}
					}
					log("ufw已放行22/80/443")
				} else {
					log("ufw未启用，跳过防火墙配置（请确认云厂商安全组已放行22/80/443）")
				}
			} else {
				log("未安装ufw，跳过防火墙配置（请确认云厂商安全组已放行22/80/443）")
			}
			return nil
		},
	}
}

// 3) install-docker：装Docker CE与compose插件。
//
// **不配置data-root指向独立分区**（设计§12.1的N4第1项）：那需要目标机器上确实
// 有第二块盘/分区，本流水线不做这个假设，也不替管理员决定磁盘布局。
// 现状由disk-guard步骤如实报告，STATUS.md记为N4未实施——
// 不在这里写一句"已配置"了事。
func stepInstallDocker(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "install-docker",
		Precheck: func(ctx context.Context, log pipeline.Logf) (bool, error) {
			return ok(ctx, d, "docker --version >/dev/null 2>&1 && docker compose version >/dev/null 2>&1"), nil
		},
		Run: func(ctx context.Context, log pipeline.Logf) error {
			log("安装Docker CE与compose插件（用官方便捷脚本，版本随上游稳定版）")
			// 说明：便捷脚本装的是上游stable，与CLAUDE.md「禁止未固定版本」有张力。
			// 固定Docker版本需要按发行版拼apt版本串，且各发行版可用版本不同；
			// 当前接受这一点，并在versions.lock里如实记为未固定项。
			if _, err := run(ctx, d, `set -e
curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
sh /tmp/get-docker.sh
rm -f /tmp/get-docker.sh`); err != nil {
				return err
			}
			return nil
		},
	}
}

// 4) dns-allocate：确保A记录存在并生效。
//
// **失败即阻断，绝不继续到compose-up**（§5.3关键约束2）：DNS没生效就起Caddy会
// 连续触发LE验证失败，撞上「每标识符每小时5次失败」后即使DNS修好也要等一小时。
func stepDNSAllocate(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "dns-allocate",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			changeID, err := d.DNS.UpsertA(ctx, d.Zone, d.FQDN, d.PublicIP, 60)
			if err != nil {
				return err
			}
			log("需要的记录：%s", dns.Instructions(d.FQDN, d.PublicIP))
			if err := d.DNS.WaitPropagated(ctx, d.Zone, changeID); err != nil {
				// manual模式下这是常态：管理员还没加记录。提示后停在这一步，
				// 加完记录点重试即可（断点续跑）。
				return fmt.Errorf("%w——请添加上面的A记录后重试本次部署", err)
			}
			log("DNS已生效：%s → %s", d.FQDN, d.PublicIP)
			return nil
		},
	}
}

// 5) push-source：把仓库源码解到节点的RepoRoot。
//
// **这一步是compose-up能成立的前提**：渲染出的compose.yaml用
// `context: ..` + `dockerfile: deploy/Dockerfile.X`，而三个Dockerfile都
// `COPY . .`后从Go源码构建二进制——节点上必须有完整的源码树，
// 只推几个编排文件是起不来的。
//
// 传输走SSH的stdin（RunStdin），不把内容拼进命令行——命令行在节点上
// 对所有用户可见（ps aux），且有长度上限。
// precheck比对源码包的sha256标记文件，未变则跳过。
func stepPushSource(d Deps) pipeline.Step {
	const shaMarker = "/.ccw-src-sha256"
	return pipeline.Step{
		Name: "push-source",
		Precheck: func(ctx context.Context, log pipeline.Logf) (bool, error) {
			if d.SourceTar == nil {
				return false, nil
			}
			tar, err := d.SourceTar()
			if err != nil {
				return false, err
			}
			res, err := d.Exec.Run(ctx, "cat "+shellQuote(d.RepoRoot+shaMarker)+" 2>/dev/null")
			if err != nil {
				return false, err
			}
			return strings.TrimSpace(res.Stdout) == sha256HexBytes(tar), nil
		},
		Run: func(ctx context.Context, log pipeline.Logf) error {
			if d.SourceTar == nil {
				return fmt.Errorf("未提供源码包：节点无法构建镜像（检查Console镜像是否内置了node-src.tar.gz）")
			}
			tar, err := d.SourceTar()
			if err != nil {
				return err
			}
			// 目录归属给当前用户：后续步骤（写.env、docker compose）都以该用户执行。
			if _, err := run(ctx, d, d.Sudo+"mkdir -p "+shellQuote(d.RepoRoot)+
				" && "+d.Sudo+"chown -R $(id -u):$(id -g) "+shellQuote(d.RepoRoot)); err != nil {
				return err
			}
			// --overwrite保证重跑时用新版本覆盖；不删已有文件（.env在deploy/下，必须保留）。
			cmd := fmt.Sprintf("tar xzf - -C %s --overwrite", shellQuote(d.RepoRoot))
			res, err := d.Exec.RunStdin(ctx, cmd, bytesReader(tar))
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return &pipeline.ExitError{Code: res.ExitCode,
					Err: fmt.Errorf("解包源码失败: %s", firstLine(res.Stderr))}
			}
			if _, err := run(ctx, d, fmt.Sprintf("printf %%s %s > %s",
				shellQuote(sha256HexBytes(tar)), shellQuote(d.RepoRoot+shaMarker))); err != nil {
				return err
			}
			log("源码已推送并解包到 %s（%d KiB）", d.RepoRoot, len(tar)/1024)
			return nil
		},
	}
}

// 6) push-artifacts：把按本节点项目列表渲染的compose.yaml覆盖进deploy/。
//
// 其余编排文件（Caddyfile、三个Dockerfile）来自源码包，不在这里重复推送——
// 两处各存一份迟早漂移。
func stepPushArtifacts(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "push-artifacts",
		Precheck: func(ctx context.Context, log pipeline.Logf) (bool, error) {
			if len(d.Artifacts) == 0 {
				return false, nil
			}
			for name, content := range d.Artifacts {
				want := sha256Hex(content)
				out, err := d.Exec.Run(ctx, fmt.Sprintf("sha256sum %s 2>/dev/null | cut -d' ' -f1",
					shellQuote(d.RepoRoot+"/"+name)))
				if err != nil {
					return false, err
				}
				if strings.TrimSpace(out.Stdout) != want {
					return false, nil
				}
			}
			return true, nil
		},
		Run: func(ctx context.Context, log pipeline.Logf) error {
			for _, name := range sortedKeys(d.Artifacts) {
				content := d.Artifacts[name]
				path := d.RepoRoot + "/" + name
				// 用heredoc写文件：内容里可能有引号与变量，'EOF'（带引号）禁止展开。
				// 分隔符取一个不可能出现在YAML里的串。
				cmd := fmt.Sprintf("mkdir -p %s && cat > %s <<'CCWEOF'\n%s\nCCWEOF",
					shellQuote(dirOf(path)), shellQuote(path), content)
				if _, err := run(ctx, d, cmd); err != nil {
					return fmt.Errorf("写入%s失败: %w", name, err)
				}
				log("已写入 %s", name)
			}
			return nil
		},
	}
}

// 7) render-env：**在节点本地生成**密钥并写.env（0600）。
//
// 关键约束（§5.3）：POSTGRES_PASSWORD与CCW_TOKEN_KEY用节点上的openssl生成，
// 明文从不经过Console。这样Console库泄露不等于节点令牌泄露。
func stepRenderEnv(d Deps) pipeline.Step {
	envPath := d.deployDir() + "/.env"
	return pipeline.Step{
		Name: "render-env",
		Precheck: func(ctx context.Context, log pipeline.Logf) (bool, error) {
			// 变量齐全才算满足；缺一个就重新生成（幂等：已有值不会被覆盖，见Run）。
			return ok(ctx, d, fmt.Sprintf(
				`test -f %s && grep -q '^CCW_TOKEN_KEY=.\{64\}' %s && grep -q '^POSTGRES_PASSWORD=.' %s `+
					`&& grep -q '^CCW_DOMAIN=.' %s && grep -q '^CCW_USAGE_WEIGHTS=.' %s`,
				shellQuote(envPath), shellQuote(envPath), shellQuote(envPath),
				shellQuote(envPath), shellQuote(envPath))), nil
		},
		Run: func(ctx context.Context, log pipeline.Logf) error {
			// 已存在的值保留（重跑不会换掉数据库密码——那会让已有数据连不上）。
			script := fmt.Sprintf(`set -e
ENV=%s
touch "$ENV" && chmod 600 "$ENV"
get() { grep "^$1=" "$ENV" 2>/dev/null | head -1 | cut -d= -f2-; }
set_kv() { grep -q "^$1=" "$ENV" && sed -i "s|^$1=.*|$1=$2|" "$ENV" || echo "$1=$2" >> "$ENV"; }
[ -n "$(get POSTGRES_PASSWORD)" ] || set_kv POSTGRES_PASSWORD "$(openssl rand -hex 16)"
[ -n "$(get CCW_TOKEN_KEY)" ]     || set_kv CCW_TOKEN_KEY "$(openssl rand -hex 32)"
set_kv CCW_DOMAIN %s
[ -n "$(get CCW_USAGE_WEIGHTS)" ] || set_kv CCW_USAGE_WEIGHTS "1,5,1,1"
[ -n "$(get CLAUDE_CODE_VERSION)" ] || echo "CLAUDE_CODE_VERSION=" >> "$ENV"
chmod 600 "$ENV"`, shellQuote(envPath), shellQuote(d.FQDN))
			if _, err := run(ctx, d, script); err != nil {
				return err
			}
			log(".env已生成（密钥在节点本地生成，明文不经过Console）")
			return nil
		},
	}
}

// 8) compose-up：起栈。
func stepComposeUp(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "compose-up",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			log("构建镜像并启动（首次需要几分钟）")
			_, err := run(ctx, d, fmt.Sprintf("cd %s && %sdocker compose -p %s up -d --build",
				shellQuote(d.deployDir()), d.Sudo, shellQuote(d.ComposeProjectName)))
			if err != nil {
				return err
			}
			// 回收build cache（§7.6：可立即回收2–5 GiB）
			if _, perr := run(ctx, d, d.Sudo+"docker builder prune -f"); perr != nil {
				log("builder prune失败（不影响部署）：%v", perr)
			}
			return nil
		},
	}
}

// 9) cert-wait：轮询443握手直到证书就绪。
func stepCertWait(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "cert-wait",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			cmd := fmt.Sprintf(`for i in $(seq 1 60); do
  if echo | openssl s_client -connect %s:443 -servername %s 2>/dev/null | grep -q 'Verify return code: 0'; then
    echo READY; exit 0
  fi
  sleep 5
done
exit 1`, shellQuote(d.FQDN), shellQuote(d.FQDN))
			if _, err := run(ctx, d, cmd); err != nil {
				return fmt.Errorf("证书未在5分钟内就绪（检查DNS是否指向本机、80端口是否放行）: %w", err)
			}
			log("HTTPS证书已就绪")
			return nil
		},
	}
}

// 10) healthcheck：容器healthy + API可达。
func stepHealthcheck(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "healthcheck",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			out, err := run(ctx, d, fmt.Sprintf("cd %s && %sdocker compose -p %s ps --format '{{.Service}} {{.State}}'",
				shellQuote(d.deployDir()), d.Sudo, shellQuote(d.ComposeProjectName)))
			if err != nil {
				return err
			}
			log("容器状态：\n%s", strings.TrimSpace(out))
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if line != "" && !strings.Contains(line, "running") {
					return fmt.Errorf("容器未全部运行：%s", line)
				}
			}
			// 认证端点可达（401是正常的：没给CDK）。
			//
			// **要重试**：`docker compose ps` 说 running 不等于在听端口。control-api
			// 启动时要跑数据库迁移，重置之后是一个全新的空库，迁移比平时久；
			// 一次性探测会在"正在迁移"这个正常状态上判失败。
			//
			// **不能用 `curl -s`**：它把失败原因一起吞掉，于是错误只剩一个 `000`
			// ——连不上、TLS 校验失败、超时全都是 000，无法定位。改用 -sS 留下原因，
			// 并把 curl 的退出码一起带出来（60=证书校验失败，7=连不上，28=超时）。
			//
			// 最后仍失败时把现场一次性打出来：解析结果、80 端口有没有人应、
			// 以及 caddy 与 control-api 的近期日志——502 说明 Caddy 好而后端没起来，
			// 000 说明连 Caddy 都没碰到，两者的排查方向完全不同。
			probe := fmt.Sprintf(`fq=%[1]s
last=""
for i in $(seq 1 30); do
  code=$(curl -sS -m 10 -o /dev/null -w '%%{http_code}' -X POST "https://$fq/api/v1/auth/exchange" -H 'Content-Type: application/json' -d '{}' 2>/tmp/ccw-curl.err)
  rc=$?
  case "$code" in 401|429) echo "OK code=$code"; exit 0;; esac
  last="code=$code curl_exit=$rc $(head -1 /tmp/ccw-curl.err)"
  sleep 5
done
echo "探测30次仍未就绪：$last"
echo "--- 域名解析 ---"
getent hosts "$fq" 2>/dev/null || nslookup "$fq" 2>&1 | tail -4 || echo "(解析不到 $fq，且没有 getent/nslookup)"
echo "--- 80端口（绕开TLS，只看有没有人应）---"
curl -sS -m 10 -o /dev/null -w 'http_code=%%{http_code}\n' "http://$fq/api/v1/auth/exchange" 2>&1 | head -2
echo "--- caddy 近期日志 ---"
%[2]sdocker compose -p %[3]s logs caddy --tail=15 2>&1 | tail -15
echo "--- control-api 近期日志 ---"
%[2]sdocker compose -p %[3]s logs control-api --tail=25 2>&1 | tail -25
exit 1`,
				shellQuote(d.FQDN), d.Sudo, shellQuote(d.ComposeProjectName))

			out, perr := run(ctx, d, probe)
			// 无论成败都把输出留在日志里：成功时是一行 OK，失败时是上面那套现场。
			// **先 log 再判错**——错误本身只带得走第一行（run 用 firstLine）。
			if s := strings.TrimSpace(out); s != "" {
				log("%s", s)
			}
			if perr != nil {
				return fmt.Errorf("API端点不可达（现场见上方日志）: %w", perr)
			}
			log("API端点可达")
			return nil
		},
	}
}

// 11) init-projects：建项目并回收CDK明文。
//
// 幂等：ccwadmin init-project对已存在的slug返回现有信息且**不签发新CDK**，
// 因此重跑不会产生多余的CDK。
func stepInitProjects(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "init-projects",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			for _, slug := range d.Slugs {
				// **这一步用RunCapturingSecret而不是run**：CDK明文就打在stdout上，
				// 常规路径会在这里就把它抹成[REDACTED]，管理员再也拿不到。
				// raw只喂给解析器；报错一律用脱敏过的res。
				res, raw, err := d.Exec.RunCapturingSecret(ctx, fmt.Sprintf(
					"cd %s && %sdocker compose -p %s run --rm --entrypoint /ccwadmin control-api init-project --slug %s --json",
					shellQuote(d.deployDir()), d.Sudo, shellQuote(d.ComposeProjectName), shellQuote(slug)))
				if err != nil {
					return fmt.Errorf("建项目%s失败: %w", slug, err)
				}
				if res.ExitCode != 0 {
					return &pipeline.ExitError{
						Code: res.ExitCode,
						Err:  fmt.Errorf("建项目%s失败: %s", slug, firstLine(res.Stderr+res.Stdout)),
					}
				}
				pr := parseInitProjectJSON(raw)
				pr.Slug = slug // 以请求的slug为准，不信回显
				created := pr.Created
				if d.OnProject != nil {
					// 已存在的项目也要回调：它的配额与remote id仍需入Console镜像，
					// 否则重跑一次纳管，镜像就永远停在第一次的值上。
					d.OnProject(pr)
				}
				if created {
					// **日志里绝不出现CDK明文**——它只经OnCDK回调交给浏览器一次。
					log("项目%s已创建，CDK已签发（明文仅在浏览器显示一次）", slug)
				} else {
					log("项目%s已存在，跳过（未签发新CDK）", slug)
				}
			}
			return nil
		},
	}
}

// 12) disk-guard：校验Docker data-root与Postgres数据位置（N4）。
//
// 注意N4的闸门含义与其它步骤不同：完成后**同机项目之间的磁盘互相影响依然存在**，
// 它只把后果从"整机死亡且救不回来"降到"一起降级、机器可救"（§12.3）。
func stepDiskGuard(d Deps) pipeline.Step {
	return pipeline.Step{
		Name: "disk-guard",
		Run: func(ctx context.Context, log pipeline.Logf) error {
			out, err := run(ctx, d, d.Sudo+`docker info --format '{{.DockerRootDir}}' 2>/dev/null; df -h --output=target,pcent / | tail -1`)
			if err != nil {
				return err
			}
			log("Docker data-root与根分区水位：\n%s", strings.TrimSpace(out))
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "/var/lib/docker") {
				// 不是失败——只是把已知取舍如实说清楚（不得表述为"已隔离"）。
				// 本流水线不自动改data-root：那需要机器上真有第二块盘，
				// 且磁盘布局该由管理员决定（设计§12.1的N4第1项，未实施）。
				log("提示：data-root仍在根分区（N4的独立分区未配置）。项目在容器内写盘可撑爆该分区" +
					"并影响同机全部项目；这是当前的已接受取舍，要收敛需手动把data-root指向独立盘。")
			}
			return nil
		},
	}
}

// ProjectResult是init-projects处理完一个slug后的结果，对应
// `ccwadmin init-project --json`的输出。
//
// **CDK是明文**：它只在回调的调用栈里存在，转发给浏览器展示一次之后即丢弃。
// 其余字段都是可以进Console镜像库的信息（§10的node_projects）。
type ProjectResult struct {
	Slug      string `json:"slug"`
	RemoteID  string `json:"project_id"`
	Container string `json:"container"`
	Created   bool   `json:"created"`
	DiskGiB   int64  `json:"disk_gib"`
	FiveHour  int64  `json:"five_hour"`
	SevenDay  int64  `json:"seven_day"`
	PublicID  string `json:"public_id"`
	CDK       string `json:"cdk"`
}

// parseInitProjectJSON从ccwadmin --json输出里取项目信息。
//
// 只截取第一个{到最后一个}再解析：`docker compose run`会在stdout混进
// 「Creating network…」这类噪声，直接Unmarshal整段必然失败。
// 解析失败返回零值而不是报错——调用方靠退出码判断成败，
// 这里失败只意味着镜像信息缺失，不该让一次成功的建项目变成失败。
func parseInitProjectJSON(out string) ProjectResult {
	i := strings.IndexByte(out, '{')
	j := strings.LastIndexByte(out, '}')
	if i < 0 || j <= i {
		return ProjectResult{}
	}
	var pr ProjectResult
	if err := json.Unmarshal([]byte(out[i:j+1]), &pr); err != nil {
		return ProjectResult{}
	}
	return pr
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

// BootstrapStepNames返回 bootstrap 流水线的规范步骤名（按执行顺序）。
//
// 供 Console 的 UI 渲染步骤轨与步骤清单使用：UI 不再自己维护一份步骤列表，
// 因此改流水线时 UI 自动跟上，不会出现「界面上还画着已经删掉的步骤」。
// 用零值 Deps 构造是安全的——步骤构造函数只捕获 d，不解引用它。
func BootstrapStepNames() []string {
	steps := BootstrapSteps(Deps{})
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}
