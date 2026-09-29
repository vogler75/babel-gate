# Smart Routing with Laya

BabelGate can expose a virtual model (default `smart-router`) that picks an upstream model per request:

1. A classifier rates the request as `SIMPLE`, `MEDIUM`, `COMPLEX` or `REASONING`.
2. The router tries the targets of that tier in order and skips targets that are cooling down after errors, over budget, or too small for the request's context.
3. If a tier has no targets, the router climbs to the next higher tier. A session never downgrades within `session_affinity_ttl_seconds`.

The classifier can be the built-in keyword heuristic, any HTTP endpoint, or [Laya](https://huggingface.co/) (`laya-serve`), a small self-hosted "System 1" decision model that answers in roughly 30-400 ms without generating text. If Laya is unreachable, slow or unsure (`min_prob`), BabelGate falls back to the heuristic and then to `default_tier`.

## 1. Run Laya (optional)

Laya is a Python package with an HTTP server (`POST /v1/systemone`). The server listens on port 8000 by default.

**Local Python:**

```bash
pip install "laya[serve]"
laya-serve
```

**Docker (CPU):**

```dockerfile
FROM python:3.11-slim-bookworm
ENV PIP_NO_CACHE_DIR=1 HF_HOME=/home/laya/.cache/huggingface LAYA_HOST=0.0.0.0 LAYA_PORT=8000
RUN pip install torch --index-url https://download.pytorch.org/whl/cpu \
 && pip install "laya[serve]"
RUN useradd --create-home laya && mkdir -p /home/laya/.cache/huggingface && chown -R laya /home/laya
USER laya
EXPOSE 8000
CMD ["laya-serve"]
```

```bash
docker build -t laya:local -f Dockerfile.laya .
docker run -d --name laya -p 8000:8000 \
  -e LAYA_PRELOAD=1 -e LAYA_MODELS=english,multilingual -e LAYA_THREADS=4 \
  -v laya-cache:/home/laya/.cache/huggingface \
  laya:local
```

The first start downloads the model weights, about 1.5 GB, into the cache volume. To protect the server, set `LAYA_API_KEY` on the container and put the same value in `smart.classifier.api_key`. BabelGate sends it as a bearer token.

Skip this step to use the built-in heuristic only.

## 2. Configure BabelGate

Add a `smart` section to `config.yaml`. Targets are `provider/model`, using provider names from the `providers` section:

```yaml
smart:
  enabled: true
  model: smart-router
  default_tier: COMPLEX
  session_affinity_ttl_seconds: 3600
  allowed_fails: 2
  cooldown_seconds: 300
  usage_log: logs/usage.jsonl
  classifier:
    type: auto                      # laya when url is set, otherwise heuristic
    url: "http://localhost:8000"
    # api_key: "${LAYA_API_KEY}"
    timeout_ms: 800                 # raise to ~3000 on slow CPUs
    min_prob: 0.40
  tiers:
    SIMPLE:    ["onprem/gpt-oss-120b", "copilot/claude-haiku-4.5"]
    MEDIUM:    ["copilot/claude-haiku-4.5"]
    COMPLEX:   ["copilot/claude-sonnet-5", "anthropic/claude-sonnet-5"]
    REASONING: ["copilot/claude-opus-5.5", "anthropic/claude-opus-5"]
  context_windows:
    onprem/gpt-oss-120b: 131072
    copilot/claude-haiku-4.5: 200000
  budgets:
    copilot:
      limit: 30
      currency: USD
      period_days: 30
      tier_weights: { REASONING: 0.6, COMPLEX: 0.3, MEDIUM: 0.07, SIMPLE: 0.03 }
      default_price: [3, 15, 0.3, 3.75]   # per 1M tokens: input, output, cache read, cache write
```

### Context windows

`context_windows` maps targets to their context size in tokens. Before trying a target, the router estimates `prompt tokens + max_tokens`. If the estimate exceeds the target's window, the target is skipped and the dashboard shows `target (context N>M)`. Skipped targets are still tried last, but only if every other target fails.

With context windows set, clients can use a larger window than the smallest model in the tiers. For example, run Claude Code with `CLAUDE_CODE_MAX_CONTEXT_TOKENS=200000`: long conversations then leave the 131k on-prem model for larger models instead of failing, and Claude Code doesn't compact too early.

### Budgets

`budgets` caps estimated spend per provider over a rolling window. `tier_weights` splits the limit between tiers. A provider that has used up its share for a tier is skipped for that tier. Spend is stored in `database.path`, so it survives restarts.

## 3. Start and verify

```bash
./bin/babelgate -config config.yaml
curl -s http://localhost:4000/api/smart
```

`GET /api/smart` returns:
- the tiers
- the recent routing decisions, with the classifier source, confidence and skipped targets
- budgets and cooldowns
- `context_windows`
- the absolute `usage_log` path

`POST /api/smart` reloads the `smart` section from the config file without a restart. The TUI `r` key does the same.

Each smart request appends one JSON line to `usage_log`, with these fields:

```json
{"ts":1790675761.88,"ok":true,"tier":"COMPLEX","source":"laya","target":"copilot/claude-sonnet-5","provider":"copilot","model":"claude-sonnet-5","in":89285,"out":425,"cache_read":0,"cache_write":0,"cost":0.27,"sec":4.95}
```

Status lines and scripts can read this file for live model, tier and spend information. `usage_log` from `/api/smart` tells them where it is.

## 4. Use it from Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:4000
export ANTHROPIC_AUTH_TOKEN=<server.api_key>
export ANTHROPIC_MODEL=smart-router
export ANTHROPIC_DEFAULT_HAIKU_MODEL=smart-router
export ANTHROPIC_DEFAULT_SONNET_MODEL=smart-router
export ANTHROPIC_DEFAULT_OPUS_MODEL=smart-router
export CLAUDE_CODE_MAX_CONTEXT_TOKENS=200000
claude
```

PowerShell:

```powershell
$env:ANTHROPIC_BASE_URL = "http://localhost:4000"
$env:ANTHROPIC_AUTH_TOKEN = "<server.api_key>"
$env:ANTHROPIC_MODEL = "smart-router"
$env:ANTHROPIC_DEFAULT_HAIKU_MODEL = "smart-router"
$env:ANTHROPIC_DEFAULT_SONNET_MODEL = "smart-router"
$env:ANTHROPIC_DEFAULT_OPUS_MODEL = "smart-router"
$env:CLAUDE_CODE_MAX_CONTEXT_TOKENS = "200000"
claude
```

Prompts containing `ultrathink` or `think harder` always route to `REASONING`. Configure more rules with `smart.keywords`.
