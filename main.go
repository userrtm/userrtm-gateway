package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const (
	dataDir    = "/etc/userrtm-gateway"
	dbPath     = dataDir + "/userrtm-gateway.db"
	backupDir  = dataDir + "/backups"
	configPath = "/etc/nginx/conf.d/userrtm-gateway.conf"
	appDir     = "/opt/userrtm-gateway"
)

type App struct {
	db       *sql.DB
	sessions map[string]time.Time
	mu       sync.Mutex
}

type Backend struct {
	ID        int       `json:"id,omitempty"`
	Name      string    `json:"name"`
	Country   string    `json:"country"`
	IP        string    `json:"ip"`
	Port      int       `json:"port"`
	Path      string    `json:"path"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type Settings struct {
	Mode   string
	Domain string
	Email  string
}

type PageData struct {
	Title    string
	Active   string
	Message  string
	Error    string
	Total    int
	Enabled  int
	Nginx    bool
	Backends []Backend
	Backend  Backend
	Settings Settings
	Config   string
}

func main() {
	if err := os.MkdirAll(backupDir, 0750); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app := &App{db: db, sessions: map[string]time.Time{}}
	if err := app.migrate(); err != nil {
		log.Fatal(err)
	}
	if err := app.ensureAdmin(); err != nil {
		log.Fatal(err)
	}

	r := mux.NewRouter()
	r.HandleFunc("/login", app.loginPage).Methods("GET")
	r.HandleFunc("/login", app.login).Methods("POST")
	r.HandleFunc("/logout", app.logout).Methods("POST")
	r.HandleFunc("/", app.auth(app.dashboard)).Methods("GET")
	r.HandleFunc("/backends", app.auth(app.backendsPage)).Methods("GET")
	r.HandleFunc("/backends", app.auth(app.createBackend)).Methods("POST")
	r.HandleFunc("/backends/{id:[0-9]+}/edit", app.auth(app.editBackendPage)).Methods("GET")
	r.HandleFunc("/backends/{id:[0-9]+}/edit", app.auth(app.updateBackend)).Methods("POST")
	r.HandleFunc("/backends/{id:[0-9]+}/toggle", app.auth(app.toggleBackend)).Methods("POST")
	r.HandleFunc("/backends/{id:[0-9]+}/delete", app.auth(app.deleteBackend)).Methods("POST")
	r.HandleFunc("/bulk", app.auth(app.bulkPage)).Methods("GET")
	r.HandleFunc("/bulk/import", app.auth(app.bulkImport)).Methods("POST")
	r.HandleFunc("/bulk/export", app.auth(app.bulkExport)).Methods("GET")
	r.HandleFunc("/settings", app.auth(app.settingsPage)).Methods("GET")
	r.HandleFunc("/settings", app.auth(app.saveSettings)).Methods("POST")
	r.HandleFunc("/nginx/preview", app.auth(app.previewNginx)).Methods("GET")
	r.HandleFunc("/nginx/apply", app.auth(app.applyNginx)).Methods("POST")
	r.HandleFunc("/nginx/rollback", app.auth(app.rollbackNginx)).Methods("POST")
	r.HandleFunc("/health", app.health).Methods("GET")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir(appDir+"/static"))))

	addr := getenv("USERRTM_LISTEN", ":3389")
	log.Printf("UserrTM Gateway listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, securityHeaders(r)))
}

func (a *App) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS backends (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			country TEXT NOT NULL,
			ip TEXT NOT NULL,
			port INTEGER NOT NULL,
			path TEXT UNIQUE NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, q := range queries {
		if _, err := a.db.Exec(q); err != nil {
			return err
		}
	}
	initialDomain := strings.TrimSpace(os.Getenv("USERRTM_DOMAIN"))
	initialEmail := strings.TrimSpace(os.Getenv("USERRTM_EMAIL"))
	initialMode := "ip"
	if initialDomain != "" {
		initialMode = "domain"
	}
	_, _ = a.db.Exec(`INSERT INTO settings(key,value) VALUES('mode',?) ON CONFLICT(key) DO NOTHING`, initialMode)
	_, _ = a.db.Exec(`INSERT INTO settings(key,value) VALUES('domain',?) ON CONFLICT(key) DO NOTHING`, initialDomain)
	_, _ = a.db.Exec(`INSERT INTO settings(key,value) VALUES('email',?) ON CONFLICT(key) DO NOTHING`, initialEmail)
	return nil
}

func (a *App) ensureAdmin() error {
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	user := getenv("USERRTM_ADMIN_USER", "obi")
	pass := getenv("USERRTM_ADMIN_PASS", "vps123@vpsss")
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`INSERT INTO users(username,password_hash) VALUES(?,?)`, user, string(hash))
	return err
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) render(w http.ResponseWriter, page string, data PageData) {
	t, err := template.ParseFiles(
		appDir+"/templates/base.html",
		appDir+"/templates/"+page,
	)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (a *App) renderLogin(w http.ResponseWriter, data PageData) {
	t, err := template.ParseFiles(appDir + "/templates/login.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := t.Execute(w, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	a.renderLogin(w, PageData{Title: "Login"})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	var hash string
	err := a.db.QueryRow(`SELECT password_hash FROM users WHERE username=?`, r.FormValue("username")).Scan(&hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))) != nil {
		a.renderLogin(w, PageData{Title: "Login", Error: "Invalid username or password"})
		return
	}
	token := randomToken(32)
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(12 * time.Hour)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "userrtm_gateway_session", Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 43200,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("userrtm_gateway_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "userrtm_gateway_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("userrtm_gateway_session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		a.mu.Lock()
		exp, ok := a.sessions[c.Value]
		if ok && time.Now().After(exp) {
			delete(a.sessions, c.Value)
			ok = false
		}
		a.mu.Unlock()
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	items, _ := a.listBackends()
	enabled := 0
	for _, b := range items {
		if b.Enabled {
			enabled++
		}
	}
	a.render(w, "dashboard.html", PageData{
		Title: "Dashboard", Active: "dashboard",
		Total: len(items), Enabled: enabled, Nginx: serviceActive("nginx"),
	})
}

func (a *App) backendsPage(w http.ResponseWriter, r *http.Request) {
	items, err := a.listBackends()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.render(w, "backends.html", PageData{
		Title: "Backends", Active: "backends", Backends: items,
		Message: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"),
	})
}

func (a *App) listBackends() ([]Backend, error) {
	rows, err := a.db.Query(`SELECT id,name,country,ip,port,path,enabled,created_at FROM backends ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backend
	for rows.Next() {
		var b Backend
		var enabled int
		if err := rows.Scan(&b.ID, &b.Name, &b.Country, &b.IP, &b.Port, &b.Path, &enabled, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Enabled = enabled == 1
		out = append(out, b)
	}
	return out, rows.Err()
}

func validateBackend(b Backend) error {
	b.Name = strings.TrimSpace(b.Name)
	b.Country = strings.TrimSpace(b.Country)
	b.IP = strings.TrimSpace(b.IP)
	b.Path = strings.TrimSpace(b.Path)
	if b.Name == "" {
		return errors.New("name is required")
	}
	if b.Country == "" {
		return errors.New("country is required")
	}
	if net.ParseIP(b.IP) == nil {
		return errors.New("invalid backend IP")
	}
	if b.Port < 1 || b.Port > 65535 {
		return errors.New("invalid port")
	}
	if !strings.HasPrefix(b.Path, "/") || len(b.Path) < 3 {
		return errors.New("path must start with /")
	}
	if strings.ContainsAny(b.Path, " \t\r\n{};\\") {
		return errors.New("path contains invalid characters")
	}
	return nil
}

func backendFromForm(r *http.Request) Backend {
	port, _ := strconv.Atoi(r.FormValue("port"))
	return Backend{
		Name:    strings.TrimSpace(r.FormValue("name")),
		Country: strings.TrimSpace(r.FormValue("country")),
		IP:      strings.TrimSpace(r.FormValue("ip")),
		Port:    port,
		Path:    strings.TrimSpace(r.FormValue("path")),
		Enabled: r.FormValue("enabled") == "on",
	}
}

func (a *App) createBackend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	b := backendFromForm(r)
	b.Enabled = true
	if err := validateBackend(b); err != nil {
		redirect(w, r, "/backends", "", err.Error())
		return
	}
	_, err := a.db.Exec(`INSERT INTO backends(name,country,ip,port,path,enabled) VALUES(?,?,?,?,?,1)`,
		b.Name, b.Country, b.IP, b.Port, b.Path)
	if err != nil {
		redirect(w, r, "/backends", "", err.Error())
		return
	}
	redirect(w, r, "/backends", "Backend added", "")
}

