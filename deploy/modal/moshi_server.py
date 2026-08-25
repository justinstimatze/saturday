"""
Modal deployment of the real Rust `moshi-server` binary (STT + TTS only --
no vLLM, no Unmute) for saturday-voice's Phase 1b live test.

This runs moshi-server@0.6.4 unmodified. moshiclient's existing msgpack
WebSocket client code (moshiclient/stt.go, tts.go) talks to it directly --
same protocol it already speaks against the Runpod-hosted binary. See
~/.claude/plans/wobbly-honking-valley.md, "Phase 1b hosting pivot", for
why: Modal's own official examples (quillman, the Kyutai STT example)
both reimplement Moshi in Python with a different, incompatible wire
protocol -- ruled out as templates for exactly that reason.

One-time setup (see deploy/modal/README.md for the full walkthrough):
    modal secret create saturday-voice-moshi-auth MOSHI_AUTH_TOKEN=<token>

Deploy:    modal deploy deploy/modal/moshi_server.py
Logs:      modal app logs saturday-voice-moshi
Tear down: modal app stop saturday-voice-moshi
"""

import subprocess

import modal

app = modal.App("saturday-voice-moshi")

MOSHI_SERVER_VERSION = "0.6.4"
# The TTS half of moshi-server embeds a Python component via PyO3.
# Package list matches Unmute's own dockerless/start_tts.sh pinned
# pyproject.toml (fetched from kyutai-labs/moshi commit 9837ca3, rust/
# moshi-server/pyproject.toml) -- but installed straight into the same
# system Python moshi-server links against, not a separate venv. That
# pyproject.toml pins requires-python==3.12.8 for Kyutai's own build
# reproducibility, not because moshi==0.2.8 itself needs exactly that
# version -- and it can't be honored here anyway: PyO3 embeds Python by
# linking a specific libpythonX.Y.so at *compile* time (3.10 here, see
# LIBRARY_PATH below), so a `uv run --project <3.12-venv>` wrapper at
# *runtime* never reaches the embedded interpreter at all -- it's not a
# subprocess PATH lookup, it's a fixed library link. First deploy tried
# that wrapper and crash-looped: `ModuleNotFoundError: No module named
# 'huggingface_hub'`, because the 3.12 venv it built had the deps and the
# 3.10-linked interpreter that's actually running never saw it.
TTS_PY_PACKAGES = [
    "moshi==0.2.8", "setuptools", "xformers", "pydantic", "julius",
    "torchaudio", "huggingface_hub",
]

STT_PORT = 8090
TTS_PORT = 8089

MOSHI_CONFIG_DIR = "/opt/moshi-configs"
RENDERED_CONFIG_DIR = "/tmp/moshi-configs-rendered"

HF_CACHE_PATH = "/root/.cache/huggingface"
hf_cache_vol = modal.Volume.from_name(
    "saturday-voice-moshi-hf-cache", create_if_missing=True
)

# Full CUDA toolkit (not just runtime libs) -- `cargo install --features
# cuda` needs nvcc/headers to compile against.
CUDA_TAG = "12.4.1-devel-ubuntu22.04"

