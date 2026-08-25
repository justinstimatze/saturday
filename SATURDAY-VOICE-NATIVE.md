# Saturday Native — implementation spec

**Status:** design, not yet started — but §2's mechanism is hand-tested and working
**Supersedes:** `SATURDAY-MCP-MODE.md` (kept in git history for the record — its premise didn't survive contact with research, see §0)
**Audience:** a Claude Code session with the `saturday` repo checked out
**Goal:** voice-drive existing Claude Code sessions from a phone, without depending on any vendor's consumer voice app to invoke custom tools.

---

## 0. Why this replaces MCP mode

MCP mode's whole appeal was "don't build a voice client, borrow Anthropic's." That premise didn't hold up:

- **Voice mode doesn't reliably invoke custom MCP connectors.** Confirmed directly against `github.com/anthropics/claude-ai-mcp#146` ("No registered MCP connectors are discovered in voice chat") — closed `NOT_PLANNED`, an Anthropic engineer stated voice conversations run on a different execution path than text chat and connector tools aren't supported there. Re-checked same day, unchanged. This isn't neglect — Anthropic is actively expanding voice+tools, just scoped to their own first-party connectors (Gmail/Calendar/Docs/Slack, expanding through August 2026). Custom connectors, which is what an inject tool would have to be, are explicitly out of scope.
- **Even if it worked, the privacy trade was bad.** Custom Connector traffic routes through Anthropic's cloud infrastructure by design, and consumer plans — Free, Pro, and Max alike — have no Zero Data Retention option. Max buys model access and quota, not a different retention tier. Standard retention (30-day tail after delete; up to 5yr de-identified if model-improvement is on; up to 2yr/7yr if flagged) applies to every session summary and transcript excerpt MCP mode would have fetched.
- **MCP's own protocol restrictions ruled out real ambient awareness anyway.** Server-initiated push is foreclosed by design (SEP-2260) — nothing could tell the user a background task finished except the user remembering to ask.

None of that is a reason to abandon the goal. It turned into a reason to try a second approach — §2 below — that gets voice mode's actual first-party connectors to do the same job custom MCP connectors were closed as `NOT_PLANNED` for.

---

## 1. What actually changes vs. local mode

Less than either replacement idea implied. Local mode's back half — the part that knows what sessions are doing and gets text into them — needs **zero changes** under either §2 or §3 below. What moves is only the physical location of the STT/TTS/routing front end.

| Component | Local mode (today) | Saturday Native |
|---|---|---|
| STT/TTS | `saturday-audio`, laptop CPU (Whisper/Kokoro) | phone-side, via whichever front end (§2 or §3) |
| router (Haiku) | direct API call | **unchanged** — same call, now triggered by relayed text |
| expander (Sonnet) | direct API call, opt-in | **unchanged** |
| arc summarizer (Haiku, 5-min) | direct API call | **unchanged** |
| `saturday-watcher` | required | **unchanged** |
| inject path (`tmux send-keys`, direct-write) | required | **unchanged**, extracted into a package (Phase 0 below) |
| completion detector | drives TTS callback | **unchanged mechanism, new delivery** — push, not poll |
| callsign rule | required, disambiguation-oriented | **repurposed** — see §1a |
| ambient awareness | TTS speaks unprompted | push notification, not a poll |

The entire routing/expansion/summarization LLM logic is untouched either way — it was never the vendor's job to do that reasoning, it was always Saturday's own Haiku/Sonnet calls, at the same privacy tier (standard API terms) local mode already accepts today.

### 1a. Callsigns: kept, for a different reason than before

MCP mode's panel review argued callsigns were solving a problem that stopped existing once Claude does contextual disambiguation instead of a cold Haiku router — reasonable, but it was reasoning about the *routing* layer. Hands-on testing surfaced a problem one layer up: **"lucida" took several tries for voice STT to recognize correctly, cold, with no surrounding context.** That's not a routing-ambiguity problem an LLM can reason its way past — it's a transcription failure that happens before any reasoning runs. Saturday's actual project names lean into mining vocabulary (`winze`, `stope`, `cupel`, `adit`) that's plausibly similar rare-word/low-STT-coverage risk.

Fix: keep project names everywhere they already work (git, filesystem, this doc). For voice addressing specifically, reuse `withCallsignRule`'s existing mechanism, but repoint its justification — pick each session's spoken alias for maximum phonetic distinctiveness from other aliases and from common English words, not for enumerable-token-space reasons. Old mechanism, new reason to keep it.

---

## 2. Validated shortcut: voice → first-party connector → private backend

**This is hand-tested and working as of this session, not speculative.** It resurrects "use the vendor's actual voice product" — which MCP mode's whole premise depended on and lost — by routing through a first-party connector Anthropic actually supports in voice, instead of a custom one they've closed as not-planned.

### Mechanism

A private, dedicated Google Drive folder acts as a message bus. An account-wide instruction tells Claude: when a Claude Code session is named alongside something you want done, write a plain-language note describing the request to that folder — not a literal shell command, a description of intent, the same relationship Whisper's loose transcript has to local mode's Sonnet expander today. `saturday-backend` polls the folder, feeds new notes into the same router/expander local mode already runs, and injects the result. Nothing command-shaped ever has to exist inside the voice conversation itself.

### What's confirmed, and how

- **Write access exists on Max, not just Team/Enterprise.** Confirmed directly against Anthropic's own connector documentation (Gmail send/reply/forward with a per-action approval prompt; Drive read/write with none).
- **No paid Google Workspace or domain required.** Confirmed directly against Anthropic's help article — the requirement is "authenticate with your Google account," full stop. The admin-approval friction that shows up in search results is scoped to Claude Team/Enterprise plans or to the *connected* Google account being an org-managed Workspace account — neither applies to a plain personal Gmail.
- **One Google connector per individual Claude account, not additive.** Connecting a second Google account replaces the first rather than adding to it (confirmed as of July 2026, open unshipped feature request for named multi-account support). Not a blocker here specifically because nothing personal was already connected — a dedicated, previously-unused Gmail account was repurposed for this with zero conflict.
- **Doesn't violate the Usage Policy or the automated-access clause in the Consumer Terms** — read directly, not summarized. The "circumvent guardrails" language in the Usage Policy is scoped to servers listed in Anthropic's public Connector Directory; this never gets submitted there. The Consumer Terms' bot/script restriction is about accessing Claude's own Services through automated means without an API key; a human drives every live voice turn here, and the only things touched programmatically are Google's Drive API and the Anthropic API key Saturday's router/expander already uses today.
- **The end-to-end mechanism actually fires** — confirmed through a real sequence of hands-on tests, not assumption: a trivial trigger phrase worked first (proving account-wide settings *can* reach a live voice turn at all); a literal command-authoring instruction was refused by Claude's own conversational memory-save step (revealing memory can shape text responses but won't be stored as a standing trigger for autonomous tool use); a plain-language "write a note" version, phrased as a non-optional habit rather than a stylistic preference, was accepted; a test conversation correctly declined to write anything when the stated condition wasn't met; a follow-up correctly wrote a real file when the condition was met — in voice mode specifically, not text.

