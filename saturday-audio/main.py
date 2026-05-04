#!/usr/bin/env python3
"""saturday-audio — V0.2.0 voice sidecar.

Captures mic audio, runs Silero VAD for utterance segmentation, transcribes
flushed utterances with faster-whisper, and emits line-delimited JSON over
a Unix socket to saturday-mayor.

Open-mic by default (configurable to ptt later — V0.2.1). Press SPACEBAR
in the terminal to toggle mute; 'q' to quit. When muted, audio still flows
through VAD (cheap) but transcripts are dropped before STT — neither
logged nor emitted.

Vocab bias: every 60s the sidecar pulls the watcher's session list and
extracts 'weird words' (project names, identifiers, file basenames, jargon
mined from recent user/assistant text) to bias Whisper toward terms it
otherwise mishears (e.g. 'adit' → 'add it', 'lucida' → 'lucid' / 'lit a').
Pinned terms via --vocab-pin always take priority.

Wire format:  one JSON object per line, e.g.
    {"type":"utterance","text":"in lucida, run git status","ts":1714780123.45}

Mayor must be running with --audio-sock pointing at the same socket. If
mayor isn't up yet, the sidecar retries every 2s until it is or you quit.
"""
import argparse
import datetime as dt
import hashlib
import http.client
import json
import os
import queue
import random
import re
import socket
import sys
import termios
import threading
import time
import tty
import urllib.request
from pathlib import Path

import numpy as np
import sounddevice as sd
import torch
from faster_whisper import WhisperModel
from silero_vad import VADIterator, load_silero_vad


SAMPLE_RATE = 16000
CHUNK_SAMPLES = 512  # 32ms @ 16kHz; matches Silero's expected window
SILENCE_FLUSH_MS = 500  # was 700; tightened for snappier flush
TTS_SAMPLE_RATE = 24000  # Kokoro emits 24kHz mono float32
DEFAULT_SOCK = "/tmp/saturday-audio.sock"
DEFAULT_MODEL = "small.en"
DEFAULT_TTS_VOICE = "bm_george"
DEFAULT_HOTPHRASE = "would you kindly"
DEFAULT_LOG_DIR = Path.home() / ".claude" / "saturday" / "transcripts"

# FUI-style status renderer state. The spinner thread owns a single line at
# the bottom of the pane, refreshing at ~6Hz. All other writers go through
# slog() which clears the spinner line first (\r + ANSI clear-line) and
# prints its message + newline. The spinner's next tick then redraws on the
# fresh bottom line. Lock serializes writes so concurrent threads don't
# garble each other or the spinner mid-render.
_stderr_lock = threading.Lock()
SPINNER_FRAMES = "⣾⣽⣻⢿⡿⣟⣯⣷"
SPINNER_HZ = 6.5  # frames per second


def slog(msg: str, end: str = "\n"):
    """Thread-safe stderr write that politely clears the spinner line first."""
    with _stderr_lock:
        sys.stderr.write("\r\033[K" + msg + end)
        sys.stderr.flush()


# ANSI color helpers for slog. Keep palette small + register-coherent:
# vocab/chrome dim, user-input bright cyan, system-output green, mute red.
_C_DIM = "\033[2m"
_C_DIM_CYAN = "\033[2;36m"
_C_BRI_CYAN = "\033[1;36m"
_C_BRI_GREEN = "\033[1;32m"
_C_BRI_RED = "\033[1;31m"
_C_BRI_YELLOW = "\033[1;33m"
_C_RESET = "\033[0m"


def _c(prefix: str, msg: str) -> str:
    return f"{prefix}{msg}{_C_RESET}"


STOCK_PHRASES = [
    "On it.",
    "Got it.",
    "Right away.",
    "Sure thing.",
    "Doing that now.",
]


class StockTTS:
    """Pre-rendered acknowledgment phrases. Same Kokoro voice as fresh narration
    so they sound continuous. Picks at random for variety."""
    def __init__(self, samples_list, sample_rate):
        self.samples = samples_list
        self.sample_rate = sample_rate

    def random(self):
        return random.choice(self.samples) if self.samples else None


class State:
    def __init__(self):
        self.muted = threading.Event()
        self.quit = threading.Event()
        # set by tts_player while playing audio out, so transcribe_loop drops
        # any chunks the mic captures from the speakers (cheap echo guard).
        self.tts_playing = threading.Event()
        # FUI-status flags: hearing = VAD says speech in progress; transcribing
        # = STT model is running. Cleared at the appropriate boundary.
        self.hearing = threading.Event()
        self.transcribing = threading.Event()
        # mayor_activity is set from incoming {"type":"state",...} events on
        # the audio sock — the sidecar mirrors mayor's current micro-state
        # in its own status line so the operator sees the whole pipeline.
        self.mayor_activity = ""
        self.mayor_activity_lock = threading.Lock()
        self.audio_q = queue.Queue()
        # outbox_q: bytes (json+\n) we want to send to mayor. drained by conn_manager.
        # event_q: parsed JSON dicts. Two sources: (1) sidecar's own stock-ack
        # injections on hotphrase match, (2) speak events received FROM mayor.
        # tts_player drains and dispatches.
        self.outbox_q = queue.Queue()
        self.event_q = queue.Queue()
        # set by tts_player after pre-rendering stock acks. None until ready.
        self.stock_tts = None
        # set by main once all subsystems have logged [ready]. Until then the
        # status renderer shows "warming up" so user knows not to talk yet.
        self.ready = threading.Event()
        # V0.2.7: live mic level in dBFS, written by mic callback, read by
        # rms_ticker. Smoothed via fast attack / slow decay so the spark
        # tracks transient peaks but doesn't flicker on quiet frames.
        # -90 = silence floor; 0 = full-scale.
        self.last_rms_db = -90.0
        self.last_rms_lock = threading.Lock()

    def set_mayor_activity(self, label: str):
        with self.mayor_activity_lock:
            self.mayor_activity = label

    def get_mayor_activity(self) -> str:
        with self.mayor_activity_lock:
            return self.mayor_activity


