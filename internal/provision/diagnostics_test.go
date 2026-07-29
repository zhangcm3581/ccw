package provision

import "testing"

// 诊断输出按标记切分；登录状态只在能明确判定时才给布尔。
func TestSplitDiag(t *testing.T) {
	raw := diagMarker + "容器状态\nccw-project-a\tUp 2 hours\tccw-claude\n" +
		diagMarker + "登录状态 ccw-project-a\nloggedIn: true\n" +
		diagMarker + "登录状态 ccw-project-b\nloggedIn: false\n" +
		diagMarker + "磁盘与 data-root\n/dev/vda1  75G  20G  55G  27% /\n"

	secs := splitDiag(raw)
	if len(secs) != 4 {
		t.Fatalf("应切出4段，got %d: %+v", len(secs), secs)
	}
	if secs[0].Title != "容器状态" || !contains(secs[0].Output, "Up 2 hours") {
		t.Errorf("第一段错: %+v", secs[0])
	}
	if secs[1].LoggedIn == nil || !*secs[1].LoggedIn {
		t.Errorf("应判定为已登录: %+v", secs[1])
	}
	if secs[2].LoggedIn == nil || *secs[2].LoggedIn {
		t.Errorf("应判定为未登录: %+v", secs[2])
	}
	if secs[3].LoggedIn != nil {
		t.Error("非登录段不该有登录判定")
	}
}

// **判不出来时必须留空，不能当成未登录**——那会让人白折腾一轮重新授权。
func TestSplitDiagUnknownLoginStaysNil(t *testing.T) {
	raw := diagMarker + "登录状态 ccw-a\nError: command not found\n"
	secs := splitDiag(raw)
	if len(secs) != 1 {
		t.Fatalf("got %d", len(secs))
	}
	if secs[1-1].LoggedIn != nil {
		t.Errorf("输出无法判定时应为nil，got %v", *secs[0].LoggedIn)
	}
	// 原文仍要给出来，让人自己看
	if !contains(secs[0].Output, "command not found") {
		t.Error("原始输出必须保留")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
