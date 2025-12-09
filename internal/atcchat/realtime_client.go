package atcchat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yegors/co-atc/pkg/logger"
)

// RealtimeClient handles OpenAI realtime API interactions
// Note: This is a simplified implementation since the OpenAI Go SDK doesn't support realtime APIs yet
type RealtimeClient struct {
	apiKey        string
	httpClient    *http.Client
	config        SessionConfig
	logger        *logger.Logger
	// baseURL allows overriding the default OpenAI API endpoint (e.g. when using a proxy).
	// It should not contain a trailing slash.
	baseURL string

	// websocketPath is the path portion used when constructing realtime websocket URLs.
	// It can be overridden via the OPENAI_WEBSOCKET_PATH env var. Default: /v1/realtime
	websocketPath string
}

// NewRealtimeClient creates a new OpenAI realtime client
// The function accepts an optional `baseURL` parameter. If `baseURL` is non-empty it will be used.
// Otherwise the function falls back to the OPENAI_API_BASE environment variable, then the default.
// The websocket path can be overridden with OPENAI_WEBSOCKET_PATH; if unset a sensible default is used.
func NewRealtimeClient(apiKey string, config SessionConfig, logger *logger.Logger, baseURL string) *RealtimeClient {
	if apiKey == "" {
		logger.Warn("OpenAI API key is empty - ATC Chat features will not work")
	}

	// Determine base URL (prefer explicit baseURL parameter, then OPENAI_API_BASE env, then default).
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = os.Getenv("OPENAI_API_BASE")
	}
	if base == "" {
		base = "https://api.openai.com"
	}
	base = strings.TrimRight(base, "/")

	// Determine websocket path: prefer explicit env override, otherwise default to standard OpenAI realtime path.
	wsPath := strings.TrimSpace(os.Getenv("OPENAI_WEBSOCKET_PATH"))
	if wsPath == "" {
		wsPath = "/v1/realtime"
	}

	return &RealtimeClient{
		apiKey: apiKey,
		config: config,
		logger: logger.Named("realtime-client"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL:       base,
		websocketPath: wsPath,
	}
}

// GetBaseURL returns the configured base URL (no trailing slash).
func (rc *RealtimeClient) GetBaseURL() string {
	return strings.TrimRight(rc.baseURL, "/")
}

// GetWebsocketPath returns the configured websocket path (leading slash included).
// If no explicit value was configured, it returns the default "/v1/realtime".
func (rc *RealtimeClient) GetWebsocketPath() string {
	if rc.websocketPath == "" {
		return "/v1/realtime"
	}
	return rc.websocketPath
}

// SessionRequest represents the request to create a realtime session
type SessionRequest struct {
	InputAudioFormat         string                `json:"input_audio_format"`
	OutputAudioFormat        string                `json:"output_audio_format"`
	Instructions             string                `json:"instructions"`
	MaxResponseTokens        int                   `json:"max_response_output_tokens"`
	Modalities               []string              `json:"modalities"`
	Model                    string                `json:"model"`
	Temperature              float64               `json:"temperature"`
	Voice                    string                `json:"voice"`
	TurnDetection            *TurnDetectionConfig  `json:"turn_detection,omitempty"`
	InputAudioNoiseReduction *NoiseReductionConfig `json:"input_audio_noise_reduction,omitempty"`
}

// TurnDetectionConfig represents turn detection configuration
type TurnDetectionConfig struct {
	Type              string   `json:"type"`
	Threshold         *float64 `json:"threshold,omitempty"`
	SilenceDurationMs *int     `json:"silence_duration_ms,omitempty"`
}

// NoiseReductionConfig represents noise reduction configuration
type NoiseReductionConfig struct {
	Type string `json:"type"`
}

// SessionResponse represents the response from creating a realtime session
type SessionResponse struct {
	ID           string `json:"id"`
	ClientSecret struct {
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
	} `json:"client_secret"`
}

