package e2e

import (
	"os/exec"
	"testing"
)

// e2e 在目标 VPS（Docker + PostgreSQL + 已部署服务）上运行；无 Docker 自动跳过。
// 这些子测试是端到端验收清单，覆盖 spec 第13节场景。
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available; e2e runs on the target VPS")
	}
}

func TestTwoProjectsEndToEnd(t *testing.T) {
	requireDocker(t)
	t.Run("bootstrap", testBootstrap)
	t.Run("cdk_isolation", testCDKIsolation)
	t.Run("sync_local_to_cloud", testSyncLocalToCloud)
	t.Run("sync_cloud_to_local", testSyncCloudToLocal)
	t.Run("sync_conflict", testSyncConflict)
	t.Run("disk_quota_reject", testDiskQuotaReject)
	t.Run("hard_quota_isolation", testHardQuotaIsolation)
	t.Run("terminal_reconnect", testTerminalReconnect)
	t.Run("quota_closes_terminal", testQuotaClosesTerminal)
	t.Run("no_secret_leak", testNoSecretLeak)
}

// 断言在 VPS 联调时填充（用 docker/curl/tmux 驱动真实环境）。
// 未实现的一律 Skip，避免把"没验证"误报成"通过"。
func testBootstrap(t *testing.T) { t.Skip("VPS: compose up + migrate + ccwadmin init-project A/B") }
func testCDKIsolation(t *testing.T) {
	t.Skip("VPS: CDK-A 只能连 A；伪造 B 的 project_id 被拒")
}
func testSyncLocalToCloud(t *testing.T) {
	t.Skip("VPS: 本地建文件 → cclaude → 容器 /workspace 出现该文件")
}
func testSyncCloudToLocal(t *testing.T) {
	t.Skip("VPS: 容器内改文件 → reconcile → 客户端拉回本地")
}
func testSyncConflict(t *testing.T) {
	t.Skip("VPS: 双端同改 → 生成 .conflict-remote 副本且不覆盖本地")
}
func testDiskQuotaReject(t *testing.T) {
	t.Skip("VPS: 超逻辑配额的 put 被 reject；删除/缩小仍允许")
}
func testHardQuotaIsolation(t *testing.T) {
	t.Skip("VPS: 容器内 dd 写满 A 的 loop fs，B 与宿主机不受影响")
}
func testTerminalReconnect(t *testing.T) {
	t.Skip("VPS: 断开 WebSocket 重连后 tmux 会话仍在、可见断开前输出")
}
func testQuotaClosesTerminal(t *testing.T) {
	t.Skip("VPS: 灌 A 用量超限 → 30秒内 A 的已连接终端被关闭，B 不受影响")
}
func testNoSecretLeak(t *testing.T) {
	t.Skip("VPS: 服务与反代日志中无 ccw_ 明文、无 OAuth token")
}
