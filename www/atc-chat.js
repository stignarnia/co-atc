// ATC Chat Frontend Component
class ATCChat {
  constructor() {
    this.sessionId = null;
    this.isRecording = false;
    this.isConnected = false;
    this.websocket = null;
    this.stream = null;
    this.audioQueue = [];

    // Audio Recorder instance
    this.audioRecorder = null;
    this.isPlaying = false;
    this.pushToTalkActive = false;
    this.currentAIResponse = null; // For accumulating AI response text
    this.aiVisualizationFrameId = null;
    this.audioAnalyser = null;
    this.audioDataArray = null;

    // Transcript functionality
    this.transcripts = [];
    this.filteredTranscripts = [];
    this.transcriptViewerVisible = false;
    this.transcriptSearchTerm = "";
    this.transcriptIdCounter = 0;

    // Initialize filtered transcripts
    this.filterTranscripts();

    this.init();
  }

  // Visual status indicator methods
  showStatusIndicator(state, text, showAnimation = false) {
    const statusElement = document.getElementById("ai-advisory-status");
    const activityIndicator = document.getElementById("ai-activity-indicator");
    const visBar = document.getElementById("ai-vis-bar");

    if (!statusElement) return;

    // Update status text
    statusElement.textContent = text;

    // Update activity indicator
    if (activityIndicator) {
      switch (state) {
        case "transmitting":
        case "push-to-talk":
          activityIndicator.textContent = "TX";
          activityIndicator.className =
            "absolute top-0 right-1 text-xs text-red-400 p-0.5 font-bold animate-pulse";
          break;
        case "processing":
          activityIndicator.textContent = "AI";
          activityIndicator.className =
            "absolute top-0 right-1 text-xs text-yellow-400 p-0.5 font-bold animate-pulse";
          break;
        case "playing":
          activityIndicator.textContent = "RX";
          activityIndicator.className =
            "absolute top-0 right-1 text-xs text-green-400 p-0.5 font-bold animate-pulse";
          break;
        case "connected":
          activityIndicator.textContent = "RDY";
          activityIndicator.className =
            "absolute top-0 right-1 text-xs text-purple-400 p-0.5 font-bold";
          break;
        case "disconnecting":
          activityIndicator.textContent = "END";
          activityIndicator.className =
            "absolute top-0 right-1 text-xs text-orange-400 p-0.5 font-bold animate-pulse";
          break;
        default:
          activityIndicator.textContent = "--";
          activityIndicator.className =
            "absolute top-0 right-1 text-xs text-neutral-400 p-0.5";
      }
      activityIndicator.style.fontSize = "0.6rem";
      activityIndicator.style.lineHeight = "1";
      activityIndicator.style.pointerEvents = "none";
    }
  }

  hideStatusIndicator() {
    const statusElement = document.getElementById("ai-advisory-status");
    const activityIndicator = document.getElementById("ai-activity-indicator");

    if (statusElement) {
      statusElement.textContent = "Disconnected";
    }
    if (activityIndicator) {
      activityIndicator.textContent = "--";
      activityIndicator.className =
        "absolute top-0 right-1 text-xs text-neutral-400 p-0.5";
      activityIndicator.style.fontSize = "0.6rem";
      activityIndicator.style.lineHeight = "1";
      activityIndicator.style.pointerEvents = "none";
    }
  }

  // PTT Visual Feedback Methods
  addPTTVisualFeedback() {
    const aiContainer = document.getElementById("ai-advisory-container");
    if (aiContainer) {
      aiContainer.classList.add("ptt-active");
    }
  }

  removePTTVisualFeedback() {
    const aiContainer = document.getElementById("ai-advisory-container");
    if (aiContainer) {
      aiContainer.classList.remove("ptt-active");
    }
  }

