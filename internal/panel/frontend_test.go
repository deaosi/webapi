package panel

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter', origin: 'http://127.0.0.1:4321' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages", "#apikeys", "#channels", "#zen"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
  ctx
);
process.stdout.write(JSON.stringify({
  credit: ctx.fmtCredit(1.25),
  zero: ctx.fmtCredit(0),
  hundred: ctx.fmtCredit(100),
  ratio: ctx.fmtCreditRatio(12.5, 2, 400),
  noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
  noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
  rate: ctx.fmtModelRate('0.5'),
  noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// 模型限流时间必须同时支持上游 reset_at、网关 until 和无重置时间三种形态。
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function rateLimitRowsHtml');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
const rate = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
  rate: rate.detail,
  unavailable: unavailable.detail,
  unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"rate":"预计 2026-09-28 16:00 解封（剩余 2时00分） · 网关最快 1时00分 后重试","unavailable":"预计 1时00分 后重试","unknown":"预计解封时间未知"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// 请求指标滚动行必须紧凑、可读，并对失败结果使用日志高亮。
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText; this.requestLogLine=requestLogLine;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1' };
const bad = { ...good, status: 500, outcome: 'http_error', request_id: 'req-2' };
process.stdout.write(JSON.stringify({
  good: ctx.requestLogText(good),
  goodLine: ctx.requestLogLine(good),
  badLine: ctx.requestLogLine(bad),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	const text = "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit | req-1"
	want := `{"good":` + strconv.Quote(text) +
		`,"goodLine":` + strconv.Quote(`<span class="ln">`+text+`</span>`) +
		`,"badLine":` + strconv.Quote(`<span class="ln e">14:05:06 | 500 HTTP 错误 | glm-5.3 | 账号(uid8) | 1.25s | 2.3k tok | 0.12 credit | req-2</span>`) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// 同到期时间按面额降序；其余未用完包与零/负余额包分别聚合。
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
  { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
  { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
  { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
  { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
  { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
  { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
  { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
  { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
  visible: out.visible.map(p => p.id),
  rest: out.rest.map(p => p.id),
  used: out.used.map(p => p.id),
  restSize: out.restSize,
  restRemain: out.restRemain,
  usedSize: out.usedSize,
  defaultLimit: ctx.pkDetailLimit({}),
  configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
  unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// 精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表。
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 100000;
const out = ctx.summarizeCreditDays([
  { uid: 'a', remain: 100, packages: [
    { name: 'soon-a', remain: 30, expires_at: now + day },
    { name: 'later', remain: 70, expires_at: now + 7 * day },
  ] },
  { uid: 'b', remain: 55, packages: [
    { name: 'soon-b', remain: 20, expires_at: now + day },
    { name: 'unknown', remain: 5, end_time: '' },
  ] },
  { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
  rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
  accountCount: out.accountCount,
  unavailable: out.unavailable,
  colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
  colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	const want = `{"rows":[{"days":1,"credits":50},{"days":7,"credits":70}],"accountCount":3,"unavailable":1,"colorA":"#4f8cff","colorB":"#25b08b"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}

// 覆盖调用密钥创建失败后的输入、一次显示的 secret 清理和管理动作。
func TestAppJSAPIKeysWorkflow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; API key workflow skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const nodes = new Map();
function element() {
  return { value: '', textContent: '', innerHTML: '', hidden: false, disabled: false,
    dataset: {}, children: [], isConnected: true,
    // classList 真实记录状态：弹窗开合就是 class 增删，空实现会让"保存后应自动关闭"这类断言永远为真（测不出 bug）。
    classList: { _s: new Set(),
      add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); },
      toggle(c) { this._s.has(c) ? this._s.delete(c) : this._s.add(c); },
      contains(c) { return this._s.has(c); }, has(c) { return this._s.has(c); } },
    focus() {}, select() {}, addEventListener() {}, appendChild() {}, remove() {},
    querySelectorAll: () => [] };
}
const get = id => { if (!nodes.has(id)) nodes.set(id, element()); return nodes.get(id); };
// trigger(containerId, act) 模拟"点击容器里 data-act=act 的那个元素"，并把它交给
// document 上的全部 click 监听器（即真实分发链路）。
// closest 按选择器区分：候选标签是 <span>，匹配 '[data-act]' 但**不**匹配
// 'button[data-act]'——分发器若只找 button，这里就会返回 null、点击静默失效，
// 正好复现线上那个"点了没反应"的 bug。
const trigger = (containerId, act) => {
  const html = get(containerId).innerHTML || '';
  const m = new RegExp('data-act="' + act + '"([^>]*)').exec(html);
  if (!m) throw new Error('容器 #' + containerId + ' 里没有 data-act=' + act + ' 的元素');
  const dataM = /data-m="([^"]*)"/.exec(m[1]);
  const el = {
    tagName: 'SPAN', dataset: { act, m: dataM ? dataM[1] : undefined }, disabled: false,
    closest: sel => (sel === 'button[data-act]' ? null : (sel === '[data-act]' ? el : null)),
  };
  for (const fn of (document._click || [])) fn({ target: el, preventDefault() {} });
};
const sandbox = {
  location: { hash: '#accounts', origin: 'http://127.0.0.1:4321' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() { throw new Error('call keys must not be persisted'); } },
  navigator: {},
  document: { querySelectorAll: () => [], querySelector: () => null, getElementById: get,
    addEventListener() {}, documentElement: element(), head: element(), body: element(),
    createElement: element, activeElement: element(), cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval: () => 1, clearInterval() {}, setTimeout: () => 1, clearTimeout() {},
  confirm: () => true,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set,
  RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent,
  URL, Symbol, Proxy, Reflect,
};
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: 'app.js' });
const run = code => vm.runInContext(code, sandbox);
const requests = [];
let failure = false, response;
sandbox.mockAPI = async (path, options) => {
  requests.push({ path, method: options && options.method, body: options && options.body });
  if (failure) throw new Error('save failed');
  return response;
};
run('api = mockAPI');
(async () => {
  assert.deepStrictEqual(JSON.parse(run("JSON.stringify(apiEndpoints('https://api.example:9443'))")), {
    base: 'https://api.example:9443/v1', chat: 'https://api.example:9443/v1/chat/completions', models: 'https://api.example:9443/v1/models'
  });
  assert.deepStrictEqual(JSON.parse(run("JSON.stringify(adapterEndpoints('https://api.example:9443'))")), {
    base: 'https://api.example:9443/adapter/v1', chat: 'https://api.example:9443/adapter/v1/chat/completions',
    responses: 'https://api.example:9443/adapter/v1/responses', messages: 'https://api.example:9443/adapter/v1/messages',
    countTokens: 'https://api.example:9443/adapter/v1/messages/count_tokens', models: 'https://api.example:9443/adapter/v1/models'
  });
  assert.deepStrictEqual(JSON.parse(run("JSON.stringify(adapterEndpoints('http://127.0.0.1:8888'))")), {
    base: 'http://127.0.0.1:8788/v1', chat: 'http://127.0.0.1:8788/v1/chat/completions',
    responses: 'http://127.0.0.1:8788/v1/responses', messages: 'http://127.0.0.1:8788/v1/messages',
    countTokens: 'http://127.0.0.1:8788/v1/messages/count_tokens', models: 'http://127.0.0.1:8788/v1/models'
  });
  run('renderAPIEndpoints()');
  assert.strictEqual(get('akBaseURL').value, 'http://127.0.0.1:4321/v1');
  assert.strictEqual(get('akTriBaseURL').value, 'http://127.0.0.1:4321/adapter/v1');
  assert.strictEqual(get('akTriRespURL').value, 'http://127.0.0.1:4321/adapter/v1/responses');
  assert.strictEqual(get('akTriMsgURL').value, 'http://127.0.0.1:4321/adapter/v1/messages');
  failure = true;
  run("openAPIKeyModal('create'); $('akName').value = 'Client <one>'");
  await run('submitAPIKey()');
  assert.strictEqual(get('akName').value, 'Client <one>');
  assert.strictEqual(run('apiKeyState.modalOpen'), true);
  assert.strictEqual(get('akModalError').hidden, false);
  assert.strictEqual(get('akSave').disabled, false);
  failure = false;
  response = { key: { id: 'key-1', name: 'Client <one>', prefix: 'sk-wb2-prefix', enabled: true,
    created_at: '2026-09-30T10:00:00Z', secret: 'unexpected-private-field' }, secret: 'new-call-secret' };
  await run('submitAPIKey()');
  assert.strictEqual(requests.at(-1).method, 'POST');
  assert.strictEqual(get('akSecret').value, 'new-call-secret');
  assert.strictEqual(get('akCreated').hidden, false);
  assert.strictEqual(run("'secret' in apiKeyState.rows[0]"), false);
  assert(get('akBody').innerHTML.includes('Client &lt;one&gt;'));
  assert(!get('akBody').innerHTML.includes('new-call-secret'));
  run('closeAPIKeyModal()');
  assert.strictEqual(run('apiKeyState.secret'), '');
  assert.strictEqual(get('akSecret').value, '');
  assert.strictEqual(run('apiKeyState.modalOpen'), false);
  const listedKey = { id: 'key-1', name: 'Client <one>', prefix: 'sk-wb2a-abcd1234', enabled: true,
    created_at: '2026-09-30T10:00:00Z' };
  response = { keys: [listedKey], usage: {} };
  await run('loadAPIKeys()');
  assert(get('akBody').innerHTML.includes('未使用'));
  response = { keys: [listedKey], usage: { 'key-1': { requests: 12, errors: 2, prompt_tokens: 3000,
    output_tokens: 1500, last_used_at: '2026-10-06T03:00:00Z', last_model: 'deepseek-v4.1-flash' } } };
  await run('loadAPIKeys()');
  const keyHTML = get('akBody').innerHTML;
  assert(keyHTML.includes('12 次'));
  assert(keyHTML.includes('失败 2'));
  assert(keyHTML.includes('4.5k tok'));
  assert(keyHTML.includes('deepseek-v4.1-flash'));
  assert.strictEqual(run("apiKeyState.usage['key-1'].requests"), 12);
  assert.strictEqual(run("apiKeyUsage({}, 'key-1').requests"), 0);
  run("openAPIKeyModal('rename', 'key-1'); $('akName').value = 'Renamed'");
  response = { key: { id: 'key-1', name: 'Renamed', prefix: 'sk-wb2-prefix', enabled: true, created_at: '2026-09-30T10:00:00Z' } };
  await run('submitAPIKey()');
  assert.strictEqual(requests.at(-1).method, 'PATCH');
  assert.strictEqual(run('apiKeyState.rows[0].name'), 'Renamed');
  assert.strictEqual(run('apiKeyState.modalOpen'), false);
  response.key.enabled = false;
  await run("setAPIKeyEnabled('key-1')");
  assert.strictEqual(JSON.parse(requests.at(-1).body).enabled, false);
  assert.strictEqual(run('apiKeyState.rows[0].enabled'), false);
  response = { ok: true };
  await run("deleteAPIKey('key-1')");
  assert.strictEqual(requests.at(-1).method, 'DELETE');
  assert.strictEqual(run('apiKeyState.rows.length'), 0);
  console.log('API KEY WORKFLOW OK');
})().catch(error => { console.error(error.stack); process.exitCode = 1; });
`
	f, err := os.CreateTemp(t.TempDir(), "api-keys-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("API key frontend workflow failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("API KEY WORKFLOW OK")) {
		t.Fatalf("API key frontend workflow did not finish:\n%s", out)
	}
}

// 轮询策略面板：overview.pick 渲染（模式/分组/账号权重行/占比）与保存 body 形态。
func TestAppJSPickPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; pick panel skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const nodes = new Map();
function element() {
  return { value: '', textContent: '', innerHTML: '', hidden: false, disabled: false,
    dataset: {}, children: [], isConnected: true,
    // classList 真实记录状态：弹窗开合就是 class 增删，空实现会让"保存后应自动关闭"这类断言永远为真（测不出 bug）。
    classList: { _s: new Set(),
      add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); },
      toggle(c) { this._s.has(c) ? this._s.delete(c) : this._s.add(c); },
      contains(c) { return this._s.has(c); }, has(c) { return this._s.has(c); } },
    focus() {}, select() {}, addEventListener() {}, appendChild() {}, remove() {},
    querySelectorAll: () => [], closest: () => null };
}
const get = id => { if (!nodes.has(id)) nodes.set(id, element()); return nodes.get(id); };
// savePick 从 DOM 回读编辑值：桩按 selector 返回与 renderPick 输出同构的假输入。
function fakeInput(dataset, value) {
  return { dataset, value: String(value), closest: () => null };
}
// acctInputs 可在用例中改值：验证「空权重输入不得被写成 0（软摘除）」。
let acctInputs = [
  fakeInput({ uid: 'u-main-0001', f: 'group' }, 'main'),
  fakeInput({ uid: 'u-main-0001', f: 'priority' }, '0'),
  fakeInput({ uid: 'u-main-0001', f: 'weight' }, '3'),
  fakeInput({ uid: 'u-back-0002', f: 'group' }, 'backup'),
  fakeInput({ uid: 'u-back-0002', f: 'priority' }, '10'),
  fakeInput({ uid: 'u-back-0002', f: 'weight' }, '1'),
];
const sandbox = {
  location: { hash: '#accounts', origin: 'http://127.0.0.1:4321' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: {},
  document: {
    querySelectorAll: (sel) => {
      if (sel === '#pickAcctBody [data-uid]') return acctInputs;
      if (sel === '#grpBody [data-grp]') return [
        { dataset: { grp: 'main', f: 'priority' }, value: '0', closest: () => ({ querySelector: () => ({ value: 'main' }) }) },
        { dataset: { grp: 'main', f: 'weight' }, value: '1', closest: () => ({ querySelector: () => ({ value: 'main' }) }) },
        { dataset: { grp: 'backup', f: 'priority' }, value: '10', closest: () => ({ querySelector: () => ({ value: 'backup' }) }) },
        { dataset: { grp: 'backup', f: 'weight' }, value: '1', closest: () => ({ querySelector: () => ({ value: 'backup' }) }) },
      ];
      return [];
    },
    querySelector: () => null, getElementById: get,
    // 记录 click 监听器：分发器就是 document 上的 click 监听器，no-op 桩会让
    // "点击后应生效"这类用例无从验证。
    _click: [],
    addEventListener(t, fn) { if (t === 'click') document._click.push(fn); },
    documentElement: element(), head: element(), body: element(),
    createElement: element, activeElement: element(), cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval: () => 1, clearInterval() {}, setTimeout: () => 1, clearTimeout() {},
  confirm: () => true,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set,
  RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent,
  URL, Symbol, Proxy, Reflect,
};
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: 'app.js' });
const run = code => vm.runInContext(code, sandbox);
const requests = [];
sandbox.mockAPI = async (path, options) => {
  requests.push({ path, method: options && options.method, body: options && options.body });
  return { ok: true, restart_required: [] };
};
run('api = mockAPI');
(async () => {
  // 夹具用 Node 侧对象 + JSON.stringify 注入：本测试脚本体是 Go 反引号原始字符串，
  // 内部不能再出现反引号（否则提前截断），故不走 JS 模板字面量。
  const pickFixture = { accounts: [], pick: {
    mode: 'priority', inner: 'swrr',
    prefer_expiring: true, expiring_soon: '168h',
    groups: [{ name: 'main', priority: 0, weight: 1, members: 1 },
             { name: 'backup', priority: 10, weight: 1, members: 1 }],
    accounts: [
      { uid: 'u-main-0001', nickname: '主号A', weight: 3, priority: 0, group: 'main', w_final: 12.5, sw_current: 0, picks: 30 },
      { uid: 'u-back-0002', nickname: '备号B', weight: 1, priority: 10, group: 'backup', w_final: 4.2, sw_current: 0, picks: 10 }
    ],
    distribution: { 'u-main-0001': 30, 'u-back-0002': 10 } } };
  run('overviewData = ' + JSON.stringify(pickFixture));
  run('renderPick()');
  assert.strictEqual(get('pickMode').value, 'priority');
  assert.strictEqual(get('pickInner').value, 'swrr');
  assert.strictEqual(get('pickPreferExpiring').checked, true);
  assert.strictEqual(get('pickExpiringSoon').value, '168h');
  assert(get('grpBody').innerHTML.includes('main'));
  assert(get('grpBody').innerHTML.includes('backup'));
  const rows = get('pickAcctBody').innerHTML;
  // 账号列必须显示昵称（只给 uid 前 8 位运维认不出是哪个号），短 uid 作次行。
  assert(rows.includes('主号A'), '账号列应显示昵称');
  assert(rows.includes('备号B'), '账号列应显示昵称');
  assert(rows.includes('u-back-0002'.slice(0, 16)), '短 uid 应作次行显示');
  // 只读表：权重是文本列而非输入框（行内 input 已删，编辑走弹窗）；旧 DOM 的
  // data-f="weight" 输入框必须消失，否则"并发写盘"老路还在。
  assert(!rows.includes('data-f="weight"'), '账号表不得再有行内权重输入框');
  assert(rows.includes('data-act="acct-edit"'), '账号行应有编辑按钮（弹窗入口）');
  assert(get('grpBody').innerHTML.includes('data-act="grp-edit"'), '分组行应有编辑按钮（弹窗入口）');
  assert(rows.includes('12.50'));
  assert(rows.includes('75%'));
  // 保存策略：只含模式/算法/快过期四键——不再从 DOM 全量回读分组与账号。
  // 注意 savePick 成功后会触发 loadOverview（记一条 overview 请求），
  // 故按 method 找 POST。
  await run('savePick()');
  const posts = requests.filter(r => r.path === 'config' && r.method === 'POST');
  assert.strictEqual(posts.length, 1, 'savePick 应恰提交一次 config POST');
  const body = JSON.parse(posts[0].body);
  assert.strictEqual(body.pool.pick_mode, 'priority');
  assert.strictEqual(body.pool.pick_inner, 'swrr');
  assert.strictEqual(body.pool.prefer_expiring, true);
  assert.strictEqual(body.pool.expiring_soon, '168h');
  assert.strictEqual(body.pool.accounts, undefined, '策略保存不得再碰 accounts（走弹窗单次入库）');
  assert.strictEqual(body.pool.groups, undefined, '策略保存不得再碰 groups（走弹窗单次入库）');
  // 分组弹窗提交：改 main 的权重，只动这一条分组。
  run('editGroupModal("edit", "main")');
  get('editWeight').value = '5';
  get('editPriority').value = '0';
  await run('editSubmit()');
  const gposts = requests.filter(r => r.path === 'config' && r.method === 'POST');
  assert.strictEqual(gposts.length, 2, '分组弹窗提交应再有一次 config POST');
  const gb = JSON.parse(gposts[1].body);
  // pushPoolPatch 读盘（mock 返回 {ok:true} 无 config → 空表），只写入改动的分组。
  assert.strictEqual(gb.pool.groups.main.weight, 5, '分组弹窗只改该条分组');
  // 账号弹窗提交：改 u-back-0002 的权重与分组。
  // 先重设 overviewData：前面的 editSubmit 成功后调了 loadOverview，而桩的 overview
  // 无 pick 字段（pickState.pick 会被清空导致找不到行）。这是桩的局限，不是实现 bug。
  run('overviewData = ' + JSON.stringify(pickFixture));
  run('editAcctModal("u-back-0002")');
  get('editWeight').value = '2';
  get('editPriority').value = '10';
  get('editGroupSel').value = 'main';
  await run('editSubmit()');
  const aposts = requests.filter(r => r.path === 'config' && r.method === 'POST');
  assert.strictEqual(aposts.length, 3, '账号弹窗提交应再有一次 config POST');
  const ab = JSON.parse(aposts[2].body);
  assert.strictEqual(ab.pool.accounts['u-back-0002'].weight, 2, '账号弹窗只改该账号');
  assert.strictEqual(ab.pool.accounts['u-back-0002'].group, 'main');
  // 保存成功后弹窗必须自动关闭：closeEditModal 的 saving 守卫会吞掉程序化关闭
  // （saving 要到 finally 才复位），曾导致"点了保存窗口还杵在那"。
  assert(get('editVeil').classList.has('on') === false, '账号弹窗保存后应自动关闭');
  console.log('PICK PANEL OK');
})().catch(error => { console.error(error.stack); process.exitCode = 1; });
`
	f, err := os.CreateTemp(t.TempDir(), "pick-panel-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("pick panel frontend test failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("PICK PANEL OK")) {
		t.Fatalf("pick panel frontend test did not finish:\n%s", out)
	}
}

// 上游渠道面板：定义表三行渲染是昵称 + 成员 + 白名单 + 停用标记；密钥绑定行按分组打标签；
// 保存 body 必须是 channels 段完整状态（删渠道/解绑靠整体替换，不是靠省略键）。
func TestAppJSChannelsPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; channels panel skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const nodes = new Map();
function element() {
  return { value: '', textContent: '', innerHTML: '', hidden: false, disabled: false,
    dataset: {}, children: [], isConnected: true,
    // classList 真实记录状态：弹窗开合就是 class 增删，空实现会让"保存后应自动关闭"这类断言永远为真（测不出 bug）。
    classList: { _s: new Set(),
      add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); },
      toggle(c) { this._s.has(c) ? this._s.delete(c) : this._s.add(c); },
      contains(c) { return this._s.has(c); }, has(c) { return this._s.has(c); } },
    focus() {}, select() {}, addEventListener() {}, appendChild() {}, remove() {},
    querySelectorAll: () => [], querySelector: () => null,
    closest: () => null };
}
const get = id => { if (!nodes.has(id)) nodes.set(id, element()); return nodes.get(id); };
// chInputs 可在用例中改值：渠道保存同样从 DOM 回读。
// 注意桩的忠实度：saveChannels 先 el.closest('tr') 再行内 querySelector('[data-f=...]')，
// 所以每个输入的 closest 必须返回同行的查找器，否则行会被整行跳过、defs 为空。
const chMainRow = [
  { dataset: { chg: 'main', f: 'name' }, value: 'main' },
  { dataset: { chg: 'main', f: 'label' }, value: '主渠道' },
  { dataset: { chg: 'main', f: 'realm' }, value: 'cn' },
  { dataset: { chg: 'main', f: 'models' }, value: 'glm-5*, deepseek-v3' },
  { dataset: { chg: 'main', f: 'enabled' }, checked: true },
];
for (const el of chMainRow) {
  el.closest = () => ({ querySelector: (sel) => {
    if (sel === '[data-f="name"]') return chMainRow[0];
    if (sel === '[data-f="label"]') return chMainRow[1];
    if (sel === '[data-f="realm"]') return chMainRow[2];
    if (sel === '[data-f="models"]') return chMainRow[3];
    if (sel === '[data-f="enabled"]') return chMainRow[4];
    if (sel === '[data-grp-name]') return null;
    return null;
  } });
}
// 额外加一条“停用渠道”行：验证停用标记能随保存发出（unchecked checkbox 场景）。
const chDeadRow = [
  { dataset: { chg: 'dead', f: 'name' }, value: 'dead' },
  { dataset: { chg: 'dead', f: 'label' }, value: '' },
  { dataset: { chg: 'dead', f: 'realm' }, value: '' },
  { dataset: { chg: 'dead', f: 'models' }, value: '' },
  { dataset: { chg: 'dead', f: 'enabled' }, checked: false },
];
for (const el of chDeadRow) {
  el.closest = () => ({ querySelector: (sel) => {
    if (sel === '[data-f="name"]') return chDeadRow[0];
    if (sel === '[data-f="label"]') return chDeadRow[1];
    if (sel === '[data-f="realm"]') return chDeadRow[2];
    if (sel === '[data-f="models"]') return chDeadRow[3];
    if (sel === '[data-f="enabled"]') return chDeadRow[4];
    if (sel === '[data-grp-name]') return null;
    return null;
  } });
}
let chGroupInputs = chMainRow.concat(chDeadRow);
let chKeySels = [
  { dataset: { chk: 'key-1' }, value: 'main' },
  { dataset: { chk: 'key-2' }, value: '' },
];
const sandbox = {
  location: { hash: '#channels', origin: 'http://127.0.0.1:4321' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: {},
  document: {
    querySelectorAll: (sel) => {
      if (sel === '#chGroupBody [data-chg]') return chGroupInputs;
      if (sel === '#chKeyBody [data-chk]') return chKeySels;
      return [];
    },
    querySelector: () => null, getElementById: get,
    addEventListener() {}, documentElement: element(), head: element(), body: element(),
    createElement: element, activeElement: element(), cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval: () => 1, clearInterval() {}, setTimeout: () => 1, clearTimeout() {},
  confirm: () => true,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set,
  RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent,
  URL, Symbol, Proxy, Reflect,
};
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: 'app.js' });
const run = code => vm.runInContext(code, sandbox);
const requests = [];
sandbox.mockAPI = async (path, options) => {
  requests.push({ path, method: options && options.method, body: options && options.body });
  // loadChannels 一次并行三路：config / api_keys / overview。
  if (String(path).indexOf('config') >= 0) return { ok: true, path: '/x/config.json', config: {
    channels: {
      enabled: true,
      groups: { main: { label: '主渠道', realm: 'cn', models: ['glm-5*'], enabled: true },
                dead: { label: '', realm: '', models: [], enabled: false },
                ext: { label: '外部示例', realm: '', models: ['glm-5.1'], enabled: true,
                       upstream_url: 'https://up.example.com/v1', upstream_key: 'sk-up-secret',
                       upstream_type: 'openai' } },
      keys: { 'key-1': 'main', 'key-2': 'ext' },
    } } };
  if (String(path).indexOf('api_keys') >= 0) return { ok: true, keys: [
    { id: 'key-1', name: '对外A', prefix: 'sk-wb2a-aaaa1111', enabled: true, created_at: '' },
    { id: 'key-2', name: '测试B', prefix: 'sk-wb2a-bbbb2222', enabled: false, created_at: '' } ] };
  if (String(path).indexOf('overview') >= 0) return { accounts: [], pick: { accounts: [
    { uid: 'u1', nickname: '昵1', group: 'main' },
    { uid: 'u2', nickname: '昵2', group: 'main' } ] } };
  return { ok: true, restart_required: [] };
};
run('api = mockAPI');
(async () => {
  await run('loadChannels()');
  assert.strictEqual(get('chEnabled').checked, true);
  assert(get('chNote').textContent.indexOf('3 个渠道定义') >= 0, '应显示渠道数');
  assert(get('chNote').textContent.indexOf('2 条密钥绑定') >= 0, '应显示绑定数');
  const ghtml = get('chGroupBody').innerHTML;
  // 三行合一：名/label/白名单/members/停用都落在同一行字符串里才能查（“三分开”是上次权重列的坑）。
  assert(ghtml.indexOf('主渠道') >= 0, '定义行应带展示名');
  assert(ghtml.indexOf('glm-5*') >= 0, '定义行应带白名单原文');
  assert(ghtml.indexOf('2 个') >= 0, '应显示成员数 2（来自 overview.pick.accounts.group）');
  const khtml = get('chKeyBody').innerHTML;
  assert(khtml.indexOf('对外A') >= 0, '绑定行应带密钥名称而不是裸 prefix');
  assert(khtml.indexOf('sk-wb2a-aaaa1111') >= 0, '绑定行应带密钥前缀');
  assert(khtml.indexOf('限 main') >= 0, '绑定的生效列应显示可用的分组与成员数');
  assert(khtml.indexOf('已禁用') >= 0, '禁用的 key 应有标记');
  assert(khtml.indexOf('data-act="key-bind"') >= 0, '绑定行应有“绑定”按钮（弹窗编辑入口）');
  assert(ghtml.indexOf('data-act="ch-edit"') >= 0, '渠道行应有“编辑”按钮（弹窗编辑入口）');
  // 外部上游渠道**不需要**成员账号：请求直接转发到上游端点，根本不进账号池。
  // 曾按"账号池分组"口径判断，于是绑了 key 还显示"该分组无成员账号 → 请求失败"，
  // 让人以为得先去账号池塞几个号（实测被用户当场指出）。
  assert(khtml.indexOf('无成员账号') < 0, '外部渠道不该出现"该分组无成员账号"');
  assert(khtml.indexOf('外部上游 up.example.com/v1') >= 0,
    '绑定到外部渠道的生效列应显示上游地址，实得 ' + khtml.slice(0, 200));
  assert(ghtml.indexOf('up.example.com/v1') >= 0, '渠道表应显示外部上游地址');
  // 该行成员列必须是"—"（不适用），不能是警告徽标
  assert(/ext[\s\S]{0,400}?不适用|ext[\s\S]{0,400}?<span class="note"[^>]*>—<\/span>/.test(ghtml),
    '外部渠道的成员列应显示"—"而不是"无成员"警告');
  // 渠道编辑弹窗提交：改 main 的 label+白名单，只动这一条定义。
  run('chEditModal("edit", "main")');
  get('chEditLabel').value = '主渠道V2';
  get('chEditModels').value = 'glm-5*, deepseek-v3';
  get('chEditRealm').value = 'cn';
  await run('chEditSubmit()');
  // 密钥绑定弹窗提交：key-2 绑到 main。
  run('keyBindModal("key-2")');
  get('keyBindSel').value = 'main';
  await run('keyBindSubmit()');
  const posts = requests.filter(r => r.path === 'config' && r.method === 'POST');
  assert(posts.length >= 2, '两次弹窗提交应各有一次 config POST，实得 ' + posts.length);
  const chPost = JSON.parse(posts[0].body);
  assert.strictEqual(chPost.channels.groups.main.label, '主渠道V2', '渠道编辑应只改该条定义');
  assert.deepStrictEqual(chPost.channels.groups.main.models, ['glm-5*', 'deepseek-v3']);
  const kbPost = JSON.parse(posts[posts.length - 1].body);
  assert.strictEqual(kbPost.channels.keys['key-2'], 'main', '绑定弹窗只动该 key');
  assert.strictEqual(kbPost.channels.keys['key-1'], 'main', '整段带回：既有绑定不能丢');
  // 保存成功后弹窗必须自动关闭：closeChEditModal 的 saving 守卫会吞掉程序化关闭
  // （saving 要到 finally 才复位），曾导致"点了保存窗口还杵在那"。这里直接断言结果为 off。
  assert(get('chEditVeil').classList.has('on') === false, '渠道弹窗保存后应自动关闭');
  assert(get('keyBindVeil').classList.has('on') === false, '绑定弹窗保存后应自动关闭');
  // 上游模型候选标签必须能经**点击分发器**生效：曾经分发器只找 button[data-act]，
  // 而候选是 span，于是点了完全没反应且不报错。这里走真实点击链路（trigger），
  // 而不是直接调 chEditToggleFetched——后者测不出"分发器匹配不到元素"这类断链。
  run('chEditClearFetched(); chEditRenderFetched(["m-a","m-b"])');
  assert(get('chEditFetchList').innerHTML.indexOf('m-a') >= 0, '候选标签应渲染出来');
  trigger('chEditFetchList', 'chfetch-toggle');
  assert(get('chEditModels').value === 'm-a',
    '点候选标签应写入白名单，实得 ' + JSON.stringify(get('chEditModels').value));
  console.log('CHANNELS PANEL OK');
})().catch(error => { console.error(error.stack); process.exitCode = 1; });
`
	f, err := os.CreateTemp(t.TempDir(), "channels-panel-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("channels panel frontend test failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("CHANNELS PANEL OK")) {
		t.Fatalf("channels panel frontend test did not finish:\n%s", out)
	}
}
