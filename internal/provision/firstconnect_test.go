package provision

import (
	"strings"
	"testing"

	"ccw/internal/sshexec"
)

// 首次连接的凭据可以是密码、私钥，或两者都给。
//
// 背景：主流云厂商的默认镜像出厂就是`PasswordAuthentication no`
// （DigitalOcean/AWS/GCP），只支持密码等于在最常见的 VPS 上开箱即挂——
// 那种机器握手时服务端只声明 publickey，客户端连试都不会试密码，
// 报的是 `attempted methods [none]`，看起来像密码错，其实是没得选。
func TestFirstConnectAuthAcceptsKeyOrPassword(t *testing.T) {
	kp, err := sshexec.GenerateKeyPair("test")
	if err != nil {
		t.Fatal(err)
	}
	key := string(kp.PrivatePEM)

	cases := []struct {
		name       string
		privateKey string
		password   string
		wantN      int
		wantFirst  string // 期望的首选方式（labels[0]）
	}{
		{"只有密码", "", "s3cret", 1, "密码"},
		{"只有私钥", key, "", 1, "私钥"},
		// 两者都给时先试密钥：密钥失败不消耗密码错误计数，
		// 顺序反过来更容易撞上 fail2ban。
		{"两者都给时先试私钥", key, "s3cret", 2, "私钥"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth, labels, err := firstConnectAuth(c.privateKey, c.password)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if len(auth) != c.wantN {
				t.Errorf("认证方式数量 got %d want %d", len(auth), c.wantN)
			}
			if len(labels) == 0 || labels[0] != c.wantFirst {
				t.Errorf("首选方式 got %v want %s", labels, c.wantFirst)
			}
		})
	}
}

// 两者都不给要明确报错，而不是拿着空的认证列表去拨号——
// 那样报出来的是一句谁也看不懂的握手失败。
func TestFirstConnectAuthRejectsEmpty(t *testing.T) {
	_, _, err := firstConnectAuth("", "")
	if err == nil {
		t.Fatal("既无密码也无私钥时必须报错")
	}
	if !strings.Contains(err.Error(), "私钥") || !strings.Contains(err.Error(), "密码") {
		t.Errorf("错误信息应说清缺什么: %v", err)
	}
}

// 私钥解析失败要给人话，而且**绝不能把密钥内容带进错误信息**。
func TestFirstConnectAuthBadKeyStaysQuiet(t *testing.T) {
	const junk = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISECRETLOOKINGBLOB user@host"
	_, _, err := firstConnectAuth(junk, "")
	if err == nil {
		t.Fatal("公钥/垃圾内容不应被当成私钥接受")
	}
	if strings.Contains(err.Error(), "SECRETLOOKINGBLOB") {
		t.Fatal("错误信息里不得出现输入的密钥内容")
	}
	// 最常见的两种误用要在提示里点出来，否则用户只会反复重贴
	if !strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("提示应说明要贴私钥而不是公钥: %v", err)
	}
}