def strip_hotphrase(text: str, hotphrase: str):
    """If text starts with hotphrase (case-insensitive, allowing comma/period
    after), return the remainder stripped of leading punctuation/whitespace.
    Empty hotphrase = pass-through (no gating). Returns None if no match."""
    if not hotphrase:
        return text
    norm_hp = hotphrase.strip().lower()
    norm_text = text.strip().lower()
    if not norm_text.startswith(norm_hp):
        return None
    rest = text.strip()[len(hotphrase):]
    rest = rest.lstrip(" ,.;:?!-")
    return rest if rest else None


def keyboard_reader(state, mute_key, quit_key):
    fd = sys.stdin.fileno()
    if not sys.stdin.isatty():
        state.quit.wait()
        return
    old = termios.tcgetattr(fd)
    try:
        tty.setcbreak(fd)
        while not state.quit.is_set():
            ch = sys.stdin.read(1)
            if ch == mute_key:
                if state.muted.is_set():
                    state.muted.clear()
                    slog(_c(_C_BRI_GREEN, "[live]"))
                else:
                    state.muted.set()
                    slog(_c(_C_BRI_RED, "[muted]"))
            elif ch == quit_key:
                state.quit.set()
                return
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)


def make_mic_callback(state):
    def cb(indata, frames, t, status):
        if status:
            slog(f"[mic] {status}")
        chunk = indata[:, 0].copy()
        state.audio_q.put(chunk)
        # V0.2.7: dBFS RMS for the thinking-pane spark. Float32 already in
        # [-1, 1]. Fast attack / slow decay so peaks read but the bar
        # doesn't strobe between quiet frames.
        rms = float(np.sqrt(np.mean(chunk * chunk))) if len(chunk) else 0.0
        if rms < 1e-5:
            db = -90.0
        else:
            db = 20.0 * np.log10(rms)
        with state.last_rms_lock:
            prev = state.last_rms_db
            if db > prev:
                state.last_rms_db = db  # attack instantly
            else:
                state.last_rms_db = prev * 0.7 + db * 0.3  # decay
    return cb


def rms_ticker(state):
    """Pushes one {"type":"rms",...} frame to mayor every 200ms (5 Hz).
    When muted, sends -90 dBFS so the spark flatlines as a visible cue.
    Stops on quit.
    """
    while not state.quit.is_set():
        if state.muted.is_set():
            db = -90.0
        else:
            with state.last_rms_lock:
                db = state.last_rms_db
        evt = {"type": "rms", "db": round(db, 1), "ts": time.time()}
        try:
            state.outbox_q.put_nowait((json.dumps(evt) + "\n").encode("utf-8"))
        except queue.Full:
            pass
        if state.quit.wait(0.2):
            return


def connect_to_mayor(audio_sock_path, quit_event):
    while not quit_event.is_set():
        try:
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            s.connect(audio_sock_path)
            return s
        except (FileNotFoundError, ConnectionRefusedError):
            slog(f"[sock] mayor not listening on {audio_sock_path}, retry in 2s")
            if quit_event.wait(2.0):
                return None
    return None


def conn_manager(state, audio_sock_path):
    """Single owner of the mayor socket. Drains outbox_q (utterances to send),
    fills event_q (events received). Reconnects on failure. All other threads
    only touch the queues — no shared socket access, no locks needed."""
    sock = None
    pending = None  # bytes we tried to send last but failed; resend on reconnect
    buf = b""

    while not state.quit.is_set():
        if sock is None:
            sock = connect_to_mayor(audio_sock_path, state.quit)
            if sock is None:
                return  # quit signaled
            sock.settimeout(0.1)
            slog("[sock] connected to mayor")
            buf = b""

        # send: pending takes priority, else pop one from outbox
        if pending is None:
            try:
                pending = state.outbox_q.get_nowait()
            except queue.Empty:
                pending = None
        if pending is not None:
            try:
                sock.sendall(pending)
                pending = None
            except (BrokenPipeError, OSError) as e:
                slog(f"[sock] send failed: {e} — reconnecting")
                try:
                    sock.close()
                except Exception:
                    pass
                sock = None
                continue  # pending kept for next conn

        # recv: poll non-blocking via short timeout
        try:
            data = sock.recv(4096)
            if not data:
                slog("[sock] mayor closed connection — reconnecting")
                try:
                    sock.close()
                except Exception:
                    pass
                sock = None
                continue
            buf += data
            while b"\n" in buf:
                line, buf = buf.split(b"\n", 1)
                line = line.strip()
                if not line:
                    continue
                try:
                    evt = json.loads(line.decode("utf-8"))
                except (json.JSONDecodeError, UnicodeDecodeError) as e:
                    slog(f"[sock] bad incoming JSON: {e}")
                    continue
                # State events go straight to the spinner — bypass event_q
                # so they aren't queued behind a long sd.play() in tts_player.
                if evt.get("type") == "state":
                    state.set_mayor_activity(evt.get("activity") or "")
                    continue
                state.event_q.put(evt)
        except socket.timeout:
            pass
        except (BrokenPipeError, OSError) as e:
            slog(f"[sock] recv failed: {e} — reconnecting")
            try:
                sock.close()
            except Exception:
                pass
            sock = None


