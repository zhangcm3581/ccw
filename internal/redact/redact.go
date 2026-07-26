// Package redact在日志与审计写入前脱敏凭据（console-fleet-design §5.4、C5）。
//
// **这是兜底，不是主防线。**主防线是根本不把凭据交给日志：目标服务器密码只存在于
// harden步骤的进程内存、CDK明文只在一次性响应里、节点的CCW_TOKEN_KEY从不经过
// Console（§8.4）。但流水线会把节点上命令的stdout/stderr原样推给浏览器，那里面
// 可能出现任何东西——所以每一条落盘与推流的内容都要先过这里。
//
// 设计取向：**宁可多脱一点，也不能漏**。但过度脱敏会让排障日志失去价值，
// 因此模式都要求"看起来确实像凭据"的形态（足够长、在赋值上下文里、有特征前缀），
// 而不是见到关键词就整行抹掉。
package redact

import "regexp"

// Mask是替换后的占位符；审计与日志里出现它即表示此处曾有凭据。
const Mask = "[REDACTED]"

// 说明：所有模式都用子组保留"键名"部分，只替换值——
// 日志里保留`POSTGRES_PASSWORD=[REDACTED]`比整行消失更有排障价值。
var patterns = []*regexp.Regexp{
	// 1. CDK明文：ccw_<public-id>.<secret>。只抹secret，保留public-id——
	//    public-id本就是可公开的对账凭据（设计§6.6），留着能对上是哪张CDK。
	regexp.MustCompile(`(ccw_[0-9a-fA-F]{8,32})\.[A-Za-z0-9+/=_-]{16,}`),

	// 2. 私钥块：PEM与OpenSSH，整块替换（含RSA/EC/OPENSSH/PRIVATE KEY各变体）。
	regexp.MustCompile(`(?s)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----).*?(-----END [A-Z0-9 ]*PRIVATE KEY-----)`),

	// 3. AWS AccessKey ID：AKIA/ASIA/ABIA/ACCA + 16位大写字母数字，是明确特征串。
	regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`),

	// 4. 键值形态的凭据：key/secret/password/token/passwd/pwd + 分隔符 + 值。
	//    覆盖 KEY=v、"key": "v"、key: v 等写法。
	//    值取到空白/引号/逗号为止；至少8个字符，避免把 password: yes 这类误伤。
	regexp.MustCompile(`(?i)((?:aws_secret_access_key|aws_access_key_id|secret_access_key|` +
		`ccw_token_key|ccw_secret_key|token_key|secret_key|api_key|access_key|` +
		`password|passwd|pwd|secret|token)["']?\s*[:=]\s*["']?)[^\s"',;)]{8,}`),

	// 4b. 命令行参数形态：--password v / -p v / --token=v。
	//     单独一条是因为分隔符是空白，与上面的[:=]分开写更不易误伤。
	regexp.MustCompile(`(?i)(--?(?:password|passwd|pwd|secret|token|api-key|access-key)[= ])[^\s"',;)]{6,}`),

	// 5. 连接串里的密码：scheme://user:password@host
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]+:)[^@/\s]+(@)`),

	// 6. 裸的64位hex（CCW_TOKEN_KEY/CCW_SECRET_KEY的形态）。
	//    注意排除sha256:前缀的摘要——那是镜像digest，日志里需要保留。
	regexp.MustCompile(`(^|[^:a-zA-Z0-9])([0-9a-f]{64})\b`),

	// 7. 管道给sudo -S的密码：echo 'xxx' | sudo -S
	regexp.MustCompile(`(?i)(echo\s+["']?)[^"'|]{6,}(["']?\s*\|\s*sudo\s+-S)`),
}

// 各模式的替换模板：保留键名/边界，只抹值。索引与patterns一一对应。
var replacements = []string{
	"$1." + Mask,
	"$1" + Mask + "$2",
	Mask,
	"$1" + Mask,
	"$1" + Mask,
	"$1" + Mask + "$2",
	"$1" + Mask,
	"$1" + Mask + "$2",
}

// String脱敏一段文本（可含多行；干净的行原样保留）。
func String(s string) string {
	for i, re := range patterns {
		s = re.ReplaceAllString(s, replacements[i])
	}
	return s
}

// Bytes是流式推流与写盘用的入口，语义与String一致。
func Bytes(b []byte) []byte {
	return []byte(String(string(b)))
}
