package provision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"ccw/internal/consolestore"
	"ccw/internal/dns"
	"ccw/internal/pipeline"
	"ccw/internal/secretbox"
	"ccw/internal/sshexec"
)

// Orchestrator把纳管的各环节串起来（console-fleet-design §5）：
// 连接 → 凭据交接 → 域名分配 → 12步流水线。
//
// 它是唯一同时接触**首登密码**与**托管私钥明文**的地方；两者都只在本函数的
// 调用栈里存在：密码用完即弃（不落库、不进日志），私钥验证通过后立即信封加密落库。
type Orchestrator struct {
	Store  Store
	Box    *secretbox.Box
	DNS    dns.Provider
	Dial   Dialer
	Log    func(runID, line string)
	Finish func(runID string)

	// Artifacts返回要推送到节点的编排文件（compose.yaml等）。
	Artifacts func(slugs []string) (map[string]string, error)
	// ArtifactDir与ComposeProjectName是节点上的落点。
	ArtifactDir        string
	ComposeProjectName string
	// StepTimeout是单步上限（compose-up要构建镜像，需要足够长）。
	StepTimeout time.Duration
}

// Store是编排需要的持久化能力面。
type Store interface {
	GetNode(ctx context.Context, id string) (consolestore.Node, error)
	SetNodeStatus(ctx context.Context, id, status string) error
	SetNodeHostKey(ctx context.Context, id, fingerprint string) error
	SetNodeFacts(ctx context.Context, id, osRelease, arch string) error
	SaveNodeCredential(ctx context.Context, nodeID string, privEnc, nonce []byte, publicKey string) error
	NodeCredential(ctx context.Context, nodeID string) (privEnc, nonce []byte, publicKey string, err error)

	GetZone(ctx context.Context, id string) (dns.Zone, error)
	NextSeq(ctx context.Context, zoneID string) (int, error)
	DomainTaken(ctx context.Context, fqdn string) (bool, error)
	AllocateDomain(ctx context.Context, zoneID string, seq int, fqdn, nodeID, targetIP string) (string, error)
	DomainByNode(ctx context.Context, nodeID string) (consolestore.NodeDomain, error)
	MarkDomainInSync(ctx context.Context, fqdn string) error

	CreateRun(ctx context.Context, nodeID, kind, triggeredBy string) (string, error)
	pipeline.Recorder
}

// nodeCredContext是托管私钥信封加密的AAD用途标签。
const nodeCredContext = "node-cred"

// BootstrapInput是一次纳管的输入。**Password只在本次调用中存在**。
type BootstrapInput struct {
	NodeID      string
	ZoneID      string
	Password    string // 首登密码；用完即弃，永不落库（§8.4）
	Slugs       []string
	TriggeredBy string
}

// Bootstrap执行完整纳管。返回runID供UI订阅日志。
//
// 返回后Password应由调用方清除（ZeroString）。本函数不把它写进任何持久化对象。
func (o *Orchestrator) Bootstrap(ctx context.Context, in BootstrapInput) (runID string, err error) {
	runID, err = o.Store.CreateRun(ctx, in.NodeID, "bootstrap", in.TriggeredBy)
	if err != nil {
		return "", err
	}
	log := func(format string, a ...any) {
		if o.Log != nil {
			o.Log(runID, fmt.Sprintf(format, a...))
		}
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// 纳管跑在后台goroutine里：未recover的panic会终止整个Console进程。
				log("纳管异常终止：%v", r)
				o.Store.SetNodeStatus(context.Background(), in.NodeID, "degraded")
			}
			if o.Finish != nil {
				o.Finish(runID)
			}
		}()
		bctx := context.WithoutCancel(ctx) // HTTP请求结束不应中断已开始的纳管
		if err := o.runBootstrap(bctx, runID, in, log); err != nil {
			log("纳管失败：%v", err)
			o.Store.SetNodeStatus(bctx, in.NodeID, "degraded")
			return
		}
		o.Store.SetNodeStatus(bctx, in.NodeID, "ready")
		log("纳管完成，节点已就绪")
	}()
	return runID, nil
}