def parse_voice(spec: str):
    """'af_sarah' → 'af_sarah'. 'am_adam+af_sarah' → ('am_adam', 'af_sarah').
    kokoro-onnx accepts either a string voice id or a tuple of voice ids
    (averaged equally) as the `voice=` kwarg."""
    if "+" in spec:
        parts = tuple(p.strip() for p in spec.split("+") if p.strip())
        return parts if len(parts) > 1 else parts[0]
    return spec


KOKORO_CACHE_DIR = Path.home() / ".cache" / "saturday-audio" / "kokoro"
KOKORO_MODEL_URL = "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx"
KOKORO_VOICES_URL = "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin"
STOCK_TTS_CACHE_DIR = Path.home() / ".cache" / "saturday-audio" / "stock"


def prune_stock_cache(max_files: int = 50):
    """Cap STOCK_TTS_CACHE_DIR at max_files entries (oldest by mtime first).
    Stock phrases × voice combinations grow as users try different voices;
    50 entries ≈ 10 voices' worth of the 5-phrase set. ~50 KB each, so cap
    is ~2.5 MB. Each .npy has a sibling .sr — count them as a pair."""
    if not STOCK_TTS_CACHE_DIR.exists():
        return
    npys = []
    for p in STOCK_TTS_CACHE_DIR.iterdir():
        if p.suffix == ".npy" and p.is_file():
            try:
                npys.append((p.stat().st_mtime, p))
            except OSError:
                continue
    if len(npys) <= max_files:
        return
    npys.sort()  # oldest first
    to_remove = npys[: len(npys) - max_files]
    for _mtime, p in to_remove:
        for sibling in (p, p.with_suffix(".sr")):
            try:
                sibling.unlink()
            except OSError:
                pass


def cached_synth(tts, voice, phrase: str, kokoro_voices_path: str):
    """Look up (or synthesize and cache) the audio for a stock phrase.

    Cache key: sha256 of voice + phrase + the bytes of voices-v1.0.bin
    (so swapping the kokoro model version invalidates old caches). Stored
    as a numpy .npy file at STOCK_TTS_CACHE_DIR/<key>.npy and a sidecar
    .sr file holding the integer sample rate.

    Returns (samples ndarray, sample_rate int) or None on failure."""
    STOCK_TTS_CACHE_DIR.mkdir(parents=True, exist_ok=True)
    voice_key = voice if isinstance(voice, str) else "+".join(voice)
    # voices-bin mtime is a cheap proxy for "kokoro model files unchanged"
    try:
        voices_mtime = int(Path(kokoro_voices_path).stat().st_mtime)
    except OSError:
        voices_mtime = 0
    key = hashlib.sha256(f"{voice_key}|{phrase}|{voices_mtime}".encode("utf-8")).hexdigest()[:16]
    npy_path = STOCK_TTS_CACHE_DIR / f"{key}.npy"
    sr_path = STOCK_TTS_CACHE_DIR / f"{key}.sr"
    if npy_path.exists() and sr_path.exists():
        try:
            samples = np.load(npy_path)
            sample_rate = int(sr_path.read_text().strip())
            return samples, sample_rate
        except Exception:
            pass  # fall through to re-synth
    s, sr = tts.create(phrase, voice=voice, speed=1.0, lang="en-us")
    samples = np.asarray(s, dtype=np.float32)
    try:
        np.save(npy_path, samples)
        sr_path.write_text(str(int(sr)))
    except Exception as e:
        slog(f"[tts] stock cache write failed for {phrase!r}: {e}")
    return samples, sr


def kokoro_files():
    """Return (model_path, voices_path) for kokoro-onnx, downloading from
    the kokoro-onnx GitHub release on first run.

    The huggingface 'onnx-community/Kokoro-82M-ONNX' repo doesn't have a
    single voices.bin (voices are separate files there). The canonical
    kokoro-onnx artifacts live on github.com/thewh1teagle/kokoro-onnx
    releases — stable URLs, no auth needed, ~350MB total."""
    KOKORO_CACHE_DIR.mkdir(parents=True, exist_ok=True)
    model_path = KOKORO_CACHE_DIR / "kokoro-v1.0.onnx"
    voices_path = KOKORO_CACHE_DIR / "voices-v1.0.bin"

    def _dl(url, dest, label):
        if dest.exists() and dest.stat().st_size > 0:
            return
        slog(f"[tts] downloading {label} → {dest}…")
        tmp = dest.with_suffix(dest.suffix + ".part")
        try:
            urllib.request.urlretrieve(url, tmp)
            tmp.replace(dest)
            slog(f"[tts] {label} ok ({dest.stat().st_size // (1024*1024)}MB)")
        except Exception:
            try:
                tmp.unlink()
            except FileNotFoundError:
                pass
            raise

    _dl(KOKORO_MODEL_URL, model_path, "kokoro model (~325MB)")
    _dl(KOKORO_VOICES_URL, voices_path, "kokoro voices (~28MB)")
    return str(model_path), str(voices_path)


