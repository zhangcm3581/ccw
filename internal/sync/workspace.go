package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

// 工作区隔离（2026-07-29定）。
//
// **要解决的问题**：一个项目的云端workspace此前是一个平铺目录，客户端在本地任意
// 目录里跑都映射到同一处。于是在 ~/code 下用过一次，再进 ~/work 跑，
// 云端会把 code 的全部文件同步下来——两个毫不相干的本地目录互相污染。
//
// **解法**：客户端按本地目录算出一个工作区键，云端按键分层：
//
//	<workspace-root>/<slug>/<ws>/...     文件
//	file_index.path = "<ws>/相对路径"      索引（因此不需要改表结构）
//	tmux 会话名 = <ws>，工作目录 = /workspace/<ws>
//
// 键 = 可读的目录名 + 绝对路径哈希前8位。两段都必要：
//   - 目录名让管理员在服务器上一眼看出这是哪个文件夹
//   - 哈希区分 ~/a/code 与 ~/b/code——只用目录名它们会撞在一起，
//     那正是本次要修的那个 bug 的另一种形态
//
// 键**不含设备名**：同一个人在笔记本与台式机上的同名路径应当是同一个工作区，
// 那本来就是"本地目录与云端双向同步"想要的效果。

// wsMaxName是键里可读部分的长度上限。够认人即可，太长会让路径难看。
const wsMaxName = 24

var wsUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// WorkspaceKey按本地绝对路径算出工作区键。
//
// 入参应当是清理过的绝对路径；调用方拿到的若是相对路径，先自行Abs——
// 这里不做Abs，是为了让函数保持纯函数、可测。
func WorkspaceKey(absDir string) string {
	clean := filepath.Clean(absDir)
	sum := sha256.Sum256([]byte(clean))
	short := hex.EncodeToString(sum[:])[:8]

	name := strings.ToLower(filepath.Base(clean))
	name = wsUnsafe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > wsMaxName {
		name = strings.Trim(name[:wsMaxName], "-")
	}
	if name == "" {
		// 根目录、驱动器根、或整串都是特殊字符时只留哈希。
		return "ws-" + short
	}
	return name + "-" + short
}

// validWS是服务端对工作区键的校验：只接受本函数生成的形状。
//
// **这是路径安全边界的一部分**：键会成为服务器上的目录名与索引路径的第一段，
// 放行 ".." 或 "/" 等于让客户端指定任意路径。与internal/sync/paths.go的
// 排除名单同级，改动前先想清楚。
var validWS = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}[a-z0-9]$`)

// ValidWorkspace判断客户端提交的工作区键是否可接受。
func ValidWorkspace(ws string) bool {
	if !validWS.MatchString(ws) {
		return false
	}
	// 连续连字符不是本函数会生成的形状，也没有正当用途；一并拒绝，
	// 缩小服务端要考虑的输入空间。
	return !strings.Contains(ws, "--")
}

// wsHashSuffix匹配键尾部的8位哈希。
var wsHashSuffix = regexp.MustCompile(`-[0-9a-f]{8}$`)

// DisplayName把工作区键还原成可读的文件夹名：og-vault-94137d17 → og-vault。
//
// 哈希是给机器用的（区分 ~/a/code 与 ~/b/code），摆在界面上只是噪音。
//
// **只在唯一时才省**：调用方必须用 DisplayNames 处理整批，因为去掉哈希之后
// 两个同名文件夹会变得完全一样——而这块界面是用来选删除对象的，
// 分不清就等于让人蒙着眼睛删。
//
// 键只剩哈希（本地目录名全是非 ASCII 字符时会这样）时原样返回：
// 还原出来的 "ws" 什么也没说明。
func DisplayName(ws string) string {
	if strings.HasPrefix(ws, "ws-") && wsHashSuffix.MatchString(ws) && len(ws) == 11 {
		return ws
	}
	if s := wsHashSuffix.ReplaceAllString(ws, ""); s != "" {
		return s
	}
	return ws
}

// DisplayNames给一批键算显示名：能省则省，**重名的保留哈希**。
func DisplayNames(keys []string) map[string]string {
	count := map[string]int{}
	for _, k := range keys {
		count[DisplayName(k)]++
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if n := DisplayName(k); count[n] == 1 {
			out[k] = n
		} else {
			out[k] = k // 撞名了：留着哈希，否则无法分辨该删哪个
		}
	}
	return out
}
