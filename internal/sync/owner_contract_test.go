package sync

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// ContainerUID 必须与 deploy/Dockerfile.claude 里的 useradd -u 保持一致。
//
// 这两个数字必须相同：worker-agent 按 ContainerUID chown，容器按 Dockerfile 的
// UID 运行。漂了的表现是"文件同步上去了，容器里读不了"——**而且不报任何错**，
// 只能靠用户抱怨才发现。所以这里直接读 Dockerfile 比对，而不是靠注释提醒。
func TestContainerUIDMatchesDockerfile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/Dockerfile.claude")
	if err != nil {
		t.Fatalf("读不到 Dockerfile.claude：%v", err)
	}
	m := regexp.MustCompile(`useradd\s+[^\n]*-u\s+(\d+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("Dockerfile.claude 里找不到 `useradd … -u <uid>`；" +
			"若改了创建用户的方式，请同时更新 ContainerUID 与本测试")
	}
	uid, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if uid != ContainerUID {
		t.Errorf("Dockerfile.claude 的 uid=%d，而 ContainerUID=%d。"+
			"两者必须一致，否则同步上去的文件容器里读不了（且不报错）", uid, ContainerUID)
	}
}
