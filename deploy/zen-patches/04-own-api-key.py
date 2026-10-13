# -*- coding: utf-8 -*-
"""给 zen 侧车加"用你自己的 OpenCode Console API key"的支持（幂等）。

有 key 时：
  - 用 `Bearer <你的key>`，并且**不再发任何伪装头**（伪造的 UA / x-opencode-client /
    session id 全部去掉）——用自己的凭据就是正常调用，没有伪装的必要。
没 key 时：行为与现在完全一致（匿名 public + 伪装），不影响既有链路。
"""
import io
import shutil
import subprocess
import sys
import time

try:
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

S = "/opt/zen-gateway/current/src"
MARK = "/* ZEN-OWN-KEY v1 */"

# ── 1) disguise.mjs ──
P1 = S + "/disguise.mjs"
s1 = io.open(P1, encoding="utf-8").read()

OLD_D = """export function disguiseHeaders(ids) {
  return {
    authorization: `Bearer ${ANONYMOUS_KEY}`,"""
NEW_D = """/**
 * 上游请求头。
 *
 * realKey 非空 = 用**你自己的** OpenCode Console API key 调用（官方文档明确支持
 * "with OpenCode or another coding agent"）。这种情况下**不再发任何伪装头**——
 * 伪造 UA / x-opencode-client / session id 是为了蹭匿名免费额度，
 * 用真凭据时就既没必要、也不该做。
 *
 * realKey 为空 = 维持原行为（匿名 public 免费通道 + CLI 伪装）。
 */
export function disguiseHeaders(ids, realKey) {
  if (realKey) {
    return {
      authorization: `Bearer ${realKey}`,
      accept: 'application/json, text/event-stream',
      'content-type': 'application/json',
    };
  }
  return {
    authorization: `Bearer ${ANONYMOUS_KEY}`,"""

OLD_D_TAIL = """    accept: 'application/json, text/event-stream',
    'content-type': 'application/json',
  };
}"""
NEW_D_TAIL = """    accept: 'application/json, text/event-stream',
    'content-type': 'application/json',
  };
}"""

# ── 2) server.mjs ──
P2 = S + "/server.mjs"
s2 = io.open(P2, encoding="utf-8").read()

OLD_DEF = "    zen: { base: 'https://opencode.ai/zen', catalog_ttl_ms: 600000, metadata_ttl_ms: 86400000, metadata_cache: '', expose_models: true },"
NEW_DEF = ("    zen: { base: 'https://opencode.ai/zen', catalog_ttl_ms: 600000, metadata_ttl_ms: 86400000, "
           "metadata_cache: '', expose_models: true, api_key: '' },")

OLD_CALL = "    const headers = disguiseHeaders(ids);"
NEW_CALL = ("    // 配了 zen.api_key 就用你自己的凭据（官方支持的用法），否则走匿名免费通道。\n"
            "    const headers = disguiseHeaders(ids, cfg.zen.api_key);  %s" % MARK)

# 先检查
checks = [
    ("disguise.mjs 头部", OLD_D in s1),
    ("server.mjs defaults", OLD_DEF in s2),
    ("server.mjs 调用点 x2", s2.count(OLD_CALL) == 2),
]
for label, ok in checks:
    if not ok:
        print("ANCHOR_MISSING:", label)
        sys.exit(2)

if MARK in s2:
    print("ALREADY_PATCHED")
    sys.exit(0)

stamp = time.strftime("%Y%m%d-%H%M%S")
for p in (P1, P2):
    shutil.copy2(p, "%s.bak-%s" % (p, stamp))
print("备份: *.bak-%s" % stamp)

s1 = s1.replace(OLD_D, NEW_D, 1)
s2 = s2.replace(OLD_DEF, NEW_DEF, 1).replace(OLD_CALL, NEW_CALL)

for p, txt in ((P1, s1), (P2, s2)):
    subprocess.run(["chmod", "u+w", p], check=True)
    try:
        io.open(p, "w", encoding="utf-8", newline="").write(txt)
    finally:
        subprocess.run(["chmod", "444", p], check=True)

print("已写入")
NODE = "/opt/skilldesk-platform/runtime/node/bin/node"
for p in (P1, P2):
    rc = subprocess.run([NODE, "--check", p], capture_output=True, text=True)
    print("  语法 %s: %s" % (p.split("/")[-1], rc.stderr.strip() or "OK"))
for pat in ("ZEN-OWN-KEY v1", "disguiseHeaders(ids, cfg.zen.api_key)", "api_key: ''"):
    n = subprocess.run("grep -F -c '%s' %s" % (pat, P2), shell=True,
                       capture_output=True, text=True).stdout.strip()
    print("  %-40s x%s" % (pat, n))
