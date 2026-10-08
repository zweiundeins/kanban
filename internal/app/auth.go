package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"kanban/internal/db"
)

// SessionTTL is how long a login lasts.
const SessionTTL = 30 * 24 * time.Hour

// argon2id parameters (OWASP's minimum: 19 MiB, 2 passes).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword returns a PHC-style argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// CheckPassword compares pw with a hash from HashPassword.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	key, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(key)))
	return subtle.ConstantTimeCompare(got, key) == 1
}

// A dummy hash to spend the same time on unknown emails.
var dummyHash, _ = HashPassword("dummy password for timing")

// ErrBadLogin is a wrong email or password; which one is not told.
var ErrBadLogin = errors.New("wrong email or password")

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Login checks the credentials and starts a session; the token goes into
// the cookie.
func (a *App) Login(ctx context.Context, email, pw string) (token string, user User, err error) {
	var hash string
	err = a.DB.QueryRow(ctx, `SELECT id, email, name, color, password_hash FROM users WHERE email = ?`, strings.TrimSpace(email)).
		Scan(&user.ID, &user.Email, &user.Name, &user.Color, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		CheckPassword(dummyHash, pw)
		return "", User{}, ErrBadLogin
	} else if err != nil {
		return "", User{}, err
	}
	if !CheckPassword(hash, pw) {
		return "", User{}, ErrBadLogin
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", User{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	err = a.DB.W.Do(ctx, func(tx *db.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix()); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
			tokenHash(token), user.ID, now.Unix(), now.Add(SessionTTL).Unix())
		return err
	})
	return token, user, err
}

// Session returns the user a token belongs to.
func (a *App) Session(ctx context.Context, token string) (User, error) {
	var u User
	if token == "" {
		return u, ErrNotFound
	}
	err := a.DB.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.color FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash(token), time.Now().Unix()).
		Scan(&u.ID, &u.Email, &u.Name, &u.Color)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// Logout ends a session.
func (a *App) Logout(ctx context.Context, token string) error {
	return a.DB.W.Do(ctx, func(tx *db.Tx) error {
		_, err := tx.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash(token))
		return err
	})
}
