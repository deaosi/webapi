# zen-gateway

One base URL, one API key: the existing **wb2api panel** models and the
**OpenCode Zen free-lane** models behind a single OpenAI-compatible endpoint.

```
client ──▶ https://wb2api.fanwulink.cn/zen/v1   (one key)
              │
              ├── cn:* / global:* / bare names ──▶ wb2api panel (127.0.0.1:7863), byte-passthrough
              └── zen:*                        ──▶ opencode.ai/zen (anonymous free lane)
```

## Why a sidecar

The panel is a Go gateway wired end to end for Tencent CodeBuddy's private
protocol (`copilot.tencent.com`). Zen's free lane is a different world:
different host, different auth (the literal `Bearer public`), and a
client-identity gate. Adding Zen models *inside* the panel would mean forking
that protocol stack and rebasing on every upstream change. This gateway is a
separate process, so the panel binary, its config, its data and its accounts
stay untouched — rollback is "stop the service, drop 3 lines of Caddy".

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/models` | merged catalog: panel models (names unchanged) + `zen:*` free models |
| POST | `/v1/chat/completions` | routed by model prefix: `zen:*` → Zen, everything else → panel |
| POST | `/v1/systemone` | `zen:jev-1.13-free` structured-decision passthrough |
| GET | `/healthz` | liveness (no key) |
| GET | `/status` | catalog/breaker/inflight/usage view (key required) |

Auth is a bearer gateway key. The panel's own key is injected server-side and
never leaves the process — clients only ever hold the gateway key.

## Model names

- Panel models keep their exact names, including the panel's own realm prefix:
  `cn:deepseek-v4.1-flash`, `cn:hy4-preview-f`, `global:…`.
- Zen free models are prefixed `zen:` — e.g. `zen:mimo-v2.6-flash-free`,
  `zen:space-bunny-free`, `zen:muse-spark-1.3-contributor-free`.
- `zen:jev-1.13-free` is listed but is **not** a chat model; call it through
  `/v1/systemone`.

## Free-lane mechanics (and why they are what they are)

The anonymous lane is the same one OpenCode's own CLI uses. Since 2026-09-16 it
checks more than the user agent, so the gateway reproduces the whole client
identity:

1. `Authorization: Bearer public` plus the CLI header set
   (`user-agent: opencode/…`, `x-opencode-client`, session/affinity/request/project).
2. A canonical session id (`ses_` + 12 hex + 14 base62), derived
   deterministically from the conversation's first user turn, so a
   conversation keeps one stable upstream session.
3. An agent-shaped body: streaming **and** function tools named `bash` and
   `read`. Plain chats get inert stubs plus `tool_choice: "none"`, so the
   model never actually calls them.
4. `reasoning_effort`: `off` is sent as `none` (the only spelling that stops
   the always-thinking free models); values a model did not declare are dropped
   rather than risking a 400.

`muse-spark-*` is Responses-only on Zen (500 on `/chat/completions`), so the
gateway translates chat → `/v1/responses` and streams the responses events back
as chat chunks.

## Limits

Measured on the production exit IP (2026-10-06, `mimo-v2.6-flash-free`,
8-token requests):

| concurrency | success | latency p50 |
|---|---|---|
| 1 | 3/3 | 4.8 s |
| 3 | 6/6 | 14.5 s |
| 5 | 10/10 | 29.4 s |
| 8 | 8/8 | 57.9 s |

The upstream does not reject on concurrency — it degrades. So the gateway
**queues** (default: 3 in flight per model, 8 global) instead of failing, and a
per-model breaker cools a 429 for 60 s doubling to 30 min.

Watchdogs: 30 s to first byte, 120 s of mid-stream silence (300 s for
Responses models). A stream that dies before any content is retried once; a
partially delivered stream is never replayed.

## Data-handling caveat

The Zen free models have their own terms. As of 2026-10-06:

- data may be used for model improvement: `big-pickle`, `fledge-alpha-free`,
  `mimo-v2.5-free`, `mimo-v2.6-flash-free`, `ling-*`;
- NVIDIA trial endpoints — *do not submit confidential data*:
  `nemotron-3-ultra-free`, `nemotron-3.5-lightning-free`;
- contributor model (prompts used to train Meta models):
  `muse-spark-1.3-contributor-free`;
- zero-retention declared: `space-bunny-free`, `longcat-2.5-preview-free`.

Pick models accordingly. Request logs never contain prompts unless
`logging.log_prompts` is explicitly enabled.

## Known limitations

- **Reasoning models can spend the whole budget thinking.** With a small
  `max_tokens`, the always-thinking free models (space-bunny, big-pickle,
  mimo, nemotron, ling) may return `finish_reason: "length"` and an empty
  `content` — the tokens went to `reasoning_content`, which the gateway passes
  through untouched. Give them room (or set `reasoning_effort: "off"`, which
  the gateway forwards as the wire value `none`). The gateway does **not**
  silently replay the request: that would double the upstream call and the
  quota cost without telling you.
- **One exit IP, shared quota.** The gateway and the panel leave the same host,
  so Zen's per-IP anonymous quota is shared with nothing else but is still a
  single bucket. There is deliberately no multi-exit proxy pool: the server has
  one egress and adding third-party proxies would mean routing prompts through
  untrusted nodes.
- **Region blocks are model-specific and exit-specific.** `muse-spark-*`
  answers 403 `RegionError` from some networks and works fine from others; the
  gateway reports it as 403 with the upstream wording (never as an auth error)
  and cools that model for the bounded window.
- **The lane is undocumented.** It is the path OpenCode's own CLI uses, and the
  identity gate has changed before (2026-09-16). The disguise lives in one
  module (`src/disguise.mjs` + `src/freelane.mjs`) so a change is a one-file fix.

## Run

```sh
node src/server.mjs --config /etc/zen-gateway/config.json
```

Configuration: copy `config.example.json`, set `api_keys` (gateway keys) and
`wb2api.api_key` (the panel's existing key). No third-party runtime
dependencies — Node 20+ built-ins only.

## Test

```sh
npm test        # 56 tests: disguise, free-lane shaping, catalog, translation, routing, limits
```

Tests run fully offline against a stubbed upstream.

## Deploy

See `deploy/`:

- `zen-gateway.service` — hardened systemd unit
- `install.sh` — install/refresh the code, config and unit on the server
- `caddy.snippet` — the 3-line Caddy route to paste into the existing site block
## Deployment record

Deployed and verified on 2026-10-06 against the live panel host:

- code at `/opt/zen-gateway/current`, systemd `zen-gateway.service`
  (hardened like `wb2api.service`), listening on `127.0.0.1:7864`;
- Caddy route `handle_path /zen/*` inside the existing
  `wb2api.fanwulink.cn` site block (Caddyfile backed up before the edit);
- rollback = stop/disable the service + drop those 3 lines + `caddy reload`.
  The panel binary, config, data and accounts were never touched.

Acceptance run against the public URL:

| Check | Result |
|---|---|
| `/zen/healthz` | 200 `{"status":"ok"}` |
| `/zen/v1/models` | 200 — 31 models = 19 panel + 12 zen, no duplicates |
| missing / wrong key | 401 |
| `zen:space-bunny-free` real chat | 200, content `PUBLIC-OK`, `finish_reason: stop` |
| `zen:mimo-v2.6-flash-free` real chat | 200, content `MIMO-OK` |
| `zen:jev-1.13-free` via `/zen/v1/systemone` | 200, `noul: 0.86` |
| **regression**: `/v1/models` with the panel key | 200, 19 models, unchanged |
| `/zen/status` | zen ready (86 live / 11 exposed), panel 19, no errors |

Notes from the live run:

- `muse-spark-1.3-contributor-free` answers 403 `RegionError` from some exit
  IPs and works from others — a property of the network, not of the gateway;
- `space-bunny-free` streams `reasoning_content` deltas, which pass through
  untouched so clients can show the model thinking;
- Zen's usage payload nests `prompt_tokens_details`; the gateway parses events
  rather than regex-matching them, so accounting stays exact.

## Layout

```
src/server.mjs      HTTP surface, auth, routing, merged catalog, logging
src/disguise.mjs    CLI identity headers + canonical session ids
src/freelane.mjs    gate tools, effort mapping, upstream error taxonomy
src/catalog.mjs     zen catalog: live list ∩ models.dev free decision + seeds
src/responses.mjs   Responses <-> chat translation (muse-spark)
src/forward.mjs     upstream fetch, watchdogs, SSE passthrough, retry
src/semaphore.mjs   per-model + global in-flight caps with FIFO queueing
src/breaker.mjs     per-model 429 cooldown
deploy/             systemd unit, installer, Caddy route snippet
```
