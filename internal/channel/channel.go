package channel

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/foxzi/vocala/internal/database"
)

var validChannelName = regexp.MustCompile(`^[a-zA-Z0-9_\-. ]{1,50}$`)

type Channel struct {
	ID        int64
	Name      string
	CreatedBy int64
	IsPrivate bool
}

type ConnectedUser struct {
	ID       int64
	Username string
	Muted    bool
	Speaking bool
}

// In-memory state for who's in which channel
var (
	mu            sync.RWMutex
	channelUsers  = make(map[int64]map[int64]*ConnectedUser) // channelID -> userID -> user
	userToChannel = make(map[int64]int64)                    // userID -> channelID
)

func ValidateName(name string) error {
	if !validChannelName.MatchString(name) {
		return errors.New("channel name must be 1-50 chars: letters, digits, _ - . or spaces")
	}
	return nil
}

func Create(name string, createdBy int64, isPrivate bool) (*Channel, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	res, err := database.DB.Exec(
		"INSERT INTO channels (name, created_by, is_private) VALUES (?, ?, ?)",
		name, createdBy, isPrivate,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	ch := &Channel{ID: id, Name: name, CreatedBy: createdBy, IsPrivate: isPrivate}

	// Creator is automatically a member of private channels
	if isPrivate {
		AddMember(id, createdBy)
	}

	return ch, nil
}

func List() ([]Channel, error) {
	rows, err := database.DB.Query("SELECT id, name, created_by, is_private FROM channels ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []Channel
	for rows.Next() {
		var ch Channel
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.CreatedBy, &ch.IsPrivate); err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	return channels, nil
}

func GetByID(id int64) (*Channel, error) {
	var ch Channel
	err := database.DB.QueryRow("SELECT id, name, created_by, is_private FROM channels WHERE id = ?", id).
		Scan(&ch.ID, &ch.Name, &ch.CreatedBy, &ch.IsPrivate)
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

func Delete(id int64) error {
	_, err := database.DB.Exec("DELETE FROM channels WHERE id = ?", id)
	return err
}

func Join(channelID int64, userID int64, username string) {
	mu.Lock()
	defer mu.Unlock()

	// Leave current channel first
	if oldCh, ok := userToChannel[userID]; ok {
		if users, exists := channelUsers[oldCh]; exists {
			delete(users, userID)
		}
	}

	if channelUsers[channelID] == nil {
		channelUsers[channelID] = make(map[int64]*ConnectedUser)
	}
	channelUsers[channelID][userID] = &ConnectedUser{
		ID:       userID,
		Username: username,
	}
	userToChannel[userID] = channelID
}

func Leave(userID int64) int64 {
	mu.Lock()
	defer mu.Unlock()

	chID, ok := userToChannel[userID]
	if !ok {
		return 0
	}

	if users, exists := channelUsers[chID]; exists {
		delete(users, userID)
	}
	delete(userToChannel, userID)
	return chID
}

func GetUsers(channelID int64) []*ConnectedUser {
	mu.RLock()
	defer mu.RUnlock()

	users := channelUsers[channelID]
	result := make([]*ConnectedUser, 0, len(users))
	for _, u := range users {
		result = append(result, u)
	}
	return result
}

func GetUserChannel(userID int64) int64 {
	mu.RLock()
	defer mu.RUnlock()
	return userToChannel[userID]
}

func SetMuted(userID int64, muted bool) {
	mu.Lock()
	defer mu.Unlock()

	chID, ok := userToChannel[userID]
	if !ok {
		return
	}
	if u, exists := channelUsers[chID][userID]; exists {
		u.Muted = muted
	}
}

func SetSpeaking(userID int64, speaking bool) {
	mu.Lock()
	defer mu.Unlock()

	chID, ok := userToChannel[userID]
	if !ok {
		return
	}
	if u, exists := channelUsers[chID][userID]; exists {
		u.Speaking = speaking
	}
}

func GetAllChannelStates() map[int64][]*ConnectedUser {
	mu.RLock()
	defer mu.RUnlock()

	result := make(map[int64][]*ConnectedUser)
	for chID, users := range channelUsers {
		list := make([]*ConnectedUser, 0, len(users))
		for _, u := range users {
			list = append(list, u)
		}
		result[chID] = list
	}
	return result
}

// --- Private channel membership ---

func AddMember(channelID, userID int64) error {
	_, err := database.DB.Exec(
		"INSERT OR IGNORE INTO channel_members (channel_id, user_id) VALUES (?, ?)",
		channelID, userID,
	)
	return err
}

func RemoveMember(channelID, userID int64) error {
	_, err := database.DB.Exec(
		"DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?",
		channelID, userID,
	)
	return err
}

func IsMember(channelID, userID int64) bool {
	var count int
	database.DB.QueryRow(
		"SELECT COUNT(*) FROM channel_members WHERE channel_id = ? AND user_id = ?",
		channelID, userID,
	).Scan(&count)
	return count > 0
}

func GetMembers(channelID int64) ([]int64, error) {
	rows, err := database.DB.Query(
		"SELECT user_id FROM channel_members WHERE channel_id = ?", channelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		members = append(members, id)
	}
	return members, nil
}

type Member struct {
	UserID   int64
	Username string
}

func GetMembersWithNames(channelID int64) ([]Member, error) {
	rows, err := database.DB.Query(
		`SELECT cm.user_id, u.username FROM channel_members cm
		 JOIN users u ON u.id = cm.user_id
		 WHERE cm.channel_id = ? ORDER BY u.username`,
		channelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username); err != nil {
			continue
		}
		members = append(members, m)
	}
	return members, nil
}

// CanJoin checks if a user has access to join a channel.
// Public channels: anyone can join.
// Private channels: members, creator, or admins.
func CanJoin(channelID, userID int64, isAdmin bool) bool {
	ch, err := GetByID(channelID)
	if err != nil {
		return false
	}
	if !ch.IsPrivate {
		return true
	}
	if ch.CreatedBy == userID || isAdmin {
		return true
	}
	return IsMember(channelID, userID)
}

// CanManage checks if a user can manage members of a private channel.
func CanManage(channelID, userID int64, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	ch, err := GetByID(channelID)
	if err != nil {
		return false
	}
	return ch.CreatedBy == userID
}

// --- Invite links ---

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateInvite generates a 7-day invite link token for a private channel.
func CreateInvite(channelID, createdBy int64) (string, error) {
	token := generateToken()
	now := time.Now().Unix()
	expires := time.Now().Add(7 * 24 * time.Hour).Unix()
	_, err := database.DB.Exec(
		`INSERT INTO channel_invites (token, channel_id, created_by, created_at, expires_at, max_uses, uses)
		 VALUES (?, ?, ?, ?, ?, 0, 0)`,
		token, channelID, createdBy, now, expires,
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// AcceptInvite validates and uses an invite token, adding the user as a member.
func AcceptInvite(token string, userID int64) (int64, error) {
	var channelID int64
	var expiresAt int64
	var maxUses, uses int
	err := database.DB.QueryRow(
		`SELECT channel_id, expires_at, max_uses, uses FROM channel_invites WHERE token = ?`,
		token,
	).Scan(&channelID, &expiresAt, &maxUses, &uses)
	if err != nil {
		return 0, fmt.Errorf("invite not found")
	}
	if time.Now().Unix() > expiresAt {
		return 0, fmt.Errorf("invite expired")
	}
	if maxUses > 0 && uses >= maxUses {
		return 0, fmt.Errorf("invite max uses reached")
	}

	// Add as member
	if err := AddMember(channelID, userID); err != nil {
		return 0, err
	}

	// Increment uses
	database.DB.Exec(`UPDATE channel_invites SET uses = uses + 1 WHERE token = ?`, token)

	return channelID, nil
}

// GetInvites returns active invites for a channel.
func GetInvites(channelID int64) ([]map[string]any, error) {
	now := time.Now().Unix()
	rows, err := database.DB.Query(
		`SELECT token, created_at, expires_at, max_uses, uses FROM channel_invites
		 WHERE channel_id = ? AND expires_at > ? ORDER BY created_at DESC`,
		channelID, now,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var invites []map[string]any
	for rows.Next() {
		var token string
		var createdAt, expiresAt int64
		var maxUses, uses int
		if err := rows.Scan(&token, &createdAt, &expiresAt, &maxUses, &uses); err != nil {
			continue
		}
		invites = append(invites, map[string]any{
			"token":      token,
			"created_at": createdAt,
			"expires_at": expiresAt,
			"max_uses":   maxUses,
			"uses":       uses,
		})
	}
	return invites, nil
}

func DeleteInvite(token string) error {
	_, err := database.DB.Exec(`DELETE FROM channel_invites WHERE token = ?`, token)
	return err
}
