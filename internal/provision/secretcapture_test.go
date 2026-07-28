package provision

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// 源码级守卫：RunCapturingSecret**只允许有一个调用点**。
//
// 它返回未脱敏的stdout，绕开了"在最靠近数据源处脱敏"这条设计约束。
// 之所以要开这个口子：`ccwadmin init-project`把新签发的CDK明文打在stdout上，
// 而脱敏发生在拿到值之前，明文就永远到不了管理员手里——那正是2026-07-28
// 真机部署时出的问题（页面上显示成 ccw_xxx.[REDACTED]）。
//
// 但这个口子每多一个调用点，就多一处"未脱敏内容可能被记进日志"的风险。
// 用AST而不是文本匹配：注释里提这个名字是有价值的（解释为什么危险），
// 文本匹配会把注释算成违规，逼着后人删掉那句解释。
func TestRunCapturingSecretHasSingleCallSite(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0) // 0＝不保留注释
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RunCapturingSecret" {
					sites = append(sites, name+":"+fset.Position(call.Pos()).String())
				}
				return true
			})
		}
	}
	if len(sites) != 1 {
		t.Errorf("RunCapturingSecret应恰好有一个调用点（init-projects），got %d: %v\n"+
			"它返回未脱敏输出；每多一处就多一条凭据进日志的路径。"+
			"确有新需求时，请连同「原文只许解析、报错一律用脱敏结果」的约定一起复核。",
			len(sites), sites)
	}
	if len(sites) == 1 && !strings.Contains(sites[0], "bootstrap.go") {
		t.Errorf("唯一调用点应在bootstrap.go的init-projects里，got %s", sites[0])
	}
}
