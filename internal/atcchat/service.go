package atcchat

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/yegors/co-atc/internal/ai"
	"github.com/yegors/co-atc/internal/ai/gemini"
	"github.com/yegors/co-atc/internal/ai/openai"
	"github.com/yegors/co-atc/internal/config"
	"github.com/yegors/co-atc/internal/templating"
	"github.com/yegors/co-atc/pkg/logger"
)

// TemplatingService interface for shared templating functionality
type TemplatingService interface {
	RenderATCChatTemplate(templatePath string) (string, error)
	GetTemplateContext(opts FormattingOptions) (*TemplateContext, error)
}

// Import templating types
type FormattingOptions = templating.FormattingOptions
type TemplateContext = templating.TemplateContext

// ATCChatFormattingOptions returns formatting options for ATC chat
func ATCChatFormattingOptions() FormattingOptions {
	return templating.ATCChatFormattingOptions()
}

// Service manages ATC chat sessions and interactions
type Service struct {
	realtimeProvider  ai.RealtimeProvider
	provider          string
	templatingService TemplatingService
	config            *config.ATCChatConfig
	logger            *logger.Logger

	// Session management
	sessions   map[string]*ai.RealtimeSession
	sessionsMu sync.RWMutex

	// WebSocket connection registry for sending updates
	wsConnections   map[string]chan string // sessionID -> update channel
	wsConnectionsMu sync.RWMutex

	// Background tasks
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewService creates a new ATC chat service
func NewService(
	templatingService TemplatingService,
	config *config.Config,
	logger *logger.Logger,
) (*Service, error) {
	provider, enabled := config.GetATCChatProvider()
	if !enabled {
		return nil, fmt.Errorf("ATC chat is disabled in configuration")
	}

	var realtimeProvider ai.RealtimeProvider
	switch provider {
	case "openai":
		realtimeProvider = openai.NewClient(
			config.OpenAI.APIKey,
			logger,
			config.OpenAI.BaseURL,
		)
	case "gemini":
		realtimeProvider = gemini.NewClient(config.Gemini.APIKey, logger)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}

	ctx, cancel := context.WithCancel(context.Background())

	service := &Service{
		realtimeProvider:  realtimeProvider,
		provider:          provider,
		templatingService: templatingService,
		config:            &config.ATCChat,
		logger:            logger.Named("atc-chat-service"),
		sessions:          make(map[string]*ai.RealtimeSession),
		wsConnections:     make(map[string]chan string),
		ctx:               ctx,
		cancel:            cancel,
	}

	// Start background tasks
	service.startBackgroundTasks()

	return service, nil
}

// CreateSession creates a new chat session
func (s *Service) CreateSession(ctx context.Context) (*ai.RealtimeSession, error) {
	s.logger.Info("Creating new ATC chat session")

	// Use templating service to render the ATC chat template
	staticPrompt, err := s.templatingService.RenderATCChatTemplate(s.config.SystemPromptPath)
	if err != nil {
		return nil, fmt.Errorf("failed to render ATC chat template: %w", err)
	}

	// Build config
	sessionConfig := ai.RealtimeSessionConfig{
		Model:             s.config.RealtimeModel,
		Voice:             s.config.Voice,
		Temperature:       s.config.Temperature,
		MaxResponseTokens: s.config.MaxResponseTokens,
		InputAudioFormat:  s.config.InputAudioFormat,
		OutputAudioFormat: s.config.OutputAudioFormat,
		TurnDetection:     s.config.TurnDetectionType,
		SampleRate:        s.config.SampleRate,
	}

	session, err := s.realtimeProvider.CreateRealtimeSession(ctx, sessionConfig, staticPrompt)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Store session
	s.sessionsMu.Lock()
	s.sessions[session.ID] = session
	s.sessionsMu.Unlock()

	s.logger.Info("Successfully created ATC chat session",
		logger.String("provider", s.provider),
		logger.String("session_id", session.ID),
		logger.String("provider_session_id", session.ProviderID),
		logger.Int("total_sessions", len(s.sessions)))

	return session, nil
}

// ConnectToProvider establishes the connection for handlers
func (s *Service) ConnectToProvider(ctx context.Context, session *ai.RealtimeSession) (ai.AIConnection, error) {
	return s.realtimeProvider.ConnectSession(ctx, session)
}

// GetSession retrieves a session by ID
func (s *Service) GetSession(sessionID string) (*ai.RealtimeSession, error) {
	s.sessionsMu.RLock()
	session, exists := s.sessions[sessionID]
	s.sessionsMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	if !s.realtimeProvider.ValidateSession(session) {
		return nil, fmt.Errorf("session invalid or expired")
	}

	return session, nil
}

// EndSession terminates a chat session
func (s *Service) EndSession(ctx context.Context, sessionID string) error {
	s.logger.Info("Ending ATC chat session", logger.String("session_id", sessionID))

	s.sessionsMu.Lock()
	session, exists := s.sessions[sessionID]
	if exists {
		delete(s.sessions, sessionID)
	}
	s.sessionsMu.Unlock()

	if !exists {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	s.UnregisterWebSocketConnection(sessionID)

	session.Active = false
	return nil
}

// GetSessionStatus returns the status of a session
func (s *Service) GetSessionStatus(sessionID string) (SessionStatus, error) {
	s.sessionsMu.RLock()
	session, exists := s.sessions[sessionID]
	s.sessionsMu.RUnlock()

	if !exists {
		return SessionStatus{
			ID:        sessionID,
			Active:    false,
			Connected: false,
			Error:     "Session not found",
		}, nil
	}

	// This assumes SessionStatus is defined in models.go in same package
	return SessionStatus{
		ID:           session.ID,
		Active:       session.Active,
		Connected:    s.realtimeProvider.ValidateSession(session),
		LastActivity: session.LastActivity,
		ExpiresAt:    session.ExpiresAt,
	}, nil

}

// ListActiveSessions returns all active sessions
func (s *Service) ListActiveSessions() []*ai.RealtimeSession {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()

	var activeSessions []*ai.RealtimeSession
	for _, session := range s.sessions {
		if s.realtimeProvider.ValidateSession(session) && s.hasActiveWebSocketConnection(session.ID) {
			activeSessions = append(activeSessions, session)
		}
	}

	return activeSessions
}

// UpdateSessionContext updates system prompt
func (s *Service) UpdateSessionContext(ctx context.Context, sessionID string) error {
	session, err := s.GetSession(sessionID)
	if err != nil {
		return err
	}

	session.LastActivity = time.Now().UTC()
	return nil
}

// startBackgroundTasks starts background maintenance tasks
func (s *Service) startBackgroundTasks() {
	s.wg.Add(1)
	go s.sessionCleanupTask()

	if s.config.RefreshSystemPrompt > 0 {
		s.wg.Add(1)
		go s.systemPromptRefreshTask()
	}
}

// sessionCleanupTask periodically cleans up expired sessions
func (s *Service) sessionCleanupTask() {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.cleanupExpiredSessions()
		}
	}
}

