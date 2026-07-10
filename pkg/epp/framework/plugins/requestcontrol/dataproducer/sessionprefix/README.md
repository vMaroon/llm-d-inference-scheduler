# Session Prefix Cache Producer Plugin

**Type:** `session-prefix-cache-producer`

Prepares per-endpoint prefix cache match data at session granularity, consumed by the `prefix-cache-scorer`. Runs in the request handling's `DataProducer` phase before scheduling.

For each request, the plugin derives a session identity from the request content as a chain of hashes over framed content blocks: hash-prefix equality holds exactly when content prefixes are byte-identical — the condition for KV reuse. Blocks follow message boundaries, and text runs are additionally chunked at a fixed size so conversations that grow inside a single message (raw completions, single-message chat) extend their chain instead of re-keying it; the partial tail of a multi-chunk run is the growing frontier and is excluded from identity until it stabilizes. Over that identity it maintains a session index mapping each session to the engines holding its prefix and the estimated token extent resident there. The index is estimate-seeded: the prefix is assumed to live where the turn was served (`PreRequest`), and the engine-reported `usage` refines the extent when the response completes (`ResponseBody`). Declared client ids — configurable session headers, `prompt_cache_key`, `previous_response_id` — alias the derived chain, bridging delta continuations that carry no history to rehash.

The plugin writes a `PrefixCacheMatchInfo` attribute onto each candidate endpoint: the covered fraction of the incoming prompt, in estimated tokens, on that endpoint. The generic `prefix-cache-scorer` binds to it via `prefixMatchInfoProducerName`.

Compared to the other prefix producers:

- Unlike `approx-prefix-cache-producer` it needs no tokenizer and keeps no per-block state; sessions match structurally (continuations, forks, template sharing) instead of through a router-side LRU simulation, so routing is deterministic on template-sharing traffic.
- Unlike `precise-prefix-cache-producer` it needs no engine-side KV events; it works against stock engines at the price of estimated rather than observed extents.

Session lineage handling:

- *Continuation:* a request whose chain extends a known session keeps its id; the head advances.
- *Fork:* a request diverging from a mid-chain append becomes a new session owning only the divergent suffix — undeclared sub-agents and template-sharing sessions match structurally, and coverage clamps at the fork point.
- *Rewrite:* a rewritten history shares no known append and becomes a new lineage, even under a stable declared id.
- *Delta continuation:* a declared id aliases its session, bridging requests that resend no prefix; content wins over the alias whenever the chain matches.

**Parameters:**

- `sessionTTLSeconds` (int, optional, default: `1800`): Idle time after which a session is dropped from the index.
- `maxSessions` (int, optional, default: `100000`): Cap on tracked sessions; expired then arbitrary sessions are evicted at capacity.
- `charsPerToken` (float, optional, default: `4.0`): Calibrates the char-based token estimate used for extents and covered fractions.
- `sessionHeaders` (string list, optional): Request headers read for declared client session ids, in precedence order. Defaults to `x-session-id`, `x-claude-code-session-id`, `x-session-affinity`, `session-id`, `session_id`.

**Configuration example:**

```yaml
plugins:
  - type: session-prefix-cache-producer
  - type: prefix-cache-scorer
    parameters:
      prefixMatchInfoProducerName: session-prefix-cache-producer
```

Session-granularity and block-granularity affinity compose: a second `prefix-cache-scorer` instance bound to an `approx-prefix-cache-producer` adds block-level matching (e.g. mid-prompt partial reuse across sessions) alongside the session signal.

## Tradeoffs

- **Estimated extents.** Without a tokenizer, extents mix char-based estimates (seeding) with engine-reported usage tokens (response). Both sides of the covered fraction share the same estimator and coverage is clamped to the incoming prompt, so miscalibration distorts the fraction, not the warm-vs-cold ranking; `charsPerToken` is the calibration knob.
- **Head matches credit the full known prefix.** The un-hashed gap between a continuation's matched prefix and the engine extent is the previous response, which reappears byte-identical in the next turn. A client that edits (rather than replays) the previous assistant turn while keeping the earlier prefix intact is over-credited by at most one response length.
- **`previous_response_id` aliasing is retry-grade.** Response ids are never observable in EPP hooks, so the alias helps only replays carrying an id the router has already seen on a request.
- **Estimate-seeded extents are the bootstrap, not the model.** The index is the session-granularity map the KV-lifecycle contracts attach to: session-labeled KV events replace estimates with engine-confirmed extents — including confirmed-cold, which makes evictions visible without a request — and retention directives act on the same coordinates. Until those land, engine-side eviction leaves stale extents bounded by the session TTL.
- **64-bit append ids.** At 10 million resident positions the collision probability is about 3e-6. A collision aliases two prefixes for routing affinity only; no correctness depends on id uniqueness.
- **Contiguous extents only.** Reuse that is not a prefix of a tracked session (e.g. a shared mid-prompt segment) scores zero; compose with a block-granularity producer where that matters.
- **Identity resolution is one text chunk (~256 estimated tokens).** A divergent tail shorter than the excluded partial chunk resolves as a replay of its parent lineage: near-identical requests (fan-out children with small suffixes) pool into one session whose extents span every pod that served them. Coverage over-claims by at most one chunk, and ranking warm replicas ahead of cold pods is the intended affinity.
- **Per-replica index.** Multiple EPP replicas converge on identity (chains are deterministic) but not on extents, matching the operational posture of the approximate producer.

## Responses API mapping

The chain design corresponds 1:1 to the OpenAI Responses (`previous_response_id`) model. A response id names "conversation state through response N"; per-message blocks give that state a content-derived name — the append id of the echoed assistant message — so response boundaries are already chain nodes:

| Responses API | Chain design |
|---|---|
| response id | append id at the echoed assistant-message block |
| `previous_response_id` | alias resolving to that position |
| server-side stored state | session extent on the serving engine |
| fork via an older response id | mid-chain divergence |
| delta `input` items | new blocks after the aliased position |

Completing the plug is a field-mapping exercise, deliberately deferred: frame `Responses` bodies (`instructions` plus per-`input`-item blocks), anchor aliases to positions rather than sessions (so a fork via an older response id clamps coverage like a structural fork), and surface the response id from response parsing so the forward alias can be recorded when the response completes.

---

## Related Documentation
- [Prefix Cache Scorer](../../../scheduling/scorer/prefix/README.md)
- [Approximate Prefix Cache Producer](../approximateprefix/README.md)
- [Precise Prefix Cache Producer](../preciseprefixcache/README.md)