### Design lessons that came out of testing, not assumption

- **The trigger word needs to survive being spoken, not just being read.** "Lucida" needed several tries. See §1a.
- **"Fun and simple" framing works, "optional-sounding" framing doesn't.** A rule stored as "a fun call-and-response" got filed by Claude's own memory system under a casual category and is a real risk for anything that needs deterministic execution. A rule stored as a plain, low-key, but explicitly non-optional habit — "always do this, don't skip it, it's how my tools stay in sync" — held up. Tone can stay warm; the "required, not decorative" signal has to survive the phrasing regardless.
- **Notes, not commands, is the right shape for the payload — not just the safer one.** Asking Claude to write literal executable syntax both risks the same false-positive classifier behavior seen in the Fable 5 incident (command-shaped content is what that classifier pattern-matches on) and creates a verbatim-transcription requirement in tension with "keep it simple." Asking it to write down *what was asked*, in plain language, sidesteps both — the actual precision work happens in Saturday's own expander afterward, same as it always has for spoken utterances.
- **The note has to be self-contained.** Saturday's backend reading a Drive file doesn't have the rest of the voice conversation the way local mode's Whisper transcript always did — the operational rule has to explicitly require naming the session and stating the full request, not a shorthand that only makes sense with context the backend will never see.
- **Casual, personal-sounding framing is plausibly the *safer* register against the classifier, not just the more pleasant one.** Clinical, technical-sounding automation language is closer to what an over-cautious classifier flags on than something that reads as a hobbyist keeping their own notes.

