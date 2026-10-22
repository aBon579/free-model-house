# M1 验收记录：免密车道完整跑通 ✅

> ⚠️ **术语说明**：本文记录的是 **2026-10-19** 那次排查，
> 当时的开关叫「跨车道回落」。
> 后来它被改名为「**自动跨模型调用**」，语义也从"换车道"
> 明确成了"**换模型**"（关闭时只重试当前模型，一个模型都不换）。
> 详见 `docs/design/00-overview.md` §5.4.3。


> 日期：2026-10-19
> 模型：`minimax-m3.1-flash-free`

---

## 结论

> **✅ 免密车道端到端跑通 —— 真实的 200 + 正确的模型回答。**
>
> ```
> ✅ 成功。finish=stop usage=Input:975 Output:27 Total:1002
>    回答: "FMH-OK"
>    思考: "The user wants exactly \"FMH-OK\". I should reply with that. No tool calls needed."
> ```

这是 FMH **最关键**的一步 —— 免密车道通不通，决定整个项目有没有意义。
现在它通了。

---

## 逐个模型的实测结果（同一时刻，同一出口 IP）

| 模型 | 结果 | 错误 |
|---|---|---|
| `minimax-m3.1-flash-free` | ✅ **成功** | — |
| `deepseek-v4.1-flash-free` | ❌ | `RATE_LIMIT` |
| `glm-5.3-flash-free` | ❌ | `RATE_LIMIT` |
| `longcat-2.5-preview-free` | ❌ | `RATE_LIMIT` |
| `mimo-v2.6-flash-free` | ❌ | `RATE_LIMIT` |
| `nemotron-3.5-super-vl-free` | ❌ | `RATE_LIMIT` |
| `beam-free` | ❌ | `REGION_BLOCKED` |
| `big-pickle` | ❌ | `RATE_LIMIT` |

### 这组数据比"成功一次"更有价值

**6 个模型限流，1 个地域封锁，只有 1 个能答。**

这直接印证了三件事：

1. **限流自动切换不是锦上添花，是必需品。**
   如果客户端点名要 `deepseek-v4.1-flash-free`，它会拿到 429；
   如果网关能自动切到 `minimax-m3.1-flash-free`，用户就拿到了回答。

2. **错误分类必须准。**
   `RATE_LIMIT`（换模型有用）和 `REGION_BLOCKED`（换出口有用）
   和 `FREE_TIER_BLOCKED`（改指纹有用）必须分开 —— 我们已经分了。

3. **"清单里能拉到"≠"能用"。**
   上游清单 85 个模型，**免费车道只有 10 个**，
   而这 10 个里当下能答的只有 1 个。模型清单必须配**实测**与**限流账本**。

---

## 已验证通过的全部闸门维度

| 维度 | 状态 | 证据 |
|---|---|---|
| 端点 `/inference/v1/chat/completions` | ✅ | 真实 200 |
| UA `opencode/latest/2.0.26/cli` | ✅ | 通过身份闸门 |
| `authorization: Bearer public` | ✅ | 通过身份闸门 |
| 四个会话头同值 | ✅ | 通过身份闸门 |
| 会话 ID 降序编码 | ✅ | 通过身份闸门 |
| `read`/`shell` 工具定义逐字 | ✅ | 通过身份闸门 |
| `tool_choice` 不下发 `none` | ✅ | 通过身份闸门 |
| 思考内容 | ✅ | 拿到了 reasoning |
| usage 计量 | ✅ | Input:975 Output:27 |
| 清单接口 | ✅ | 200，10 个免费模型 |

---

## 清单接口的实测细节

### 1. 两个端点都能用

实测（2026-10-19），两个都返回 200 和同样的 85 个模型：

```
/zen/v1/models         -> 200, 85 models
/inference/v1/models   -> 200, 85 models
```

FMH 统一用 `/inference/v1/models`（更“新”的那个）。

### 2. ⚠️ 上游**不声明任何容量**

实测 85 行的字段只有：

```json
{"id": "...", "object": "model", "created": 1792418342, "owned_by": "opencode"}
```

**一个容量字段都没有** —— 没有 `context_length`，没有 `max_output_tokens`，
没有 `architecture.input_modalities`，没有 `supported_parameters`。

**所以容量必须来自本地规则表**（`internal/lane/catalog.go` 的 `capabilityRows`），
清单只负责给出**权威的 id 集合**。

> `BuildCatalog` 的做法是"清单给 id，本地表给能力"。

### 3. ⚠️ 清单里**混着付费模型**

85 个 id 里只有 9 个匹配 free 模式，加上 `big-pickle` 共 10 个在免费车道上。

**必须过滤**，否则客户端会拿到一堆必然失败的 id。
`IsFreeLane()` 就是干这个的。

### 4. 清单里**没有** `muse-spark-*-contributor-free`

实测真实的 id 是 `muse-spark-1.2` / `muse-spark-1.3`（**没有** `-contributor-free` 后缀）。

但这两个**不在免费车道上**（不匹配 free 模式），
所以过滤后不会出现 —— 但我们的 wire 选择规则要认得它们。

实测的免费车道全名单（2026-10-19）：

```
big-pickle
beam-free
deepseek-v4.1-flash-free
glm-5.3-flash-free
jev-1.13-free
longcat-2.5-preview-free
mimo-v2.6-flash-free
minimax-m3.1-flash-free
nemotron-3.5-lightning-free
nemotron-3.5-super-vl-free
```

---

## 这次复查发现并修掉的实现问题

### 1. 清单请求用了 POST（应 GET）

**症状**：拉清单 404 → 报成 `SERVER` + 空消息。
**根因**：`postJSON` 硬编码了 POST，但清单端点是 GET-only。
**修法**：拆出 `getJSON`。

### 2. `Peek` 不消费字节却又 replay → 第一个帧读两遍

**症状**：客户端收到**重复**的内容（"hihi" 而不是 "hi"）。
**根因**：`bufio.Peek` 不消费字节，我却又把读到的字节 replay 了一次。
**修法**：改用 `Read`（消费）再 replay。
> ⚠️ 这个 bug 纯读代码极难发现 —— 是测试抓到的。

### 3. `FREE_TIER_BLOCKED` 的可切换性判断错了

**症状**：跨车道回落用不上。
**根因**：我把"换模型"和"原地重试"混成了一个判断。
**修法**：拆成 `Switchable()`（换候选）与 `RetrySameModel()`（原地重试）两个维度。
> 429 的答案是"**换模型**，不原地重试"（配额按 session 记账）。

### 4. 上下文超限被当成可切换

**症状**：换个模型也一样超，却还在重试。
**根因**：400 + "context length" 落进了 `CodeServer`。
**修法**：加 `contextRe`，归到 `CodeClient`（request scope）。

### 5. 清单解析假设了不存在的字段

**症状**：`context_length` 永远是 0。
**根因**：我假设上游声明容量 —— 实测它不声明。
**修法**：删掉那些字段，容量一律走本地规则表。

---

## 复现方式

```bash
FMH_LIVE=1 go test ./internal/lane/ -run Live -v

# 指定模型
FMH_LIVE=1 FMH_TEST_MODEL=minimax-m3.1-flash-free \
  go test ./internal/lane/ -run TestLiveZenChat -v
```

⚠️ 直播测试**会消耗免费车道的额度**，而且依赖网络。默认跳过。

---

## 下一步

M1 完成。**M2（OpenRouter 车道）现在是必需项，不是可选项** ——
因为当前 IP 下 6/10 的免费模型都在限流，而 OpenRouter 的免费模型用自己的 Key，
不受 IP 信誉影响。
