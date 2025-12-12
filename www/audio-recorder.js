class AudioRecorder {
    constructor(config = {}) {
        this.targetSampleRate = config.sampleRate || 24000;
        this.onAudioData = config.onAudioData || (() => { });
        this.audioContext = null;
        this.mediaStream = null;
        this.workletNode = null;
        this.gainNode = null; // Dummy node to keep graph active
        this.isRecording = false;

        this.handleWorkletMessage = this.handleWorkletMessage.bind(this);
    }

    async init() {
        if (!this.audioContext) {
            // Ensure we try to get a capable context
            this.audioContext = new (window.AudioContext || window.webkitAudioContext)({
                sampleRate: this.targetSampleRate,
                latencyHint: 'interactive'
            });

            try {
                await this.audioContext.audioWorklet.addModule('audio-processor.js');
                console.log('[AudioRecorder] Audio worklet module loaded');
            } catch (e) {
                console.error('[AudioRecorder] Failed to load audio worklet:', e);
                throw e;
            }
        }
        return this.audioContext;
    }

    async start() {
        if (this.isRecording) return;

        try {
            await this.init();

            if (this.audioContext.state === 'suspended') {
                await this.audioContext.resume();
            }

            this.mediaStream = await navigator.mediaDevices.getUserMedia({
                audio: {
                    channelCount: 1,
                    echoCancellation: true,
                    noiseSuppression: true,
                    autoGainControl: true,
                    sampleRate: this.targetSampleRate
                }
            });

            const source = this.audioContext.createMediaStreamSource(this.mediaStream);

            // Pass targetSampleRate and inputSampleRate to Worklet
            this.workletNode = new AudioWorkletNode(this.audioContext, 'audio-processor', {
                processorOptions: {
                    targetSampleRate: this.targetSampleRate,
                    inputSampleRate: this.audioContext.sampleRate
                }
            });

            this.workletNode.port.onmessage = this.handleWorkletMessage;

            // Connect graph to keep it alive, but mute the output
            // source -> worklet -> gain(0) -> destination
            this.gainNode = this.audioContext.createGain();
            this.gainNode.gain.value = 0;

            source.connect(this.workletNode);
            this.workletNode.connect(this.gainNode);
            this.gainNode.connect(this.audioContext.destination);

            this.isRecording = true;
            console.log(`[AudioRecorder] Recording started. Input: ${this.audioContext.sampleRate}Hz, Target: ${this.targetSampleRate}Hz`);

        } catch (error) {
            console.error('[AudioRecorder] Failed to start recording:', error);
            this.stop();
            throw error;
        }
    }

    stop() {
        if (!this.isRecording) return;

        this.isRecording = false;
        console.log('[AudioRecorder] Stopping recording...');

        if (this.mediaStream) {
            this.mediaStream.getTracks().forEach(track => track.stop());
            this.mediaStream = null;
        }

        if (this.workletNode) {
            this.workletNode.disconnect();
            this.workletNode.port.onmessage = null;
            this.workletNode = null;
        }

        if (this.gainNode) {
            this.gainNode.disconnect();
            this.gainNode = null;
        }
    }

    async cleanup() {
        this.stop();
        if (this.audioContext) {
            await this.audioContext.close();
            this.audioContext = null;
        }
    }

    handleWorkletMessage(event) {
        if (!this.isRecording) return;

        // Received Int16Array from Worklet
        const int16Data = event.data;

        // Fast Base64 Encode
        const base64 = this.arrayBufferToBase64(int16Data.buffer);

        this.onAudioData({
            base64: base64,
            raw: int16Data // Int16Array
        });
    }

    arrayBufferToBase64(buffer) {
        let binary = '';
        const bytes = new Uint8Array(buffer);
        const len = bytes.byteLength;
        const chunkSize = 8192;

        // Process in chunks to avoid stack overflow with String.fromCharCode.apply
        for (let i = 0; i < len; i += chunkSize) {
            const chunk = bytes.subarray(i, Math.min(i + chunkSize, len));
            binary += String.fromCharCode.apply(null, chunk);
        }

        return window.btoa(binary);
    }
}

window.AudioRecorder = AudioRecorder;