// cleanupExpiredSessions removes expired or invalid sessions
func (s *Service) cleanupExpiredSessions() {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	var expiredSessions []string
	for sessionID, session := range s.sessions {
		if !s.realtimeProvider.ValidateSession(session) {
			expiredSessions = append(expiredSessions, sessionID)
		}
	}
	for _, id := range expiredSessions {
		delete(s.sessions, id)
	}
}

// systemPromptRefreshTask periodically sends system prompt updates to all active sessions
func (s *Service) systemPromptRefreshTask() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Duration(s.config.RefreshSystemPrompt) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.refreshAllActiveSessions()
		}
	}
}

// refreshAllActiveSessions sends system prompt updates to all active sessions
func (s *Service) refreshAllActiveSessions() {
	activeSessions := s.ListActiveSessions()
	for _, session := range activeSessions {
		if err := s.sendSystemPromptUpdate(session.ID); err != nil {
			s.logger.Error("Failed update", logger.String("id", session.ID), logger.Error(err))
		}
	}
}

// sendSystemPromptUpdate sends a system prompt update to a specific session with detailed logging
func (s *Service) sendSystemPromptUpdate(sessionID string) error {
	promptWithVars, err := s.GenerateSystemPromptWithVariables(sessionID)
	if err != nil {
		return err
	}

	sessionUpdate := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"instructions": promptWithVars.Prompt,
		},
	}
	updateData, _ := json.Marshal(sessionUpdate)
	s.SendSessionUpdate(sessionID, string(updateData))
	return nil
}