  async init() {
    console.log("[ATC-Chat] Initializing ATC Chat...");

    // Check if ATC Chat is enabled
    try {
      const response = await fetch(
        `${window.location.protocol}//${window.location.hostname}:8000/api/v1/config`,
      );
      if (!response.ok) {
        console.log(
          "[ATC-Chat] Config not available, status:",
          response.status,
        );
        // Still try to create the button even if config fails
        this.createChatButton();
        this.setupKeyboardListeners();
        return;
      }

      const config = await response.json();
      console.log("[ATC-Chat] Config loaded:", config);

      if (!config.atc_chat?.enabled) {
        console.log("[ATC-Chat] ATC Chat is disabled in configuration");
        // Hide the button if disabled
        const chatBtn = document.getElementById("atc-chat-btn");
        if (chatBtn) {
          chatBtn.style.display = "none";
        }
        return;
      }

      console.log("[ATC-Chat] ATC Chat is enabled, setting up...");
      this.createChatButton();
      this.setupKeyboardListeners();
    } catch (error) {
      console.error("[ATC-Chat] ATC Chat initialization error:", error);
      // Still try to create the button even if there's an error
      this.createChatButton();
      this.setupKeyboardListeners();
    }
  }

  createChatButton() {
    console.log("[ATC-Chat] Setting up AI Advisory interface...");

    // Make the ATC Chat instance globally available for the UI
    window.atcChat = this;

    // Add CSS styles for dynamic states
    this.addStyles();

    console.log("[ATC-Chat] AI Advisory interface setup complete");
  }

  setupKeyboardListeners() {
    let spacePressed = false;

    document.addEventListener("keydown", (event) => {
      if (
        event.code === "Space" &&
        !event.repeat &&
        this.isConnected &&
        !spacePressed
      ) {
        event.preventDefault();
        spacePressed = true;
        this.addPTTVisualFeedback();
        this.startPushToTalk();
      }
    });

    document.addEventListener("keyup", (event) => {
      if (event.code === "Space" && spacePressed) {
        event.preventDefault();
        spacePressed = false;
        this.removePTTVisualFeedback();
        this.stopPushToTalk();
      }
    });
  }

  addStyles() {
    const style = document.createElement("style");
    style.textContent = `
            .atc-chat-button.recording {
                background: linear-gradient(135deg, #ff6b6b 0%, #ee5a24 100%) !important;
                animation: pulse 1s infinite;
            }

            .atc-chat-button.connected {
                background: linear-gradient(135deg, #00d2d3 0%, #54a0ff 100%) !important;
            }

            .atc-chat-button.disabled {
                background: #6c757d !important;
                cursor: not-allowed;
                opacity: 0.6;
            }

            .atc-chat-button.push-to-talk {
                background: linear-gradient(135deg, #ff9f43 0%, #f0932b 100%) !important;
                animation: glow 1.5s infinite alternate;
            }

            .atc-chat-button.connecting {
                background: linear-gradient(135deg, #ffa726 0%, #ff7043 100%) !important;
                animation: pulse 1.5s infinite;
            }

            @keyframes pulse {
                0% { transform: scale(1); }
                50% { transform: scale(1.05); }
                100% { transform: scale(1); }
            }

            @keyframes glow {
                0% { box-shadow: 0 2px 8px rgba(255, 159, 67, 0.4); }
                100% { box-shadow: 0 4px 16px rgba(255, 159, 67, 0.8); }
            }

            #atc-chat-status.active {
                display: block !important;
                animation: blink 2s infinite;
            }

            @keyframes blink {
                0%, 50% { opacity: 1; }
                51%, 100% { opacity: 0.3; }
            }

            #ai-advisory-container.ptt-active {
                border-color: #ef4444 !important;
                box-shadow: 0 0 0 2px rgba(239, 68, 68, 0.3) !important;
            }
        `;
    document.head.appendChild(style);
  }

  async toggleChat() {
    if (!this.isConnected) {
      await this.startChat();
    } else {
      // Remove confirmation dialog - just disconnect immediately
      await this.endChat();
    }
  }

  async startChat() {
    try {
      console.log("[ATC-Chat] Starting ATC Chat...");
      this.showStatusIndicator("connected", "Connecting...");

      // Create session
      console.log("[ATC-Chat] Creating session...");
      const response = await fetch(
        `${window.location.protocol}//${window.location.hostname}:8000/api/v1/atc-chat/session`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
          },
        },
      );

