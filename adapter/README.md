# 三协议适配层（adapter）

Panel（Go）原生只讲 Chat（`POST /v1/chat/completions`、`GET /v1/models`）。
本目录的 `proto_adapter.py` 是一个无状态翻译层，对齐 Hub `wb_proxy.py`
的 Responses / Messages 翻译语义（去掉 web 代跑、DSML、引用标注、
`previous_response_id` 存储），让同一套上游账号同时吃三种客户端：

| 客户端 | 协议 | 地址（服务端公网） | 地址（本地开发） |
|---|---|---|---|
| OpenAI SDK | Chat（透传） | `https://wb2api.fanwulink.cn/adapter/v1/chat/completions` | `http://127.0.0.1:8788/v1/chat/completions` |
| Codex | Responses | `.../adapter/v1/responses` | `...:8788/v1/responses` |
| Claude Code | Messages | `.../adapter/v1/messages`（Base URL 去掉末尾 `/v1`） | `...:8788/v1/messages` |

模型名保持网关路由协议（`cn:xxx` / `global:xxx`）；Anthropic 客户端接受
`Bearer` 或 `x-api-key` 鉴权，同一把调用密钥通用（见面板「API 密钥」页）。

## 文件

- `proto_adapter.py` —— 本体，stdlib only，无 pip 依赖
- `test_adapter.py` —— 纯函数单测（`python test_adapter.py`，13 项）
- `deploy/wb2api-adapter.service` —— systemd 单元（监听 `127.0.0.1:8788`）
- `deploy/caddy-adapter.snippet` —— Caddy 片段（`/adapter/*` 剥前缀反代 8788）

## 本地跑

```bash
python adapter/proto_adapter.py --listen 127.0.0.1:8788 --panel http://127.0.0.1:8888
python adapter/test_adapter.py   # 13/13 PASS
```

## 服务端部署

```bash
sudo install -d -o wb2api -g wb2api -m 755 /opt/wb2api-adapter
sudo install -o wb2api -g wb2api -m 644 adapter/proto_adapter.py /opt/wb2api-adapter/
sudo install -o root -g root -m 644 adapter/deploy/wb2api-adapter.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now wb2api-adapter
# Caddy site 内合入 adapter/deploy/caddy-adapter.snippet 后 reload
```

## 边界

- `previous_response_id` 直接 400（无状态，传完整 `input` 代替）
- 服务端工具声明只透传/降级不代跑；`thinking` 块丢弃；`count_tokens` 为估算值
- 直连 panel（`:7863`）仍只有 Chat，打 `/v1/responses`、`/v1/messages` 是 404
