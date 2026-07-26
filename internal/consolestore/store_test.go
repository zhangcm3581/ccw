package consolestore

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Console库集成测试：复用节点库测试的PG（CCW_TEST_DATABASE_URL），
// 但在同一实例上使用**独立数据库**ccw_console_test——生产形态就是两个库，
// 测试不把两套schema混进同一个库。无PG时skip（CLAUDE.md：没验证如实显示为skip）。
func testConsoleStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("CCW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("需要PostgreSQL：设置 CCW_TEST_DATABASE_URL=postgres://... 后重跑")
	}
	ctx := context.Background()
	base, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接基础库失败: %v", err)
	}
	// CREATE DATABASE不能在事务里；已存在时报错可忽略。
	_, _ = base.Pool.Exec(ctx, `CREATE DATABASE ccw_console_test`)
	base.Pool.Close()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析DSN失败: %v", err)
	}
	u.Path = "/ccw_console_test"
	st, err := New(ctx, u.String())
	if err != nil {
		t.Fatalf("连接console测试库失败: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	t.Cleanup(func() { st.Pool.Close() })
	return st
}

func TestMigrateIdempotent(t *testing.T) {
	st := testConsoleStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("二次Migrate应无副作用: %v", err)
	}
}

func art(version, osName, arch string) Artifact {
	name := "cclaude_" + version + "_" + osName + "_" + arch
	if osName == "windows" {
		name += ".exe"
	}
	return Artifact{Version: version, OS: osName, Arch: arch, Filename: name, SizeBytes: 1024, SHA256: strings.Repeat("ab", 32)}
}

func cleanupRelease(t *testing.T, st *Store, versions ...string) {
	t.Cleanup(func() {
		ctx := context.Background()
		for _, v := range versions {
			st.Pool.Exec(ctx, `DELETE FROM releases WHERE version=$1`, v) // artifacts级联
		}
	})
}

