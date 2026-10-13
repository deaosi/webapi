#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""zen-gateway 线上补丁（幂等）。详见同目录 README.md。

    sudo python3 apply.py            # 打补丁（已打过的跳过）
    sudo systemctl restart zen-gateway

只改 /opt/zen-gateway/current/src/server.mjs（先备份，再改，改完恢复 444）。
"""
import io
import json
import os
import shutil
import subprocess
import sys
import time

P = "/opt/zen-gateway/current/src/server.mjs"
CFG = "/etc/zen-gateway/config.json"
NODE = "/opt/skilldesk-platform/runtime/node/bin/node"
STAMP = time.strftime("%Y%m%d-%H%M%S")


def backup(path):
    bak = "%s.bak-%s" % (path, STAMP)
    shutil.copy2(path, bak)
    print("   备份 ->", bak)


def edit(path, marker, pairs):
    """按 (old, new) 顺序替换；marker 已在则跳过。返回是否有改动。"""
    s = io.open(path, encoding="utf-8").read()
    if marker in s:
        print("   已打过（%s），跳过" % marker)
        return False
    for i, (old, new) in enumerate(pairs):
        if old not in s:
            raise SystemExit("   锚点缺失（第 %d 段），未做修改" % (i + 1))
    backup(path)
    for old, new in pairs:
        s = s.replace(old, new, 1)
    subprocess.run(["chmod", "u+w", path], check=True)
    try:
        io.open(path, "w", encoding="utf-8", newline="").write(s)
    finally:
        subprocess.run(["chmod", "444", path], check=True)
    return True


# ── 1) 只缓存面板明确表态的密钥判定 ────────────────────────────────────────
MARK1 = "/* KEY-VERDICT-DEFINITIVE-ONLY v1 */"
PAIRS1 = [(
    """  async function panelAcceptsKey(key) {
    const hit = keyVerdicts.get(key);
    if (hit && now() - hit.at < cfg.wb2api.key_verify_ttl_ms) return hit.ok;
    let ok = false;
    try {
      const ctl = new AbortController();
      const t = setTimeout(() => ctl.abort(), 8000);
      try {
        const r = await doFetch(`${cfg.wb2api.base.replace(/\\/+$/, '')}/v1/models`, {
          headers: { authorization: `Bearer ${key}`, accept: 'application/json' },
          signal: ctl.signal,
        });
        ok = r.ok;
        if (r.ok) await r.arrayBuffer().catch(() => {}); else await r.arrayBuffer().catch(() => {});
      } finally { clearTimeout(t); }
    } catch {
      ok = false;
    }
    if (keyVerdicts.size > 512) keyVerdicts.clear();
    keyVerdicts.set(key, { ok, at: now() });
    return ok;
  }""",
    """  async function panelAcceptsKey(key) {
    const hit = keyVerdicts.get(key);
    if (hit && now() - hit.at < cfg.wb2api.key_verify_ttl_ms) return hit.ok;
    let ok = false;
    // 只有面板**明确表态**时才缓存判定：200 = 有效，401/403 = 确实无效。
    // 连不上 / 超时 / 5xx 属于"问不出来"——把它们也缓存下来，会把一次面板重启
    // 或一次抖动放大成 TTL 之内的持续 401（用户看到的就是"合法密钥突然无效"）。
    // 现在这类结果本次仍拒绝（fail closed，不放过未验证的密钥），但**不写缓存**，
    // 面板一恢复，下一个请求立刻重新验证通过。
    let definitive = false;
    try {
      const ctl = new AbortController();
      const t = setTimeout(() => ctl.abort(), 8000);
      try {
        const r = await doFetch(`${cfg.wb2api.base.replace(/\\/+$/, '')}/v1/models`, {
          headers: { authorization: `Bearer ${key}`, accept: 'application/json' },
          signal: ctl.signal,
        });
        await r.arrayBuffer().catch(() => {});
        ok = r.ok;
        definitive = r.ok || r.status === 401 || r.status === 403;
      } finally { clearTimeout(t); }
    } catch {
      ok = false;
      definitive = false;
    }
    if (definitive) {
      if (keyVerdicts.size > 512) keyVerdicts.clear();
      keyVerdicts.set(key, { ok, at: now() });
    }
    return ok;
  }

  %s""" % MARK1,
)]

# ── 2) expose_models 开关 ─────────────────────────────────────────────────
MARK2 = "/* EXPOSE-MODELS-SWITCH v1 */"
PAIRS2 = [
    ("    zen: { base: 'https://opencode.ai/zen', catalog_ttl_ms: 600000, metadata_ttl_ms: 86400000, metadata_cache: '' },",
     "    zen: { base: 'https://opencode.ai/zen', catalog_ttl_ms: 600000, metadata_ttl_ms: 86400000, "
     "metadata_cache: '', expose_models: true },"),
    ("""  async function handleModels(res) {
    const panel = await panelModels();
    const data = [...panel.data, ...zenModelsPayload()];
    json(res, 200, { object: 'list', data });
  }""",
     """  %s
  async function handleModels(res, callerKey) {
    // 用**调用者自己的**密钥去问面板要模型表：面板会按这个 key 的渠道绑定返回它专属的
    // 清单（池子模型 + 该 key 绑定的外部上游渠道模型）。一律用主密钥去问，绑定过的 key
    // 就永远看不到自己渠道里的模型。取不到就退回主密钥视图（宁可少列，不要空表）。
    let panel;
    try {
      panel = await panelModels(callerKey && callerKey !== cfg.wb2api.api_key ? callerKey : '');
    } catch (e) {
      panel = await panelModels();
    }
    // expose_models=false 时不再把 zen 免费模型披进 /v1/models（详见 README）。
    const data = cfg.zen.expose_models === false
      ? [...panel.data]
      : [...panel.data, ...zenModelsPayload()];
    json(res, 200, { object: 'list', data });
  }""" % MARK2),
]

# ── 3) 转发调用者密钥 ─────────────────────────────────────────────────────
MARK3 = "/* FORWARD-CALLER-KEY v1 */"
PAIRS3 = [
    ("""    headers['content-type'] = 'application/json';
    if (cfg.wb2api.api_key) headers.authorization = `Bearer ${cfg.wb2api.api_key}`;""",
     """    headers['content-type'] = 'application/json';
    %s
    // 转发**调用者自己的**密钥，而不是一律替换成主密钥。面板按密钥做渠道绑定与用量
    // 归属；一律用主密钥转发，面板看到的就永远是"未绑定任何渠道的主密钥"，按 key 绑定
    // 上游渠道整条失效（绑定过的 key 看不到渠道模型、请求掉回账号池）。
    // zen 自己的网关密钥（zen-gw-*）面板不认，那种情况仍用配置的主密钥。
    const inboundKey = bearer(req);
    const zenOwnKey = inboundKey && inboundKey !== cfg.wb2api.api_key &&
      cfg.api_keys.some((k) => safeEqual(k, inboundKey));
    if (cfg.wb2api.api_key && (!inboundKey || zenOwnKey)) {
      headers.authorization = `Bearer ${cfg.wb2api.api_key}`;
    } else if (inboundKey) {
      headers.authorization = `Bearer ${inboundKey}`;
    }""" % MARK3),
    ("""  async function panelModels() {
    if (wb2apiModels.data.length > 0 && now() - wb2apiModels.at < cfg.wb2api.models_ttl_ms) return wb2apiModels;
    try {
      const headers = { accept: 'application/json' };
      if (cfg.wb2api.api_key) headers.authorization = `Bearer ${cfg.wb2api.api_key}`;""",
     """  // callerKey 非空时问出的是"这个 key 专属的"模型表（面板按渠道绑定返回）；空则用
  // 主密钥（管理员视图）。按 key 的清单**不进共享缓存**：各自绑定不同，串了就等于
  // 给别人的 key 看你的渠道模型。
  async function panelModels(callerKey) {
    if (!callerKey && wb2apiModels.data.length > 0 && now() - wb2apiModels.at < cfg.wb2api.models_ttl_ms) return wb2apiModels;
    try {
      const headers = { accept: 'application/json' };
      const useKey = callerKey || cfg.wb2api.api_key;
      if (useKey) headers.authorization = `Bearer ${useKey}`;"""),
    ("""        wb2apiModels = { at: now(), data: Array.isArray(payload?.data) ? payload.data : [], error: '' };
      } finally { clearTimeout(t); }
    } catch (e) {
      wb2apiModels = { ...wb2apiModels, error: e instanceof Error ? e.message : String(e) };
    }
    return wb2apiModels;
  }""",
     """        const fresh = { at: now(), data: Array.isArray(payload?.data) ? payload.data : [], error: '' };
        if (!callerKey) wb2apiModels = fresh; // 只有主密钥视图进共享缓存
        return fresh;
      } finally { clearTimeout(t); }
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (callerKey) return { at: now(), data: [], error: msg };
      wb2apiModels = { ...wb2apiModels, error: msg };
    }
    return wb2apiModels;
  }"""),
    ("      if (path === '/v1/models' && req.method === 'GET') { await handleModels(res); return; }",
     "      if (path === '/v1/models' && req.method === 'GET') { await handleModels(res, key); return; }"),
]

print("1) 只缓存面板明确表态的密钥判定")
edit(P, MARK1, PAIRS1)
print("2) expose_models 开关 + 按调用者取模型表")
edit(P, MARK2, PAIRS2)
print("3) 转发调用者密钥")
edit(P, MARK3, PAIRS3)

# 本机配置：关掉对外宣告 zen 免费模型
cfg = json.load(io.open(CFG, encoding="utf-8"))
if (cfg.get("zen") or {}).get("expose_models") is not False:
    backup(CFG)
    cfg.setdefault("zen", {})["expose_models"] = False
    io.open(CFG, "w", encoding="utf-8").write(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n")
    print("4) 已设置 zen.expose_models = false")
else:
    print("4) zen.expose_models 已是 false，跳过")

print()
rc = subprocess.run([NODE, "--check", P], capture_output=True, text=True)
print("语法:", rc.stderr.strip() or "SYNTAX_OK")
print("权限:", oct(os.stat(P).st_mode)[-3:])
print("下一步: sudo systemctl restart zen-gateway")
