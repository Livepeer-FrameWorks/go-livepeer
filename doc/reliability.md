# Livepeer Orchestrator Selection and Reliability Strategy

## Overview

To achieve greater network scalability, a broadcaster works with multiple orchestrators at once. The Broadcaster stops working with any orchestrator that has gone offline or does not return transcoded segments, and "refreshes" the list of orchestrators it works with if "enough" orchestrators on its original list are unresponsive (see `Orchestrator List Refresh`). The Broadcaster distributes segments to Orchestrators using a "round robin" strategy (see `Orchestrator Selection`), given that Orchestrator cannot have more than one segment per stream in flight (this mitigates back logging). Therefore, a Broadcaster sends segments to "free" Orchestrators on their "saved list". The ability to send segments to a selection of Orchestrators gives individual Orchestrators more time to process segments, and prevents `OrchestratorBusy` errors.

## BroadcastSessionsManager

Orchestrators are managed by a `BroadcastSessionsManager` stored in the `rtmpConnections` on the `LivepeerServer` interface. The sessions manager is initiated when the RTMP stream is registered by `gotRTMPStreamHandler`. The orchestrator list is first populated then, when `refreshSessions` is called within `NewSessionManager`.

The `BroadcastSessionsManager` stores orchestrators in two lists, a `sessList` and a `sessMap`.  The `sessList` is an array containing a "working" list of orchestrators. When a Broadcaster is in need of an orchestrator, it selects and removes it from `sessList` only if said Orchestrator also exists in `sessMap`. Therefore, `sessMap` contains all orchestrators currently in use or available for use. It is a map with a string key of the URI of the orchestrator, and a value of that orchestrator's `BroadcastSession`.

## Orchestrator List Refresh

The orchestrator list is refreshed when the number of sessions in `sessList` is less than double the `HTTMPTimeout` in seconds (hard-coded to 8 seconds at the moment) divided by the length of segments (hard-coded to 2 seconds at the moment) OR less than the size of the OrchestratorPool saved on disk, whichever is less (i.e. when its length is less than what is required to keep in memory). This happens at startup (as described above), and when an orchestrator is selected for individual transcoding in `selectSession`.

## Orchestrator Selection

To give preference to O's that respond with transcoded segments quickly, instead of selecting an Orchestrator from the beginning of `sessList` when needed, and placing new Orchestrators that are finished processing a segment at the end, `selectSession` takes Orchestrators from the end of `sessList`. If transcoding is successful, it adds them back to the end of `sessList`. 

## Transcoding Errors & Retries

If there is an error uploading segment to an Orchestrator's OS, submitting the segment to an Orchestrator, downloading transcoded segments, or the segment signature check fails, the Orchestrator is removed from the `sessMap`. The segment is retried with a different Orchestrator. When `selectSession` is called in this retry scenario, though the removed session might still exist in `sessList`, only a session that still exists in `sessMap` will be selected.  If there is no error in segment transcoding, `completeSession` adds session back to `sessList`. Retries stop if `sessMap` is empty.

## Storage

To prevent segment front-running (when an Orchestrator writes to a file that should belong to another Orchestrator), each Orchestrator is given an external storage path prefix used to create its own unique OS session. The prefix is composed of the stream's ManifestID, and a randomly generated manifest Id.

## MaxSessions

When an Orchestrator - Transcoder are run on the same node, a `-maxSessions` flag can be used to specify the node's own capacity for transcoding. A `MaxSessions` hard-coded value in `Livepeernode.go` caps the number of segment channels that can be created per Orchestrator, which limits the number of streams it can ingest. `MaxSessions` is the default value that is overridden with `-maxSessions`.

## Segment budget and the deadlineMs contract (HTTP push)

Every segment pushed to `/live/<manifest>/<seq>.<ext>` gets a response budget `B`: how long the edge waits for this push. The gateway owns everything inside `B`.

### Where `B` comes from

`deadlineMs` is an integer number of milliseconds, `0..3600000`, measured from when the gateway receives the push. `0` or absent means "not set".