      console.log("[ATC-Chat] Session response status:", response.status);
      if (!response.ok) {
        const errorText = await response.text();
        throw new Error(
          `Failed to create session: ${response.status} ${response.statusText} - ${errorText}`,
        );
      }

      const session = await response.json();
      this.sessionId = session.id;

      console.log("[ATC-Chat] ATC Chat session created:", this.sessionId);

      // Initialize audio
      console.log("[ATC-Chat] Initializing audio...");
      await this.initializeAudio();

      // Connect WebSocket
      console.log("[ATC-Chat] Connecting WebSocket...");
      await this.connectWebSocket();

      this.isConnected = true;
      this.showStatusIndicator("connected", "PTT - Hold Space");

      // Start AI audio visualization
      this.startAIAudioVisualization();

      // Trigger Alpine.js reactivity
      this.triggerReactivity();

      console.log("[ATC-Chat] ATC Chat started successfully");
    } catch (error) {
      console.error("[ATC-Chat] Failed to start ATC Chat:", error);
      this.hideStatusIndicator();
      setTimeout(() => {
        this.hideStatusIndicator();
      }, 3000);
    }
  }

  async endChat() {
    try {
      this.showStatusIndicator("disconnecting", "Ending...");

      // Stop recording if active
      if (this.isRecording) {
        this.stopRecording();
      }

      // Stop AI audio visualization
      this.stopAIAudioVisualization();

      // Close WebSocket first with proper close code
      // This will trigger server-side cleanup automatically
      if (this.websocket) {
        this.websocket.close(1000, "Session ended by user");
        this.websocket = null;
      }

      // Stop audio stream
      if (this.stream) {
        this.stream.getTracks().forEach((track) => track.stop());
        this.stream = null;
      }

      // Close audio context
      if (this.audioContext) {
        await this.audioContext.close();
        this.audioContext = null;
      }

      // Clean up audio analysis
      this.audioAnalyser = null;
      this.audioDataArray = null;

      // Don't call DELETE endpoint - WebSocket closure will trigger server-side cleanup automatically
      // This prevents the duplicate session termination error
      if (this.sessionId) {
        console.log(
          "[ATC-Chat] Session cleanup will be handled by WebSocket closure",
        );
        this.sessionId = null;
      }

      this.isConnected = false;

      // Hide status indicator
      const statusIndicator = document.getElementById("atc-chat-status");
      if (statusIndicator) {
        statusIndicator.classList.remove("active");
      }

      // Reset status to disconnected
      this.hideStatusIndicator();

      // Trigger Alpine.js reactivity
      this.triggerReactivity();

      console.log("[ATC-Chat] ATC Chat session ended");
    } catch (error) {
      console.error("[ATC-Chat] Failed to end ATC Chat:", error);
      this.hideStatusIndicator();
    }
  }

  async initializeAudio() {
    try {
      // Initialize AudioRecorder
      this.audioRecorder = new AudioRecorder({
        sampleRate: 24000,
        onAudioData: (data) => {
          this.handleAudioData(data);
        }
      });

      // Initialize the audio context (required for playback as well)

      console.log("[ATC-Chat] AudioRecorder initialized.");
    } catch (error) {
      throw new Error(`Failed to initialize audio: ${error.message}`);
    }
  }

  async connectWebSocket() {
    return new Promise((resolve, reject) => {
      const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
      const wsUrl = `${protocol}//${window.location.hostname}:8000/api/v1/atc-chat/ws/${this.sessionId}`;

      // Set timeout first, before creating WebSocket
      const timeout = setTimeout(() => {
        if (this.websocket && this.websocket.readyState !== WebSocket.OPEN) {
          console.log(
            "[ATC-Chat] WebSocket connection timeout - closing connection",
          );
          this.websocket.close();
          reject(new Error("WebSocket connection timeout"));
        }
      }, 15000); // Increased timeout to 15 seconds

      this.websocket = new WebSocket(wsUrl);

      this.websocket.onopen = () => {
        clearTimeout(timeout);
        console.log("[ATC-Chat] ATC Chat WebSocket connected");
        this.isConnected = true;
        resolve();
      };

      this.websocket.onmessage = (event) => {
        this.handleWebSocketMessage(event);
      };

      this.websocket.onerror = (error) => {
        clearTimeout(timeout);
        console.error("[ATC-Chat] WebSocket error:", error);
        reject(error);
      };

      this.websocket.onclose = () => {
        clearTimeout(timeout);
        console.log("[ATC-Chat] ATC Chat WebSocket disconnected");
        if (this.isConnected) {
          this.endChat();
        }
      };
    });
  }

  handleWebSocketMessage(event) {
    try {
      // Handle both text and binary messages
      if (event.data instanceof Blob) {
        // Binary audio data from OpenAI
        this.handleBinaryAudio(event.data);
        return;
      }

      // Text message
      const message = JSON.parse(event.data);
      //console.log('Received WebSocket message:', message);

      switch (message.type) {
        case "connection_ready":
          console.log(
            "[ATC-Chat] Server connection established, waiting for AI provider...",
          );
          break;
        case "provider_ready":
          console.log(
            "[ATC-Chat] Provider connection established, ready for voice chat!",
          );
          break;
        case "connection_error":
          console.error("[ATC-Chat] Connection error:", message.error);
          break;
        case "session.update":
          // Log session update events with full payload
          console.log("[ATC-Chat] Session update received:", message);
          break;
        case "response.audio.delta":
          // OpenAI realtime API audio response
          if (message.delta) {
            console.log(
              "[ATC-Chat] Received audio delta, length:",
              message.delta.length,
            );
            this.queueAudioData(message.delta);
          }
          break;
        case "response.audio.done":
          // Audio response complete
          console.log(
            "[ATC-Chat] Audio response complete, queue length:",
            this.audioQueue.length,
          );
          this.playQueuedAudio();
          break;
        case "response.text.delta":
          // Accumulate AI response text for logging
          if (!this.currentAIResponse) {
            this.currentAIResponse = "";
          }
          if (message.delta) {
            this.currentAIResponse += message.delta;
          }
          break;
        case "response.text.done":
          // Log complete AI response
          if (this.currentAIResponse) {
            console.log("[ATC-Chat] Chat - AI-ATC:", this.currentAIResponse);
            this.addTranscript("AI", this.currentAIResponse);
            this.currentAIResponse = null;
          }
          break;
        case "response.audio_transcript.delta":
          // Accumulate AI audio transcript for logging (alternative text source)
          if (!this.currentAITranscript) {
            this.currentAITranscript = "";
          }
          if (message.delta) {
            this.currentAITranscript += message.delta;
          }
          break;
        case "response.audio_transcript.done":
          // Log complete AI audio transcript
          if (this.currentAITranscript) {
            console.log("[ATC-Chat] Chat - AI-ATC:", this.currentAITranscript);
            // Only add if we don't already have a text response
            if (!this.currentAIResponse) {
              this.addTranscript("AI", this.currentAITranscript);
            }
            this.currentAITranscript = null;
          }
          break;
        case "conversation.item.input_audio_transcription.completed":
          // Log user's transcribed speech
          if (message.transcript) {
            console.log("[ATC-Chat] Chat - Pilot:", message.transcript);
            this.addTranscript("PILOT", message.transcript);
          }
          break;
        case "conversation.item.created":
          console.log("[ATC-Chat] AI response started");
          this.showStatusIndicator("processing", "AI Responding...");

          // Clear any existing audio queue to prevent old audio from replaying
          if (this.audioQueue.length > 0) {
            console.log(
              "[ATC-Chat] Clearing old audio queue with",
              this.audioQueue.length,
              "items",
            );
            this.audioQueue = [];
          }

          // Log the full message to see what data is available
          if (message.item && message.item.content) {
            console.log("[ATC-Chat] Chat - AI-ATC:", message.item.content);
          }
          break;
        case "response.done":
          // Response completely finished - return to ready state
          console.log("[ATC-Chat] AI Response completed");
          this.showStatusIndicator("connected", "PTT - Hold Space");
          if (message.response && message.response.output) {
            console.log("[ATC-Chat] Chat - AI-ATC:", message.response.output);
          }
          break;
        case "error":
          console.error("[ATC-Chat] ATC Chat error:", message.error);
          break;
        default:
        //console.log('[ATC-Chat] OpenAI message type:', message.type, message);
      }
    } catch (error) {
      console.error("[ATC-Chat] Failed to parse WebSocket message:", error);
    }
  }

  handleBinaryAudio(blob) {
    // Handle binary audio data
    this.queueAudioBlob(blob);
  }

  // New method to handle audio from AudioRecorder
  handleAudioData(audioEvent) {
    if (!this.isConnected || !this.pushToTalkActive) return;

    // Extract data
    const base64Audio = audioEvent.base64;
    const rawData = audioEvent.raw;

    // Accumulate raw data for playback
    if (rawData && this.currentRecordingRaw) {
      this.currentRecordingRaw.push(rawData);
    }

    const message = {
      type: "input_audio_buffer.append",
      audio: base64Audio,
    };

    if (this.websocket && this.websocket.readyState === WebSocket.OPEN) {
      this.websocket.send(JSON.stringify(message));
    }
  }

  async startPushToTalk() {
    if (!this.isConnected || this.pushToTalkActive) return;

    this.pushToTalkActive = true;

    // Update context with fresh airspace data when PTT is pressed
    this.showStatusIndicator("push-to-talk", "Updating context...");
    await this.updateSessionContext();

    // Start listening after context is updated
    this.showStatusIndicator("push-to-talk", "Listening...");
    this.startRecording();
  }

  async stopPushToTalk() {
    if (!this.pushToTalkActive) return;

    this.pushToTalkActive = false;
    this.showStatusIndicator("processing", "Processing...");
    this.stopRecording();

    // Will return to ready state when AI response is complete
  }

  async updateSessionContext() {
    if (!this.sessionId) {
      console.warn("[ATC-Chat] No session ID available for context update");
      return;
    }

    try {
      const response = await fetch(
        `/api/v1/atc-chat/session/${this.sessionId}/update-context`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
          },
        },
      );

      if (!response.ok) {
        throw new Error(`HTTP ${response.status}: ${response.statusText}`);
      }

      console.log("[ATC-Chat] Session context updated successfully");
    } catch (error) {
      console.error("[ATC-Chat] Failed to update session context:", error);
      // Don't throw - continue with PTT even if context update fails
    }
  }

  async startRecording() {
    if (this.isRecording) return;

    // Reset recording buffer
    this.currentRecordingRaw = [];

    if (this.audioRecorder) {
      try {
        await this.audioRecorder.start();
        this.isRecording = true;
        console.log("[ATC-Chat] Started recording via AudioRecorder");
      } catch (e) {
        console.error("[ATC-Chat] Failed to start AudioRecorder:", e);
      }
    }
  }

  async stopRecording() {
    if (!this.isRecording) return;

    if (this.audioRecorder) {
      this.audioRecorder.stop();
      this.isRecording = false;
      console.log("[ATC-Chat] Stopped recording via AudioRecorder");

      // Process accumulated raw audio for playback
      if (this.currentRecordingRaw && this.currentRecordingRaw.length > 0) {
        // Calculate total length
        let totalLength = 0;
        for (const chunk of this.currentRecordingRaw) {
          totalLength += chunk.length;
        }

        // Create single float32 buffer (-1 to 1)
        this.lastAudioFloat32 = new Float32Array(totalLength);
        let offset = 0;
        for (const chunk of this.currentRecordingRaw) {
          // Convert Int16 to Float32
          for (let i = 0; i < chunk.length; i++) {
            const int16 = chunk[i];
            const float = int16 < 0 ? int16 / 0x8000 : int16 / 0x7FFF;
            this.lastAudioFloat32[offset++] = float;
          }
        }
        console.log(`[ATC-Chat] Processed ${totalLength} samples for playback.`);
      } else {
        this.lastAudioFloat32 = null;
      }

      // Commit and create response
      if (this.websocket && this.websocket.readyState === WebSocket.OPEN) {
        // Commit the audio buffer
        this.websocket.send(
          JSON.stringify({
            type: "input_audio_buffer.commit",
          }),
        );

        // Create a response
        this.websocket.send(
          JSON.stringify({
            type: "response.create",
            response: {
              modalities: ["text", "audio"],
            },
          }),
        );
      }
    }
  }


  async playLastRecording() {
    if (!this.lastAudioFloat32) {
      console.warn("[ATC-Chat] No recording available to play");
      return;
    }

    // Initialize audio context if needed
    if (!this.audioContext) {
      this.audioContext = new (
        window.AudioContext || window.webkitAudioContext
      )({
        sampleRate: 24000,
      });
    }

    if (this.audioContext.state === "suspended") {
      await this.audioContext.resume();
    }

    try {
      const buffer = this.audioContext.createBuffer(
        1,
        this.lastAudioFloat32.length,
        24000,
      );
      buffer.getChannelData(0).set(this.lastAudioFloat32);

      const source = this.audioContext.createBufferSource();
      source.buffer = buffer;
      source.connect(this.audioContext.destination);
      source.start();
      console.log("[ATC-Chat] Playing last recording via Web Audio API...");
    } catch (e) {
      console.error("[ATC-Chat] Failed to play recording:", e);
    }
  }



  queueAudioData(base64Audio) {
    // Queue base64 audio data for playback
    this.audioQueue.push(base64Audio);
  }

  queueAudioBlob(blob) {
    // Queue audio blob for playback
    this.audioQueue.push(blob);
  }

  async playQueuedAudio() {
    if (this.audioQueue.length === 0) {
      console.log("[ATC-Chat] No audio in queue to play");
      return;
    }

    if (this.isPlaying) {
      console.log("[ATC-Chat] Audio already playing, skipping new audio");
      return;
    }

    this.isPlaying = true;
    this.showStatusIndicator("playing", "AI Speaking...");

    try {
      // Decode each base64 chunk individually and combine the binary data
      let totalLength = 0;
      const binaryChunks = [];

      for (const base64Chunk of this.audioQueue) {
        try {
          // Clean and decode each base64 chunk
          const cleanBase64 = base64Chunk.replace(/[^A-Za-z0-9+/=]/g, "");
          const paddedBase64 =
            cleanBase64 + "=".repeat((4 - (cleanBase64.length % 4)) % 4);
          const binaryString = atob(paddedBase64);
          binaryChunks.push(binaryString);
          totalLength += binaryString.length;
        } catch (error) {
          console.warn("[ATC-Chat] Failed to decode base64 chunk:", error);
        }
      }

      // Clear the queue immediately to prevent replaying
      this.audioQueue = [];

      console.log(
        "[ATC-Chat] Playing combined audio from",
        binaryChunks.length,
        "chunks, total bytes:",
        totalLength,
      );

      // Combine all binary data
      let combinedBinary = "";
      for (const chunk of binaryChunks) {
        combinedBinary += chunk;
      }

      // Convert to PCM16 data
      const pcmData = new Int16Array(combinedBinary.length / 2);

      for (let i = 0; i < pcmData.length; i++) {
        const byte1 = combinedBinary.charCodeAt(i * 2);
        const byte2 = combinedBinary.charCodeAt(i * 2 + 1);
        pcmData[i] = (byte2 << 8) | byte1; // Little-endian
      }

      console.log("[ATC-Chat] Playing PCM audio:", {
        samples: pcmData.length,
        duration: pcmData.length / 24000,
        size: combinedBinary.length,
      });

      // Create audio context if not exists
      if (!this.audioContext) {
        this.audioContext = new (
          window.AudioContext || window.webkitAudioContext
        )({
          sampleRate: 24000,
        });
      }

      // Resume audio context if suspended
      if (this.audioContext.state === "suspended") {
        await this.audioContext.resume();
      }

      // Create audio buffer
      const audioBuffer = this.audioContext.createBuffer(
        1,
        pcmData.length,
        24000,
      );
      const channelData = audioBuffer.getChannelData(0);

      // Convert Int16 to Float32 and copy to buffer
      for (let i = 0; i < pcmData.length; i++) {
        channelData[i] = pcmData[i] / 32768.0;
      }

      // Create buffer source and play
      const source = this.audioContext.createBufferSource();
      source.buffer = audioBuffer;

      // Connect to both destination and analyser for visualization
      source.connect(this.audioContext.destination);
      if (this.audioAnalyser) {
        source.connect(this.audioAnalyser);
      }

      source.onended = () => {
        // Add a small delay to prevent audio cutoff
        setTimeout(() => {
          console.log("[ATC-Chat] Audio playback finished");
          this.isPlaying = false;
          // Return to ready state when audio finishes
          if (this.isConnected) {
            this.showStatusIndicator("connected", "Hold Space to PTT");
          }
        }, 100); // 100ms delay to ensure complete playback
      };

      source.start();
      console.log("[ATC-Chat] Playing AI response audio via Web Audio API");
    } catch (error) {
      console.error("[ATC-Chat] Failed to play queued audio:", error);
      this.isPlaying = false;
      // Return to ready state on error
      if (this.isConnected) {
        this.showStatusIndicator("connected", "Hold Space to PTT");
      }
      // Clear the queue on error
      this.audioQueue = [];
    }
  }

  // Audio visualizer methods for AI audio
  startAIAudioVisualization() {
    if (this.aiVisualizationFrameId) return;

    const renderFrame = () => {
      const visBar = document.getElementById("ai-vis-bar");
      if (!visBar) {
        if (this.aiVisualizationFrameId) {
          cancelAnimationFrame(this.aiVisualizationFrameId);
          this.aiVisualizationFrameId = null;
        }
        return;
      }

      // Get audio level from the current audio context if available
      let audioLevel = 0;
      if (this.audioAnalyser && this.audioDataArray) {
        try {
          this.audioAnalyser.getByteFrequencyData(this.audioDataArray);
          let totalSum = 0;
          let totalPoints = 0;
          const maxBin = Math.min(this.audioDataArray.length, 40);
          for (let j = 1; j < maxBin; j++) {
            const weight = 1 - (j / maxBin) * 0.5;
            totalSum += this.audioDataArray[j] * weight;
            totalPoints += weight;
          }
          audioLevel = totalPoints > 0 ? totalSum / totalPoints / 255 : 0;
        } catch (e) {
          // Fallback to simulated activity during transmission/processing
          if (this.isRecording || this.isPlaying) {
            audioLevel = 0.3 + Math.random() * 0.4;
          }
        }
      } else if (this.isRecording || this.isPlaying) {
        // Simulate audio activity when recording or playing
        audioLevel = 0.3 + Math.random() * 0.4;
      }

      const widthPercentage = Math.min(100, audioLevel * 150);
      const currentWidth = parseFloat(visBar.style.width) || 0;
      const smoothingFactor = 0.3;
      const newWidth =
        currentWidth * smoothingFactor +
        widthPercentage * (1 - smoothingFactor);

      visBar.style.width = newWidth + "%";

      this.aiVisualizationFrameId = requestAnimationFrame(renderFrame);
    };

    this.aiVisualizationFrameId = requestAnimationFrame(renderFrame);
  }

  stopAIAudioVisualization() {
    if (this.aiVisualizationFrameId) {
      cancelAnimationFrame(this.aiVisualizationFrameId);
      this.aiVisualizationFrameId = null;
    }

    const visBar = document.getElementById("ai-vis-bar");
    if (visBar) {
      visBar.style.width = "0%";
    }
  }

  // Transcript management methods
  toggleTranscriptViewer() {
    this.transcriptViewerVisible = !this.transcriptViewerVisible;
    if (this.transcriptViewerVisible) {
      this.filterTranscripts();

      // Position the transcript viewer correctly
      setTimeout(() => {
        const aiAdvisoryElement = document.getElementById(
          "ai-advisory-container",
        );
        const viewer = document.querySelector('[data-viewer-id="ai-advisory"]');

        if (aiAdvisoryElement && viewer) {
          const rect = aiAdvisoryElement.getBoundingClientRect();
          viewer.style.left = `${rect.left}px`;
          viewer.style.width = `${rect.width}px`;
          viewer.style.bottom = `${window.innerHeight - rect.top + 8}px`;
          // Ensure no transitions or animations
          viewer.style.transition = "none";
          viewer.style.transform = "none";
        }
      }, 0);
    }
    this.triggerReactivity();
    console.log(
      "[ATC-Chat] Transcript viewer toggled:",
      this.transcriptViewerVisible,
    );
  }

  addTranscript(speaker, text) {
    const transcript = {
      id: ++this.transcriptIdCounter,
      timestamp: new Date().toISOString(),
      speaker: speaker, // 'AI' or 'PILOT'
      text: text,
    };
    this.transcripts.push(transcript);

    // Ensure filteredTranscripts is updated immediately
    this.filterTranscripts();

    // Trigger Alpine.js reactivity
    this.triggerReactivity();

    // Keep only last 100 transcripts to prevent memory issues
    if (this.transcripts.length > 100) {
      this.transcripts = this.transcripts.slice(-100);
      // Re-filter after trimming
      this.filterTranscripts();
    }
  }

  filterTranscripts() {
    // Ensure arrays are initialized
    if (!this.transcripts) {
      this.transcripts = [];
    }
    if (!this.filteredTranscripts) {
      this.filteredTranscripts = [];
    }

    if (!this.transcriptSearchTerm || this.transcriptSearchTerm.trim() === "") {
      this.filteredTranscripts = [...this.transcripts];
    } else {
      const searchTerm = this.transcriptSearchTerm.toLowerCase();
      this.filteredTranscripts = this.transcripts.filter(
        (transcript) =>
          transcript.text.toLowerCase().includes(searchTerm) ||
          transcript.speaker.toLowerCase().includes(searchTerm),
      );
    }
    // Sort by timestamp, newest first
    this.filteredTranscripts.sort(
      (a, b) => new Date(b.timestamp) - new Date(a.timestamp),
    );
  }

  getTranscriptCount() {
    return this.transcripts ? this.transcripts.length : 0;
  }

  highlightSearchTerm(text) {
    if (!this.transcriptSearchTerm || this.transcriptSearchTerm.trim() === "") {
      return text;
    }

    const searchTerm = this.transcriptSearchTerm.trim();
    const regex = new RegExp(
      `(${searchTerm.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")})`,
      "gi",
    );
    return text.replace(
      regex,
      '<mark class="bg-yellow-500/30 text-yellow-200">$1</mark>',
    );
  }

  triggerReactivity() {
    // Force Alpine.js to re-evaluate by dispatching a custom event
    if (typeof window !== "undefined" && window.Alpine) {
      // Trigger a custom event that Alpine can listen to
      document.dispatchEvent(
        new CustomEvent("atc-chat-update", {
          detail: {
            isConnected: this.isConnected,
            transcriptCount: this.getTranscriptCount(),
            transcriptViewerVisible: this.transcriptViewerVisible,
            transcripts: this.transcripts,
            filteredTranscripts: this.filteredTranscripts,
            transcriptSearchTerm: this.transcriptSearchTerm,
          },
        }),
      );
    }
  }
}

// Initialize ATC Chat when DOM is loaded
document.addEventListener("DOMContentLoaded", () => {
  window.atcChat = new ATCChat();
});

// Add to Alpine.js store if available
document.addEventListener("alpine:init", () => {
  if (window.Alpine) {
    Alpine.store("atcChat", {
      isAvailable: false,
      isConnected: false,
      sessionId: null,

      async checkAvailability() {
        try {
          const response = await fetch(
            `${window.location.protocol}//${window.location.hostname}:8000/api/v1/config`,
          );
          if (response.ok) {
            const config = await response.json();
            this.isAvailable = config.atc_chat?.enabled || false;
          }
        } catch (error) {
          this.isAvailable = false;
        }
        return this.isAvailable;
      },
    });
  }
});
