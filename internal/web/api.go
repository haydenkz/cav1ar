package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/haydenkz/cav1ar/internal/encode"
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth", s.auth)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("PUT /api/password", s.setPassword)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.snapshot()) })
	mux.HandleFunc("GET /api/jobs/{id}/log", s.jobLog)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.jobOp(s.o.Queue.Cancel))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.jobOp(s.o.Queue.Retry))
	mux.HandleFunc("POST /api/jobs/{id}/next", s.jobOp(s.o.Queue.RunNext))
	mux.HandleFunc("POST /api/add", s.add)
	mux.HandleFunc("POST /api/pause", s.pause)
	mux.HandleFunc("POST /api/hold", s.hold)
	mux.HandleFunc("GET /api/history", s.historyPage)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("GET /api/browse", s.browse)
}

// decode reads a request's JSON body into v, saying so if it can't.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		errorJSON(w, http.StatusBadRequest, fmt.Errorf("reading the request: %w", err))
		return false
	}
	return true
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	hash := s.o.Queue.Settings().WebPassword
	writeJSON(w, map[string]bool{"required": hash != "", "logged_in": hash == "" || s.loggedIn(r, hash)})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password string }
	if !decode(w, r, &req) {
		return
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	hash := s.o.Queue.Settings().WebPassword
	if hash == "" || !checkPassword(hash, req.Password) {
		time.Sleep(time.Second) // slows guessing
		errorJSON(w, http.StatusUnauthorized, errors.New("that's not the password"))
		return
	}
	http.SetCookie(w, s.loginCookie(hash))
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]bool{"ok": true})
}

// setPassword sets the password the web UI asks for, or with an empty one,
// stops asking. The page that set it stays logged in.
func (s *Server) setPassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password string }
	if !decode(w, r, &req) {
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	settings := s.o.Queue.Settings()
	settings.WebPassword = ""
	if req.Password != "" {
		if len(req.Password) < 8 {
			errorJSON(w, http.StatusBadRequest, errors.New("use a password of at least 8 characters"))
			return
		}
		hash, err := hashPassword(req.Password)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, err)
			return
		}
		settings.WebPassword = hash
	}
	if err := s.o.Apply(settings); err != nil {
		errorJSON(w, http.StatusInternalServerError, err)
		return
	}
	if settings.WebPassword != "" {
		http.SetCookie(w, s.loginCookie(settings.WebPassword))
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) jobLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		errorJSON(w, http.StatusBadRequest, errors.New("no such job"))
		return
	}
	s.mu.Lock()
	lines := slices.Clone(s.logs[id])
	s.mu.Unlock()
	writeJSON(w, map[string][]string{"lines": lines})
}

func (s *Server) jobOp(op func(int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			errorJSON(w, http.StatusBadRequest, errors.New("no such job"))
			return
		}
		op(id)
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// add queues a file, or every video in a folder and its subfolders.
func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if !decode(w, r, &req) {
		return
	}
	path := strings.TrimSpace(req.Path)
	if !filepath.IsAbs(path) {
		errorJSON(w, http.StatusBadRequest, errors.New("give the full path of a video or a folder"))
		return
	}
	path = filepath.Clean(path)
	fi, err := os.Stat(path)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err)
		return
	}
	if !fi.IsDir() && !encode.IsVideo(path) {
		errorJSON(w, http.StatusBadRequest, fmt.Errorf("%s isn't a video", filepath.Base(path)))
		return
	}
	videos := []string{path}
	if fi.IsDir() {
		if videos, err = encode.FindVideos(path); err != nil {
			errorJSON(w, http.StatusBadRequest, err)
			return
		}
	}
	for _, v := range videos {
		s.o.Queue.Add(v)
	}
	writeJSON(w, map[string]int{"added": len(videos)})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	var req struct{ Paused bool }
	if decode(w, r, &req) {
		s.o.Queue.SetPaused(req.Paused)
		writeJSON(w, map[string]bool{"ok": true})
	}
}

func (s *Server) hold(w http.ResponseWriter, r *http.Request) {
	var req struct{ Holding bool }
	if decode(w, r, &req) {
		s.o.Queue.SetHolding(req.Holding)
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// historyPage returns finished jobs, newest first: ?before= the oldest ID
// already shown, ?result= done, failed or canceled to show only those, and
// ?q= text to look for in their paths.
func (s *Server) historyPage(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		errorJSON(w, http.StatusServiceUnavailable, errNoHistory)
		return
	}
	if s.noHistoryYet() {
		writeJSON(w, map[string]any{"entries": []encode.Entry{}})
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	result := r.URL.Query().Get("result")
	entries, err := s.history.Recent(r.Context(), before, 50, result, strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"entries": entries})
}

// noHistoryYet reports whether the state database is still to be made, as
// it is until folders are first watched.
func (s *Server) noHistoryYet() bool {
	_, err := os.Stat(s.o.StatePath)
	return errors.Is(err, fs.ErrNotExist)
}

// stats sums up the jobs that finished in the last ?days= days, or all of
// them if it's 0, counting days in the page's time zone: ?tz= minutes east
// of UTC.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		errorJSON(w, http.StatusServiceUnavailable, errNoHistory)
		return
	}
	if s.noHistoryYet() {
		writeJSON(w, encode.Stats{})
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	tz, _ := strconv.Atoi(r.URL.Query().Get("tz"))
	tz = min(max(tz, -14*60), 14*60)
	var since time.Time
	if days > 0 {
		// From the start of the first day, in the page's time zone.
		zone := time.FixedZone("", tz*60)
		now := time.Now().In(zone)
		since = time.Date(now.Year(), now.Month(), now.Day()-days+1, 0, 0, 0, 0, zone)
	}
	st, err := s.history.Stats(r.Context(), since, tz*60)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, st)
}

