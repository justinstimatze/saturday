// AudioWorkletProcessor for mic capture. Runs on the dedicated audio
// rendering thread, unlike the ScriptProcessorNode it replaces (2026-08-25:
// ScriptProcessorNode executes its callback on the main JS thread, a
// documented source of audio glitches under main-thread contention — a
// captured server-side WAV of the TTS output showed a clean onset with no
// click or discontinuity, ruling out moshi-server and this backend's own
// forwarding, which left client-side playback/capture as the remaining
// place a distortion report could originate).
//
// Re-chunks whatever block size the audio thread delivers (not
// guaranteed to match frameSize) into exact frameSize frames and posts
// each to the main thread as a transferred ArrayBuffer — same
// re-chunking contract index.html's ScriptProcessorNode version used.
class MicCaptureProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const opts = (options && options.processorOptions) || {};
    this.frameSize = opts.frameSize || 1920;
    this.buf = new Float32Array(0);
  }

  process(inputs) {
    const channel = inputs[0] && inputs[0][0];
    if (!channel || channel.length === 0) return true;
    const combined = new Float32Array(this.buf.length + channel.length);
    combined.set(this.buf);
    combined.set(channel, this.buf.length);
    let offset = 0;
    while (combined.length - offset >= this.frameSize) {
      const frame = combined.slice(offset, offset + this.frameSize);
      this.port.postMessage(frame.buffer, [frame.buffer]);
      offset += this.frameSize;
    }
    this.buf = combined.slice(offset);
    return true;
  }
}

registerProcessor("mic-capture-processor", MicCaptureProcessor);
