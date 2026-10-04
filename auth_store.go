package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const adminDatabaseEnv = "CLINE_DATABASE_URL"

var authDB *sql.DB

type adminUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type adminUserContextKey struct{}

func currentAdmin(r *http.Request) adminUser {
	user, _ := r.Context().Value(adminUserContextKey{}).(adminUser)
	return user
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newAPIKey() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "sk-" + id.String(), nil
}

func verifyStoredPassword(hash, salt, password string) bool {
	if hash == "" || salt == "" || password == "" {
		return false
	}
	candidate := legacyAdminPasswordHash(salt, password)
	if strings.HasPrefix(hash, adminPasswordHashPrefix) {
		candidate = hashAdminPassword(salt, password)
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(hash)) == 1
}

func initAuthStore() error {
	if authDB != nil {
		return nil
	}
	url := strings.TrimSpace(os.Getenv(adminDatabaseEnv))
	if url == "" {
		return fmt.Errorf("%s is required", adminDatabaseEnv)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		cancelPing()
		db.Close()
		return fmt.Errorf("connect auth database: %w", err)
	}
	cancelPing()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS admin_users (
			id text PRIMARY KEY, email text NOT NULL UNIQUE, password_hash text NOT NULL,
			password_salt text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
			disabled_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS admin_sessions (
			token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
			created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS admin_sessions_expiry_idx ON admin_sessions(expires_at)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id text PRIMARY KEY, name text NOT NULL, token_hash text NOT NULL UNIQUE,
			preview text NOT NULL, created_by text REFERENCES admin_users(id) ON DELETE SET NULL,
			created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz,
			last_used_at timestamptz, revoked_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS app_state (
			name text PRIMARY KEY, data jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return fmt.Errorf("initialize auth database: %w", err)
		}
	}
	if err := bootstrapAuth(ctx, db); err != nil {
		db.Close()
		return err
	}
	if err := migrateLegacyState(ctx, db); err != nil {
		db.Close()
		return fmt.Errorf("migrate legacy state: %w", err)
	}
	if err := activateStateStore(db); err != nil {
		db.Close()
		return err
	}
	authDB = db
	return nil
}

func bootstrapAuth(ctx context.Context, db *sql.DB) error {
	var users int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM admin_users`).Scan(&users); err != nil {
		return err
	}
	if users == 0 {
		email := strings.ToLower(strings.TrimSpace(os.Getenv("CLINE_ADMIN_EMAIL")))
		if email == "" {
			email = "admin@local.test"
		}
		if !validAdminEmail(email) {
			return errors.New("CLINE_ADMIN_EMAIL must be a valid email address")
		}
		p := loadPool()
		poolMu.Lock()
		hash, salt := p.AdminPasswordHash, p.AdminPasswordSalt
		poolMu.Unlock()
		if hash == "" {
			password := os.Getenv(adminPasswordEnv)
			if len(password) < 12 {
				return errors.New("CLINE_ADMIN_PASSWORD must be at least 12 characters for first setup")
			}
			salt = randomHex(16)
			hash = hashAdminPassword(salt, password)
		}
		if salt == "" || hash == "" {
			return errors.New("admin password initialization failed")
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO admin_users(id,email,password_hash,password_salt) VALUES($1,$2,$3,$4)`, randomHex(16), email, hash, salt); err != nil {
			return fmt.Errorf("create initial admin: %w", err)
		}
	}
	p := loadPool()
	poolMu.Lock()
	legacy := append([]string(nil), p.Keys...)
	legacyPassword := p.AdminPasswordHash != "" || p.AdminPasswordSalt != ""
	poolMu.Unlock()
	if len(legacy) == 0 && !legacyPassword {
		return nil
	}
	if len(legacy) > 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for i, key := range legacy {
			if key == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO api_keys(id,name,token_hash,preview) VALUES($1,$2,$3,$4) ON CONFLICT(token_hash) DO NOTHING`, randomHex(16), fmt.Sprintf("Legacy key %d", i+1), tokenHash(key), maskedAPIKeyPreview(key)); err != nil {
				return fmt.Errorf("import legacy API keys: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	poolMu.Lock()
	p.Keys = []string{}
	p.AdminPasswordHash = ""
	p.AdminPasswordSalt = ""
	poolMu.Unlock()
	savePool()
	data, err := os.ReadFile(poolPath)
	var persisted struct {
		Keys              []string `json:"keys"`
		AdminPasswordHash string   `json:"adminPasswordHash"`
		AdminPasswordSalt string   `json:"adminPasswordSalt"`
	}
	if err != nil || json.Unmarshal(data, &persisted) != nil || len(persisted.Keys) != 0 || persisted.AdminPasswordHash != "" || persisted.AdminPasswordSalt != "" {
		return errors.New("legacy credentials were imported but could not be removed from the account file")
	}
	return nil
}

func validAdminEmail(email string) bool {
	parts := strings.Split(email, "@")
	return len(email) <= 254 && len(parts) == 2 && parts[0] != "" && strings.Contains(parts[1], ".") && !strings.ContainsAny(email, " \t\r\n")
}

func lookupAdminSession(r *http.Request) (adminUser, error) {
	if authDB == nil {
		return adminUser{}, errors.New("auth database unavailable")
	}
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || cookie.Value == "" {
		return adminUser{}, sql.ErrNoRows
	}
	var user adminUser
	err = authDB.QueryRowContext(r.Context(), `SELECT u.id,u.email FROM admin_sessions s JOIN admin_users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND u.disabled_at IS NULL`, tokenHash(cookie.Value)).Scan(&user.ID, &user.Email)
	return user, err
}

type apiKeyIdentity struct {
	ID      string
	Name    string
	Preview string
}

func validAPIKey(ctx context.Context, key string) (apiKeyIdentity, error) {
	if authDB == nil {
		return apiKeyIdentity{}, errors.New("auth database unavailable")
	}
	if key == "" {
		return apiKeyIdentity{}, nil
	}
	var identity apiKeyIdentity
	err := authDB.QueryRowContext(ctx, `SELECT id,name,preview FROM api_keys WHERE token_hash=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())`, tokenHash(key)).Scan(&identity.ID, &identity.Name, &identity.Preview)
	if errors.Is(err, sql.ErrNoRows) {
		return apiKeyIdentity{}, nil
	}
	if err != nil {
		return apiKeyIdentity{}, err
	}
	// Updating at most once per minute keeps last-used useful without a write on every call.
	_, _ = authDB.ExecContext(ctx, `UPDATE api_keys SET last_used_at=now() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at<now()-interval '1 minute')`, identity.ID)
	return identity, nil
}

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	select {
	case adminLoginSlots <- struct{}{}:
		defer func() { <-adminLoginSlots }()
	default:
		writeAPI(w, http.StatusTooManyRequests, apiResponse{Error: "too many login attempts"})
		return
	}
	var input struct{ Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	var user adminUser
	var hash, salt string
	err := authDB.QueryRowContext(r.Context(), `SELECT id,email,password_hash,password_salt FROM admin_users WHERE email=$1 AND disabled_at IS NULL`, email).Scan(&user.ID, &user.Email, &hash, &salt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	if err != nil || !verifyStoredPassword(hash, salt, input.Password) {
		time.Sleep(500 * time.Millisecond)
		writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "invalid email or password"})
		return
	}
	if !strings.HasPrefix(hash, adminPasswordHashPrefix) {
		newSalt := randomHex(16)
		if _, err := authDB.ExecContext(r.Context(), `UPDATE admin_users SET password_hash=$1,password_salt=$2 WHERE id=$3`, hashAdminPassword(newSalt, input.Password), newSalt, user.ID); err != nil {
			writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
			return
		}
	}
	token := randomHex(32)
	if token == "" {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "session creation failed"})
		return
	}
	if _, err := authDB.ExecContext(r.Context(), `INSERT INTO admin_sessions(token_hash,user_id,expires_at) VALUES($1,$2,$3)`, tokenHash(token), user.ID, time.Now().Add(adminSessionTTL)); err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Value: token, Path: "/admin", HttpOnly: true, Secure: r.TLS != nil || os.Getenv("CLINE_ADMIN_SECURE_COOKIE") == "true", SameSite: http.SameSiteStrictMode, MaxAge: int(adminSessionTTL.Seconds())})
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: user})
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	if cookie, err := r.Cookie(adminSessionCookie); err == nil && authDB != nil {
		_, _ = authDB.ExecContext(r.Context(), `DELETE FROM admin_sessions WHERE token_hash=$1`, tokenHash(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

func handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: currentAdmin(r)})
}

func handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	var input struct{ CurrentPassword, NewPassword string }
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	if len(input.NewPassword) < 12 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "new password must be at least 12 characters"})
		return
	}
	user := currentAdmin(r)
	var hash, salt string
	if err := authDB.QueryRowContext(r.Context(), `SELECT password_hash,password_salt FROM admin_users WHERE id=$1`, user.ID).Scan(&hash, &salt); err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	if !verifyStoredPassword(hash, salt, input.CurrentPassword) {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "current password is incorrect"})
		return
	}
	newSalt := randomHex(16)
	tx, err := authDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `UPDATE admin_users SET password_hash=$1,password_salt=$2 WHERE id=$3`, hashAdminPassword(newSalt, input.NewPassword), newSalt, user.ID); err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM admin_sessions WHERE user_id=$1 AND token_hash<>$2`, user.ID, tokenHash(sessionCookieValue(r)))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

