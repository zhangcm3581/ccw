package redact

import (
	"strings"
	"testing"
)

// C5日志脱敏（console-fleet-design §5.4、§12.1的C5）：
// 设计明确要求"独立单测，覆盖密码/私钥/CDK/token/AccessKey五类"。
//
// 脱敏是**兜底**不是主防线：主防线是根本不把凭据传给日志（如目标服务器密码只在
// 内存里）。但流水线要把节点上的命令输出原样推流给浏览器，那些输出里可能出现
// 任何东西，因此必须有这道过滤。

const mask = "[REDACTED]"

func mustRedact(t *testing.T, in string, secrets ...string) string {
	t.Helper()
	out := String(in)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("未脱敏 %q\n输入: %s\n输出: %s", s, in, out)
		}
	}
	if !strings.Contains(out, mask) {
		t.Errorf("输出应含%s标记\n输入: %s\n输出: %s", mask, in, out)
	}
	return out
}

// 一类：CDK明文（ccw_<16hex>.<secret>）。
func TestRedactsCDK(t *testing.T) {
	secret := "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"
	cases := []string{
		"CDK (显示一次): ccw_a1b2c3d4e5f60718." + secret,
		`{"cdk":"ccw_a1b2c3d4e5f60718.` + secret + `"}`,
		"curl -d '{\"cdk\":\"ccw_a1b2c3d4e5f60718." + secret + "\"}' https://x/api",
	}
	for _, c := range cases {
		out := mustRedact(t, c, secret)
		// public-id部分可以保留（它本来就是可公开的对账凭据，设计§6.6），
		// 但secret必须消失。
		if !strings.Contains(out, "ccw_a1b2c3d4e5f60718") {
			t.Logf("注意：public-id也被一并脱敏了（可接受，但对账时看不到）：%s", out)
		}
	}
}

// 二类：私钥（PEM/OpenSSH块）。
func TestRedactsPrivateKey(t *testing.T) {
	body := "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW"
	in := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + body + "\nQyNTUxOQAAACC\n-----END OPENSSH PRIVATE KEY-----"
	mustRedact(t, in, body)

	in2 := "-----BEGIN RSA PRIVATE KEY-----\nMIIEow" + body + "\n-----END RSA PRIVATE KEY-----"
	mustRedact(t, in2, body)

	// 公钥不该被脱敏：harden步骤要把公钥打进日志供核对。
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample ccw-console"
	if got := String(pub); got != pub {
		t.Errorf("公钥不应被脱敏: %s", got)
	}
}

// 三类：令牌密钥（CCW_TOKEN_KEY等64位hex）与环境变量赋值形态。
func TestRedactsTokenKeyAndEnvAssignments(t *testing.T) {
	key := "4f3e2d1c0b9a8776655443322110ffeeddccbbaa99887766554433221100ffee"
	mustRedact(t, "CCW_TOKEN_KEY="+key, key)
	mustRedact(t, "生成密钥: "+key+" 完成", key)
	mustRedact(t, `CCW_SECRET_KEY: "`+key+`"`, key)
}

// 四类：密码（各种赋值写法，含sudo -S与psql连接串）。
func TestRedactsPasswords(t *testing.T) {
	pw := "S3cr3t-P@ssw0rd-Value"
	mustRedact(t, "POSTGRES_PASSWORD="+pw, pw)
	mustRedact(t, "CONSOLE_POSTGRES_PASSWORD: "+pw, pw)
	mustRedact(t, `{"password":"`+pw+`"}`, pw)
	mustRedact(t, "echo '"+pw+"' | sudo -S true", pw)
	mustRedact(t, "postgres://ccw:"+pw+"@postgres:5432/ccw", pw)
	mustRedact(t, "ssh --password "+pw+" host", pw)
}

// 五类：云厂商AccessKey（AKIA...与secret access key）。
func TestRedactsAWSCredentials(t *testing.T) {
	akid := "AKIAIOSFODNN7EXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	mustRedact(t, "aws_access_key_id = "+akid, akid)
	mustRedact(t, "AWS_SECRET_ACCESS_KEY="+secret, secret)
	mustRedact(t, `{"aws_access_key_id":"`+akid+`","aws_secret_access_key":"`+secret+`"}`, akid, secret)
}

// 正常日志不得被误伤：脱敏过度会让流水线日志失去排障价值。
func TestKeepsOrdinaryOutput(t *testing.T) {
	ordinary := []string{
		"docker compose up -d",
		"Container ccw-project-a  Started",
		"project created: slug=project-a id=3f2a1b0c-1111-2222-3333-444455556666 container=ccw-project-a",
		"disk=15GiB five_hour=1000000 seven_day=10000000",
		"api-03.example.com A 203.0.113.7 (TTL 60)",
		"go1.22.5 linux/amd64",
		"sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20",
		"HTTP/1.1 200 OK",
		"public_id: a1b2c3d4e5f60718",
	}
	for _, s := range ordinary {
		if got := String(s); got != s {
			t.Errorf("正常输出被误伤：\n输入: %s\n输出: %s", s, got)
		}
	}
}

// 多行流式输出：逐行处理，含密文的行被替换、其余行保持原样。
func TestMultilineStreamKeepsCleanLines(t *testing.T) {
	pw := "SuperSecret123"
	in := "step: render-env\nPOSTGRES_PASSWORD=" + pw + "\n.env written (0600)\nstep: compose-up"
	out := String(in)
	if strings.Contains(out, pw) {
		t.Fatal("密码未脱敏")
	}
	for _, keep := range []string{"step: render-env", ".env written (0600)", "step: compose-up"} {
		if !strings.Contains(out, keep) {
			t.Errorf("干净的行被丢失: %q\n输出: %s", keep, out)
		}
	}
	if n := strings.Count(out, "\n"); n != strings.Count(in, "\n") {
		t.Errorf("行数改变: in=%d out=%d", strings.Count(in, "\n"), n)
	}
}

// Bytes是流式推流用的入口，语义与String一致。
func TestBytes(t *testing.T) {
	pw := "AnotherSecret"
	got := string(Bytes([]byte("password=" + pw)))
	if strings.Contains(got, pw) {
		t.Errorf("Bytes未脱敏: %s", got)
	}
}
