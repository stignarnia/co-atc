package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/yegors/co-atc/internal/ai"
	"github.com/yegors/co-atc/internal/atcchat"
	"github.com/yegors/co-atc/pkg/logger"
)

// ATCChatHandlers contains handlers for ATC chat functionality
type ATCChatHandlers struct {
	service  *atcchat.Service
	logger   *logger.Logger
	upgrader websocket.Upgrader
}

// NewATCChatHandlers creates new ATC chat handlers
func NewATCChatHandlers(service *atcchat.Service, logger *logger.Logger) *ATCChatHandlers {
	return &ATCChatHandlers{
		service: service,
		logger:  logger.Named("atc-chat-handlers"),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

// CreateSession creates a new ATC chat session
func (h *ATCChatHandlers) CreateSession(w http.ResponseWriter, r *http.Request) {
	h.logger.Info("Creating new ATC chat session")

	session, err := h.service.CreateSession(r.Context())
	if err != nil {
		h.logger.Error("Failed to create session", logger.Error(err))
		http.Error(w, "Failed to create session", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(session); err != nil {
		h.logger.Error("Failed to encode session response", logger.Error(err))
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	h.logger.Info("Successfully created session",
		logger.String("session_id", session.ID))
}

// GetSession retrieves an existing ATC chat session
func (h *ATCChatHandlers) GetSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	session, err := h.service.GetSession(sessionID)
	if err != nil {
		h.logger.Error("Failed to get session",
			logger.String("session_id", sessionID),
			logger.Error(err))
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(session); err != nil {
		h.logger.Error("Failed to encode session response", logger.Error(err))
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

// EndSession terminates an ATC chat session
func (h *ATCChatHandlers) EndSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	if err := h.service.EndSession(r.Context(), sessionID); err != nil {
		h.logger.Error("Failed to end session",
			logger.String("session_id", sessionID),
			logger.Error(err))
		http.Error(w, "Failed to end session", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	h.logger.Info("Successfully ended session",
		logger.String("session_id", sessionID))
}

// UpdateSessionContext updates the session context with fresh airspace data
func (h *ATCChatHandlers) UpdateSessionContext(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	h.logger.Debug("Received request to update session context",
		logger.String("session_id", sessionID))

	if err := h.service.UpdateSessionContextOnDemand(sessionID); err != nil {
		h.logger.Error("Failed to update session context",
			logger.String("session_id", sessionID),
			logger.Error(err))
		http.Error(w, fmt.Sprintf("Failed to update session context: %v", err), http.StatusInternalServerError)
		return
	}

	h.logger.Info("Session context updated successfully",
		logger.String("session_id", sessionID))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "success",
		"message": "Session context updated",
	})

}

// WebSocketHandler handles WebSocket connections for realtime audio
func (h *ATCChatHandlers) WebSocketHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	session, err := h.service.GetSession(sessionID)
	if err != nil {
		h.logger.Error("Session not found for WebSocket connection",
			logger.String("session_id", sessionID),
			logger.Error(err))
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("Failed to upgrade to WebSocket", logger.Error(err))
		return
	}
	defer conn.Close()

	h.logger.Info("WebSocket connection established",
		logger.String("session_id", sessionID))

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	if err := h.bridgeRealtimeAudio(ctx, conn, session); err != nil {
		if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
			h.logger.Error("Realtime audio bridge failed", logger.Error(err))
		}
	}
}

// bridgeRealtimeAudio handles the bidirectional audio streaming
func (h *ATCChatHandlers) bridgeRealtimeAudio(ctx context.Context, clientConn *websocket.Conn, session *ai.RealtimeSession) error {
	updateChan := h.service.RegisterWebSocketConnection(session.ID)
	defer h.service.UnregisterWebSocketConnection(session.ID)

	// Send connection_ready to client
	clientConn.WriteJSON(map[string]any{
		"type":       "connection_ready",
		"session_id": session.ID,
	})

	// Connect to AI Provider
	h.logger.Info("Connecting to AI Provider", logger.String("session_id", session.ID))
	providerConn, err := h.service.ConnectToProvider(ctx, session)
	if err != nil {
		clientConn.WriteJSON(map[string]any{
			"type":  "connection_error",
			"error": "Failed to connect to AI Provider",
		})
		return fmt.Errorf("provider connection failed: %w", err)
	}
	defer providerConn.Close()

	clientConn.WriteJSON(map[string]any{
		"type":       "provider_ready",
		"session_id": session.ID,
	})

	// Safe wrapper for client writes
	safeClientConn := NewSafeWebSocketConn(clientConn)

	errChan := make(chan error, 3)

	go func() {
		errChan <- h.forwardClientToProvider(clientConn, providerConn)
	}()

	go func() {
		errChan <- h.forwardProviderToClient(ctx, providerConn, safeClientConn, session)
	}()

	go func() {
		errChan <- h.handleContextUpdates(ctx, providerConn, updateChan)
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *ATCChatHandlers) forwardClientToProvider(clientConn *websocket.Conn, providerConn ai.AIConnection) error {

	for {
		_, message, err := clientConn.ReadMessage()
		if err != nil {
			return err
		}

		// Forward to provider
		if err := providerConn.Send(message); err != nil {
			h.logger.Error("Failed to send to provider", logger.Error(err))
			return err
		}
	}
}

func (h *ATCChatHandlers) forwardProviderToClient(ctx context.Context, providerConn ai.AIConnection, clientConn *SafeWebSocketConn, session *ai.RealtimeSession) error {
	for {
		msgType, message, err := providerConn.Read()
		if err != nil {
			return err
		}

		// Intercept session.created if needed
		if msgType == websocket.TextMessage {
			var event map[string]any
			if err := json.Unmarshal(message, &event); err == nil {

				if t, ok := event["type"].(string); ok && t == "session.created" {
					// Ensure session context is updated with fresh instructions
					go h.service.UpdateSessionContext(ctx, session.ID)
				}
			}
		}

		if err := clientConn.WriteMessage(msgType, message); err != nil {
			return err
		}
	}
}

func (h *ATCChatHandlers) handleContextUpdates(ctx context.Context, providerConn ai.AIConnection, updateChan <-chan string) error {

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-updateChan:
			if err := providerConn.Send([]byte(msg)); err != nil {
				return err
			}
		}
	}
}

func (h *ATCChatHandlers) GetSessionStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		http.Error(w, "ID required", http.StatusBadRequest)
		return
	}
	status, err := h.service.GetSessionStatus(sessionID)
	if err != nil {
		http.Error(w, "Status failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// Helpers
type SafeWebSocketConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func NewSafeWebSocketConn(conn *websocket.Conn) *SafeWebSocketConn {
	return &SafeWebSocketConn{conn: conn}
}
func (s *SafeWebSocketConn) WriteMessage(mt int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteMessage(mt, data)
}
func (s *SafeWebSocketConn) Close() error {
	return s.conn.Close()
}
