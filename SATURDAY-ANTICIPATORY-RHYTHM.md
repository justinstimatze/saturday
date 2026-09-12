# Saturday — anticipatory rhythm

**Status: proposal, unbuilt.** Written for a session picking this up cold — it
carries its own citations and doesn't assume the conversation that produced it.

## The claim this is answering

Terence Tao, "The paradox at the heart of AI and science" (Big Think Clips,
published 2026-09-03, 30:11, https://youtu.be/svl_1upFpQo — transcript pulled
via `yt-dlp`'s auto-captions the same day this doc was written). Tao describes
what happens with a long-running human collaborator: you get "mentally
attuned" to the point of completing each other's sentences — you can throw out
an idea before finishing it and the other person is already running with it.
He then says directly that current AI tools don't have this: interactions are
turn-based, not fluid, and when he tried having an AI present alongside a
human collaborator in person, its presence broke the rhythm of the human
conversation. He still prefers AI for secondary tasks (literature search,
proof-checking, code, proofreading) over the live problem-solving process,
and names this — not capability — as the current limit.

This doc is the saturday-shaped version of that gap: not "can the model
answer well" but "does the interaction have the turn-taking latency of a
person, or of a dispatch queue."

## Where saturday actually sits today

Per `README.md`'s own framing, saturday's whole premise is removing the
window-switch tax: mic → VAD/Whisper → `saturday-mayor` (router + expander) →
`tmux send-keys` into the live pane → `saturday-watcher` polling the JSONL
transcript at 200ms for completion. That already answers a different problem
than Tao's — not modality friction, turn latency.

Two things are already true and matter here:

- **Output-side streaming shipped.** `ROADMAP.md` (commit `dfc98ad`,
  2026-08-25 20:36): `RunAskStreaming` decodes Anthropic's SSE tool-input
  stream incrementally, and `SpeakStream`/`wordBatcher` in `orchestrator`
  batches it for TTS. Once Claude starts producing an answer, the latency to
  first spoken word is already cut.
- **Input-side anticipation is not.** The wire protocol between
  `saturday-audio` and its consumers already reserves a message-type slot for
  this — `saturday-audio/README.md:103` notes `partial` and `cancel` can be
  added to the schema "without breaking compatibility" — but nothing today
  emits a `partial` message. The pipeline is batch-per-utterance on the way
  in: mayor sees nothing until VAD calls the utterance done.

That's the actual gap. Tao's "before you even finish the sentence" case is an
input-side phenomenon — his collaborator's prediction starts from an
incomplete signal. Saturday's streaming so far is entirely on the output
side: it makes the answer arrive faster once mayor already has the whole
question.

`ROADMAP.md:189` already names the right next check before building anything
here: *"Real round-trip latency for whichever path ships — measure before
assuming either feels conversational."* This proposal should not skip that —
measure the current utterance-end → first-spoken-word latency before adding
anticipatory machinery on top of an unmeasured baseline. If it's already
sub-second, the case for this doc weakens; if it's multiple seconds, it's the
first thing worth attacking.

## The proposal

Wire the reserved `partial` message type through: `saturday-audio` emits
interim ASR hypotheses as the utterance is still being spoken (most streaming
ASR backends, including Whisper-family streaming variants, support this
natively), and `saturday-mayor` gets a second, cheaper code path that watches
the partial stream rather than waiting for the final one.

Two shapes worth prototyping, not necessarily either/or:

1. **Speculative pre-fetch.** At a plausible sentence-completion checkpoint
   (heuristic: N words with falling pitch / a recognized routing keyword
   already resolved / partial-transcript stability over K frames), start the
   expander call speculatively. If VAD ends the utterance close to what was
   guessed, the inject is already most of the way through the pipe. If the
   speaker keeps going past the checkpoint, cancel and restart — this is what
   the reserved `cancel` message type is for.
2. **Local heuristic head start.** Cheaper, no LLM call: once the partial
   transcript resolves the target session name and rough intent
   deterministically (a lot of saturday's routing already looks rule-shaped
   per `mayor`'s router), pre-warm the tmux pane / pre-resolve the session
   target while the person is still finishing the sentence, so the expander
   call that does fire has less dead time in front of it.

(1) is the one that actually chases Tao's phenomenon — a real guess at what's
coming, sometimes wrong, corrected before it costs anything visible. (2) is
lower-risk and worth shipping first as the measurement vehicle for whether
(1) is even worth the false-positive cost.

This is the same tradeoff `SATURDAY-VOICE-NATIVE.md:95` already named for a
different feature — a deliberate correctness-over-latency call, made
explicitly rather than defaulted into. Speculative pre-fetch needs the same
explicit reckoning: a wrong guess that fires an expander call is wasted
compute and, worse, a wrong-session inject if the cancel path has a race. The
router/expander eval harness already referenced in `ROADMAP.md`'s V0.2.7 entry
(pass-rate tracking, `over_cautious` vs `over_eager` failure profiles) is the
right place to hang a new eval axis for this: false-speculative-fire rate,
not just accuracy.

## A second thing Tao's talk raises, orthogonal to latency

His in-person observation — an AI's presence breaking the rhythm of a human
conversation — is a caution against saturday's mic-open-by-default posture
specifically in paired-human settings, distinct from the solo-user case the
project is built around. The existing `SPACEBAR`-mute control (README, audio
pane) is the coarse version of an answer. Worth a sharper one: a
"who-initiated-this-turn" signal, or a mode that defaults to silent/inject-
only (no spoken completion report) when two people are audibly in a live
back-and-forth, rather than every mode defaulting to open-mic + spoken
callback. Not scoped further here — flagging it because it's a real, named
cost in the source material, not a hypothetical.

## Open questions for whoever builds this

1. Does the current ASR backend (`saturday-audio`) actually expose
   partial/interim hypotheses, or would this require a backend swap? Check
   before designing the mayor side.
2. Get the round-trip latency measurement from `ROADMAP.md:189` done first —
   it's the number that says whether this doc is worth building at all.
3. Design the cancel path's failure mode explicitly: what happens if a
   speculative inject already landed in the tmux pane before cancel arrives?
4. Does this want its own eval axis (false-fire rate) before it ships, given
   the V0.2.7 lesson that an under-specified prompt change regressed pass
   rate 77% → 57%?

---
Drafted 2026-09-07, following a conversation comparing Tao's interview
against saturday's and ettle's architectures. Citations above are to primary
sources (the transcript, this repo's own files) — verify line numbers against
current `HEAD` before trusting them, since roadmap files move fast here.
