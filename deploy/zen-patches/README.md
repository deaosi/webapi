# zen-gateway 线上补丁

`/opt/zen-gateway/current/src/server.mjs` 是**只读**的（`ProtectSystem=strict` + `chmod 444`），
而且不在本仓库里。下面三处修复打在服务器上，**重新部署 zen 会丢**，所以在此留档。

用法（在服务器上）：

```bash
sudo python3 deploy/zen-patches/apply.py     # 幂等：已打过的会跳过
sudo systemctl restart zen-gateway
```

每个补丁都会先在 `/opt/zen-gateway/` 留一份 `server.mjs.bak-<时间戳>`，并在代码里带一个
唯一标记注释，便于日后确认是否已应用。

## 1. `KEY-VERDICT-DEFINITIVE-ONLY v1` — 只缓存面板**明确表态**的密钥判定

`panelAcceptsKey()` 原来把「面板说这个 key 无效」和「面板连不上/超时/5xx」当成一回事，
**都缓存 60 秒**（`key_verify_ttl_ms`）。于是一次面板重启（部署 1~2 秒）就能让一把合法密钥
被判定无效并**粘住一分钟**，客户端表现为「API 密钥无效」。

改后：只有 200 / 401 / 403 才写缓存。连不上/超时/5xx 本次仍拒绝（fail closed，不放过
未验证的密钥），但**不缓存**——面板一恢复，下一个请求立刻重新验证通过。

实测：重启面板后恢复时间从 ~60s 降到 **0.4s**。

## 2. `EXPOSE-MODELS-SWITCH v1` — 不再对外宣告必然失败的 zen 免费模型

上游收紧后，`zen:*` 的免费模型全部返回
`503 FreeTierError: OpenCode's free tier can only be used from within OpenCode`。
它们挂在 `/v1/models` 里只会让客户端（尤其会自动选模型的 agent）选中，然后把上游的
限制误报成「API 密钥无效」，把人引到完全错误的方向查。

新增配置开关 `zen.expose_models`（默认 `true`）。本机设为 `false`：
`/etc/zen-gateway/config.json` → `zen.expose_models = false`。

**只是不再对外宣告**：`zen:*` 的调用路径一字未动。上游恢复或改用付费 key 后改回 `true` 即可。

## 3. `FORWARD-CALLER-KEY v1` — 转发调用者自己的密钥（**最关键的一处**）

`proxyToPanel()` 原来会丢掉入站的 `authorization`，一律替换成配置里的**面板主密钥**：

```js
if (['...','authorization',...].includes(k.toLowerCase())) continue;
if (cfg.wb2api.api_key) headers.authorization = `Bearer ${cfg.wb2api.api_key}`;
```

面板是按**密钥**做渠道绑定与用量归属的，所以这么转发等于：面板看到的永远是「未绑定任何
渠道的主密钥」→ **按 key 绑定上游渠道在公网路径上整条失效**：绑定过的 key 在 `/v1/models`
里看不到自己的渠道模型，请求也全部掉回账号池报 `no_healthy_account`。

改后：
- 转发调用者自己的 bearer；只有调用者用的是 zen 自己的网关密钥（`zen-gw-*`，面板不认）
  或没带密钥时，才回落到主密钥。
- `handleModels()` 也改用调用者密钥去问面板要模型表，**该 key 专属的清单不进共享缓存**
  （各自的渠道绑定不同，串了就等于给别人的 key 看你的渠道模型）；取不到则退回主密钥视图。

实测（绑定 key，走公网）：

```
面板直连: 28 条 | 渠道模型 8 个
经 zen  : 28 条 | 渠道模型 8 个     ← 修复前只有 20 条、渠道模型 0 个
主密钥  : 20 条 | 非 cn: 0 个        ← 不泄漏他人的渠道模型
日志: [server] upstream channel="channel1" model=kimi-k3 status=200
```

## 4. `ZEN-OWN-KEY v1` — 用你自己的 OpenCode Console API key（**官方支持的用法**）

脚本：`04-own-api-key.py`（独立、幂等；`apply.py` 覆盖上面 1-3）。

**为什么加这个**：OpenCode 把**免费额度**锁在它自己的客户端里了，于是匿名 `public` 通道
（`disguise.mjs` 伪造 `user-agent: opencode/1.18.31` + `x-opencode-client: cli` + canonical
session id 那一套）整体失效，所有 `zen:*` 一律返回：

```
403 {"type":"error","error":{"type":"FreeTierError",
     "message":"OpenCode's free tier can only be used from within OpenCode..."}}
```

实测 chat 端点在没有有效凭据时，**无论带什么 key / 伪装头，返回的都是同一段 403**
（无 auth、`Bearer public`、假 key、假 key+伪装头、`x-api-key` —— 五种组合完全一致）；
对照 `GET /v1/models` 带假 key 会返回 `401 Invalid credential`，说明鉴权层本身能区分。
也就是说：**没有有效凭据 → 落回免费通道 → 被客户端限制拦住；有有效凭据 → 走付费通道，
不受这条限制。**

而官方文档（`https://opencode.ai/docs/zen/`）明确写着这套 API key 可以
"use them with OpenCode **or another coding agent**"，页面还有 `Add $20 Pay as you go
balance` 与 pay-as-you-go 定价。**所以"接到自建网关里"是官方允许的用法。**

**改动**：
- `disguise.mjs`：`disguiseHeaders(ids, realKey)`。`realKey` 非空时只发
  `authorization` / `accept` / `content-type` 三个头，**不再发任何伪装头**——
  用真凭据就是正常调用，伪造客户端身份既没必要也不该做。
- `server.mjs`：默认值加 `zen.api_key: ''`，两处调用点传 `cfg.zen.api_key`。

**没配 key 时行为与之前完全一致**（已验证：仍是同一个 403）。

**启用**（拿到 key 之后）：

```bash
sudo python3 - <<'EOF'
import json
p = "/etc/zen-gateway/config.json"
c = json.load(open(p))
c.setdefault("zen", {})["api_key"] = "你的 OpenCode Console API key"
json.dump(c, open(p, "w"), ensure_ascii=False, indent=2)
EOF
sudo systemctl restart zen-gateway
```

**没有 key 时不要继续加固伪装那一套**：那是绕对方的免费额度限制，不在本仓库支持的范围内。

## 注意

第 3 条**不是**绕过任何上游限制，纯粹是把调用者身份如实传给面板，让面板的既有功能
（按 key 绑定渠道、按 key 统计用量）真正生效。

第 2 条与之相反：它是**停止宣称**一个被上游限制掉的免费通道；那套伪装成 OpenCode 客户端的
逻辑（`src/disguise.mjs`）本身是上游明令限制的行为。第 4 条给出了正路：用你自己的凭据，
并**去掉**伪装。
