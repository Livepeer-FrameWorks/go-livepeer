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

The segment duration is the `Content-Duration` request header in milliseconds (2000 when absent). For live the edge should send `deadlineMs = Content-Duration + 1000` on every request and give up no earlier than that.

### What the gateway does inside `B`

- Processing runs detached from the HTTP request. A re-POST of the same segment (same sequence number and bytes) joins the transcode that is still running, or gets its cached result, instead of starting another one.
- Upload to the orchestrator, transcoding and downloading the renditions all count against `B`. One orchestrator's round trip is capped at `0.8 x B`, so a stalled orchestrator always leaves time for another one.
- Hedging: if the chosen orchestrator has not returned a result after `min(0.6 x segment duration, that orchestrator's estimated p90 round trip)` (never below 250 ms), the segment is also submitted to one other idle orchestrator. The first success is used and the other submission is cancelled. A segment is hedged at most once across all of its attempts. The p90 estimate is `mean + 2 x mean absolute deviation` of the orchestrator's successful round trips, kept in the shared Redis performance store (`rtt_ms_ewma`, `rtt_ms_dev`). The `segment_hedges` and `segment_hedge_wins` metrics count hedges and hedges won by the second orchestrator.
- When the stream has no usable orchestrator session (discovery came back empty at stream start, or every session failed), the gateway refreshes discovery every 250 ms until one appears or `B` runs out, instead of failing the segment at once.
- An orchestrator whose round trip exceeds the segment duration on two consecutive segments is suspended.
- When `B - 500 ms` passes without a result the gateway answers `503`. The transcode keeps running until `B`, so a re-POST can still pick up its result.

### Errors returned by orchestrators

Any error an orchestrator returns, including FFmpeg errors such as `Invalid argument`, is attributed to that orchestrator: it is suspended in the suspension store (shared through Redis by every stream and gateway with the same region, workload and rendition set), removed from the stream's session pool, and the segment is retried on another orchestrator within `B`. With the Redis store configured, sessions of other streams that still hold a suspended orchestrator stop using it while they have alternatives.

The push answers `422` only when the segment itself is at fault:

- the gateway's own probe of the input fails (codec detection, unsupported codec);
- two distinct orchestrators return the same non-retryable error for the segment (their suspensions are then lifted);
- the sequence number is reused with different content, or falls outside the replay window.

Orchestrator failures, an exhausted attempt count, an exhausted budget and an empty orchestrator pool answer `503`.

### Payment cost of hedging

A hedge is a normal paid submission to a second orchestrator, sized like any submission (the estimated fee of the segment's renditions). The cancelled submission keeps whatever it was paid. Hedging therefore adds at most one extra segment payment per segment, and only for segments whose first orchestrator was slower than its own p90 (or than 0.6 x segment duration).