def tts_player(state, args):
    """Drain event_q. On {type:'speak', text:...}, synthesize via kokoro-onnx
    and play out the default audio device. Sets state.tts_playing during
    playback so transcribe_loop drops mic chunks (echo guard).

    Uses kokoro-onnx (pure ONNX runtime, no spacy/transformers deps) instead
    of the kokoro PyPI package — the latter pulls misaki[en] → spacy 4.0-dev
    which fails to build under Python 3.13."""
    slog(f"[tts] loading kokoro-onnx (voice={args.tts_voice})…")
    try:
        from kokoro_onnx import Kokoro
    except ImportError as e:
        slog(f"[tts] disabled — kokoro-onnx not installed ({e})")
        while not state.quit.is_set():
            try:
                state.event_q.get(timeout=1.0)
            except queue.Empty:
                continue
        return
    try:
        model_path, voices_path = kokoro_files()
    except Exception as e:
        slog(f"[tts] disabled — model fetch failed: {e}")
        while not state.quit.is_set():
            try:
                state.event_q.get(timeout=1.0)
            except queue.Empty:
                continue
        return
    try:
        tts = Kokoro(model_path, voices_path)
    except Exception as e:
        slog(f"[tts] disabled — kokoro init failed: {e}")
        while not state.quit.is_set():
            try:
                state.event_q.get(timeout=1.0)
            except queue.Empty:
                continue
        return
    voice = parse_voice(args.tts_voice)

    # Pre-render or load-from-cache stock acks. Cached entries load in ~ms;
    # only the first run with a new voice pays the synth cost.
    slog(f"[tts] preparing {len(STOCK_PHRASES)} stock acks…")
    stock_samples = []
    stock_sr = None
    cache_hits = 0
    for phrase in STOCK_PHRASES:
        if state.quit.is_set():
            return
        try:
            existed_before = (STOCK_TTS_CACHE_DIR / hashlib.sha256(
                f"{voice if isinstance(voice, str) else '+'.join(voice)}|{phrase}|"
                f"{int(Path(voices_path).stat().st_mtime) if Path(voices_path).exists() else 0}".encode()
            ).hexdigest()[:16]).with_suffix(".npy").exists()
            samples, sr = cached_synth(tts, voice, phrase, voices_path)
            stock_samples.append(samples)
            stock_sr = sr
            if existed_before:
                cache_hits += 1
        except Exception as e:
            slog(f"[tts] stock pre-render failed for {phrase!r}: {e}")
    state.stock_tts = StockTTS(stock_samples, stock_sr)
    slog(f"[tts] stock acks ready ({len(stock_samples)}, {cache_hits} cached)")
    # LRU-cap the stock cache; runs after we've loaded/refreshed the entries
    # we actually need so they're recently-touched and won't be pruned.
    prune_stock_cache(max_files=50)
    slog("[tts] ready")
    hp_show = (args.hotphrase or "(disabled)").strip() or "(disabled)"
    slog(f"\033[1;32m[ready] saturday-audio — mic ALWAYS live (open-mic). Default mode=verbatim; expand-prefix={hp_show!r} flips one utterance to LLM-rewritten mode. Voice={args.tts_voice}.\033[0m")
    # Flip the ready flag so the spinner stops showing "warming up" and
    # transitions to its normal state cascade.
    state.ready.set()

    while not state.quit.is_set():
        try:
            evt = state.event_q.get(timeout=0.5)
        except queue.Empty:
            continue
        evt_type = evt.get("type")
        if evt_type == "ack":
            samples = state.stock_tts.random() if state.stock_tts else None
            if samples is None:
                continue
            state.tts_playing.set()
            try:
                sd.play(samples, samplerate=state.stock_tts.sample_rate, blocking=True)
            except Exception as e:
                slog(f"[tts] play error: {e}")
            finally:
                state.tts_playing.clear()
            continue
        if evt_type != "speak":
            continue
        text = (evt.get("text") or "").strip()
        if not text:
            continue
        try:
            samples, sample_rate = tts.create(
                text, voice=voice, speed=1.0, lang="en-us"
            )
        except Exception as e:
            slog(f"[tts] synth error: {e}")
            continue
        preview = text if len(text) <= 80 else text[:77] + "…"
        slog(f"[tts] speaking: {preview}")
        state.tts_playing.set()
        try:
            sd.play(samples, samplerate=sample_rate, blocking=True)
        except Exception as e:
            slog(f"[tts] play error: {e}")
        finally:
            state.tts_playing.clear()


def write_transcript(log_dir: Path, text: str):
    log_dir.mkdir(parents=True, exist_ok=True)
    today = dt.date.today().isoformat()
    path = log_dir / f"{today}.log"
    with path.open("a") as f:
        f.write(f"{dt.datetime.now().isoformat()}  {text}\n")


# A small stoplist of common English words. Anything else is "weird"
# (likely a project name, identifier, technical term, jargon) and worth
# biasing Whisper toward via initial_prompt. ~500 words, lowercase, deduped.
COMMON_WORDS = set(
    "a about above after again against all am an and any are as at be "
    "because been before being below between both but by can could did "
    "do does doing don down during each else even ever every few for "
    "from further had has have having he her here hers herself him "
    "himself his how however i if in into is it its itself just like "
    "made make many may me might more most much must my myself need "
    "never new no nor not now of off often on once one only or other "
    "ought our ours ourselves out over own quite rather really right "
    "same say see seem seen she should since so some still such than "
    "that the their theirs them themselves then there these they "
    "this those though through thus to too under until up upon us "
    "use used using very was way we well were what when where "
    "whether which while who whom why will with within without would "
    "yes yet you your yours yourself yourselves it's that's there's "
    "here's he's she's we're they're you're i'm i've we've they've "
    "i'll we'll they'll you'll he'll she'll won't can't don't didn't "
    "doesn't isn't aren't wasn't weren't hasn't haven't hadn't shouldn't "
    "wouldn't couldn't mustn't shan't into onto across along around "
    "above below before after between among against during through "
    "without within toward away back away forward backward beyond "
    "near far also instead therefore moreover otherwise meanwhile "
    "always sometimes usually rarely often seldom ever never once "
    "twice maybe perhaps probably possibly definitely actually "
    "really truly really very quite somewhat rather indeed certainly "
    "okay ok yeah yep nope sure fine good bad great big small "
    "long short high low old new young new first last next previous "
    "early late soon now then today tomorrow yesterday week day "
    "year hour minute second time work job task thing stuff way "
    "place way thing reason fact case point part end start begin "
    "stop go come get put take give make do say tell ask want need "
    "feel try help mean know think believe seem look find use call "
    "show let bring keep hold turn move start stop add remove run "
    ".".split()
)