1. `deadlineMs` in this request's `Livepeer-Transcode-Configuration` JSON header, when set. If the stream also has an authoritative `deadlineMs`, the smaller one wins.
2. Otherwise the stream's `deadlineMs`: the auth webhook response field `deadlineMs` when an auth webhook is configured, or the first request's header value when it is not. It is fixed for the life of the stream.
3. Otherwise `segment duration + 1000 ms` for `workload: "live"` (and when no workload is set), and `30000 ms` for `workload: "vod"`.

The segment duration is the `Content-Duration` request header in milliseconds (2000 when absent). For live the edge should send `deadlineMs = Content-Duration + 1000` on every request and give up no earlier than that, except on a session's first segment (below).

### First segment: session setup allowance

A segment that pays for setting an orchestrator session up gets `B + 2000 ms`. The gateway treats a segment that way when it is segment 0 of the stream, or when the stream has no orchestrator session on its reuse list that has returned a result: at stream start before any session answered, after a warm-up that failed, and after the session the stream used was suspended or replaced by one that is still setting up. Within that segment the gateway registers the stream, discovers orchestrators and the orchestrator assigns a transcoder and opens its decoder and encoders, all before the segment's own transcode. On public orchestrators the slowest first segments took up to about 4.5 s from the gateway receiving segment 0 to its answer, against 2.5 s for a 2 s segment's steady budget; the 2000 ms allowance covers that.

