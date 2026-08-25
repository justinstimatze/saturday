package main

import (
	"encoding/binary"
	"math"
)

// Client-facing audio wire format: every binary WebSocket message, both
// directions, is one frame of mono PCM at moshiclient.SampleRate,
// little-endian 32-bit floats — the same layout a browser's
// Float32Array.buffer gives for free, so the client JS needs no encoding
// library of its own.

// bytesToFloat32 decodes a little-endian float32 PCM frame from raw bytes
// as received from the client. Silently drops a trailing partial sample
// (a malformed frame) rather than erroring — audio frames are best-effort
// by nature.
func bytesToFloat32(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := binary.LittleEndian.Uint32(b[i*4 : i*4+4])
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// float64PCMToBytes encodes a PCM frame (as received from moshi-server's
// TTS, decoded as float64 — see TTSAudioMessage's doc comment) into
// little-endian float32 bytes for the client.
func float64PCMToBytes(pcm []float64) []byte {
	out := make([]byte, len(pcm)*4)
	for i, v := range pcm {
		bits := math.Float32bits(float32(v))
		binary.LittleEndian.PutUint32(out[i*4:i*4+4], bits)
	}
	return out
}