WORD_RE = re.compile(r"[A-Za-z][A-Za-z0-9_-]{2,29}")


def extract_weird_words(text: str):
    """Pull tokens from text that are likely useful as STT vocab bias.

    A token is 'weird' if any of:
      - mixed case (camelCase, PascalCase) — almost certainly an identifier
      - contains _ or - (snake_case, kebab-case)
      - lowercase but not in the common-English stoplist (likely a domain term,
        project name, jargon)
    """
    out = []
    seen = set()
    if not text:
        return out
    for raw in WORD_RE.findall(text):
        norm = raw.lower()
        if norm in seen:
            continue
        has_case_mix = any(c.isupper() for c in raw[1:]) and any(c.islower() for c in raw)
        has_separator = "_" in raw or "-" in raw
        is_uncommon = norm not in COMMON_WORDS and len(norm) >= 4
        if has_case_mix or has_separator or is_uncommon:
            seen.add(norm)
            out.append(raw)
    return out


def file_basename_terms(path: str):
    """Extract identifier-shaped chunks from a file path.

    /home/x/saturday-mayor/llmcore/router.go → ['saturday-mayor','llmcore','router']
    """
    if not path:
        return []
    out = []
    for part in path.replace("\\", "/").split("/"):
        if not part:
            continue
        # strip extension
        if "." in part:
            part = part.rsplit(".", 1)[0]
        if WORD_RE.fullmatch(part):
            out.append(part)
    return out


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, unix_path):
        super().__init__("localhost")
        self._unix_path = unix_path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self._unix_path)
        self.sock = s


def fetch_sessions(watcher_sock_path: str):
    conn = UnixHTTPConnection(watcher_sock_path)
    try:
        conn.request("GET", "/state")
        resp = conn.getresponse()
        if resp.status != 200:
            return []
        return json.loads(resp.read())
    finally:
        conn.close()


PATH_NOISE = {
    # filesystem locations
    "home", "root", "usr", "var", "etc", "tmp", "opt", "mnt", "media",
    "bin", "lib", "share", "src", "local", "documents", "downloads",
    "desktop", "pictures", "videos", "music", "node_modules", "dist",
    "build", "target", "vendor", "cache", "log", "logs",
    # code-organization conventions (universal across projects, no
    # signal as project-specific vocab; whisper already knows them)
    "internal", "pkg", "cmd", "api", "common", "core", "util", "utils",
    "helpers", "helper", "models", "model", "services", "service",
    "test", "tests", "spec", "specs", "mock", "mocks", "fixture",
    "fixtures", "main", "index", "config", "configs",
}


def _classify(raw: str):
    """Classify a token as 'id' (identifier — strong signal), 'plain'
    (uncommon English word — needs frequency proof), or None (skip)."""
    norm = raw.lower()
    if norm in PATH_NOISE or norm in COMMON_WORDS or len(norm) < 4:
        return None
    has_case_mix = any(c.isupper() for c in raw[1:]) and any(c.islower() for c in raw)
    has_separator = "_" in raw or "-" in raw
    if has_case_mix or has_separator:
        return "id"
    return "plain"


def split_separators(raw: str):
    """Normalize a token for inclusion in Whisper's initial_prompt:
      - alphanumeric only (drop tokens with anything else)
      - ≥3 chars
      - not in PATH_NOISE
      - kebab-case / snake_case → split into parts

    camelCase / PascalCase passes through whole. Returns a list of zero or
    more clean tokens.

    Why split + alphanumeric: hyphenated tokens in Whisper's initial_prompt
    leak a 'compound-everything' pattern signal — the model starts ad-hoc
    hyphenating unrelated word pairs ('lucida-project-list-the-changed').
    Feeding only alphanumeric parts gives per-word recognition bias without
    the compound pattern."""
    if "-" not in raw and "_" not in raw:
        if len(raw) >= 3 and raw.isalnum() and raw.lower() not in PATH_NOISE:
            return [raw]
        return []
    parts = []
    for p in re.split(r'[-_]', raw):
        if len(p) >= 3 and p.isalnum() and p.lower() not in PATH_NOISE:
            parts.append(p)
    return parts


def gather_vocab_from_sessions(sessions, max_terms=20):
    """Walk session entries, pull weird words from project, cwd, recent
    text fields and modified-file paths.

    Priority order:
      1. project name (always — it's the user's primary referent)
      2. identifiers (camelCase / snake_case / kebab-case) — one occurrence enough
      3. plain lowercase words that appear ≥ 2 times across gathered text
         (frequency proof guards against single-shot common words slipping
         past the small stoplist)

    Common path components (home, Documents, ...) are blocklisted so they
    don't crowd out real terms.
    """
    project_names: list = []
    identifiers: list = []
    plain_candidates: list = []
    plain_counts: dict = {}

    def consider_path(path: str):
        for sub in file_basename_terms(path or ""):
            kind = _classify(sub)
            if kind == "id":
                identifiers.append(sub)
            elif kind == "plain":
                plain_candidates.append(sub)
                plain_counts[sub.lower()] = plain_counts.get(sub.lower(), 0) + 1

    def consider_text(text: str):
        if not text:
            return
        for raw in WORD_RE.findall(text):
            kind = _classify(raw)
            if kind == "id":
                identifiers.append(raw)
            elif kind == "plain":
                plain_candidates.append(raw)
                plain_counts[raw.lower()] = plain_counts.get(raw.lower(), 0) + 1

    for entry in sessions or []:
        st = entry.get("state") or {}
        proj = st.get("project") or ""
        if proj:
            project_names.append(proj)
        consider_path(st.get("cwd") or "")
        for f in st.get("modified_files") or []:
            consider_path(f)
        ltu = st.get("last_tool_use") or {}
        consider_text(ltu.get("input_summary") or "")
        for field in ("last_user_turn", "last_assistant_text", "last_tool_result_tail"):
            consider_text(st.get(field) or "")

    seen: set = set()
    out: list = []

    def add(token):
        norm = token.lower()
        if norm in seen:
            return
        seen.add(norm)
        out.append(token)

    for n in project_names:
        if len(out) >= max_terms:
            return out
        add(n)
    for n in identifiers:
        if len(out) >= max_terms:
            return out
        add(n)
    for n in plain_candidates:
        if len(out) >= max_terms:
            return out
        if plain_counts.get(n.lower(), 0) >= 2:
            add(n)
    return out


