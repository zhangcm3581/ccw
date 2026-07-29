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
