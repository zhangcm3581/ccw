package provision

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// allowedSecretCapture是允许调用RunCapturingSecret的文件白名单。
//
// 只有**输出里确实含有要交付给管理员的CDK明文**的调用点才该在这里：
//   - bootstrap.go：init-projects，纳管时签发的CDK
//   - admincmd.go：issue-cdk / rotate-cdk，后台页面上签发与轮换的CDK
//
// 这两条都是同一件事的两条入口。2026-07-28只改了前者，
// 结果后台页面上签发出来的CDK照样是[REDACTED]——白名单写成显式的两项，
// 就是为了让"还有没有第三条路"这个问题在改动时被看见。
var allowedSecretCapture = map[string]bool{
	"bootstrap.go": true,
	"admincmd.go":  true,
}

// 源码级守卫：RunCapturingSecret只允许出现在白名单文件里。
//
// 它返回未脱敏的stdout，绕开了"在最靠近数据源处脱敏"这条设计约束——
// 每多一个调用点，就多一处"未脱敏内容可能被记进日志"的风险。
//
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
	if len(sites) == 0 {
		t.Fatal("一个调用点都没有？CDK明文的交付路径可能又被改回脱敏版本了")
	}
	for _, site := range sites {
		file := site[:strings.IndexByte(site, ':')]
		if !allowedSecretCapture[file] {
			t.Errorf("%s 调用了RunCapturingSecret，但它不在白名单里。\n"+
				"这个方法返回未脱敏输出；每多一处就多一条凭据进日志的路径。"+
				"确有需要时，请连同「原文只许解析、报错一律用脱敏结果」的约定一起复核，"+
				"再把文件加进allowedSecretCapture。", site)
		}
	}
}
