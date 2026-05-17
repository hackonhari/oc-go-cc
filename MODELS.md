# OpenCode Go Models Guide

Comprehensive guide to OpenCode Go models with capabilities, costs, and routing recommendations.

**Source:** [OpenCode Go Documentation](https://opencode.ai/docs/go/)

---

## Phase 1 Architecture (2026-05-15+): Key Pool + Zen Free Fallback

oc-go-cc now manages a **pool of paid OpenCode Go API keys** with automatic rotation on exhaustion, and falls through to **anonymous Zen free models** when all paid keys are dry. This replaces the prior single-key `-pr ocgo` + `-pr ocgo-hk` two-proxy setup.

### Configuration (~/.config/oc-go-cc/config.json)

```json
{
  "host": "127.0.0.1",
  "port": 3456,
  "api_keys": [
    {"token": "sk-...", "account": "ocgo-primary"},
    {"token": "sk-...", "account": "ocgo-hk"},
    {"token": "sk-...", "account": "ocgo-3-fresh"}
  ],
  "free_fallback": {
    "base_url": "https://opencode.ai/zen/v1",
    "models": ["deepseek-v4-flash-free", "qwen3.6-plus-free", "minimax-m2.5-free", "nemotron-3-super-free"]
  },
  "default_model": "deepseek-v4-pro",
  "model_aliases": {...},
  "opencode_go": {...}
}
```

Legacy `api_key: "${OC_GO_CC_API_KEY}"` is auto-migrated to `api_keys[0]` on first startup with a `config.json.backup.<unix-ts>` written.

### State file (~/.config/oc-go-cc/key-states.json)

Runtime mirror of the pool with per-key state:

```json
{
  "api_keys": [
    {
      "token": "sk-...", "account": "ocgo-primary",
      "exhaustedAt": "2026-05-15T11:54:40Z",
      "resetDate": "2026-06-14T00:00:00Z",
      "lastUsed": "2026-05-15T11:54:40Z",
      "weeklyUsagePercent": null,
      "monthlyUsagePercent": null
    }
  ],
  "free_fallback": {...}
}
```

Editable directly — the proxy hot-reloads every 60s. Atomic writes (tmp+rename), RWMutex-guarded.

### Rotation behavior

| Upstream response | Behavior |
|---|---|
| 200 | Success, key remains active, `LastUsed` updated |
| **401 + `CreditsError` body** | Monthly cap hit. Mark exhausted, rotate to next key. |
| **429 + quota keywords in body** | Hard rotate. |
| 429 + `Retry-After ≤60s` + benign body | Transient throttle: retry SAME key after backoff (max 2 retries) |
| 429 + `Retry-After >60s` | Hard rotate. |
| 429 + no header + no quota body | Ambiguous → treated as hard (safer). |
| 5xx | NOT key's fault: return as-is, pool unchanged. |

When **all paid keys exhausted**: handler engages `freepool.Resolver`, hits `opencode.ai/zen/v1` anonymously with first model in `free_fallback.models`. On free model failure, tries next. All free models failed → structured 502 with `next_reset` date.

### Free fallback model catalog

Free models on Zen are anonymous (no auth header required), $0 cost. Verified live 2026-05-15:

| Model ID | Endpoint format | Use |
|---|---|---|
| `deepseek-v4-flash-free` | `/chat/completions` | Recommended default — mirrors paid Flash family |
| `qwen3.6-plus-free` | `/chat/completions` | Strong coding |
| `minimax-m2.5-free` | `/messages` | Long context |
| `nemotron-3-super-free` | `/chat/completions` | NVIDIA 120B reasoning — validated live 2026-05-17 when other 3 were simultaneously 429 |
| `big-pickle` | `/chat/completions` | Reasoning (often 429-locked) |
| `trinity-large-preview-free` | `/chat/completions` | Preview (often 429-locked) |

`ring-2.6-1t-free` graduated to paid 2026-05-17 (see OpenRouter `inclusionai/ring-2.6-1t`) — removed from candidates.

### Dual-endpoint paths

```
POST /v1/messages              → paid pool with rotation → free fallback on exhaustion
POST /v1/chat/completions      → same
POST /free/v1/messages         → SKIPS paid pool, goes straight to free models (mode=forced)
POST /free/v1/chat/completions → same
GET  /health                   → liveness check
```

### `hh-cd --free` flag

`hh-cd -pr ocgo --free` appends `/free` to `ANTHROPIC_BASE_URL`, routing all session traffic to the free-only path. Refuses with non-`ocgo` providers. `--model NAME` works in both modes:
- Paid mode: `--model X` honored upstream
- Free mode: `--model X` honored IF X is a recognized free-tier model (suffix `-free` or `big-pickle`); else falls back to configured chain order

### Operations

```bash
oc-go-cc keys-status                # table view (tokens redacted)
oc-go-cc keys-status --tail         # last 50 rotation events
oc-go-cc keys-status --tail --lines 200
oc-go-cc keys-status --json         # structured (tokens redacted)
```

Rotation event log: `~/.cache/oc-go-cc/rotation-YYYYMMDD.log` (JSONL, daily-rotated by filename). Event types: `key_acquired`, `key_exhausted_hard`, `key_throttle_transient`, `key_reset_autoclear`, `free_fallback_engaged` (with `mode: forced|fallback`), `free_fallback_model_failed`, `all_exhausted_502`.

### Adding a new account

Append to `~/.config/oc-go-cc/key-states.json` → `api_keys[]`. Hot-reload picks it up within 60s.

```json
{
  "token": "sk-new-token-here",
  "account": "ocgo-4",
  "resetDate": "2026-07-01T00:00:00Z"
}
```

OR edit `config.json` → `api_keys[]` and restart the proxy.

### Rollback procedure

Phase 1 deployment 2026-05-15 wrote a timestamped backup bundle. To revert:

```bash
~/.local/bin/oc-go-cc stop
cp ~/.config/oc-go-cc/config.json.backup.20260515-115229 ~/.config/oc-go-cc/config.json
cp ~/.config/oc-go-cc/config-hk.json.backup.20260515-115229 ~/.config/oc-go-cc/config-hk.json
cp ~/.claude/ocgo.env.backup.20260515-115229 ~/.claude/ocgo.env
cp ~/.claude/ocgo-hk.env.backup.20260515-115229 ~/.claude/ocgo-hk.env
rm ~/.local/bin/oc-go-cc
cp ~/.local/bin/oc-go-cc.bin.pre-rotation ~/.local/bin/oc-go-cc
~/.local/bin/oc-go-cc serve --background
~/.local/bin/oc-go-cc serve --background --config ~/.config/oc-go-cc/config-hk.json
```

Old binary kept until 2026-05-22 then can be removed.

### Decommissioned by Phase 1

- `~/.config/oc-go-cc/config-hk.json` — merged into unified `api_keys[]`
- `~/.claude/ocgo-hk.env` — `-pr ocgo-hk` no longer needed
- Second proxy instance on port 3457 — single proxy on 3456 owns all keys
- `hh-cd -pr ocgo-hk` — error on use (env file gone)

---


## Quick Cost Comparison

> 💰 **Cost-conscious routing matters!** GLM-5.1 gives you 880 requests per 5-hour block, while Qwen3.5 Plus gives you **10,200** — that's **11.6x more requests** for the same $12 budget.

| Model            | Requests per $12 (5hr) | Cost Efficiency | Quality |
| ---------------- | ---------------------- | --------------- | ------- |
| **Qwen3.5 Plus** | **10,200**             | ★★★★★           | ★★☆☆☆   |
| **MiniMax M2.5** | **6,300**              | ★★★★★           | ★★☆☆☆   |
| **MiniMax M2.7** | **3,400**              | ★★★★☆           | ★★★☆☆   |
| **Qwen3.6 Plus** | **3,300**              | ★★★★☆           | ★★★☆☆   |
| **MiMo-V2-Omni** | **2,150**              | ★★★☆☆           | ★★★☆☆   |
| **Kimi K2.5**    | **1,850**              | ★★☆☆☆           | ★★★★☆   |
| **MiMo-V2-Pro**  | **1,290**              | ★★☆☆☆           | ★★★★☆   |
| **Kimi K2.6**    | **~1,150**             | ★☆☆☆☆           | ★★★★★   |
| **GLM-5**        | **1,150**              | ★☆☆☆☆           | ★★★★☆   |
| **GLM-5.1**      | **880**                | ☆☆☆☆☆           | ★★★★★   |

## Important: API Endpoints

⚠️ **Critical:** Not all models use the same API endpoint! oc-go-cc handles this automatically, but you should know:

| Models                                                                                                            | Endpoint                                         | Format                   |
| ----------------------------------------------------------------------------------------------------------------- | ------------------------------------------------ | ------------------------ |
| GLM-5, GLM-5.1, Kimi K2.6, Kimi K2.5, MiMo-V2-Pro, MiMo-V2-Omni, Qwen3.5 Plus, Qwen3.6 Plus, DeepSeek V4 Pro/Flash | `https://opencode.ai/zen/go/v1/chat/completions` | OpenAI-compatible        |
| **MiniMax M2.5, MiniMax M2.7**                                                                                    | `https://opencode.ai/zen/go/v1/messages`         | **Anthropic-compatible** |

**Why this matters:** MiniMax models expect Anthropic format natively. oc-go-cc detects MiniMax models and routes them to the correct endpoint automatically without transformation. This means MiniMax models work seamlessly with Claude Code.

DeepSeek V4 Pro and Flash are OpenAI-compatible in OpenCode Go. oc-go-cc transforms Claude Code's Anthropic request into OpenAI Chat Completions format, including tools, tool results, thinking history, `reasoning_effort`, and `thinking`.

For Claude Code and OpenCode-style agent workflows, DeepSeek V4 supports max thinking mode with:

```json
{
  "model_id": "deepseek-v4-pro",
  "reasoning_effort": "max",
  "thinking": {
    "type": "enabled"
  }
}
```

Use `deepseek-v4-pro` for default, complex, thinking, and long-context routing. Use `deepseek-v4-flash` for fast, background, budget, or subagent-style workloads.

## Cost-Conscious Routing Strategy

### Default to Cheap, Upgrade When Necessary

**Most requests should use cheap models.** Only upgrade to expensive models when:

1. **Task complexity demands it** (multi-step reasoning, architecture)
2. **You've tried cheaper models and they failed**
3. **Code quality is critical** (production code review)

### Recommended Routing

```json
{
  "models": {
    "budget": {
      // Default for most tasks
      "model_id": "qwen3.6-plus",
      "max_tokens": 4096
    },
    "background": {
      // Simple operations
      "model_id": "qwen3.5-plus",
      "max_tokens": 2048
    },
    "default": {
      // Better quality, moderate cost
      "model_id": "kimi-k2.6",
      "max_tokens": 4096
    },
    "long_context": {
      // Large files only
      "model_id": "minimax-m2.5",
      "context_threshold": 80000
    },
    "think": {
      // Reasoning tasks
      "model_id": "glm-5",
      "max_tokens": 8192
    },
    "complex": {
      // Complex architecture only
      "model_id": "glm-5.1",
      "max_tokens": 4096
    }
  }
}
```

### Decision Tree

```
Is context > 80K tokens?
├── YES → Use MiniMax M2.5 (1M context, 6,300 req/$12)
│
Is it a simple task (read file, grep, list dir)?
├── YES → Use Qwen3.5 Plus (10,200 req/$12)
│
Is it a reasoning/planning task?
├── YES → Use GLM-5 (1,150 req/$12)
│
Is it complex architecture or critical code?
├── YES → Use GLM-5.1 (880 req/$12)
│
Default → Use Kimi K2.6 (1,850 req/$12, ★★★★★) or Qwen3.6 Plus (3,300 req/$12)
```

## Detailed Model Profiles

### Budget Champions 💰

#### Qwen3.5 Plus — The Workhorse

- **Model ID:** `qwen3.5-plus`
- **Cost:** **10,200 requests per $12** (best value!)
- **Context:** ~128K tokens
- **Quality:** ★★☆☆☆ (adequate for simple tasks)
- **Best For:**
  - File reading operations
  - Directory listing
  - Grep/search
  - Simple questions
  - Bulk operations
  - Background tasks
- **When to Use:** When you need to do lots of operations cheaply

#### MiniMax M2.5 — Long Context on a Budget

- **Model ID:** `minimax-m2.5`
- **Endpoint:** **Anthropic-compatible** (`/v1/messages`)
- **Cost:** **6,300 requests per $12**
- **Context:** **~1M tokens** (1 million!)
- **Quality:** ★★☆☆☆ (acceptable)
- **Speed:** Fast
- **Best For:**
  - Very large files
  - Long conversations
  - Multi-file context
- **When to Use:** When you need 1M context but want to minimize cost
- **Note:** Uses Anthropic endpoint - oc-go-cc handles this automatically

### Balanced Models (Quality + Cost)

#### DeepSeek V4 Pro — Agentic Coding + Max Thinking

- **Model ID:** `deepseek-v4-pro`
- **Endpoint:** **OpenAI-compatible** (`/chat/completions`)
- **Context:** **~1M tokens**
- **Quality:** ★★★★★
- **Best For:**
  - Claude Code agent workflows
  - Complex implementation and debugging
  - Architecture and refactoring
  - Long-context coding tasks
  - Max thinking mode
- **Recommended Config:**

  ```json
  {
    "provider": "opencode-go",
    "model_id": "deepseek-v4-pro",
    "temperature": 0.1,
    "max_tokens": 8192,
    "reasoning_effort": "max",
    "thinking": {
      "type": "enabled"
    }
  }
  ```

#### DeepSeek V4 Flash — Fast Agent Workloads

- **Model ID:** `deepseek-v4-flash`
- **Endpoint:** **OpenAI-compatible** (`/chat/completions`)
- **Context:** **~1M tokens**
- **Quality:** ★★★★☆
- **Best For:**
  - Fast routing
  - Background tasks
  - Budget routing
  - Subagent-style work
  - Fallback for DeepSeek V4 Pro
- **Recommended Config:**

  ```json
  {
    "provider": "opencode-go",
    "model_id": "deepseek-v4-flash",
    "temperature": 0.1,
    "max_tokens": 4096,
    "reasoning_effort": "max",
    "thinking": {
      "type": "enabled"
    }
  }
  ```

#### Qwen3.6 Plus — Cost-Effective General Coding ⭐ RECOMMENDED DEFAULT

- **Model ID:** `qwen3.6-plus`
- **Cost:** **3,300 requests per $12** (3.8x more than GLM-5.1!)
- **Context:** ~128K tokens
- **Quality:** ★★★☆☆ (good enough for most tasks)
- **Speed:** Fast
- **Best For:**
  - General coding (default choice)
  - Feature implementation
  - Bug fixes
  - Refactoring
- **When to Use:** Default for cost-conscious users

#### Kimi K2.6 — Best Quality at Balanced Cost

- **Model ID:** `kimi-k2.6`
- **Cost:** **~1,850 requests per $12**
- **Context:** ~256K tokens (successor to K2.5 with improvements)
- **Quality:** ★★★★★ (excellent — successor improvements)
- **Speed:** Fast
- **Best For:**
  - Complex coding tasks
  - Code review
  - Architecture discussions
  - General-purpose default (best quality-to-cost ratio)
- **When to Use:** Default choice — better quality than K2.5 at similar cost

#### Kimi K2.5 — Quality + Reasonable Cost (Predecessor)

- **Model ID:** `kimi-k2.5`
- **Cost:** **1,850 requests per $12**
- **Context:** ~256K tokens (2x most others)
- **Quality:** ★★★★☆ (excellent)
- **Speed:** Fast
- **Best For:**
  - Complex coding tasks
  - Code review
  - Architecture discussions
  - When you need better quality than budget models
- **When to Use:** When quality matters more than maximum cost savings

### Premium Models (Use Sparingly!)

#### GLM-5 — Reasoning Specialist

- **Model ID:** `glm-5`
- **Cost:** **1,150 requests per $12** (9x more expensive than Qwen3.5 Plus!)
- **Context:** ~200K tokens
- **Quality:** ★★★★☆ (excellent)
- **Best For:**
  - Multi-step reasoning
  - Complex planning
  - Algorithm design
  - Difficult debugging
- **When to Use:** When reasoning/planning is required and budget models fail

#### GLM-5.1 — Maximum Quality

- **Model ID:** `glm-5.1`
- **Cost:** **880 requests per $12** (11.6x more expensive than Qwen3.5 Plus!)
- **Context:** ~200K tokens
- **Quality:** ★★★★★ (best available)
- **Speed:** Moderate
- **Best For:**
  - Critical architectural decisions
  - Complex multi-file refactoring
  - Production code review
  - When you need the absolute best quality
- **When to Use:** Only when cheaper models can't handle the task

## Usage Limits

OpenCode Go limits:

- **5-hour limit:** $12 of usage
- **Weekly limit:** $30 of usage
- **Monthly limit:** $60 of usage

### Cost Comparison Example

**Scenario:** You want to make 5,000 requests this month.

| Model        | Cost | Can you do it?        |
| ------------ | ---- | --------------------- |
| Qwen3.5 Plus | ~$6  | ✅ Yes, easily        |
| MiniMax M2.5 | ~$10 | ✅ Yes                |
| Qwen3.6 Plus | ~$18 | ✅ Yes                |
| Kimi K2.5    | ~$32 | ❌ Exceeds $30 weekly |
| GLM-5        | ~$52 | ❌ Exceeds limits     |
| GLM-5.1      | ~$68 | ❌ Exceeds limits     |

### Optimizing Your Usage

**Strategy 1: Tiered Approach**

```
1. Start with Qwen3.6 Plus (cheap, good quality)
2. If it fails, try Kimi K2.5 (better quality)
3. If still failing, use GLM-5 (reasoning)
4. Only for critical tasks: GLM-5.1 (premium)
```

**Strategy 2: Task-Based Selection**

```
Background ops (grep, ls, cat) → Qwen3.5 Plus
General coding → Qwen3.6 Plus or Kimi K2.5
Complex features → Kimi K2.5
Architecture/Planning → GLM-5
Critical review → GLM-5.1 (rarely)
```

## Fallback Chains for Cost Efficiency

```json
{
  "fallbacks": {
    "budget": [{ "model_id": "kimi-k2.6" }, { "model_id": "mimo-v2-pro" }],
    "background": [
      { "model_id": "qwen3.6-plus" },
      { "model_id": "minimax-m2.5" }
    ],
    "long_context": [{ "model_id": "minimax-m2.5" }],
    "default": [{ "model_id": "mimo-v2-pro" }, { "model_id": "qwen3.6-plus" }],
    "think": [{ "model_id": "kimi-k2.6" }],
    "complex": [{ "model_id": "glm-5" }]
  }
}
```

**Rule of thumb:** If a task succeeds with a cheap model, it doesn't need an expensive one. Only fall back to expensive models when necessary.

## Quick Reference

| Task Type             | Recommended  | Cost (req/$12) | Fallback     |
| --------------------- | ------------ | -------------- | ------------ |
| Read file, ls, grep   | Qwen3.5 Plus | 10,200         | Qwen3.6 Plus |
| General coding        | Qwen3.6 Plus | 3,300          | Kimi K2.5    |
| Complex features      | Kimi K2.6    | 1,850          | Kimi K2.5    |
| Long context (>80K)   | MiniMax M2.5 | 6,300          | MiniMax M2.7 |
| Reasoning/planning    | GLM-5        | 1,150          | Kimi K2.5    |
| Critical architecture | GLM-5.1      | 880            | GLM-5        |
| Bulk operations       | Qwen3.5 Plus | 10,200         | MiniMax M2.5 |

## Cost-Saving Tips

1. **Use Qwen3.6 Plus as default** — 3,300 req/$12 is plenty for most tasks
2. **Reserve GLM-5.1 for critical tasks only** — 880 req/$12 drains budget fast
3. **Use Qwen3.5 Plus for simple operations** — 10,200 req/$12 is unbeatable
4. **MiniMax M2.5 for long context** — 6,300 req/$12 with 1M context is amazing value
5. **Monitor your usage** in the [OpenCode console](https://opencode.ai/auth)

## See Also

- [OpenCode Go Documentation](https://opencode.ai/docs/go/)
- [oc-go-cc Configuration](../configs/config.example.json)
- [README.md](../README.md) for setup instructions