func sessionCookieValue(r *http.Request) string {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

type apiKeyRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Preview    string     `json:"preview"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

func handleManagedKeysList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	rows, err := authDB.QueryContext(r.Context(), `SELECT id,name,preview,created_at,expires_at,last_used_at,revoked_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	defer rows.Close()
	keys := []apiKeyRecord{}
	for rows.Next() {
		var key apiKeyRecord
		var expires, lastUsed, revoked sql.NullTime
		if err := rows.Scan(&key.ID, &key.Name, &key.Preview, &key.CreatedAt, &expires, &lastUsed, &revoked); err != nil {
			writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
			return
		}
		if expires.Valid {
			key.ExpiresAt = &expires.Time
		}
		if lastUsed.Valid {
			key.LastUsedAt = &lastUsed.Time
		}
		if revoked.Valid {
			key.RevokedAt = &revoked.Time
		}
		keys = append(keys, key)
	}
	if rows.Err() != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": keys}})
}

func handleManagedKeyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	var input struct{ Name, ExpiresAt string }
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 80 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "key name must be 1-80 characters"})
		return
	}
	var expires *time.Time
	if input.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, input.ExpiresAt)
		if err != nil || !parsed.After(time.Now()) {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "expiresAt must be a future ISO timestamp"})
			return
		}
		expires = &parsed
	}
	key, err := newAPIKey()
	if err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "key generation failed"})
		return
	}
	record := apiKeyRecord{ID: randomHex(16), Name: name, Preview: maskedAPIKeyPreview(key), CreatedAt: time.Now().UTC(), ExpiresAt: expires}
	_, err = authDB.ExecContext(r.Context(), `INSERT INTO api_keys(id,name,token_hash,preview,created_by,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, record.ID, record.Name, tokenHash(key), record.Preview, currentAdmin(r).ID, record.CreatedAt, record.ExpiresAt)
	if err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "key creation failed"})
		return
	}
	writeAPI(w, http.StatusCreated, apiResponse{Success: true, Data: map[string]any{"key": key, "record": record}})
}

func handleManagedKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	var input struct{ ID string }
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	result, err := authDB.ExecContext(r.Context(), `UPDATE api_keys SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, input.ID)
	if err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "API key not found or already revoked"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}