image = (
    # add_python="3.11" was dropped after a third deploy attempt: whatever
    # minimal interpreter it provisions didn't give the linker a findable
    # libpython3.11.so (see the LIBRARY_PATH note below). Dropping it
    # entirely then broke a DIFFERENT thing, found on the fifth attempt:
    # Modal's own control plane needs a registered Python version inside
    # the image regardless of what the Rust binary itself links against
    # ("unable to determine the version of Python installed in the
    # Image"). Fix: add_python="3.10", matching the base image's actual
    # apt-default (not a mismatched 3.11) -- this registers Modal's own
    # Python without reintroducing the earlier linker mismatch, since the
    # Rust build links against whatever apt-installed python3-dev
    # provides (3.10 on this Ubuntu 22.04 base), not against add_python's
    # own interpreter.
    modal.Image.from_registry(f"nvidia/cuda:{CUDA_TAG}", add_python="3.10")
    .entrypoint([])  # drop the base image's verbose banner on every exec
    .apt_install(
        # pkg-config/libssl-dev/build-essential: same base build deps
        # Phase 0 needed on Runpod. cmake/libopus-dev: new, found live on
        # the second real deploy attempt -- audiopus_sys (a moshi-server
        # dependency) builds Opus from source via CMake when pkg-config
        # can't find a system libopus; installing libopus-dev directly
        # lets pkg-config find it and skip that build entirely, cmake
        # stays as the fallback in case it doesn't. python3.11-dev: see
        # the add_python note above.
        "pkg-config", "libssl-dev", "build-essential", "curl", "git",
        "cmake", "libopus-dev", "python3-dev", "python3-venv",
    )
    .run_commands(
        "curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y",
        "curl -LsSf https://astral.sh/uv/install.sh | sh",
    )
    .env({"PATH": "/root/.cargo/bin:/root/.local/bin:$PATH"})
    .run_commands(
        # Same two fixes Phase 0 found the hard way on Runpod (see the
        # plan file): moshi-server embeds Python via PyO3, so it needs
        # LD_LIBRARY_PATH pointed at Python's own libdir at *build* time
        # too, not just at run time -- skip this and the build fails with
        # a misleading numpy/python-linking error, not an LD_LIBRARY_PATH
        # error. CXXFLAGS is a GCC 15 Sentencepiece build fix.
        #
        # CUDA_COMPUTE_CAP is new, found live on the first real deploy
        # attempt, not anticipated in the plan: candle-kernels' build.rs
        # (via bindgen_cuda) shells out to `nvidia-smi` to detect the
        # compute capability, but Modal's image-build step runs on a
        # CPU-only builder with no GPU attached -- `nvidia-smi` isn't
        # even on PATH there. bindgen_cuda's own source
        # (github.com/Narsil/bindgen_cuda, compute_cap()) checks
        # CUDA_COMPUTE_CAP first, parsed as a bare usize -- "86", not
        # "8.6" (the nvidia-smi fallback path literally strips the dot
        # before parsing). 86 = A10G's real compute capability, confirmed
        # against NVIDIA's own developer.nvidia.com/cuda-gpus list, not
        # guessed. If the GPU class below changes, this must change too.
        #
        # LIBRARY_PATH is new, found live on the third real deploy
        # attempt: the build got all the way through compiling and only
        # failed at the final link step, "unable to find library
        # -lpython3.11". LD_LIBRARY_PATH controls the *runtime* dynamic
        # loader's search path; the *linker* (invoked here via `cc`)
        # reads the separate LIBRARY_PATH variable for its own -L search
        # dirs. Same directory, different variable, both needed.
        #
        # Plain `python3`, not a pinned 3.11 -- found live on the fourth
        # attempt: querying python3.11 explicitly while pyo3's own build
        # script independently detects the *system default* python3
        # (3.10 on this Ubuntu 22.04 base) produced a mismatched
        # LIBRARY_PATH (pointed at 3.11's dir while the linker asked for
        # -lpython3.10). Matching Unmute's own start_tts.sh, which also
        # just uses bare python3 -- one interpreter, no ambiguity.
        #
        # /usr/bin/python3.10 explicitly, not bare `python3` -- found live
        # on the seventh attempt: TTS crash-looped with `ModuleNotFoundError:
        # No module named '_contextvars'` importing numpy, from an
        # interpreter reporting itself as "Python3.10 from
        # /usr/local/bin/python3" -- that's add_python="3.10"'s own
        # interpreter (installed under /usr/local/bin, which wins over
        # apt's /usr/bin on PATH by Debian convention), not the complete
        # apt python3.10-dev this build actually needs -- and it's
        # missing a stdlib C-extension module, which a normal CPython
        # build never is. Pin the full apt path everywhere (link-time
        # LIBRARY_PATH here, and the pip-install target below) so every
        # step targets the one definitely-complete interpreter, with zero
        # dependence on which one PATH happens to resolve first.
        "export PYLIBDIR=$(/usr/bin/python3.10 -c 'import sysconfig; print(sysconfig.get_config_var(\"LIBDIR\"))') && "
        "export LD_LIBRARY_PATH=$PYLIBDIR && "
        "export LIBRARY_PATH=$PYLIBDIR && "
        "export CXXFLAGS='-include cstdint' && "
        "export CUDA_COMPUTE_CAP=86 && "
        f"cargo install --features cuda moshi-server@{MOSHI_SERVER_VERSION}",
    )
    # TTS's own Python deps, installed into the same system Python
    # moshi-server is linked against (see the TTS_PY_PACKAGES note above
    # for why this replaced an earlier uv-venv-per-project attempt) --
    # baked into the image at build time, not re-downloaded per cold
    # start.
    .run_commands(
        "uv pip install --python /usr/bin/python3.10 --break-system-packages "
        + " ".join(TTS_PY_PACKAGES),
    )
    # The two moshi-server configs, pulled verbatim from Unmute's own
    # services/moshi-server/configs/{stt,tts}.toml (see deploy/modal/
    # configs/*.toml for the copy and its provenance note) -- real HF
    # weight repos, not placeholders. authorized_ids is a template
    # placeholder, rendered from the MOSHI_AUTH_TOKEN secret at container
    # start (see _render_configs below), not committed as a real token.
    .add_local_dir("deploy/modal/configs", MOSHI_CONFIG_DIR, copy=True)
)