def build_initial_prompt(names, pinned):
    """Compose Whisper initial_prompt from dynamic + pinned vocab.

    All terms go through split_separators — alphanumeric only, hyphens and
    underscores split into parts. Period-separated output (not comma list)
    so Whisper reads each term as a discrete prior mention rather than as
    items in a 'join everything' enumeration."""
    seen = set()
    merged = []
    for src in (pinned, names):
        for n in src:
            for part in split_separators(n):
                k = part.lower()
                if k in seen:
                    continue
                seen.add(k)
                merged.append(part)
    if not merged:
        return None
    return ". ".join(merged) + "."


SEEN_TERMS_MAX = 5000  # FIFO cap; sole purpose is suppressing dup [vocab] log lines


class VocabState:
    def __init__(self, watcher_sock, pinned):
        self.watcher_sock = watcher_sock
        self.pinned = pinned
        self.prompt = None
        # insertion-ordered dict acts as FIFO set: oldest term evicted at cap
        self.seen_terms = {}
        self.lock = threading.Lock()

    def _remember(self, terms_lower):
        for t in terms_lower:
            if t in self.seen_terms:
                continue
            self.seen_terms[t] = None
            if len(self.seen_terms) > SEEN_TERMS_MAX:
                self.seen_terms.pop(next(iter(self.seen_terms)))

    def refresh(self):
        try:
            sessions = fetch_sessions(self.watcher_sock)
        except (FileNotFoundError, ConnectionRefusedError, OSError) as e:
            slog(_c(_C_DIM, f"[vocab] watcher fetch failed: {e}"))
            return
        names = gather_vocab_from_sessions(sessions)
        new = build_initial_prompt(names, self.pinned)
        with self.lock:
            old = self.prompt
            self.prompt = new
        # First successful build → log full preview so user knows vocab is wired
        # up. Subsequent refreshes only log the *new* terms — nothing if the
        # pool is unchanged or only shrunk.
        if not new:
            return
        terms = [t for t in (s.strip() for s in new.rstrip(".").split(". ")) if t]
        if old is None:
            preview = new if len(new) <= 180 else new[:177] + "…"
            slog(_c(_C_DIM, f"[vocab] {preview}"))
            self._remember(t.lower() for t in terms)
            return
        added = [t for t in terms if t.lower() not in self.seen_terms]
        if added:
            preview = ", ".join(added)
            if len(preview) > 180:
                preview = preview[:177] + "…"
            slog(_c(_C_DIM, f"[vocab] +{preview}"))
            self._remember(t.lower() for t in added)

    def get(self):
        with self.lock:
            return self.prompt


def vocab_refresher(vocab_state, quit_event, period_sec=60):
    while not quit_event.is_set():
        vocab_state.refresh()
        if quit_event.wait(period_sec):
            return


def status_renderer(state):
    """Render an animated status line at the bottom of the audio pane.
    Reflects the highest-priority current micro-state in the pipeline. All
    other stderr writers go through slog() which clears this line first;
    the next tick redraws it cleanly."""
    period = 1.0 / SPINNER_HZ
    n = len(SPINNER_FRAMES)
    i = 0
    while not state.quit.is_set():
        # Priority cascade — highest one wins. Local sidecar states first
        # (closest to physical reality), then mayor's pipeline state, then
        # mute, then idle. Special-case warmup BEFORE everything since the
        # operator must know not to talk yet during init.
        if not state.ready.is_set():
            label, color = "warming up — please wait", "33"
        elif state.tts_playing.is_set():
            label, color = "speaking", "95"
        elif state.transcribing.is_set():
            label, color = "transcribing", "96"
        elif state.hearing.is_set():
            label, color = "hearing", "92"
        elif (m := state.get_mayor_activity()):
            label, color = m, "94"
        elif state.muted.is_set():
            label, color = "muted", "91"
        else:
            label, color = "idle", "90"
        spin = SPINNER_FRAMES[i % n]
        with _stderr_lock:
            sys.stderr.write(f"\r\033[K\033[{color}m{spin}\033[0m \033[2m{label}\033[0m")
            sys.stderr.flush()
        i += 1
        time.sleep(period)


# Narrate-mode prefixes. Strip-and-set-narrate prefixes are explicit speech
# directives (user told us how they want output back). Question-word
# prefixes are heuristic — natural questions usually want a readable answer
# in the CC pane, not a spoken summary.
_STRIP_FORCE = ("tell me ", "say ", "speak ")
_STRIP_SILENT = ("show me ", "list ")
_SILENT_HINT = ("what ", "where ", "which ", "how ", "why ", "who ", "when ")


