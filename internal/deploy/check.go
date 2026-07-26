package deploy

import "sort"

// CheckDrift比对渲染输入的slug集合与数据库projects表的slug集合（--check模式，验收B8）。
//
// 返回值：
//   - missingInCompose：库里有但compose里没有。**这是危险的一侧**——项目的CDK能通过
//     认证，容器却不存在，客户拿到的是"能登录但连不上"的死CDK（渲染计划§6）。
//   - extraInCompose：compose里有但库里没有。容器会起来但无人能认证进去，浪费资源
//     但不直接伤害客户。
//
// 两个返回slice均按字典序排序，保证输出稳定可比对。
func CheckDrift(rendered, db []string) (missingInCompose, extraInCompose []string) {
	inRendered := map[string]bool{}
	for _, s := range rendered {
		inRendered[s] = true
	}
	inDB := map[string]bool{}
	for _, s := range db {
		inDB[s] = true
	}
	for s := range inDB {
		if !inRendered[s] {
			missingInCompose = append(missingInCompose, s)
		}
	}
	for s := range inRendered {
		if !inDB[s] {
			extraInCompose = append(extraInCompose, s)
		}
	}
	sort.Strings(missingInCompose)
	sort.Strings(extraInCompose)
	return missingInCompose, extraInCompose
}
