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

## 注意

第 3 条**不是**绕过任何上游限制，纯粹是把调用者身份如实传给面板，让面板的既有功能
（按 key 绑定渠道、按 key 统计用量）真正生效。

第 2 条与之相反：它是**停止宣称**一个被上游限制掉的免费通道；这套伪装成 OpenCode 客户端
的逻辑（`src/disguise.mjs`）本身是上游明令限制的行为，不要在此基础上继续加固。