- Without a request `deadlineMs`, `B` for such a segment is the stream's budget (rule 2 or 3 above) plus the allowance.
- An edge that knows the segment starts a session (its first segment, or the first segment it sends a gateway after switching to it) sends `deadlineMs` with the allowance included, `Content-Duration + 3000` for live, and waits that long. The stream's `deadlineMs` caps it only after adding the allowance.
- An edge that sends its steady `deadlineMs` for such a segment (it cannot see the gateway's orchestrator swaps) still gets `503` at its own `deadlineMs - 500 ms`, but processing runs until the stream's budget plus the allowance, so its re-POST of the segment joins the transcode and gets the result.

### What the gateway does inside `B`

- Processing runs detached from the HTTP request. A re-POST of the same segment (same sequence number and bytes) joins the transcode that is still running, or gets its cached result, instead of starting another one.
- Upload to the orchestrator, transcoding and downloading the renditions all count against `B`. An orchestrator's submission runs until processing ends; there is no per-orchestrator cap below that. A submission still running when processing ends is cancelled, which by itself is not an orchestrator error: the orchestrator is not suspended for it.
- Hedging: if the chosen orchestrator has not returned a result after `min(0.6 x segment duration, that orchestrator's estimated p90 round trip)` (never below 250 ms), the segment is also submitted to one other idle orchestrator. The first success is used and the other submission is cancelled. When the hedge wins, the stream reuses that orchestrator for its next segment ahead of the one it hedged. A segment is hedged at most once across all of its attempts. The p90 estimate is `mean + 2 x mean absolute deviation` of the orchestrator's successful round trips, kept in the shared Redis performance store (`rtt_ms_ewma`, `rtt_ms_dev`). The `segment_hedges` and `segment_hedge_wins` metrics count hedges and hedges won by the second orchestrator.
- When the stream has no usable orchestrator session (discovery came back empty at stream start, or every session failed), the gateway refreshes discovery every 250 ms until one appears or `B` runs out, instead of failing the segment at once.
- Slowness suspends an orchestrator only through slow strikes: a round trip longer than the workload tolerates is a strike, and two consecutive strikes suspend it. The workload tolerates the segment duration for `live`, the segment duration divided by `minSpeed` (and never less than the segment duration) for `vod`, so `minSpeed` 0.5 tolerates twice the segment duration. A submission cancelled because processing ended or the other orchestrator of a hedge answered first is a strike when it had already run longer than that, so an orchestrator that never answers is suspended after two such segments; one cancelled sooner neither adds nor clears a strike.
- Segments submitted to a session before its orchestrator returned the session's first result also pay for the orchestrator setting the session up (decoder, encoders, transcoder assignment). Their round trips are never strikes and never count toward the shared performance score, they never make the session look too slow to reuse, and their cancellation is never held against the orchestrator, so the stream keeps the orchestrator it warmed up instead of moving to another one that has to set up again. That holds until processing ends on one of them without a result: the setup had its allowance, and the session's later submissions count as usual.
- A segment tries up to `max(3, ceil(B / max(segment duration, 1 s)))` orchestrators, so a `vod` segment with the default 30 s budget and 2 s segments can try 15 before it answers `503`. `B` still ends processing first when attempts are slow.
- When `B - 500 ms` passes without a result the gateway answers `503`. The transcode keeps running until `B` (or until the stream's budget plus the session setup allowance, when that is later), so a re-POST can still pick up its result. When an orchestrator has already returned the transcode at that point and only its renditions are still being downloaded, the gateway waits for them until `B` instead, and answers `503` at `B` if they have not arrived.

### Session warm-up

Setting a session up on a cold orchestrator can take longer than a live segment's whole `B`. A client can start that setup before its first segment is complete: `POST /live/<manifestID>/session` with the stream's first keyframe as MPEG-TS (at most 8 MiB) and the same `Livepeer-Transcode-Configuration` header as its segments (`Content-Duration`, the keyframe's duration in milliseconds, is optional). `Content-Frame-Rate` carries the source frame rate in frames per second: a single frame has none to probe, and the stream registers with the frame rate the auth webhook checks the job's profiles against, so a client should always send it. The request is authorized exactly like a segment (auth webhook, job token, source address, trusted proxies) and registers the stream when it is new. The gateway answers `202` and, in the background for at most 8 s, discovers orchestrators, submits the keyframe to the session the stream's first segment will use and discards the result. That session stays first on the stream's reuse list and counts as answered, so the first segment goes to it and its `B` pays only for whatever part of the setup is still running. A segment that arrives while the warm-up is still in flight joins the same session. The warm-up's own round trip is not a performance sample; FrameWorks telemetry reports its attempt with disposition `session_warmup`.

A stream is warmed at most once: a warm-up for a stream that a warm-up or a segment already used is answered `202` and does nothing. A warm-up whose orchestrator returns an error suspends that orchestrator like a failed segment would; one that runs out of time or whose keyframe is refused leaves the session to the first segment. Clients send it once, without waiting for the answer, and ignore failures: a gateway without the endpoint answers `400`.

### Errors returned by orchestrators

Any error an orchestrator returns, including FFmpeg errors such as `Invalid argument`, is attributed to that orchestrator: it is suspended in the suspension store (shared through Redis by every stream and gateway with the same region, workload and rendition set), removed from the stream's session pool, and the segment is retried on another orchestrator within `B`. A suspension lasts 10 minutes for `live` and 2 minutes for `vod` from the latest failure. While it lasts, discovery refreshes do not add the orchestrator back to the stream's pool unless the pool holds no other session. Without Redis the suspension applies to the stream that saw the failure; with the Redis store configured it applies to every stream, and sessions of other streams that still hold a suspended orchestrator stop using it while they have alternatives.

The push answers `422` only when the segment itself is at fault:

- the gateway's own probe of the input fails (codec detection, unsupported codec);
- two distinct orchestrators return the same non-retryable error for the segment (their suspensions are then lifted);
- the sequence number is reused with different content, or falls outside the replay window.

Orchestrator failures, an exhausted attempt count, an exhausted budget and an empty orchestrator pool answer `503`.

### Payment cost of hedging

A hedge is a normal paid submission to a second orchestrator, sized like any submission (the estimated fee of the segment's renditions). The cancelled submission keeps whatever it was paid. Hedging therefore adds at most one extra segment payment per segment, and only for segments whose first orchestrator was slower than its own p90 (or than 0.6 x segment duration).