def detect_narrate_mode(text):
    """Returns (stripped_text, narrate_mode) where narrate_mode is one of
    'force' (always speak the Phase 3 summary, override min-growth/elapsed
    filters), 'silent' (don't speak; user reads in target pane), or 'auto'
    (current default — speak if filters allow). Force/silent strip prefixes
    that are explicit directives; question-words set silent without
    stripping (the question itself is the inject)."""
    s = text.lstrip()
    sl = s.lower()
    for p in _STRIP_FORCE:
        if sl.startswith(p):
            return s[len(p):].lstrip(), "force"
    for p in _STRIP_SILENT:
        if sl.startswith(p):
            return s[len(p):].lstrip(), "silent"
    for p in _SILENT_HINT:
        if sl.startswith(p):
            return s, "silent"
    return s, "auto"


def _run_whisper(model, audio, initial_prompt):
    """One STT pass. Returns (text, mean_avg_logprob). Empty string + 0.0
    if no segments (which means whisper gave up — treat as low confidence)."""
    segments, _info = model.transcribe(
        audio, beam_size=1, language="en",
        initial_prompt=initial_prompt,
    )
    parts = []
    logprobs = []
    for seg in segments:
        parts.append(seg.text.strip())
        if seg.avg_logprob is not None:
            logprobs.append(seg.avg_logprob)
    text = " ".join(parts).strip()
    if not logprobs:
        return text, 0.0  # no segments → treat as no signal, neutral logprob
    return text, sum(logprobs) / len(logprobs)


def _pick_better(text1, lp1, text2, lp2, min_len_ratio=1.5, lp_gap=0.2):
    """Pick between vocab-biased pass (text1) and no-vocab pass (text2).

    Returns (text, logprob, source) where source ∈ {"first", "second"}.

    Heuristics, in order:
    1. If only one is non-empty, take that one.
    2. If second is meaningfully longer (≥ min_len_ratio words), it likely
       caught speech that vocab-bias collapsed — take it.
    3. If second's logprob is meaningfully better (gap ≥ lp_gap), take it.
    4. Else keep first (vocab-aware tends to spell project names right).
    """
    n1 = len(text1.split())
    n2 = len(text2.split())
    if n1 == 0 and n2 > 0:
        return text2, lp2, "second"
    if n2 == 0 and n1 > 0:
        return text1, lp1, "first"
    if n2 >= n1 * min_len_ratio and n2 > n1:
        return text2, lp2, "second"
    if lp2 - lp1 >= lp_gap:
        return text2, lp2, "second"
    return text1, lp1, "first"