### Still open, not yet tested

- **Whether a genuinely cold-started voice conversation reads the rule at all** — sidestepped rather than resolved: the intended workflow is to open each session in text first (which reliably reads Settings, confirmed) and switch to voice within that same continuous conversation, exactly the shape every successful test so far already had. A true cold voice start never happens under this workflow, so the question doesn't need answering. One small thing worth confirming in passing, not a dedicated test: whether merely opening a text conversation triggers the read, or whether an actual message has to be sent first.
- **Resolved 2026-08-24: Instructions for Claude is the field that holds.** Both the note-writing rule and the session-manifest-check addendum below live there and both fire reliably in voice mode. Earlier ambiguity (Preferences appeared to work first) was never a controlled comparison — treat this as settled rather than re-testing the field choice again.
- **Ambient awareness here is async, not integrated** — Slack or Drive's own mobile push (when the connector eventually writes a result back) is a real notification channel, better than anything MCP mode had, but it's "notice a push, then reopen voice mode to ask about it," not a spoken interruption mid-conversation the way §3's APNs design is.
- **Session-name inventory pushed back to Drive — confirmed live on both ends, 2026-08-24.** `saturday-backend` writes a small `saturday-sessions.txt` manifest into the same folder every poll tick, listing every live session by project name, split into reachable-now (has a tmux pane) and headless-only (`manifest.go`). This closed a real gap found live 2026-08-24: voice mode had no way to know which session names actually exist, and a stale test note ("Lucida," no live session by that name) got force-routed onto whatever session happened to be live instead of being caught. Voice mode reading the file back before naming a session is confirmed too now, via an addendum in the same Instructions for Claude field. Measured cost: reading the Drive files adds ~2s of latency per conversational turn — tolerable, not ideal. Separate caveat, not about voice mode: the manifest is only as fresh as the last poll tick that wrote it — if `saturday-backend` isn't running, voice mode reads whatever it last wrote, not a live snapshot.

---

## 3. The other option: own the client entirely

Kept as the fallback/upgrade path if §2's approach hits a wall (Anthropic tightens connector scopes, adds validation that breaks the note-relay pattern, or the cold-start question above resolves badly) — or if the async-only ambient awareness in §2 stops being good enough.

### Hardware roles

