/**
 * AudioWorkletProcessor for handling real-time audio capture and processing.
 * Performs downsampling and PCM16 conversion on the audio thread.
 */
class AudioProcessor extends AudioWorkletProcessor {
    constructor(options) {
        super();
        this.bufferSize = 2048;
        this.pcmBuffer = new Int16Array(this.bufferSize);
        this.bufferIndex = 0;
        this._remainder = 0;

        const opts = options?.processorOptions || {};
        this.targetRate = opts.targetSampleRate || 24000;
        // Allow passing input rate explicitly, fallback to global sampleRate
        // Note: sampleRate is a global in AudioWorkletGlobalScope
        this.overrideInputRate = opts.inputSampleRate;
    }

    process(inputs) {
        // Robust input check
        if (!inputs || inputs.length === 0 || !inputs[0] || inputs[0].length === 0) {
            return true;
        }

        const channel0 = inputs[0][0];
        if (!channel0 || channel0.length === 0) return true;

        // Determine input rate safely
        // Use explicitly passed rate if available (static), otherwise dynamic global
        const inputRate = this.overrideInputRate || sampleRate;

        if (!inputRate) return true; // Safety abort if rate is unknown

        if (inputRate === this.targetRate) {
            // Direct copy
            for (let i = 0; i < channel0.length; i++) {
                this.pushSample(channel0[i]);
            }
        } else {
            // Resampling
            const ratio = inputRate / this.targetRate;

            // Loop with protection against infinite loops if ratio is invalid
            if (!ratio || ratio <= 0) return true;

            // Nearest-neighbor resampling
            while (this._remainder < channel0.length) {
                const sampleIndex = Math.min(Math.floor(this._remainder), channel0.length - 1);
                this.pushSample(channel0[sampleIndex]);
                this._remainder += ratio;
            }
            this._remainder -= channel0.length;
        }

        return true;
    }

    pushSample(floatSample) {
        // Clamp and convert
        const s = Math.max(-1, Math.min(1, floatSample));
        this.pcmBuffer[this.bufferIndex++] = s < 0 ? s * 0x8000 : s * 0x7fff;

        if (this.bufferIndex >= this.bufferSize) {
            this.port.postMessage(this.pcmBuffer.slice(0, this.bufferSize));
            this.bufferIndex = 0;
        }
    }
}

registerProcessor('audio-processor', AudioProcessor);