func (o *Orchestrator) runBootstrap(ctx context.Context, runID string, in BootstrapInput,
	log func(string, ...any)) error {
	node, err := o.Store.GetNode(ctx, in.NodeID)
	if err != nil {
		return err
	}
	if err := o.Store.SetNodeStatus(ctx, in.NodeID, "provisioning"); err != nil {
		return err
	}

	// ---- connect：首登并固定host key（TOFU，§5.2）----
	target := sshexec.Target{Host: node.Host, Port: node.SSHPort, User: node.SSHUser}
	if node.HostKeyFP != nil {
		target.KnownFingerprint = *node.HostKeyFP
	}
	cli, sudo, err := o.connect(ctx, node, target, in.Password, log)
	if err != nil {
		if errors.Is(err, sshexec.ErrHostKeyChanged) {
			// 指纹变了：可能是重装、也可能是中间人。中止并标记，需管理员显式确认（A25）。
			o.Store.SetNodeStatus(ctx, in.NodeID, "host_key_changed")
			return fmt.Errorf("host key与记录不符，已中止（可能是重装或中间人攻击）：%w", err)
		}
		return err
	}
	defer cli.Close()

	// ---- harden：凭据交接（仅在尚无托管密钥时执行）----
	if _, _, _, cerr := o.Store.NodeCredential(ctx, in.NodeID); errors.Is(cerr, consolestore.ErrNotFound) {
		log("生成托管密钥并注入节点")
		hr, err := Harden(ctx, cli, o.Dial, target, in.Password, log)
		if err != nil {
			return err
		}
		enc, nonce, err := o.Box.Seal(hr.KeyPair.PrivatePEM, nodeCredContext)
		if err != nil {
			return err
		}
		// **验证通过之后才落库**（§9）
		if err := o.Store.SaveNodeCredential(ctx, in.NodeID, enc, nonce, hr.KeyPair.AuthorizedKey); err != nil {
			return err
		}
		log("托管密钥已加密保存；首登密码不再需要，也从未落库")
	} else if cerr != nil {
		return cerr
	} else {
		log("已有托管密钥，跳过凭据交接")
	}

	// ---- 域名分配（幂等：已分配则复用）----
	zone, err := o.Store.GetZone(ctx, in.ZoneID)
	if err != nil {
		return err
	}
	fqdn, err := o.ensureDomain(ctx, in.NodeID, node.Host, zone, log)
	if err != nil {
		return err
	}

	// ---- 12步流水线 ----
	artifacts, err := o.Artifacts(in.Slugs)
	if err != nil {
		return err
	}
	deps := Deps{
		Exec: cli, Sudo: sudo, DNS: o.DNS, Zone: zone, FQDN: fqdn,
		PublicIP: node.Host, Slugs: in.Slugs,
		ArtifactDir: o.ArtifactDir, Artifacts: artifacts,
		ComposeProjectName: o.ComposeProjectName,
		OnHostFacts: func(osRelease, arch string) {
			o.Store.SetNodeFacts(ctx, in.NodeID, osRelease, arch)
		},
		OnCDK: func(slug, cdk, publicID string) {
			// **CDK明文不进日志、不入Console库**（§8.4）。这里只记public_id
			// 供对账；明文的交付路径是节点上的一次性输出，管理员从运行详情页
			// 的CDK区取（C17实施前，需从节点日志人工取回）。
			log("项目%s已签发CDK（public_id=%s；明文见节点侧输出，不经Console存储）", slug, publicID)
		},
	}
	runner := &pipeline.Runner{
		Recorder:       o.Store,
		Log:            func(format string, a ...any) { log(format, a...) },
		DefaultTimeout: o.stepTimeout(),
	}
	if err := runner.Run(ctx, runID, BootstrapSteps(deps)); err != nil {
		return err
	}
	if err := o.Store.MarkDomainInSync(ctx, fqdn); err != nil {
		return err
	}
	return nil
}

// connect优先用托管密钥；没有时用首登密码。返回可用的client与sudo前缀。
func (o *Orchestrator) connect(ctx context.Context, node consolestore.Node, target sshexec.Target,
	password string, log func(string, ...any)) (*sshexec.Client, string, error) {
	var auth []ssh.AuthMethod
	if privEnc, nonce, _, err := o.Store.NodeCredential(ctx, node.ID); err == nil {
		priv, derr := o.Box.Open(privEnc, nonce, nodeCredContext)
		if derr != nil {
			return nil, "", fmt.Errorf("托管私钥无法解密（CCW_SECRET_KEY是否变过？）")
		}
		m, kerr := sshexec.AuthFromPrivateKey(priv)
		if kerr != nil {
			return nil, "", kerr
		}
		auth = append(auth, m)
		log("用托管密钥连接 %s@%s", node.SSHUser, node.Host)
	} else if password != "" {
		auth = append(auth, ssh.Password(password))
		log("用密码首次连接 %s@%s", node.SSHUser, node.Host)
	} else {
		return nil, "", errors.New("既无托管密钥也未提供密码")
	}

	cli, err := o.Dial(ctx, target, auth)
	if err != nil {
		return nil, "", err
	}
	// TOFU：首次连接后固定指纹供后续校验与带外核对
	if node.HostKeyFP == nil {
		if err := o.Store.SetNodeHostKey(ctx, node.ID, cli.Fingerprint); err != nil {
			cli.Close()
			return nil, "", err
		}
		log("已记录host key指纹（请带外核对）：%s", cli.Fingerprint)
	}

	needPw, err := DetectSudo(ctx, cli, node.SSHUser, password)
	if err != nil {
		cli.Close()
		return nil, "", err
	}
	sudoPw := ""
	if needPw {
		sudoPw = password
	}
	return cli, SudoPrefix(node.SSHUser, sudoPw), nil
}

// ensureDomain分配（或复用）子域名。序号永不回收（§6.2）。
func (o *Orchestrator) ensureDomain(ctx context.Context, nodeID, ip string, zone dns.Zone,
	log func(string, ...any)) (string, error) {
	if d, err := o.Store.DomainByNode(ctx, nodeID); err == nil {
		log("复用已分配的域名 %s", d.FQDN)
		return d.FQDN, nil
	} else if !errors.Is(err, consolestore.ErrNotFound) {
		return "", err
	}
	taken := func(fqdn string) bool {
		ok, err := o.Store.DomainTaken(ctx, fqdn)
		return err != nil || ok // 查询失败按"已占用"处理，宁可跳号也不撞名
	}
	seq, fqdn, err := dns.Allocate(ctx, seqAllocFunc(o.Store.NextSeq), zone, taken)
	if err != nil {
		return "", err
	}
	if _, err := o.Store.AllocateDomain(ctx, zone.ID, seq, fqdn, nodeID, ip); err != nil {
		return "", err
	}
	log("已分配域名 %s（序号%d，永不回收）", fqdn, seq)
	return fqdn, nil
}

func (o *Orchestrator) stepTimeout() time.Duration {
	if o.StepTimeout > 0 {
		return o.StepTimeout
	}
	return 20 * time.Minute // compose-up要在节点上构建镜像
}

// seqAllocFunc把方法适配成dns.SeqAllocator。
type seqAllocFunc func(ctx context.Context, zoneID string) (int, error)

func (f seqAllocFunc) NextSeq(ctx context.Context, zoneID string) (int, error) {
	return f(ctx, zoneID)
}