// 发布语义：未publish的版本对下载面完全不可见（设计§3.2：下载页从表渲染、
// 不扫目录，避免半成品被下载）。
func TestRegisterPublishLatest(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	v1 := "t1-" + uuid.NewString()[:8]
	v2 := "t2-" + uuid.NewString()[:8]
	cleanupRelease(t, st, v1, v2)

	if err := st.RegisterRelease(ctx, v1, "第一版", []Artifact{art(v1, "linux", "amd64"), art(v1, "windows", "amd64")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LatestPublished(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未发布时LatestPublished应为ErrNotFound，got %v", err)
	}
	if err := st.Publish(ctx, v1); err != nil {
		t.Fatal(err)
	}
	rel, arts, err := st.LatestPublished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != v1 || len(arts) != 2 {
		t.Fatalf("LatestPublished错误: %+v %d个产物", rel, len(arts))
	}
	if rel.PublishedAt == nil {
		t.Error("published_at应已写入")
	}

	// 注册更新的版本但不发布：Latest仍是v1。
	if err := st.RegisterRelease(ctx, v2, "", []Artifact{art(v2, "linux", "amd64")}); err != nil {
		t.Fatal(err)
	}
	rel, _, _ = st.LatestPublished(ctx)
	if rel.Version != v1 {
		t.Errorf("未发布的v2不应成为Latest，got %s", rel.Version)
	}
	if err := st.Publish(ctx, v2); err != nil {
		t.Fatal(err)
	}
	rel, _, _ = st.LatestPublished(ctx)
	if rel.Version != v2 {
		t.Errorf("发布后Latest应为v2，got %s", rel.Version)
	}
}

// 重复注册＝重建产物集合（重新构建后sha变化）；published_at不被注册动作清掉。
func TestRegisterIsRebuildUpsert(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	v := "t3-" + uuid.NewString()[:8]
	cleanupRelease(t, st, v)

	if err := st.RegisterRelease(ctx, v, "", []Artifact{art(v, "linux", "amd64"), art(v, "darwin", "arm64")}); err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(ctx, v); err != nil {
		t.Fatal(err)
	}
	// 重新构建：只剩一个产物、sha变化。
	a := art(v, "linux", "amd64")
	a.SHA256 = strings.Repeat("cd", 32)
	if err := st.RegisterRelease(ctx, v, "重建", []Artifact{a}); err != nil {
		t.Fatal(err)
	}
	rel, arts, err := st.LatestPublished(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != v || rel.PublishedAt == nil {
		t.Errorf("重注册不应清掉published_at: %+v", rel)
	}
	if len(arts) != 1 || arts[0].SHA256 != strings.Repeat("cd", 32) {
		t.Errorf("产物集合应被重建: %+v", arts)
	}

	// Publish幂等：重复publish不报错、不改时间。
	first := *rel.PublishedAt
	if err := st.Publish(ctx, v); err != nil {
		t.Fatal(err)
	}
	rel2, _, _ := st.LatestPublished(ctx)
	if !rel2.PublishedAt.Equal(first) {
		t.Error("重复Publish不应改变published_at")
	}
}

func TestPublishUnknownVersion(t *testing.T) {
	st := testConsoleStore(t)
	if err := st.Publish(context.Background(), "no-such-version"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知版本Publish应返回ErrNotFound，got %v", err)
	}
}

// /dist只发已发布版本的产物：未发布或未知文件名一律ErrNotFound。
func TestArtifactByFilename(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()
	vPub := "t4-" + uuid.NewString()[:8]
	vDraft := "t5-" + uuid.NewString()[:8]
	cleanupRelease(t, st, vPub, vDraft)

	pub := art(vPub, "linux", "amd64")
	draft := art(vDraft, "linux", "amd64")
	if err := st.RegisterRelease(ctx, vPub, "", []Artifact{pub}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRelease(ctx, vDraft, "", []Artifact{draft}); err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(ctx, vPub); err != nil {
		t.Fatal(err)
	}

	got, err := st.ArtifactByFilename(ctx, pub.Filename)
	if err != nil || got.SHA256 != pub.SHA256 {
		t.Errorf("已发布产物应可查: %+v %v", got, err)
	}
	if _, err := st.ArtifactByFilename(ctx, draft.Filename); !errors.Is(err, ErrNotFound) {
		t.Errorf("未发布产物不得可下载（半成品保护），got %v", err)
	}
	if _, err := st.ArtifactByFilename(ctx, "cclaude_ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知文件名应ErrNotFound，got %v", err)
	}
}

// /connect解析链：cdk_issues → node_projects → nodes → node_domains（设计§10）。
func TestResolveAPIDomain(t *testing.T) {
	st := testConsoleStore(t)
	ctx := context.Background()

	zoneID, nodeID := uuid.NewString(), uuid.NewString()
	npID := uuid.NewString()
	pubActive, pubRevoked := "aaaa000000000001", "aaaa000000000002"
	suffix := uuid.NewString()[:8]

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO dns_zones (id, domain, provider) VALUES ($1, $2, 'manual')`,
		zoneID, suffix+".example.com")
	mustExec(`INSERT INTO nodes (id, name, host, ssh_user, status) VALUES ($1, $2, '203.0.113.7', 'ccw', 'ready')`,
		nodeID, "node-"+suffix)
	mustExec(`INSERT INTO node_domains (id, zone_id, seq, fqdn, node_id, target_ip, record_state)
	          VALUES ($1, $2, 3, $3, $4, '203.0.113.7', 'insync')`,
		uuid.NewString(), zoneID, "api-03."+suffix+".example.com", nodeID)
	mustExec(`INSERT INTO node_projects (id, node_id, slug, remote_project_id, disk_limit_bytes, five_hour_limit, seven_day_limit)
	          VALUES ($1, $2, 'proj-x', $3, 1, 1, 1)`, npID, nodeID, uuid.NewString())
	mustExec(`INSERT INTO cdk_issues (id, node_project_id, public_id) VALUES ($1, $2, $3)`,
		uuid.NewString(), npID, pubActive)
	mustExec(`INSERT INTO cdk_issues (id, node_project_id, public_id, revoked_at) VALUES ($1, $2, $3, now())`,
		uuid.NewString(), npID, pubRevoked)
	t.Cleanup(func() {
		st.Pool.Exec(ctx, `DELETE FROM nodes WHERE id=$1`, nodeID) // node_projects/cdk_issues级联
		st.Pool.Exec(ctx, `DELETE FROM node_domains WHERE zone_id=$1`, zoneID)
		st.Pool.Exec(ctx, `DELETE FROM dns_zones WHERE id=$1`, zoneID)
	})

	fqdn, err := st.ResolveAPIDomain(ctx, pubActive)
	if err != nil {
		t.Fatalf("有效public_id应解析出域名: %v", err)
	}
	if fqdn != "api-03."+suffix+".example.com" {
		t.Errorf("fqdn=%s", fqdn)
	}
	// 已撤销/未知：统一ErrNotFound——查询页据此返回「未找到」，不区分原因（设计§6.6）。
	if _, err := st.ResolveAPIDomain(ctx, pubRevoked); !errors.Is(err, ErrNotFound) {
		t.Errorf("已撤销应ErrNotFound，got %v", err)
	}
	if _, err := st.ResolveAPIDomain(ctx, "ffffffffffffffff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知应ErrNotFound，got %v", err)
	}
}