func (a *App) editBackendPage(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var b Backend
	var enabled int
	err := a.db.QueryRow(`SELECT id,name,country,ip,port,path,enabled,created_at FROM backends WHERE id=?`, id).
		Scan(&b.ID, &b.Name, &b.Country, &b.IP, &b.Port, &b.Path, &enabled, &b.CreatedAt)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b.Enabled = enabled == 1
	a.render(w, "edit_backend.html", PageData{Title: "Edit Backend", Active: "backends", Backend: b})
}

func (a *App) updateBackend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	id := mux.Vars(r)["id"]
	b := backendFromForm(r)
	if err := validateBackend(b); err != nil {
		redirect(w, r, "/backends", "", err.Error())
		return
	}
	enabled := 0
	if b.Enabled {
		enabled = 1
	}
	_, err := a.db.Exec(`UPDATE backends SET name=?,country=?,ip=?,port=?,path=?,enabled=? WHERE id=?`,
		b.Name, b.Country, b.IP, b.Port, b.Path, enabled, id)
	if err != nil {
		redirect(w, r, "/backends", "", err.Error())
		return
	}
	redirect(w, r, "/backends", "Backend updated", "")
}

func (a *App) toggleBackend(w http.ResponseWriter, r *http.Request) {
	_, _ = a.db.Exec(`UPDATE backends SET enabled=CASE enabled WHEN 1 THEN 0 ELSE 1 END WHERE id=?`, mux.Vars(r)["id"])
	http.Redirect(w, r, "/backends", http.StatusSeeOther)
}

