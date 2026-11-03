package panel

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestAppJSRefsExistInHTML 静态对账：app.js 里所有 $('id') 引用的元素，
// 必须在 index.html 里真的存在。
//
// 为什么需要：app.js 是整份顺序求值的脚本，任意一行 $('x').onclick = ... 在 x
// 不存在时抛 TypeError，**它后面所有代码都不会执行**——包括事件分发器。
// 2026-11 就因此让「上游渠道」整页点击失效（分页切不动、按钮全无反应），
// 而当时所有 Go 测试与 node 语法检查全绿：桩 DOM 的 $() 对任意 id 都返回对象，
// 永远不会是 null。此测试直接比对真实 HTML，补上这个盲区。
func TestAppJSRefsExistInHTML(t *testing.T) {
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	js := string(jsBytes)

	idRe := regexp.MustCompile(`\bid="([^"]+)"`)
	ids := map[string]bool{}
	for _, m := range idRe.FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}

	refRe := regexp.MustCompile(`\$\('([^']+)'\)`)
	refs := map[string]bool{}
	for _, m := range refRe.FindAllStringSubmatch(js, -1) {
		refs[m[1]] = true
	}

	var missing []string
	for id := range refs {
		if !ids[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("app.js 引用了 index.html 中不存在的元素 id（会让整页脚本在求值阶段中断）：%s",
			strings.Join(missing, ", "))
	}
}
