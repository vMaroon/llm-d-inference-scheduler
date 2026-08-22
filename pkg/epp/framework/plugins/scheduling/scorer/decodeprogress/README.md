# Decode Progress Scorer

Type: `decode-progress-scorer`

Scores decode endpoints using request lifecycle information observed by the EPP. Lower active-request count is the primary ordering. Equal-count endpoints are ordered by fewer requests awaiting their first response, fewer non-streaming requests with unobservable progress, and more recent streaming response progress.

The scorer consumes `InFlightLoad` from `inflight-load-producer`. It does not estimate output length or token rate. Non-streaming requests contribute only to the active and unobservable request counts.

The scorer is intended for a pure decode scheduling profile. Prefill profiles should continue to use token-load-aware routing.

```yaml
plugins:
- type: decode-progress-scorer
  name: decode-progress

schedulingProfiles:
- name: decode
  plugins:
  - pluginRef: decode-filter
  - pluginRef: decode-progress
  - pluginRef: max-score-picker
```

`inFlightLoadProducerName` selects a named `inflight-load-producer` instance when the default instance is not used.