func (a *App) deleteBackend(w http.ResponseWriter, r *http.Request) {
	_, _ = a.db.Exec(`DELETE FROM backends WHERE id=?`, mux.Vars(r)["id"])
	redirect(w, r, "/backends", "Backend deleted", "")
}

func (a *App) bulkPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, "bulk.html", PageData{
		Title: "Bulk Import / Export", Active: "bulk",
		Message: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"),
	})
}

func (a *App) bulkImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		redirect(w, r, "/bulk", "", "invalid upload")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		redirect(w, r, "/bulk", "", "file required")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 10<<20))
	if err != nil {
		redirect(w, r, "/bulk", "", err.Error())
		return
	}
	var items []Backend
	if err := json.Unmarshal(raw, &items); err != nil {
		redirect(w, r, "/bulk", "", "invalid JSON: "+err.Error())
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer tx.Rollback()
	for i, b := range items {
		if err := validateBackend(b); err != nil {
			redirect(w, r, "/bulk", "", fmt.Sprintf("row %d: %v", i+1, err))
			return
		}
		enabled := 0
		if b.Enabled {
			enabled = 1
		}
		if _, err := tx.Exec(`INSERT INTO backends(name,country,ip,port,path,enabled) VALUES(?,?,?,?,?,?)`,
			b.Name, b.Country, b.IP, b.Port, b.Path, enabled); err != nil {
			redirect(w, r, "/bulk", "", fmt.Sprintf("row %d: %v", i+1, err))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	redirect(w, r, "/bulk", "Import completed", "")
}

func (a *App) bulkExport(w http.ResponseWriter, r *http.Request) {
	items, err := a.listBackends()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="userrtm-gateway-backends.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(items)
}

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request) {
	s, _ := a.getSettings()
	a.render(w, "settings.html", PageData{
		Title: "Settings", Active: "settings", Settings: s,
		Message: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"),
	})
}

func (a *App) getSettings() (Settings, error) {
	s := Settings{Mode: "ip"}
	rows, err := a.db.Query(`SELECT key,value FROM settings`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s, err
		}
		switch k {
		case "mode":
			s.Mode = v
		case "domain":
			s.Domain = v
		case "email":
			s.Email = v
		}
	}
	return s, rows.Err()
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	mode := r.FormValue("mode")
	if mode != "ip" && mode != "domain" {
		mode = "ip"
	}
	values := map[string]string{
		"mode":   mode,
		"domain": strings.TrimSpace(r.FormValue("domain")),
		"email":  strings.TrimSpace(r.FormValue("email")),
	}
	for k, v := range values {
		_, _ = a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	}
	redirect(w, r, "/settings", "Settings saved", "")
}

func (a *App) previewNginx(w http.ResponseWriter, r *http.Request) {
	s, _ := a.getSettings()
	items, _ := a.listBackends()
	cfg, err := buildNginxConfig(s, items)
	if err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	a.render(w, "preview.html", PageData{
		Title: "Nginx Preview", Active: "settings", Config: cfg,
	})
}

