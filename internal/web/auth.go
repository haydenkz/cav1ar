package web

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A password is kept as a PBKDF2 hash: "pbkdf2-sha256$<iterations>$<salt>$<key>",
// the salt and key in unpadded base64.
const hashIterations = 600_000

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, 32)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", hashIterations, b64(salt), b64(key)), nil
}

func checkPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || err1 != nil || err2 != nil || iter < 1 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// A login is a cookie holding when it runs out and a signature over that and
// the password's hash, so changing the password ends every login.
const (
	cookieName = "cav1ar_login"
	loginFor   = 30 * 24 * time.Hour
)

func (s *Server) sign(expires int64, hash string) string {
	m := hmac.New(sha256.New, s.key)
	fmt.Fprintf(m, "%d|%s", expires, hash)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) loginCookie(hash string) *http.Cookie {
	expires := time.Now().Add(loginFor)
	return &http.Cookie{Name: cookieName, Value: strconv.FormatInt(expires.Unix(), 10) + "." + s.sign(expires.Unix(), hash),
		Path: "/", Expires: expires, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

// loggedIn reports whether r comes with a login that is still good.
func (s *Server) loggedIn(r *http.Request, hash string) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	at, sig, ok := strings.Cut(c.Value, ".")
	expires, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || time.Now().Unix() > expires {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.sign(expires, hash)))
}

// guard turns away API requests without a login, when there's a password,
// and changes sent other than as JSON, which a page on another site can't
// send without the browser asking first.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
				errorJSON(w, http.StatusUnsupportedMediaType, errors.New("send changes as JSON"))
				return
			}
		}
		hash := s.o.Queue.Settings().WebPassword
		open := r.URL.Path == "/api/auth" || r.URL.Path == "/api/login"
		if hash != "" && !open && !s.loggedIn(r, hash) {
			errorJSON(w, http.StatusUnauthorized, errors.New("log in first"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loadKey reads the key that signs logins, making one the first time.
func loadKey(path string) ([]byte, error) {
	if key, err := os.ReadFile(path); err == nil && len(key) >= 32 {
		return key, nil
	}
	key := make([]byte, 32)
	rand.Read(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, key, 0o600)
}
