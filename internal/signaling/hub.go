package signaling

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/foxzi/vocala/internal/auth"
	"github.com/foxzi/vocala/internal/channel"
	"github.com/foxzi/vocala/internal/database"
	rtc "github.com/foxzi/vocala/internal/webrtc"
	"github.com/gorilla/websocket"
)

// Maximum WebSocket message size (default 512 KB)
var maxMessageSize int64 = 512 * 1024

// SetMaxMessageSize overrides the default WebSocket message size limit.
func SetMaxMessageSize(size int64) {
	maxMessageSize = size
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients
		}
		// Allow same-origin requests
		host := r.Host
		return origin == "http://"+host || origin == "https://"+host
	},
}

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Client struct {
	UserID    int64
	Username  string
	IsAdmin   bool
	Conn      *websocket.Conn
	Send      chan []byte // text JSON messages
	SendMedia chan []byte // binary media frames
	IsWSMedia bool        // true if this client uses WS media transport (mobile)
}

type Hub struct {
	mu      sync.RWMutex
	clients map[int64]*Client
}

var GlobalHub = &Hub{
	clients: make(map[int64]*Client),
}

// Screen share preview store: channelID -> latest screen_preview JSON message
var (
	previewMu       sync.RWMutex
	channelPreviews = map[int64][]byte{}
)

func (h *Hub) Register(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Close previous connection for the same user (#8)
	if old, ok := h.clients[client.UserID]; ok && old != client {
		old.Conn.Close()
	}
	h.clients[client.UserID] = client
}

func (h *Hub) Unregister(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Only remove if this is still the current client for this user
	// (a newer connection may have replaced this one via Register)
	if current, ok := h.clients[client.UserID]; ok && current == client {
		delete(h.clients, client.UserID)
	}
}

func (h *Hub) Broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, client := range h.clients {
		select {
		case client.Send <- msg:
		default:
			// drop message if client is too slow
		}
	}
}

func (h *Hub) BroadcastToChannel(channelID int64, msg []byte) {
	users := channel.GetUsers(channelID)
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, u := range users {
		if client, ok := h.clients[u.ID]; ok {
			select {
			case client.Send <- msg:
			default:
			}
		}
	}
}

func (h *Hub) SendTo(userID int64, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if client, ok := h.clients[userID]; ok {
		select {
		case client.Send <- msg:
		default:
		}
	}
}

func HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromRequest(r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("websocket upgrade error:", err)
		return
	}

	client := &Client{
		UserID:    user.ID,
		Username:  user.Username,
		IsAdmin:   user.IsAdmin,
		Conn:      conn,
		Send:      make(chan []byte, 256),
		SendMedia: make(chan []byte, 64),
	}

	GlobalHub.Register(client)

	go client.writePump()
	go client.readPump()
}