- **This laptop** — unchanged either way. Hosts the tmux sessions, `saturday-watcher`, the inject path. Never needed a GPU for this and still doesn't.
- **iPhone 14** — on-device STT (Apple's Speech framework) and on-device TTS (`AVSpeechSynthesizer`), both available on any modern iPhone, **not** gated behind Apple's newer on-device Foundation Models framework (that needs 15 Pro+, and isn't used here — no on-device LLM reasoning on the phone, by design). The phone's only job is listen, relay text, speak, and receive push.
- **PC, RTX 3080 (10GB)** — hosts `saturday-backend`: inject relay, session state, cursor/settle bookkeeping. Also the candidate to later host real speech-native inference (Moshi/Unmute) if a checkpoint fits under ~10GB or the card gets upgraded — checked directly against both projects' documented requirements: Moshi's PyTorch path needs 24GB with no quantization support in that backend; Unmute's full stack (STT+TTS+LLM) needs 16GB total. Neither fits today. Not required for this path to work regardless.
- **Cloud, scoped narrowly** — fallback tunnel ingress for off-tailnet use, fronting *your own* app rather than a vendor's; and/or occasional rented GPU time (RunPod/Lambda-style, single-tenant, retention you control) to run Moshi on demand without owning that hardware full-time.

### Ambient awareness — the thing §2 can't fully fix

A plain APNs push when `saturday-watcher`'s completion detector fires is a true interrupt, sent the instant a background session settles regardless of what the user is doing. No MCP session-affinity constraint to satisfy, because the phone app isn't an MCP client — `saturday-backend` can hold durable per-device state as plainly as any normal server does.

### Security posture

Same threat as always: **Saturday types arbitrary text into live shells.** What differs from MCP mode's framing is *who* the client is, not the stakes.

- **Auth belongs to you, not a vendor's connector UI** — a WireGuard/Tailscale identity is often sufficient on a private tailnet; add a proper token or mTLS cert for the cloud-tunnel fallback.
- **Prompt injection into `inject` is still the top risk, independent of transport or which of §2/§3 is in play.** A booby-trapped file surfaced through a session's transcript or arc summary can still steer an LLM-authored inject prompt. Keep session-derived content clearly delimited as data, not instructions, wherever it reaches a model.
- **No tunnel-provider blast radius on the home-network path** — only the cloud-fallback path needs to reason about Cloudflare/Tailscale Funnel's own edge-termination behavior, and only when actually used.

### Known limits

- **Native iOS app is real iOS development, not a config file** — the one place this costs meaningfully more up-front effort than §2.
- **iOS push reliability for a home-grown app is a real unknown, not assumed** — spike before treating it as given.
- **No on-device LLM reasoning on the iPhone 14** — by design. Routing/expansion/summarization stays server-side either way.

---

## 4. Implementation phases

### Phase 0 — extraction (no new behavior, needed regardless of §2 vs §3)

- `internal/inject` — pane discovery, `tmux send-keys`, direct-write fallback, threshold logic, `withCallsignRule` (repurposed per §1a), das blinkenlights.
- `internal/watcherclient` — unix socket client for watcher state.
- `internal/settle` — size-stable + text-block detector, Haiku ≤15-word summarizer.

Acceptance: local mode behaves identically; `make ci` green.

### Phase 1 — Drive-relay backend (§2's path — lowest remaining effort)

New binary `saturday-backend`: polls the dedicated Drive folder, feeds new notes into the extracted router/expander, injects the result. No app to build — the front end already works, hand-tested.

Acceptance: open in text, switch to voice, say a session name and a request, and see it land in the pane — the intended everyday workflow, not a cold voice start.

### Phase 2 — push closes the loop (§3's path, if pursued)

Native phone app, `saturday-backend`'s HTTP/gRPC API, APNs wired to the completion detector.

Acceptance: kick off a 3-minute test run, put the phone down, get a push the moment it settles.

### Phase 3 — cloud fallback

Tunnel fronting `saturday-backend` for off-tailnet use, whichever front end is in play.

### Deferred — local speech-native inference

Moshi or Unmute on the 3080, contingent on a checkpoint that fits under ~10GB or a card upgrade. Not blocking either path above.

---

## 5. Open questions

1. **Whether opening a text conversation (vs. sending a first message) is what triggers the Settings read** — minor, confirm in passing during Phase 1 testing. The cold-voice-start question itself is resolved by workflow, not by test: always text-first, switch to voice within the same conversation.
2. **Native Swift app vs. a PWA using the Web Speech API**, if §3 ever gets built — a PWA would be far less effort, but iOS's Web Speech API and home-screen push support have historically lagged native.
3. **Does Apple's on-device Speech framework support anything like Whisper's `initial_prompt` vocabulary biasing?** Relevant only if §3 is pursued — local mode leans on this for project-specific terms.
4. **Real round-trip latency** for whichever path ships — measure before assuming either feels conversational.