// settingsView is the settings as a page edits them, with what it needs to
// offer the choices: never the password's hash.
type settingsView struct {
	Settings     encode.Settings `json:"settings"`
	Version      string          `json:"version"` // sent back with changes, to catch ones made elsewhere since
	PasswordSet  bool            `json:"password_set"`
	Devices      []encode.Device `json:"devices"`
	Recommended  encode.Settings `json:"recommended"`
	Placeholders []string        `json:"placeholders"`
	MaxHeights   []int           `json:"max_heights"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.o.Queue.Settings()
	v := settingsView{Settings: settings, PasswordSet: settings.WebPassword != "", Devices: s.o.Queue.Devices(),
		Recommended: settings.Recommended(), Placeholders: encode.NamePlaceholders, MaxHeights: encode.MaxHeights}
	v.Settings.WebPassword, v.Recommended.WebPassword = "", ""
	v.Version = settingsVersion(settings)
	writeJSON(w, v)
}

// settingsVersion tells settings apart, but for the password, which is
// changed on its own.
func settingsVersion(s encode.Settings) string {
	s.WebPassword = ""
	data, _ := json.Marshal(s)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var settings encode.Settings
	if !decode(w, r, &settings) {
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	current := s.o.Queue.Settings()
	if v := r.Header.Get("If-Match"); v != "" && v != settingsVersion(current) {
		errorJSON(w, http.StatusConflict, errors.New("the settings were changed somewhere else since this page loaded them"))
		return
	}
	settings.WebPassword = current.WebPassword // set on its own, with setPassword
	if err := s.check(&settings); err != nil {
		errorJSON(w, http.StatusBadRequest, err)
		return
	}
	if err := s.o.Apply(settings); err != nil {
		errorJSON(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// check tidies settings from a page and says what's wrong with them, as
// the terminal UI's settings screen does.
func (s *Server) check(settings *encode.Settings) error {
	settings.Language = strings.ToLower(strings.TrimSpace(settings.Language))
	settings.Title = strings.TrimSpace(settings.Title)
	settings.ReleaseGroup = strings.TrimSpace(settings.ReleaseGroup)
	settings.NameTemplate = strings.TrimSpace(settings.NameTemplate)
	settings.OutDir = strings.TrimSpace(settings.OutDir)
	settings.Preroll = strings.TrimSpace(settings.Preroll)
	if settings.AudioKbps == encode.AudioCopy {
		settings.Downmix = false // copied audio can't be changed
	}
	if err := settings.Validate(); err != nil {
		return err
	}
	isDir := func(path string) error {
		if fi, err := os.Stat(path); err != nil {
			return err
		} else if !fi.IsDir() {
			return fmt.Errorf("%s isn't a folder", path)
		}
		return nil
	}
	if settings.OutDir != "" {
		if err := isDir(settings.OutDir); err != nil {
			return fmt.Errorf("output folder: %w", err)
		}
	}
	for _, f := range settings.Watch {
		if err := isDir(f.Path); err != nil {
			return fmt.Errorf("watched folder: %w", err)
		}
	}
	if settings.Preroll != "" {
		if fi, err := os.Stat(settings.Preroll); err != nil || fi.IsDir() {
			return fmt.Errorf("preroll: %s isn't a file", settings.Preroll)
		}
	}
	devices := s.o.Queue.Devices()
	if len(devices) > 0 && !slices.ContainsFunc(devices, func(d encode.Device) bool { return settings.Slots(d, devices) > 0 }) {
		return errors.New("every encoder is off; turn one on")
	}
	return nil
}

// entry is a folder or video in a listing.
type entry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

// browse lists a folder's folders and videos, for picking one.
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	path := filepath.Clean("/" + r.URL.Query().Get("path"))
	items, err := os.ReadDir(path)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err)
		return
	}
	var entries []entry
	for _, it := range items {
		if strings.HasPrefix(it.Name(), ".") {
			continue
		}
		dir := it.IsDir()
		if it.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(path, it.Name())); err == nil {
				dir = fi.IsDir()
			}
		}
		e := entry{Name: it.Name(), Dir: dir}
		if !dir {
			if !encode.IsVideo(it.Name()) {
				continue
			}
			if fi, err := it.Info(); err == nil {
				e.Size = fi.Size()
			}
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	writeJSON(w, map[string]any{"path": path, "parent": filepath.Dir(path), "entries": entries})
}