// UpdateSessionContextOnDemand updates the context for a specific session with fresh airspace data
func (s *Service) UpdateSessionContextOnDemand(sessionID string) error {
	return s.sendSystemPromptUpdate(sessionID)
}

// GenerateSystemPromptWithVariables generates a templated system prompt and returns individual variables
func (s *Service) GenerateSystemPromptWithVariables(sessionID string) (*PromptWithVariables, error) {
	prompt, err := s.templatingService.RenderATCChatTemplate(s.config.SystemPromptPath)
	if err != nil {
		return nil, err
	}

	context, err := s.templatingService.GetTemplateContext(ATCChatFormattingOptions())
	if err != nil {
		// Fallback
		return &PromptWithVariables{Prompt: prompt, Variables: map[string]any{}}, nil
	}

	variables := map[string]any{
		"Aircraft":             templating.FormatAircraftData(context.Aircraft, context.Airport),
		"Weather":              templating.FormatWeatherData(context.Weather),
		"Runways":              templating.FormatRunwayData(context.Runways),
		"TranscriptionHistory": templating.FormatTranscriptionHistory(context.TranscriptionHistory),
		"Airport":              templating.FormatAirportData(context.Airport),
	}

	return &PromptWithVariables{Prompt: prompt, Variables: variables}, nil
}

// Structs
type PromptWithVariables struct {
	Prompt    string         `json:"prompt"`
	Variables map[string]any `json:"variables"`
}

// RegisterWebSocketConnection registers a WebSocket connection for session updates
func (s *Service) RegisterWebSocketConnection(sessionID string) chan string {
	s.wsConnectionsMu.Lock()
	defer s.wsConnectionsMu.Unlock()
	updateChan := make(chan string, 10)
	s.wsConnections[sessionID] = updateChan
	return updateChan
}

// UnregisterWebSocketConnection removes a WebSocket connection from the registry
func (s *Service) UnregisterWebSocketConnection(sessionID string) {
	s.wsConnectionsMu.Lock()
	defer s.wsConnectionsMu.Unlock()
	if updateChan, exists := s.wsConnections[sessionID]; exists {
		close(updateChan)
		delete(s.wsConnections, sessionID)
	}
}

// hasActiveWebSocketConnection checks if a session has an active WebSocket connection
func (s *Service) hasActiveWebSocketConnection(sessionID string) bool {
	s.wsConnectionsMu.RLock()
	defer s.wsConnectionsMu.RUnlock()
	_, exists := s.wsConnections[sessionID]
	return exists
}

// SendSessionUpdate sends a session update to a specific session's WebSocket connection
func (s *Service) SendSessionUpdate(sessionID string, updateMessage string) {
	s.wsConnectionsMu.RLock()
	updateChan, exists := s.wsConnections[sessionID]
	s.wsConnectionsMu.RUnlock()
	if exists {
		select {
		case updateChan <- updateMessage:
		default:
		}
	}
}

// Shutdown gracefully shuts down the service
func (s *Service) Shutdown(ctx context.Context) error {
	s.cancel()
	// Logic to wait for wg... simplified for now
	s.sessionsMu.Lock()
	for id := range s.sessions {
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()
	return nil
}

func (s *Service) GetConfig() *config.ATCChatConfig {
	return s.config
}

// IsEnabled
func (s *Service) IsEnabled() bool {
	return s.provider != ""
}

// GetAirspaceStatus returns the current status of the airspace (aircraft, weather, etc.)
func (s *Service) GetAirspaceStatus() map[string]any {
	context, err := s.templatingService.GetTemplateContext(ATCChatFormattingOptions())
	if err != nil {
		s.logger.Error("Failed to get template context for status", logger.Error(err))
		return map[string]any{
			"error": "Failed to retrieve airspace status",
		}
	}

	return map[string]any{
		"aircraft_count": len(context.Aircraft),
		"weather":        context.Weather,
		"airport":        context.Airport.Code,
		"timestamp":      time.Now().UTC(),
	}
}
