// Package deploy渲染节点侧的部署编排（ccwadmin render-compose，设计C13）。
//
// slug校验是安全边界的一部分：slug会被拼进容器名、卷名、宿主机路径（渲染计划§6），
// 与internal/sync/paths.go的路径校验同属一类——独立单测、从严拒绝。
package deploy

import "fmt"

// MaxProjectsPerNode是单节点项目容器数的产品硬上限（设计§7.6，用户2026-07-26定）。
// 不可由代码或配置绕过；渲染器是强制点之一，ccwadmin init-project独立强制同一条规则。
const MaxProjectsPerNode = 3

// reservedSlugs与compose内既有service同名，作为项目slug会导致service冲突（渲染计划§6）。
var reservedSlugs = map[string]bool{
	"postgres":     true,
	"control-api":  true,
	"worker-agent": true,
	"caddy":        true,
}

// ValidateSlug校验单个slug：
// 只允许小写字母、数字、连字符；不得以连字符开头或结尾；总长2–32；拒绝保留名。
// （计划§6的正则允许单字符，但文字规定总长2–32——取更严格口径，拒绝单字符。）
func ValidateSlug(s string) error {
	if len(s) < 2 || len(s) > 32 {
		return fmt.Errorf("deploy: slug %q 非法：长度必须为2–32，得到%d", s, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return fmt.Errorf("deploy: slug %q 非法：不得以连字符开头或结尾", s)
			}
		default:
			return fmt.Errorf("deploy: slug %q 非法：只允许小写字母、数字、连字符", s)
		}
	}
	if reservedSlugs[s] {
		return fmt.Errorf("deploy: slug %q 是保留名（与compose内既有service冲突）", s)
	}
	return nil
}

// ValidateSlugs校验整个列表：逐个校验 + 拒绝重复 + 强制项目数上限。
// 任何一项失败即拒绝整次渲染，不做「跳过非法项继续」——部分成功会产出一个
// 看似正常但少了项目的compose，比直接失败更危险（渲染计划§6）。
func ValidateSlugs(slugs []string) error {
	if len(slugs) == 0 {
		return fmt.Errorf("deploy: 项目列表为空，至少需要1个slug")
	}
	if len(slugs) > MaxProjectsPerNode {
		return fmt.Errorf("deploy: 项目数%d超过单节点上限%d（产品规则，console-fleet-design §7.6，不可绕过）",
			len(slugs), MaxProjectsPerNode)
	}
	seen := map[string]bool{}
	for _, s := range slugs {
		if err := ValidateSlug(s); err != nil {
			return err
		}
		if seen[s] {
			return fmt.Errorf("deploy: slug %q 在同一次渲染中重复", s)
		}
		seen[s] = true
	}
	return nil
}