def transcribe_loop(state, args, vocab_state):
    slog(f"[stt] loading {args.model} (int8 cpu)…")
    model = WhisperModel(args.model, device="cpu", compute_type="int8")
    slog("[vad] loading silero…")
    vad_model = load_silero_vad()
    vad = VADIterator(
        vad_model,
        threshold=0.5,
        sampling_rate=SAMPLE_RATE,
        min_silence_duration_ms=SILENCE_FLUSH_MS,
    )
    slog(_c(_C_BRI_GREEN, "[live]"))

    accum = []
    in_speech = False

    while not state.quit.is_set():
        try:
            chunk = state.audio_q.get(timeout=0.5)
        except queue.Empty:
            continue

        if in_speech:
            accum.append(chunk)

        chunk_t = torch.from_numpy(chunk)
        speech_event = vad(chunk_t, return_seconds=False)

        if speech_event is None:
            continue

        if "start" in speech_event:
            in_speech = True
            accum = [chunk]
            if not state.muted.is_set() and not state.tts_playing.is_set():
                state.hearing.set()
        elif "end" in speech_event and in_speech:
            in_speech = False
            state.hearing.clear()
            if state.muted.is_set() or state.tts_playing.is_set():
                accum = []
                continue
            audio = np.concatenate(accum) if accum else np.zeros(0, dtype=np.float32)
            accum = []
            if audio.size < SAMPLE_RATE * 0.2:
                continue  # < 200ms; almost certainly noise burst
            state.transcribing.set()
            t0 = time.time()
            try:
                # Pass 1: vocab-biased. Then if confidence is low, run a
                # second pass WITHOUT vocab and pick the better result.
                # Vocab-biasing collapses noisy audio onto high-prior terms
                # (e.g. "show me what files in lucida" → "lucida"); the
                # logprob gate catches that and the no-vocab pass is the
                # check.
                text, logprob = _run_whisper(model, audio, vocab_state.get())
                if logprob < args.logprob_gate:
                    text2, logprob2 = _run_whisper(model, audio, None)
                    text, logprob, picked = _pick_better(text, logprob, text2, logprob2)
                    if picked == "second":
                        slog(f"  ↳ stt rerolled no-vocab: lp={logprob2:.2f} (was {logprob:.2f}), text={text2!r}")
            finally:
                state.transcribing.clear()
            latency_ms = int((time.time() - t0) * 1000)
            if not text:
                continue
            slog(f"{_C_DIM}[stt] ({latency_ms}ms lp={logprob:.2f}){_C_RESET} {_c(_C_BRI_CYAN, text)}")
            write_transcript(args.transcripts, text)
            # Hotphrase gates expander mode. Default is verbatim (no LLM
            # interpretation; route + raw inject). Hotphrase prefix opts into
            # the full router+expander pipeline.
            stripped = strip_hotphrase(text, args.hotphrase)
            if stripped is not None:
                mode = "expand"
                send_text = stripped
                slog(f"  ↳ hotphrase '{args.hotphrase}' → expand mode: {send_text!r}")
            else:
                mode = "verbatim"
                send_text = text
            # Narrate mode detection runs after hotphrase strip, on the
            # inject text. "tell me/say/speak" force narration; "show
            # me/list/what/where/..." suppress it.
            send_text, narrate = detect_narrate_mode(send_text)
            if narrate != "auto":
                slog(f"  ↳ narrate={narrate} → {send_text!r}")
            # Fire stock ack immediately (instant audible feedback while
            # mayor's pipeline runs in parallel).
            state.event_q.put({"type": "ack"})
            payload = (json.dumps({"type": "utterance", "text": send_text, "mode": mode, "narrate": narrate, "ts": time.time()}) + "\n").encode("utf-8")
            state.outbox_q.put(payload)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--audio-sock", default=DEFAULT_SOCK,
                   help=f"unix socket to write utterances to (default {DEFAULT_SOCK})")
    p.add_argument("--model", default=DEFAULT_MODEL,
                   help=f"faster-whisper model name (default {DEFAULT_MODEL})")
    p.add_argument("--mute-key", default=" ", help="key to toggle mute (default spacebar)")
    p.add_argument("--quit-key", default="q", help="key to quit (default 'q')")
    p.add_argument("--transcripts", type=Path, default=DEFAULT_LOG_DIR,
                   help=f"directory to append daily transcript logs (default {DEFAULT_LOG_DIR})")
    p.add_argument("--mode", choices=["open", "ptt"], default="open",
                   help="open=always-on (default); ptt=push-to-talk (V0.2.1, not yet implemented)")
    p.add_argument("--watcher-sock", default="/tmp/saturday-watcher.sock",
                   help="watcher Unix socket — sidecar pulls active project names + extracts weird-word vocab from session state to bias STT (e.g. 'adit' vs 'add it'). Refreshed every 60s.")
    p.add_argument("--vocab-pin", default="",
                   help="comma-separated terms to ALWAYS include in STT vocab bias on top of the dynamic watcher pool — for jargon that may not appear in any active session yet (e.g. 'stope,adit,lucida'). Pinned terms take priority.")
    p.add_argument("--vocab-refresh-sec", type=int, default=60,
                   help="how often to re-pull vocab from watcher (default 60s)")
    p.add_argument("--tts-voice", default=DEFAULT_TTS_VOICE,
                   help=f"Kokoro voice ID. Single: 'bm_george' (default), 'af_sarah', 'am_adam', etc. Blend: 'am_adam+af_sarah' averages two voice embeddings (negligible perf cost — just an embedding average pre-synthesis). For HF rate limits / faster downloads, set HF_TOKEN env var; honored automatically.")
    p.add_argument("--hotphrase", default=DEFAULT_HOTPHRASE,
                   help=f"NOT a wake-word — the mic is always live. This is a per-utterance pipeline-mode switch. Default: '{DEFAULT_HOTPHRASE}'. Without the prefix, utterances pass through to mayor in VERBATIM mode (router picks session, raw STT text becomes the inject — no expander LLM call, no narration). WITH the prefix, the prefix is stripped and the rest goes through the full router+expander pipeline (LLM rewrites + spoken narration). Set empty to send everything in expand mode (no verbatim path).")
    p.add_argument("--logprob-gate", type=float, default=-0.6,
                   help="if first-pass STT avg_logprob is below this, run a second pass without vocab bias and pick the better result. Vocab-biased Whisper collapses noisy audio onto high-prior terms (e.g. 'show me what files in lucida' → 'lucida'); the gate catches that. -0.6 ≈ 'low confidence'; lower = stricter (fewer rerolls); 0 = always reroll; -10 effectively disables.")
    args = p.parse_args()

    if args.mode == "ptt":
        print("ptt mode not yet implemented (V0.2.1).", file=sys.stderr)
        sys.exit(1)

    def show_key(k):
        return "<space>" if k == " " else f"'{k}'"

    print(f"saturday-audio — open-mic, sock={args.audio_sock}", file=sys.stderr)
    print(f"keys: {show_key(args.mute_key)}=toggle mute, {show_key(args.quit_key)}=quit", file=sys.stderr)

    state = State()

    # Status renderer FIRST — shows "warming up" while the heavier subsystems
    # (kokoro download/init, whisper model load) come up over the next ~5-30s.
    # Operator gets a clear "do not talk yet" signal until state.ready flips.
    threading.Thread(
        target=status_renderer,
        args=(state,),
        daemon=True,
    ).start()

    threading.Thread(
        target=keyboard_reader,
        args=(state, args.mute_key, args.quit_key),
        daemon=True,
    ).start()

    # Conn manager owns the socket — drains outbox_q, fills event_q, reconnects.
    threading.Thread(
        target=conn_manager,
        args=(state, args.audio_sock),
        daemon=True,
    ).start()

    # TTS player drains event_q on {type:'speak'} → kokoro synth → speakers.
    threading.Thread(
        target=tts_player,
        args=(state, args),
        daemon=True,
    ).start()

    pinned = [t.strip() for t in args.vocab_pin.split(",") if t.strip()]
    vocab_state = VocabState(args.watcher_sock, pinned)
    vocab_state.refresh()  # synchronous first refresh so STT starts with vocab in hand
    threading.Thread(
        target=vocab_refresher,
        args=(vocab_state, state.quit, args.vocab_refresh_sec),
        daemon=True,
    ).start()

    threading.Thread(
        target=transcribe_loop,
        args=(state, args, vocab_state),
        daemon=True,
    ).start()

    threading.Thread(
        target=rms_ticker,
        args=(state,),
        daemon=True,
    ).start()

    try:
        with sd.InputStream(
            samplerate=SAMPLE_RATE,
            channels=1,
            dtype="float32",
            blocksize=CHUNK_SAMPLES,
            callback=make_mic_callback(state),
        ):
            while not state.quit.is_set():
                time.sleep(0.1)
    except KeyboardInterrupt:
        state.quit.set()

    print("\nbye.", file=sys.stderr)


if __name__ == "__main__":
    main()
