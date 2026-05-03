// RNNoise AudioWorklet processor for Vocala.
// Loads @jitsi/rnnoise-wasm (sync version) into the AudioWorkletGlobalScope
// and applies frame-by-frame noise suppression on a mono input.
// Port of jitsi-meet's NoiseSuppressorWorklet.ts to plain JS.

importScripts('/static/vendor/rnnoise-sync.js');

const RNNOISE_SAMPLE_LENGTH = 480;        // RNNoise frame size (samples)
const RNNOISE_BUFFER_BYTES = 480 * 4;     // Float32 buffer in wasm heap
const PROC_BLOCK_SIZE = 128;              // AudioWorklet quantum
const CIRCULAR_BUFFER_LEN = 1920;         // LCM(128, 480)
const SHIFT_16_BIT = 32768;

class RnnoiseProcessor {
    constructor(wasmModule) {
        this._wasm = wasmModule;
        this._inputPtr = this._wasm._malloc(RNNOISE_BUFFER_BYTES);
        if (!this._inputPtr) {
            throw new Error('rnnoise: failed to allocate wasm buffer');
        }
        this._inputF32Index = this._inputPtr >> 2;
        this._context = this._wasm._rnnoise_create();
    }

    // Process 480 Float32 samples in place. Returns VAD score [0..1].
    processFrame(pcmFrame) {
        const heap = this._wasm.HEAPF32;
        const idx = this._inputF32Index;
        for (let i = 0; i < RNNOISE_SAMPLE_LENGTH; i++) {
            heap[idx + i] = pcmFrame[i] * SHIFT_16_BIT;
        }
        const vad = this._wasm._rnnoise_process_frame(this._context, this._inputPtr, this._inputPtr);
        for (let i = 0; i < RNNOISE_SAMPLE_LENGTH; i++) {
            pcmFrame[i] = heap[idx + i] / SHIFT_16_BIT;
        }
        return vad;
    }
}

class NoiseSuppressorWorklet extends AudioWorkletProcessor {
    constructor() {
        super();
        // createRNNWasmModuleSync is exposed by rnnoise-sync.js as a global.
        this._proc = new RnnoiseProcessor(createRNNWasmModuleSync());
        this._buffer = new Float32Array(CIRCULAR_BUFFER_LEN);
        this._inputLen = 0;       // total samples written to buffer
        this._denoisedLen = 0;    // total samples denoised
        this._outIdx = 0;         // next read position for output
    }

    process(inputs, outputs) {
        const inData = inputs[0] && inputs[0][0];
        const outData = outputs[0] && outputs[0][0];
        if (!inData || !outData) {
            return true;
        }

        // Append incoming 128-sample chunk to circular buffer.
        this._buffer.set(inData, this._inputLen);
        this._inputLen += inData.length;

        // Denoise as many 480-sample frames as we can in place.
        while (this._denoisedLen + RNNOISE_SAMPLE_LENGTH <= this._inputLen) {
            const frame = this._buffer.subarray(this._denoisedLen, this._denoisedLen + RNNOISE_SAMPLE_LENGTH);
            this._proc.processFrame(frame);
            this._denoisedLen += RNNOISE_SAMPLE_LENGTH;
        }

        // How many denoised samples are unread?
        let unsent;
        if (this._outIdx > this._denoisedLen) {
            // rollover already happened on read side
            unsent = CIRCULAR_BUFFER_LEN - this._outIdx;
        } else {
            unsent = this._denoisedLen - this._outIdx;
        }

        if (unsent >= outData.length) {
            const slice = this._buffer.subarray(this._outIdx, this._outIdx + outData.length);
            outData.set(slice);
            this._outIdx += outData.length;
        }
        // While buffer warms up (first ~3 quanta) outData stays zero-filled.

        if (this._outIdx === CIRCULAR_BUFFER_LEN) {
            this._outIdx = 0;
        }
        if (this._inputLen === CIRCULAR_BUFFER_LEN) {
            this._inputLen = 0;
            this._denoisedLen = 0;
        }

        return true;
    }
}

registerProcessor('NoiseSuppressorWorklet', NoiseSuppressorWorklet);