func buildNginxConfig(s Settings, items []Backend) (string, error) {
	if s.Mode == "domain" && s.Domain == "" {
		return "", errors.New("domain mode requires a domain")
	}
	var b strings.Builder
	b.WriteString(`map $http_upgrade $connection_upgrade {
    default upgrade;
    '' close;
}

`)
	if s.Mode == "ip" {
		b.WriteString(`server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name _;

`)
		writeLocations(&b, items)
		b.WriteString("}\n")
		return b.String(), nil
	}

	b.WriteString("server {\n")
	b.WriteString("    listen 80;\n    listen [::]:80;\n")
	fmt.Fprintf(&b, "    server_name %s;\n\n", s.Domain)
	b.WriteString(`    location /.well-known/acme-challenge/ {
        root /var/www/certbot;
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
`)
	fmt.Fprintf(&b, "    server_name %s;\n\n", s.Domain)
	fmt.Fprintf(&b, "    ssl_certificate /etc/letsencrypt/live/%s/fullchain.pem;\n", s.Domain)
	fmt.Fprintf(&b, "    ssl_certificate_key /etc/letsencrypt/live/%s/privkey.pem;\n", s.Domain)
	b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n\n")
	writeLocations(&b, items)
	b.WriteString("}\n")
	return b.String(), nil
}

func writeLocations(b *strings.Builder, items []Backend) {
	for _, x := range items {
		if !x.Enabled {
			continue
		}
		fmt.Fprintf(b, `    location = %s {
        proxy_pass http://%s:%d;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_connect_timeout 10s;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
        proxy_buffering off;
    }

`, x.Path, x.IP, x.Port)
	}
	b.WriteString(`    location / {
        return 404;
    }
`)
}

func (a *App) applyNginx(w http.ResponseWriter, r *http.Request) {
	s, _ := a.getSettings()
	items, _ := a.listBackends()
	cfg, err := buildNginxConfig(s, items)
	if err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	if s.Mode == "domain" {
		cert := "/etc/letsencrypt/live/" + s.Domain + "/fullchain.pem"
		key := "/etc/letsencrypt/live/" + s.Domain + "/privkey.pem"
		if _, err := os.Stat(cert); err != nil {
			redirect(w, r, "/settings", "", "SSL certificate not found for domain")
			return
		}
		if _, err := os.Stat(key); err != nil {
			redirect(w, r, "/settings", "", "SSL private key not found for domain")
			return
		}
	}

	if old, err := os.ReadFile(configPath); err == nil {
		name := filepath.Join(backupDir, time.Now().Format("20060102-150405")+".conf")
		_ = os.WriteFile(name, old, 0640)
	}
	tmp := configPath + ".new"
	if err := os.WriteFile(tmp, []byte(cfg), 0640); err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	// Validate the candidate while preserving the active configuration.
	candidate := configPath + ".candidate"
	if err := os.Rename(tmp, candidate); err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	defer os.Remove(candidate)

	active, activeErr := os.ReadFile(configPath)
	if err := os.Rename(candidate, configPath); err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	if out, err := exec.Command("nginx", "-t").CombinedOutput(); err != nil {
		if activeErr == nil {
			_ = os.WriteFile(configPath, active, 0640)
		} else {
			_ = os.Remove(configPath)
		}
		redirect(w, r, "/settings", "", "nginx test failed: "+string(out))
		return
	}
	if out, err := exec.Command("systemctl", "reload", "nginx").CombinedOutput(); err != nil {
		redirect(w, r, "/settings", "", "nginx reload failed: "+string(out))
		return
	}
	redirect(w, r, "/settings", "Nginx configuration applied", "")
}

func (a *App) rollbackNginx(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(backupDir)
	if err != nil || len(entries) == 0 {
		redirect(w, r, "/settings", "", "no backup available")
		return
	}
	var latest os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if latest == nil || e.Name() > latest.Name() {
			latest = e
		}
	}
	if latest == nil {
		redirect(w, r, "/settings", "", "no backup available")
		return
	}
	raw, err := os.ReadFile(filepath.Join(backupDir, latest.Name()))
	if err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	if err := os.WriteFile(configPath, raw, 0640); err != nil {
		redirect(w, r, "/settings", "", err.Error())
		return
	}
	if out, err := exec.Command("nginx", "-t").CombinedOutput(); err != nil {
		redirect(w, r, "/settings", "", "rollback config invalid: "+string(out))
		return
	}
	_ = exec.Command("systemctl", "reload", "nginx").Run()
	redirect(w, r, "/settings", "Rollback completed", "")
}

func redirect(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	q := ""
	if msg != "" {
		q = "?msg=" + queryEscape(msg)
	}
	if errMsg != "" {
		q = "?err=" + queryEscape(errMsg)
	}
	http.Redirect(w, r, path+q, http.StatusSeeOther)
}

func queryEscape(s string) string {
	replacer := strings.NewReplacer(
		"%", "%25", " ", "+", "&", "%26", "?", "%3F", "#", "%23",
		"+", "%2B", "\n", "%0A", "\r", "%0D",
	)
	return replacer.Replace(s)
}

func serviceActive(name string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", name).Run() == nil
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}