// CreateSession creates a new realtime session with OpenAI
func (rc *RealtimeClient) CreateSession(ctx context.Context, systemPrompt string) (*ChatSession, error) {
	// Check if OpenAI API key is provided - fail fast if missing
	if rc.apiKey == "" {
		return nil, fmt.Errorf("OpenAI API key is required for ATC Chat sessions")
	}

	rc.logger.Info("Creating new OpenAI realtime session",
		logger.String("model", rc.config.Model),
		logger.String("voice", rc.config.Voice))

	// Create the session request with required parameters
	sessionReq := SessionRequest{
		Model:             rc.config.Model,
		Instructions:      systemPrompt,
		Voice:             rc.config.Voice,
		Modalities:        []string{"text", "audio"},
		InputAudioFormat:  rc.config.InputAudioFormat,
		OutputAudioFormat: rc.config.OutputAudioFormat,
	}

	// Add optional parameters if configured
	if rc.config.MaxResponseTokens > 0 {
		sessionReq.MaxResponseTokens = rc.config.MaxResponseTokens
	}

	// OpenAI realtime API requires temperature >= 0.6
	if rc.config.Temperature >= 0.6 {
		sessionReq.Temperature = rc.config.Temperature
	} else {
		// Use default temperature of 0.8 if not configured or below minimum
		sessionReq.Temperature = 0.8
	}

	// Note: Speed parameter not yet supported in OpenAI realtime API
	// Will be added when OpenAI supports it

	// Add turn detection based on configuration
	// If TurnDetectionType is empty or "none", omit turn_detection entirely (turn off)
	// Otherwise, configure with the specified type
	if rc.config.TurnDetectionType != "" && rc.config.TurnDetectionType != "none" {
		turnDetection := &TurnDetectionConfig{
			Type: rc.config.TurnDetectionType,
		}

		if rc.config.VADThreshold > 0 {
			turnDetection.Threshold = &rc.config.VADThreshold
		}

		if rc.config.SilenceDurationMs > 0 {
			turnDetection.SilenceDurationMs = &rc.config.SilenceDurationMs
		}

		sessionReq.TurnDetection = turnDetection
	}
	// If empty or "none", leave TurnDetection as nil (omitted from JSON)

	// Marshal request to JSON
	jsonData, err := json.Marshal(sessionReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal session request: %w", err)
	}

	// Log the full request payload
	rc.logger.Info("=== OpenAI Session Creation Request ===")
	apiURL := fmt.Sprintf("%s/v1/realtime/sessions", strings.TrimRight(rc.baseURL, "/"))
	rc.logger.Info("Request URL: "+apiURL)
	rc.logger.Info("Request Headers:",
		logger.String("Content-Type", "application/json"),
		logger.String("Authorization", "Bearer [REDACTED]"),
		logger.String("OpenAI-Beta", "realtime=v1"))
	rc.logger.Info("Request Payload:", logger.String("json", string(jsonData)))

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", rc.apiKey))
	req.Header.Set("OpenAI-Beta", "realtime=v1")

	// Execute request
	resp, err := rc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check response status and log detailed error if not OK
	if resp.StatusCode != http.StatusOK {
		// Read the error response body
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			rc.logger.Error("Failed to read error response body", logger.Error(readErr))
			return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		var errorBody map[string]interface{}
		if json.Unmarshal(bodyBytes, &errorBody) == nil {
			rc.logger.Error("OpenAI session creation failed with detailed error",
				logger.Int("status_code", resp.StatusCode),
				logger.Any("error_response", errorBody))
		} else {
			rc.logger.Error("OpenAI session creation failed",
				logger.Int("status_code", resp.StatusCode),
				logger.String("response_body", string(bodyBytes)))
		}

		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Read response body for logging
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Log the full response
	rc.logger.Info("=== OpenAI Session Creation Response ===")
	rc.logger.Info("Response Status:", logger.Int("status_code", resp.StatusCode))
	rc.logger.Info("Response Headers:")
	for name, values := range resp.Header {
		for _, value := range values {
			rc.logger.Info("  " + name + ": " + value)
		}
	}
	rc.logger.Info("Response Payload:", logger.String("json", string(bodyBytes)))

	// Parse response
	var sessionResp SessionResponse
	if err := json.Unmarshal(bodyBytes, &sessionResp); err != nil {
		return nil, fmt.Errorf("failed to decode session response: %w", err)
	}

	// Create our session object
	chatSession := &ChatSession{
		ID:              generateSessionID(),
		OpenAISessionID: sessionResp.ID,
		ClientSecret:    sessionResp.ClientSecret.Value,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       time.Unix(sessionResp.ClientSecret.ExpiresAt, 0),
		Active:          true,
		LastActivity:    time.Now().UTC(),
	}

	rc.logger.Info("Successfully created realtime session",
		logger.String("session_id", chatSession.ID),
		logger.String("openai_session_id", chatSession.OpenAISessionID),
		logger.Time("expires_at", chatSession.ExpiresAt))

	return chatSession, nil
}

// UpdateSessionInstructions updates the system instructions for an existing session
func (rc *RealtimeClient) UpdateSessionInstructions(ctx context.Context, sessionID string, instructions string) error {
	rc.logger.Debug("Updating session instructions",
		logger.String("session_id", sessionID))

	// Note: This would need to be implemented when the realtime API supports instruction updates
	// For now, we'll log that this functionality is not yet available

	rc.logger.Warn("Session instruction updates not yet implemented in realtime API",
		logger.String("session_id", sessionID))

	return nil
}

// EndSession terminates a realtime session
func (rc *RealtimeClient) EndSession(ctx context.Context, sessionID string) error {
	rc.logger.Info("Ending realtime session",
		logger.String("session_id", sessionID))

	// Note: The realtime API might not have direct session termination endpoints yet
	// For now, we'll just log the termination

	rc.logger.Info("Session marked for termination",
		logger.String("session_id", sessionID))

	return nil
}

// ValidateSession checks if a session is still valid
func (rc *RealtimeClient) ValidateSession(session *ChatSession) bool {
	if session == nil {
		return false
	}

	// Check if session has expired
	if time.Now().UTC().After(session.ExpiresAt) {
		rc.logger.Debug("Session has expired",
			logger.String("session_id", session.ID),
			logger.Time("expired_at", session.ExpiresAt))
		return false
	}

	// Check if session is still active
	if !session.Active {
		rc.logger.Debug("Session is not active",
			logger.String("session_id", session.ID))
		return false
	}

	return true
}

// RefreshSession creates a new session to replace an expiring one
func (rc *RealtimeClient) RefreshSession(ctx context.Context, oldSession *ChatSession, systemPrompt string) (*ChatSession, error) {
	rc.logger.Info("Refreshing realtime session",
		logger.String("old_session_id", oldSession.ID))

	// Create a new session
	newSession, err := rc.CreateSession(ctx, systemPrompt)
	if err != nil {
		return nil, fmt.Errorf("failed to create replacement session: %w", err)
	}

	// End the old session
	if err := rc.EndSession(ctx, oldSession.OpenAISessionID); err != nil {
		rc.logger.Warn("Failed to properly end old session",
			logger.String("old_session_id", oldSession.ID),
			logger.Error(err))
	}

	rc.logger.Info("Successfully refreshed session",
		logger.String("old_session_id", oldSession.ID),
		logger.String("new_session_id", newSession.ID))

	return newSession, nil
}

// GetSessionStatus returns the current status of a session
func (rc *RealtimeClient) GetSessionStatus(session *ChatSession) SessionStatus {
	if session == nil {
		return SessionStatus{
			Active:    false,
			Connected: false,
			Error:     "Session is nil",
		}
	}

	status := SessionStatus{
		ID:           session.ID,
		Active:       session.Active,
		Connected:    rc.ValidateSession(session),
		LastActivity: session.LastActivity,
		ExpiresAt:    session.ExpiresAt,
	}

	if !status.Connected {
		if time.Now().UTC().After(session.ExpiresAt) {
			status.Error = "Session expired"
		} else if !session.Active {
			status.Error = "Session inactive"
		}
	}

	return status
}

// generateSessionID generates a unique session ID
func generateSessionID() string {
	return fmt.Sprintf("atc_chat_%d", time.Now().UnixNano())
}

// IsSessionExpiringSoon checks if a session will expire within the given duration
func (rc *RealtimeClient) IsSessionExpiringSoon(session *ChatSession, within time.Duration) bool {
	if session == nil {
		return true
	}

	expiryThreshold := time.Now().UTC().Add(within)
	return session.ExpiresAt.Before(expiryThreshold)
}

// GetTimeUntilExpiry returns the time until session expiry
func (rc *RealtimeClient) GetTimeUntilExpiry(session *ChatSession) time.Duration {
	if session == nil {
		return 0
	}

	return time.Until(session.ExpiresAt)
}
