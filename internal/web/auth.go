// Package web provides the HTTP web interface for DarkWeb Tracker.
// Authentication is handled via the Telegram Login Widget.
// Access control verifies that the authenticated user is a member
// of the configured Telegram channel.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TelegramUser contains the profile returned by the Telegram Login Widget.
type TelegramUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
	PhotoURL  string `json:"photo_url"`
	AuthDate  int64  `json:"auth_date"`
}

// VerifyTelegramAuth validates the Telegram Login Widget callback data.
//
// The verification algorithm (from Telegram docs):
//  1. Remove the "hash" field from data.
//  2. Sort remaining fields alphabetically: key=value pairs joined with "\n".
//  3. Compute HMAC-SHA256(data_check_string, SHA256(bot_token)).
//  4. Compare hex(HMAC) with the received hash.
//  5. Reject if auth_date is older than 24 hours.
func VerifyTelegramAuth(params url.Values, botToken string) (TelegramUser, error) {
	receivedHash := params.Get("hash")
	if receivedHash == "" {
		return TelegramUser{}, fmt.Errorf("missing hash")
	}

	// Build the data-check string.
	var pairs []string
	for k, vs := range params {
		if k == "hash" {
			continue
		}
		pairs = append(pairs, k+"="+vs[0])
	}
	sort.Strings(pairs)
	dataCheckStr := strings.Join(pairs, "\n")

	// secret = SHA256(bot_token) — NOT hex-encoded, raw bytes.
	h := sha256.New()
	h.Write([]byte(botToken))
	secret := h.Sum(nil)

	// HMAC-SHA256(data_check_string, secret).
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(dataCheckStr))
	expectedHash := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expectedHash), []byte(receivedHash)) {
		return TelegramUser{}, fmt.Errorf("invalid hash")
	}

	// Check freshness (within 24 hours).
	authDate, err := strconv.ParseInt(params.Get("auth_date"), 10, 64)
	if err != nil {
		return TelegramUser{}, fmt.Errorf("invalid auth_date: %w", err)
	}
	if time.Now().Unix()-authDate > 86400 {
		return TelegramUser{}, fmt.Errorf("auth data expired")
	}

	u := TelegramUser{
		ID:        authDate, // will be overwritten below
		AuthDate:  authDate,
		FirstName: params.Get("first_name"),
		LastName:  params.Get("last_name"),
		Username:  params.Get("username"),
		PhotoURL:  params.Get("photo_url"),
	}
	if id, err := strconv.ParseInt(params.Get("id"), 10, 64); err == nil {
		u.ID = id
	}

	return u, nil
}

// memberStatus values from the Telegram getChatMember API.
var memberStatuses = map[string]bool{
	"creator":       true,
	"administrator": true,
	"member":        true,
	// "restricted" members can still view channel content — treat as member.
	"restricted": true,
}

// CheckChannelMembership calls the Telegram Bot API to check whether
// userID is a member of channelID. channelID may be "@username" or "-100xxx".
func CheckChannelMembership(ctx context.Context, client *http.Client, botToken, channelID string, userID int64) (bool, error) {
	apiURL := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getChatMember?chat_id=%s&user_id=%d",
		botToken,
		url.QueryEscape(channelID),
		userID,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("getChatMember: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decode response: %w", err)
	}
	if !result.OK {
		return false, fmt.Errorf("telegram api error: %s", result.Description)
	}

	return memberStatuses[result.Result.Status], nil
}

// ---------------------------------------------------------------------------
// Session management — HMAC-signed JSON cookies (no external deps).
// ---------------------------------------------------------------------------

// sessionPayload is the signed cookie content.
type sessionPayload struct {
	UserID    int64  `json:"uid"`
	Username  string `json:"un"`
	FirstName string `json:"fn"`
	PhotoURL  string `json:"ph"`
	ExpiresAt int64  `json:"exp"` // unix timestamp
}

const sessionCookieName = "dwt_session"
const sessionDuration = 24 * time.Hour

// CreateSession serialises a session payload and signs it with HMAC-SHA256.
// Returns a cookie value of the form: base64payload.base64sig
func CreateSession(user TelegramUser, secret string) string {
	payload := sessionPayload{
		UserID:    user.ID,
		Username:  user.Username,
		FirstName: user.FirstName,
		PhotoURL:  user.PhotoURL,
		ExpiresAt: time.Now().Add(sessionDuration).Unix(),
	}
	data, _ := json.Marshal(payload)
	b64data := base64.RawURLEncoding.EncodeToString(data)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(b64data))
	b64sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return b64data + "." + b64sig
}

// ValidateSession parses and verifies a cookie value.
// Returns the payload on success, or an error if invalid/expired.
func ValidateSession(cookie, secret string) (sessionPayload, error) {
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return sessionPayload{}, fmt.Errorf("malformed session")
	}
	b64data, b64sig := parts[0], parts[1]

	// Verify signature.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(b64data))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expectedSig), []byte(b64sig)) {
		return sessionPayload{}, fmt.Errorf("invalid signature")
	}

	// Decode payload.
	data, err := base64.RawURLEncoding.DecodeString(b64data)
	if err != nil {
		return sessionPayload{}, fmt.Errorf("decode payload: %w", err)
	}
	var p sessionPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return sessionPayload{}, fmt.Errorf("unmarshal payload: %w", err)
	}
	if time.Now().Unix() > p.ExpiresAt {
		return sessionPayload{}, fmt.Errorf("session expired")
	}

	return p, nil
}

// RandomSecret generates a 32-byte cryptographically random secret.
func RandomSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