func (c *Client) writePump() {
	defer c.Conn.Close()
	for {
		select {
		case msg, ok := <-c.Send:
			if !ok {
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case media, ok := <-c.SendMedia:
			if !ok {
				return
			}
			if err := c.Conn.WriteMessage(websocket.BinaryMessage, media); err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		chID := channel.Leave(c.UserID)
		GlobalHub.Unregister(c)
		c.Conn.Close()
		if chID > 0 {
			cleanupWebRTC(chID, c.UserID)
			clearPreviewIfSharer(chID, c.UserID)
			broadcastChannelUpdate(chID)
		}
		broadcastPresence()
	}()

	c.Conn.SetReadLimit(maxMessageSize)

	for {
		msgType, raw, err := c.Conn.ReadMessage()
		if err != nil {
			return
		}

		if msgType == websocket.BinaryMessage {
			handleBinaryMedia(c, raw)
			continue
		}

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		handleMessage(c, msg)
	}
}

// handleBinaryMedia relays binary audio/video frames to other clients in the same channel.
// Frame format: [1 byte type][payload]
// Type: 0x01=audio, 0x02=video
// Server prepends sender userID (8 bytes big-endian) before relaying.
func handleBinaryMedia(c *Client, raw []byte) {
	if len(raw) < 2 {
		return
	}

	chID := channel.GetUserChannel(c.UserID)
	if chID == 0 {
		return
	}

	// Build relay frame: [1 byte type][8 bytes userID][payload]
	frame := make([]byte, 9+len(raw)-1)
	frame[0] = raw[0] // media type
	binary.BigEndian.PutUint64(frame[1:9], uint64(c.UserID))
	copy(frame[9:], raw[1:]) // payload

	// Send to all other clients in the channel
	users := channel.GetUsers(chID)
	GlobalHub.mu.RLock()
	for _, u := range users {
		if u.ID == c.UserID {
			continue
		}
		if client, ok := GlobalHub.clients[u.ID]; ok {
			select {
			case client.SendMedia <- frame:
			default:
				// drop if slow
			}
		}
	}
	GlobalHub.mu.RUnlock()
}

func handleMessage(c *Client, msg Message) {
	switch msg.Type {
	case "join_channel":
		var p struct {
			ChannelID int64 `json:"channel_id"`
		}
		json.Unmarshal(msg.Payload, &p)

		// Check access for private channels
		if !channel.CanJoin(p.ChannelID, c.UserID, c.IsAdmin) {
			errMsg, _ := json.Marshal(map[string]any{
				"type":  "error",
				"error": "access_denied",
				"text":  "You don't have access to this private channel",
			})
			GlobalHub.SendTo(c.UserID, errMsg)
			return
		}

		oldCh := channel.GetUserChannel(c.UserID)
		if oldCh > 0 {
			cleanupWebRTC(oldCh, c.UserID)
			clearPreviewIfSharer(oldCh, c.UserID)
		}
		channel.Join(p.ChannelID, c.UserID, c.Username)

		if oldCh > 0 {
			broadcastChannelUpdate(oldCh)
		}
		broadcastChannelUpdate(p.ChannelID)
		broadcastPresence()

		// Send current screen preview to the joining user if one exists
		previewMu.RLock()
		if preview, ok := channelPreviews[p.ChannelID]; ok {
			GlobalHub.SendTo(c.UserID, preview)
		}
		previewMu.RUnlock()

		// Send chat history to joining user
		if history, err := database.GetChatHistory(p.ChannelID, 50); err == nil && len(history) > 0 {
			historyMsg, _ := json.Marshal(map[string]any{
				"type":     "chat_history",
				"messages": history,
			})
			GlobalHub.SendTo(c.UserID, historyMsg)
		}

	case "leave_channel":
		chID := channel.Leave(c.UserID)
		if chID > 0 {
			cleanupWebRTC(chID, c.UserID)
			clearPreviewIfSharer(chID, c.UserID)
			broadcastChannelUpdate(chID)
		}
		broadcastPresence()

	case "mute":
		var p struct {
			Muted bool `json:"muted"`
		}
		json.Unmarshal(msg.Payload, &p)
		channel.SetMuted(c.UserID, p.Muted)
		chID := channel.GetUserChannel(c.UserID)
		if chID > 0 {
			broadcastChannelUpdate(chID)
		}

	case "speaking":
		var p struct {
			Speaking bool `json:"speaking"`
		}
		json.Unmarshal(msg.Payload, &p)
		channel.SetSpeaking(c.UserID, p.Speaking)
		chID := channel.GetUserChannel(c.UserID)
		if chID > 0 {
			broadcastChannelUpdate(chID)
		}

	case "webrtc_offer":
		var p struct {
			SDP string `json:"sdp"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			log.Printf("signaling: bad webrtc_offer from user %d: %v", c.UserID, err)
			return
		}
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		sfu := rtc.GetOrCreateSFU(chID, func(userID int64, data []byte) {
			GlobalHub.SendTo(userID, data)
		})
		if err := sfu.HandleOffer(c.UserID, c.Username, p.SDP); err != nil {
			log.Printf("signaling: webrtc offer failed for user %d: %v", c.UserID, err)
		}
		// Clear preview if the renegotiation removed the video track
		if !rtc.SDPHasVideoSending(p.SDP) {
			clearPreviewIfSharer(chID, c.UserID)
		}

	case "webrtc_answer":
		var p struct {
			SDP string `json:"sdp"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		sfu := rtc.GetOrCreateSFU(chID, func(userID int64, data []byte) {
			GlobalHub.SendTo(userID, data)
		})
		if err := sfu.HandleAnswer(c.UserID, p.SDP); err != nil {
			log.Printf("signaling: webrtc answer failed for user %d: %v", c.UserID, err)
		}

	case "camera_on":
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		sfu := rtc.GetSFU(chID)
		if sfu != nil {
			sfu.SetExpectCamera(c.UserID, true)
		}

	case "camera_off":
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		sfu := rtc.GetSFU(chID)
		if sfu != nil {
			sfu.SetExpectCamera(c.UserID, false)
		}

	case "ws_media_mode":
		c.IsWSMedia = true
		log.Printf("signaling: user %d (%s) switched to WS media transport", c.UserID, c.Username)

	case "chat_message":
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Text == "" {
			return
		}
		// Limit message length
		text := p.Text
		if len(text) > 2000 {
			text = text[:2000]
		}
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		// Generate message ID for reactions
		now := time.Now()
		msgID := fmt.Sprintf("%d-%d", c.UserID, now.UnixMilli())
		ts := now.Unix()

		// Save to database
		if err := database.SaveChatMessage(database.ChatMessage{
			ID:        msgID,
			ChannelID: chID,
			UserID:    c.UserID,
			Username:  c.Username,
			Text:      text,
			CreatedAt: ts,
		}); err != nil {
			log.Printf("signaling: failed to save chat message: %v", err)
		}

		chatMsg, _ := json.Marshal(map[string]any{
			"type":       "chat_message",
			"id":         msgID,
			"user_id":    c.UserID,
			"username":   c.Username,
			"text":       text,
			"channel_id": chID,
			"timestamp":  ts,
		})
		GlobalHub.BroadcastToChannel(chID, chatMsg)

	case "chat_reaction":
		var p struct {
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" || p.Emoji == "" {
			return
		}
		// Limit emoji length
		if len(p.Emoji) > 16 {
			return
		}
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		reactionMsg, _ := json.Marshal(map[string]any{
			"type":       "chat_reaction",
			"message_id": p.MessageID,
			"user_id":    c.UserID,
			"username":   c.Username,
			"emoji":      p.Emoji,
			"channel_id": chID,
		})
		GlobalHub.BroadcastToChannel(chID, reactionMsg)

	case "screen_preview":
		var p struct {
			Image string `json:"image"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Image == "" {
			return
		}
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		broadcastMsg, _ := json.Marshal(map[string]any{
			"type":     "screen_preview",
			"user_id":  c.UserID,
			"username": c.Username,
			"payload":  map[string]string{"image": p.Image},
		})
		previewMu.Lock()
		channelPreviews[chID] = broadcastMsg
		previewMu.Unlock()
		// Broadcast to all channel members except the sender
		users := channel.GetUsers(chID)
		GlobalHub.mu.RLock()
		for _, u := range users {
			if u.ID == c.UserID {
				continue
			}
			if client, ok := GlobalHub.clients[u.ID]; ok {
				select {
				case client.Send <- broadcastMsg:
				default:
				}
			}
		}
		GlobalHub.mu.RUnlock()

	case "ice_candidate":
		var p struct {
			Candidate json.RawMessage `json:"candidate"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			return
		}
		chID := channel.GetUserChannel(c.UserID)
		if chID == 0 {
			return
		}
		sfu := rtc.GetOrCreateSFU(chID, func(userID int64, data []byte) {
			GlobalHub.SendTo(userID, data)
		})
		if err := sfu.HandleICECandidate(c.UserID, p.Candidate); err != nil {
			log.Printf("signaling: ice candidate failed for user %d: %v", c.UserID, err)
		}
	}
}

func cleanupWebRTC(channelID int64, userID int64) {
	sfu := rtc.GetSFU(channelID)
	if sfu == nil {
		return
	}
	sfu.RemovePeer(userID)
	rtc.RemoveSFU(channelID)
}

func broadcastChannelUpdate(channelID int64) {
	users := channel.GetUsers(channelID)
	data, _ := json.Marshal(map[string]any{
		"type":       "channel_users",
		"channel_id": channelID,
		"users":      users,
	})
	GlobalHub.Broadcast(data)
}

func broadcastPresence() {
	states := channel.GetAllChannelStates()
	data, _ := json.Marshal(map[string]any{
		"type":     "presence",
		"channels": states,
	})
	GlobalHub.Broadcast(data)
}

// ClearChannelPreview removes the stored screen preview for a channel (e.g. when channel is deleted).
func ClearChannelPreview(channelID int64) {
	previewMu.Lock()
	delete(channelPreviews, channelID)
	previewMu.Unlock()
}

// clearPreviewIfSharer clears the screen preview for a channel if the given user was the sharer.
func clearPreviewIfSharer(channelID int64, userID int64) {
	previewMu.Lock()
	stored, ok := channelPreviews[channelID]
	if !ok {
		previewMu.Unlock()
		return
	}
	// Check if the stored preview belongs to this user
	var preview struct {
		UserID int64 `json:"user_id"`
	}
	if err := json.Unmarshal(stored, &preview); err != nil || preview.UserID != userID {
		previewMu.Unlock()
		return
	}
	delete(channelPreviews, channelID)
	previewMu.Unlock()

	// Broadcast clear message to channel
	clearMsg, _ := json.Marshal(map[string]any{
		"type": "screen_preview_clear",
	})
	GlobalHub.BroadcastToChannel(channelID, clearMsg)
}
