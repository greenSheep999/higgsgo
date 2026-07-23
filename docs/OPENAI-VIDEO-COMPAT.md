# OpenAI-Compatible Video Surface

> Implemented 2026-07-21; reviewed 2026-07-23. Owner: higgsgo core.
>
> This document is the wire-level specification for the video
> endpoints on higgsgo that mimic OpenAI's Sora / video shape closely enough
> that downstream new-api / OneAPI deployments can plug higgsgo in behind
> their built-in Sora TaskAdaptor with zero code changes for create, poll,
> and content. It is the implementation contract: changes to the wire shape
> must update this file in the same PR.

## 1. Motivation

higgsgo already exposes two video paths — `POST /v1/videos/generations`
(higgsgo legacy) and `POST /v1/video/generations` (new-api single-form
alias). Both accept higgsgo's *native* body (`width` / `height` /
`duration` / `resolution` / `mode` / `sound` / `quality` / …) and return
higgsgo's *native* response (`{id, status, cost, poll_url, upstream_job_id,
result_url, …}`).

Downstream integrators cannot use the official OpenAI Python / TypeScript
SDKs against those endpoints because the SDKs (a) hit `POST /v1/videos`
(no `/generations` suffix), (b) send `size:"1280x720"` and `seconds:"5"`
(strings), and (c) expect responses to omit private fields like `cost` and
`poll_url` while surfacing exactly `{id, object, model, status, progress,
created_at, seconds, size}`.

Rather than force every downstream operator to hand-write a custom client
per relay hop, higgsgo — the natural convergence point for every video
model — grows a second public surface that speaks the OpenAI dialect.
Existing `/v1/video[s]/generations` callers keep working unchanged.

The upstream client this surface targets is documented at:

- `relay/channel/task/sora/adaptor.go` in the new-api repo (the exact
  client that will call these endpoints).
- The captured samples in the new-api tree under
  `docs/zh-CN/higgs-request-samples.md`.

If the source and this document disagree, the source wins.

## 2. Design goals and non-goals

**Goals.**

- Passing an unmodified OpenAI SDK call `client.videos.create(model=...,
  prompt=..., size="1280x720", seconds="5")` at higgsgo produces a valid
  higgsgo job and a Sora-shaped response.
- Every video-output alias in the registry is routable through the new
  surface. Successful generation still depends on the model's body template,
  required parameters, plan gate, media inputs, and resolution vocabulary;
  routable does not mean that a minimal Sora request is sufficient.
- All higgsgo-private request parameters (`mode`, `sound`, `resolution`,
  `aspect_ratio`, `quality`, `generate_audio`, `audio`, …) are reachable
  by SDK callers via OpenAI SDK's `extra_body`.
- Ordinary image-to-video accepts an HTTP(S) URL, a `data:` URI, or a raw
  multipart upload. Video/audio/end-frame transforms still need their
  model-specific fields and are not covered by `input_reference` alone.
- The final MP4 is delivered through higgsgo itself; downstream clients
  never see the higgsfield.ai CDN URL.
- `billing_expr` unchanged: after body conversion the internal request is
  indistinguishable from a legacy `/v1/videos/generations` call so the
  existing pricing expressions keep matching.

**Non-goals.**

- We are not trying to be a general OpenAI proxy. Only video-shaped
  endpoints are added. Audio, images, chat, etc. keep their current
  shapes.
- The legacy `/v1/video[s]/generations` endpoints are frozen but not
  migrated. Their body and response shape stay exactly as they are.
- OpenAI's `POST /v1/videos/{id}/remix` is out of scope for the first
  iteration.

## 3. Route table

| Method | Path                             | Auth               | Purpose                                          |
|--------|----------------------------------|--------------------|--------------------------------------------------|
| POST   | `/v1/videos`                     | `Bearer sk-hg-*`   | Create a video generation job (Sora shape)       |
| GET    | `/v1/videos/{id}`                | `Bearer sk-hg-*`   | Poll job status                                  |
| GET    | `/v1/videos/{id}/content`        | `Bearer sk-hg-*`   | Reverse-proxied MP4 download                     |
| POST   | `/v1/videos/generations`         | unchanged          | *(legacy, unchanged)*                            |
| POST   | `/v1/video/generations`          | unchanged          | *(legacy, unchanged)*                            |

All three new endpoints mount on the same public listener (`:8180`
in production) behind the existing `APIKeyAuth` + rate-limit middleware
chain — same as the legacy video routes.

## 4. `POST /v1/videos` — create job

### 4.1 Request

Content-Type is either `application/json` **or** `multipart/form-data`.
The chosen content type only affects how `input_reference` reaches the
server; every other field has the same semantics in both.

#### 4.1.1 JSON body

| Field             | Type              | Required | Notes                                                       |
|-------------------|-------------------|----------|-------------------------------------------------------------|
| `model`           | string            | yes      | higgsgo native alias, e.g. `kling-3-turbo`, `seedance-2-0-mini` |
| `prompt`          | string            | no       | Text prompt. Optional for image-to-video with a reference   |
| `seconds`         | string \| int     | no       | Duration in seconds. String per OpenAI SDK; int also accepted |
| `size`            | string            | no       | `"WxH"`, e.g. `"1280x720"`, `"1024x1792"`. Not preset names |
| `input_reference` | string            | no       | HTTP(S) URL or `data:image/*;base64,...` URI                |
| `group_id`        | string            | no       | Optional pool group override (same semantics as legacy)     |
| `async`           | bool              | no       | Force async even when the client would otherwise sync-poll  |
| `callback_url`    | string            | no       | Same as legacy: webhook fired on terminal state             |

Any additional top-level key that is **not** one of the fields above is
forwarded verbatim into higgsgo's internal `UserParams` map. This is the
extension point: OpenAI SDK callers pass private higgsgo parameters via
`extra_body={"mode":"quality", "sound":"on", "generate_audio":true}`,
which the SDK merges into the top-level JSON body before sending.

#### 4.1.2 Multipart body

`multipart/form-data` requests carry:

- All JSON fields above, each as a single form field with the same name.
  Values are strings (no int coercion needed; the handler parses).
- Optional `input_reference` file part — the raw image bytes. When
  present, the file is uploaded to higgsfield's media store and the
  resulting media_id is forwarded downstream, matching the existing
  `image_url` upload path used by `internal/api/v1/images.go`.

Multipart requests without any file part are equivalent to JSON.

### 4.2 Body conversion (Sora → higgsgo native)

The handler applies these rules **before** calling `proxy.Service.Generate`:

- `size:"WxH"` →
  - `UserParams["width"]` int (parsed from the first component).
  - `UserParams["height"]` int (parsed from the second component).
  - `UserParams["resolution"]` string, derived from the **shorter** side
    using the tier table below. Shorter-side (aka minor-axis) derivation
    matches Sora's convention and the sample expectations in Appendix A —
    e.g. `1024x1792` is treated as `"1080p"` portrait, not `"4k"`.
    `resolution` is a first-class higgsfield token, not a display hint —
    it is passed verbatim into the upstream params map by
    `internal/core/proxy/service.go` and read by `billing_expr` for
    pricing.
- `seconds:"5"` or `seconds:5` → `UserParams["duration"]` int. String
  form is parsed with `strconv.Atoi`; ints are accepted directly. On
  parse failure the handler returns HTTP 400 with an `invalid_body`
  error.
- `input_reference`:
  - Prefix `http://` or `https://` → a URL-only `MediaInput`. `buildBody`
    injects it into the media slot present in the model template:
    `input_images`, `medias`, or `input_image`.
  - Prefix `data:image/*;base64,...` → decoded to raw bytes; uploaded to
    higgsfield's media store via the three-step protocol documented in
    §Appendix C; the resulting media object is injected into the same
    template-selected slot.
  - Multipart file part (see §4.1.2) → same three-step upload as the
    data-URI case, ending in the template-selected media slot.
- `model` and `prompt` → verbatim into the internal request.
- `group_id`, `async`, `callback_url` → mapped onto the same fields on
  the internal `GenerationRequest` that the legacy handler uses.
- All other top-level keys and keys inside an explicit `extra_body` object →
  merged verbatim into UserParams. OpenAI SDKs normally flatten
  `extra_body` before sending; accepting the explicit object also supports
  raw JSON/new-api callers and allows `extra_body.model` to reach
  `params.model` without colliding with the public alias. This is
  how private higgsgo params (`mode`, `sound`, `generate_audio`,
  `quality`, `aspect_ratio`, `preset_id`, …) travel through.

#### Resolution tier table

Derived from the exhaustive set of `resolution` string literals actually
accepted by the higgsfield upstream, based on the shipped body-templates
under `data/reference/body-templates/`. The threshold is the **shorter**
side of `size` — this matches Sora's convention and the industry norm
where a portrait `1024x1792` frame is a 1080p video, not a 4k one.

| Shorter side (pixels) | `resolution` value |
|-----------------------|--------------------|
| `≤ 480`               | `"480p"`           |
| `≤ 720`               | `"720p"`           |
| `≤ 1080`              | `"1080p"`          |
| `> 1080`              | `"4k"`             |

Note: `"1440p"` and `"8k"` are **not** valid higgsfield tokens and must
not be produced. The `"4k"` bucket absorbs anything above 1080p; the
upstream model policy decides whether the request is honoured at true
4k or downscaled.

Special case — `seedance-2-0` (non-mini) accepts unsuffixed integer
strings (`"480"`, `"720"`, `"1080"`) instead of `p`-suffixed tokens.
This is a per-model quirk documented in `data/reference/sealed.json` and
handled by the model spec, not by this conversion layer. The
Sora-compat handler emits the `p`-suffixed form uniformly; the model
spec's request-templating pipeline (see §7 of `ARCHITECTURE.md`)
translates when needed.

The internal request that leaves the handler is byte-compatible with a
legacy `/v1/videos/generations` call, so `billing_expr` picks the correct
tier without any changes.

### 4.3 Response

Success returns HTTP 200 with this envelope, regardless of whether the
underlying call was sync-polled or async:

```json
{
  "id":         "job_01hxyz...",
  "object":     "video",
  "model":      "kling-3-turbo",
  "status":     "queued",
  "progress":   0,
  "created_at": 1784570000,
  "seconds":    "5",
  "size":       "1280x720"
}
```

Field-by-field:

| Field         | Type   | Notes                                                                     |
|---------------|--------|---------------------------------------------------------------------------|
| `id`          | string | higgsgo `JobID`. Stable, usable as path parameter in the poll endpoint.   |
| `object`      | string | Constant `"video"`.                                                       |
| `model`       | string | Echo of the resolved model alias.                                         |
| `status`      | string | See §7 for allowed values.                                                |
| `progress`    | int    | 0–100. `0` when the job is queued; `100` on `completed`.                  |
| `created_at`  | int    | Unix seconds.                                                             |
| `completed_at`| int    | Unix seconds. Present only when `status == "completed"`.                  |
| `seconds`     | string | Echo of the requested duration as a string (OpenAI convention).           |
| `size`        | string | Echo of `"WxH"`.                                                          |

Fields that must **not** appear (they exist on higgsgo's native response
but the OpenAI adapter passes the response through to the SDK client and
these would leak): `poll_url`, `cost`, `upstream_job_id`, `result_url`,
`error_detail`, and any internal timing fields.

### 4.4 Failure response

When higgsgo's underlying `proxy.Service.Generate` returns an error before
a job row exists (e.g. auth failure, model not found, no capacity):

```json
{
  "error": {
    "message": "no capacity for model kling-3-turbo",
    "code":    "no_capacity"
  }
}
```

HTTP status matches the underlying error class (401 / 404 / 429 / 5xx).

When a job row *is* created but the upstream terminates with `status ==
"failed"`, the create response returns HTTP 200 with

```json
{
  "id":     "job_...",
  "object": "video",
  "model":  "kling-3-turbo",
  "status": "failed",
  "error": {
    "message": "upstream policy violation",
    "code":    "upstream_error"
  },
  "created_at": 1784570000,
  "completed_at": 1784570008
}
```

so that clients driving OpenAI-style polling receive the error via a
terminal `status`, matching Sora's contract.

## 5. `GET /v1/videos/{id}` — poll status

Path parameter is the `id` returned by `POST /v1/videos` (which is the
higgsgo `JobID`; the caller does not see the upstream job id).

Response is exactly the same envelope as §4.3 / §4.4, reflecting the
current terminal state of the job. `progress` monotonically increases;
`status` transitions follow the state machine in §7.

`404 Not Found` with the standard error envelope when the job id does not
exist for the caller.

## 6. `GET /v1/videos/{id}/content` — reverse-proxied MP4

- Preconditions: the job exists, belongs to the caller, and its status is
  `"completed"`.
- Behaviour: higgsgo reads the internal `result_url` off the job record,
  issues a server-side `GET` against it (no auth header — the upstream
  URLs are unsigned public CloudFront paths on `d*.cloudfront.net` and
  `cdn.higgsfield.ai`, verified via
  `internal/core/upstream/client.go:288-292`), and streams the response
  body back to the client using `io.Copy` with a bounded buffer.
- Response headers propagated from upstream: `Content-Type`,
  `Content-Length`, `Last-Modified`, `ETag`, `Accept-Ranges`. `Range`
  headers on the request are forwarded upstream so clients can seek.
- Status codes: `200`, `206` (range), `404` (job or content not found),
  `409` (job not yet completed), `502` (upstream fetch failed).

We do not 302-redirect to the higgsfield CDN, and the internal
`result_url` is never emitted to the caller.

## 7. Status vocabulary

The `status` field takes exactly one of these literal string values:

- `queued` — job accepted, waiting for a worker.
- `in_progress` — worker started, upstream is generating.
- `completed` — terminal success; `content` endpoint is now valid.
- `failed` — terminal failure; `error` block populated.

higgsgo's internal `JobStatus` enum already uses these strings
(`internal/domain/job.go`), so no mapping table is required. `pending`
and `refunded` internal states are surfaced as `queued` and `failed`
respectively in the OpenAI-facing response, if they ever occur on a job
originating from this surface.

## 8. Streaming and back-pressure

For `GET /v1/videos/{id}/content`, higgsgo uses `http.Transport` defaults
with a per-request context and a 32 KiB streaming buffer. No chunk of the
video body ever lives in higgsgo's heap for longer than one loop
iteration. This keeps memory pressure bounded even for large 4K exports.

## 9. Test surface

Handlers ship with:

- Table-driven unit tests using the two verbatim request bodies from
  new-api's `higgs-request-samples.md` §一. Each row asserts that the
  converted internal request has (a) the expected `width`/`height`,
  (b) a `resolution` in the tier table above, (c) `duration` as an int,
  (d) all extra fields preserved in `UserParams`.
- Router mount test: `POST /v1/videos`, `GET /v1/videos/x`, and
  `GET /v1/videos/x/content` return anything other than `404`. This is
  the same anti-regression pattern used by `TestPublicRouter_VideoAliasBothPathsMounted`.
- Response-shape test: golden JSON marshalled from a synthetic job asserts
  the response object contains exactly the fields in §4.3 and none of the
  forbidden fields in §4.3.
- Streaming test: a fake upstream serves an MP4 with `Range` support;
  the content handler forwards headers and body byte-for-byte.

Legacy tests for `/v1/videos/generations` and `/v1/jobs/{id}` remain
untouched and green.

## 10. Non-breaking guarantees

The following invariants must hold after this change lands, verified by
existing tests:

- `POST /v1/videos/generations` — same handler, same request shape, same
  response shape.
- `POST /v1/video/generations` — same handler as above.
- `GET /v1/jobs/{id}` — same response shape, including `poll_url`,
  `cost`, `upstream_job_id`, `result_url`.
- `billing_expr` in the SQLite store — no change.
- Model registry — no change.

## 11. Resolved decisions

Locked before implementation, sourced from code (higgsgo + higgsfield-register).

1. **Resolution tier table** — see §4.2. The valid values are `"480p"`,
   `"720p"`, `"1080p"`, and `"4k"`. `"1440p"` and `"8k"` are not valid
   higgsfield tokens; the tier table absorbs anything > 1080p into `"4k"`.
2. **Multipart file parts on text-only models** — the handler always
   attempts the upload; the resulting `media_id` sits in UserParams as
   `media_id`. Models that ignore `media_id` (pure text-to-video)
   discard it upstream, which is a no-op for us. No 400.
3. **Field-name collisions.** If a caller sends both `seconds` and
   `duration`, `seconds` wins (this is the Sora surface — OpenAI-shaped
   fields take precedence over the higgsgo-native form).
4. **Rate limits for `/content`.** First iteration reuses the same
   per-key rate-limit bucket as the rest of `/v1`. A second bucket
   sized for large streaming pulls is deferred to a follow-up if we
   observe egress saturation.

## 12. Rollout

- Ship behind the existing `APIKeyAuth` gate. No feature flag.
- Version tag: `v0.6.0` — minor bump because a new public API surface
  appears. Downstream new-api operators point their Sora channel base_url
  at `https://higgs.aibbq.xyz` and use `higgs`-shaped model names in the
  channel model list.
- Communication: docs update to `docs/API_REFERENCE.md` alongside the
  handler PR, plus a CHANGELOG entry referring back to this file.

---

## Appendix A. Captured input samples

Verbatim from `new-api/docs/zh-CN/higgs-request-samples.md § 一`, used
as inputs to the table-driven test in §9.

### A.1 kling-3-turbo, 720p, 5 seconds

```
POST /v1/videos HTTP/2
Authorization: Bearer sk-hg-...
Content-Type: application/json
Content-Length: 88

{"model":"kling-3-turbo","prompt":"a cat playing piano","seconds":"5","size":"1280x720"}
```

Expected internal request after conversion:

- `Model`: `"kling-3-turbo"`
- `UserParams["prompt"]`: `"a cat playing piano"`
- `UserParams["width"]`: `1280`
- `UserParams["height"]`: `720`
- `UserParams["duration"]`: `5`
- `UserParams["resolution"]`: `"720p"`

### A.2 sora2-video, portrait 1024x1792, 8 seconds

```
POST /v1/videos HTTP/2
Authorization: Bearer sk-hg-...
Content-Type: application/json
Content-Length: 89

{"model":"sora2-video","prompt":"a portrait test scene","seconds":"8","size":"1024x1792"}
```

Expected internal request after conversion:

- `Model`: `"sora2-video"`
- `UserParams["prompt"]`: `"a portrait test scene"`
- `UserParams["width"]`: `1024`
- `UserParams["height"]`: `1792`
- `UserParams["duration"]`: `8`
- `UserParams["resolution"]`: `"1080p"` (shorter side is 1024, ≤ 1080 → `"1080p"` per the §4.2 tier table; the longer 1792 axis is ignored in tier derivation, matching Sora's convention that portrait framing does not upgrade the resolution class)

## Appendix C. Higgsfield media upload protocol

Reference: `higgsfield-register/src/upstream/media.mjs` — the JS
implementation used by the register worker. The Sora-compat handler
needs to reproduce this flow for `data:` URIs and multipart file parts.
It is a three-step protocol against `fnf.higgsfield.ai`.

1. **Reserve.** `POST /media` with JSON `{"content_type":"image/jpeg"}`
   (or `image/png`, `video/mp4`, …). Response:
   ```
   { "id": "<media_uuid>", "url": "<final_cdn_url>", "upload_url": "<s3_presigned_put>" }
   ```
2. **Upload.** `PUT <s3_presigned_put>` with the raw bytes and the
   matching `Content-Type` header. This talks to S3 directly, not to
   fnf.higgsfield.ai — no higgsfield auth header on this request.
3. **Commit.** `POST /media/{id}/upload` with empty JSON body `{}` to
   flip the reservation into `uploaded`.

For video/audio media, the reserve/commit endpoints are `/video` and
`/audio` respectively (see `media.mjs:44-85`) with a payload of
`{force_nsfw_check: false, force_ip_check: false, surface: "ai_video"}`
for video. The Sora-compat handler only needs the `image/*` path.

Optional step: poll `GET /media/{id}` until `status ∈ {uploaded, ready,
completed}` before returning `media_id` to the caller of the internal
handler. In practice this is not needed because the immediately following
`POST /jobs/...` retries on transient `media_not_ready` responses.

Failure modes:

- `POST /media` returns 4xx → return 502 to the caller with
  `error.code = "media_reserve_failed"`.
- `PUT <upload_url>` returns non-2xx → 502, `error.code =
  "media_upload_failed"`.
- `POST /media/{id}/upload` returns 4xx → 502, `error.code =
  "media_commit_failed"`.

Implementation note: this protocol is not currently in higgsgo's
`internal/core/upstream/client.go`. Adding it is part of this change's
scope. Wrap it as `UpstreamClient.UploadImage(ctx, contentType, r io.Reader) (mediaID string, err error)`.

## Appendix B. Field-mapping reference

| OpenAI SDK field    | Internal higgsgo field  | Notes                            |
|---------------------|-------------------------|----------------------------------|
| `model`             | `Model` on request       | verbatim, no aliasing            |
| `prompt`            | `UserParams["prompt"]`   | verbatim                         |
| `seconds` (str/int) | `UserParams["duration"]` | int; string parsed via `strconv` |
| `size`              | `UserParams["width"]`, `UserParams["height"]`, `UserParams["resolution"]` | tier from §4.2 |
| `input_reference`   | template media slot (`input_images`, `medias`, or `input_image`) | URL-only or uploaded media object |
| `extra_body.*`      | `UserParams[*]`          | verbatim, all higgsgo private params reachable |
| `group_id`          | request `GroupID`        | passthrough                      |
| `async`             | request `Async`          | passthrough                      |
| `callback_url`      | request `CallbackURL`    | passthrough                      |