@app.cls(
    image=image,
    gpu="A10G",
    volumes={HF_CACHE_PATH: hf_cache_vol},
    scaledown_window=300,
    timeout=600,
    secrets=[
        modal.Secret.from_name("saturday-voice-moshi-auth"),
        # Reused from an existing Modal secret (crystal/shroud already use
        # it), not created fresh -- HF_TOKEN, huggingface_hub's own
        # default env var. TTS's voice-snapshot download hit HF's
        # anonymous rate limit without it (429 Too Many Requests).
        modal.Secret.from_name("huggingface"),
    ],
)
class MoshiServer:
    def _render_config(self, name: str) -> str:
        import os

        os.makedirs(RENDERED_CONFIG_DIR, exist_ok=True)
        src = f"{MOSHI_CONFIG_DIR}/{name}"
        dst = f"{RENDERED_CONFIG_DIR}/{name}"
        token = os.environ["MOSHI_AUTH_TOKEN"]
        with open(src) as f:
            text = f.read()
        with open(dst, "w") as f:
            f.write(text.replace("__AUTH_TOKEN__", token))
        return dst

    def _env(self):
        import os

        # /usr/bin/python3.10 explicitly -- must match the interpreter
        # moshi-server was actually linked against at build time (see the
        # image build's own note on why bare `python3`/add_python's
        # interpreter is the wrong one).
        #
        # PYTHONHOME/PYTHONPATH are new, found live on the eighth deploy
        # attempt: even with the linked library and pip-install target
        # both correctly pinned to /usr/bin/python3.10, TTS still
        # crash-looped on `No module named 'huggingface_hub'` -- confirmed
        # actually installed there (deploy-8's own log: "+
        # huggingface-hub==0.33.5"). PyO3's embedded interpreter doesn't
        # inherit LD_LIBRARY_PATH as an import-path hint -- CPython's own
        # startup does a landmark-file search relative to sys.executable,
        # which here is the `moshi-server` Rust binary's own path, not
        # any real Python install, so it silently falls back to whatever
        # prefix the library was compiled with rather than finding the
        # site-packages pip actually wrote to. Setting PYTHONHOME/
        # PYTHONPATH explicitly bypasses that search and pins the
        # embedded interpreter to exactly what `/usr/bin/python3.10`
        # itself reports -- the standard CPython override mechanism,
        # honored during embedding the same as a normal launch.
        info = subprocess.run(
            [
                "/usr/bin/python3.10",
                "-c",
                "import sys, sysconfig; "
                "print(sysconfig.get_config_var('LIBDIR')); "
                "print(sysconfig.get_config_var('prefix')); "
                "print(':'.join(sys.path))",
            ],
            capture_output=True,
            text=True,
            check=True,
        ).stdout.strip().split("\n")
        libdir, prefix, syspath = info[0], info[1], info[2]
        return {
            **os.environ,
            "LD_LIBRARY_PATH": libdir,
            "PYTHONHOME": prefix,
            "PYTHONPATH": syspath,
        }

    @modal.web_server(STT_PORT, startup_timeout=180)
    def stt(self):
        config = self._render_config("stt.toml")
        subprocess.Popen(
            ["moshi-server", "worker", "--config", config, "--port", str(STT_PORT)],
            env=self._env(),
        )

    @modal.web_server(TTS_PORT, startup_timeout=180)
    def tts(self):
        config = self._render_config("tts.toml")
        subprocess.Popen(
            ["moshi-server", "worker", "--config", config, "--port", str(TTS_PORT)],
            env=self._env(),
        )