type adminUserRecord struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	CreatedAt  time.Time  `json:"createdAt"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
}

func handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := authDB.QueryContext(r.Context(), `SELECT id,email,created_at,disabled_at FROM admin_users ORDER BY created_at`)
		if err != nil {
			writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
			return
		}
		defer rows.Close()
		users := []adminUserRecord{}
		for rows.Next() {
			var user adminUserRecord
			var disabled sql.NullTime
			if err := rows.Scan(&user.ID, &user.Email, &user.CreatedAt, &disabled); err != nil {
				writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
				return
			}
			if disabled.Valid {
				user.DisabledAt = &disabled.Time
			}
			users = append(users, user)
		}
		if rows.Err() != nil {
			writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
			return
		}
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"users": users}})
	case http.MethodPost:
		var input struct{ Email, Password string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
			return
		}
		email := strings.ToLower(strings.TrimSpace(input.Email))
		if !validAdminEmail(email) || len(input.Password) < 12 {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "valid email and password of at least 12 characters required"})
			return
		}
		var existing string
		err := authDB.QueryRowContext(r.Context(), `SELECT id FROM admin_users WHERE email=$1`, email).Scan(&existing)
		if err == nil {
			writeAPI(w, http.StatusConflict, apiResponse{Error: "email already exists"})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
			return
		}
		salt := randomHex(16)
		user := adminUser{ID: randomHex(16), Email: email}
		if _, err := authDB.ExecContext(r.Context(), `INSERT INTO admin_users(id,email,password_hash,password_salt) VALUES($1,$2,$3,$4)`, user.ID, user.Email, hashAdminPassword(salt, input.Password), salt); err != nil {
			writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "user creation failed"})
			return
		}
		writeAPI(w, http.StatusCreated, apiResponse{Success: true, Data: user})
	default:
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
	}
}

func handleAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: tAPI(r, "method_not_allowed")})
		return
	}
	var input struct {
		ID       string
		Disabled bool
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: tAPI(r, "invalid_json")})
		return
	}
	if input.Disabled && input.ID == currentAdmin(r).ID {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "cannot disable your own account"})
		return
	}
	tx, err := authDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `UPDATE admin_users SET disabled_at=CASE WHEN $2 THEN now() ELSE NULL END WHERE id=$1`, input.ID, input.Disabled)
	if err == nil && input.Disabled {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM admin_sessions WHERE user_id=$1`, input.ID)
	}
	if err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "admin user not found"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeAPI(w, http.StatusServiceUnavailable, apiResponse{Error: "auth database unavailable"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true})
}
