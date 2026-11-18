// Command shadowcheck audits Twitch channels through the official Helix API.
//
// It is both a tiny web app and a one-shot CLI. The UI in web/index.html is
// embedded into the binary, so a release build is a single self-contained
// executable with no database, no static web root and no external assets:
//
//	./shadowcheck --channel redalertdj
//	./shadowcheck --channel redalertdj --json
//	TWITCH_CLIENT_ID=... TWITCH_CLIENT_SECRET=... ./shadowcheck
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// indexHTML is the complete single-page UI. Embedding it keeps a release build
// a single binary: no web root, no static file server, no database.
//
//go:embed web/index.html
var indexHTML []byte

const (
	defaultPort = "8080"
	version     = "0.3.0"
)

// Twitch upstream configuration and ShadowCheck limits.
const (
	twitchTokenEndpoint = "https://id.twitch.tv/oauth2/token"
	twitchHelixBaseURL  = "https://api.twitch.tv/helix"

	// twitchTokenRefreshMargin renews the cached app access token well before
	// it expires so requests never race against the expiry timestamp.
	twitchTokenRefreshMargin = 5 * time.Minute

	// twitchHTTPTimeout bounds a single Twitch HTTP request.
	twitchHTTPTimeout = 8 * time.Second

	// shadowCheckTimeout bounds one full audit (token + up to five Helix calls).
	shadowCheckTimeout = 12 * time.Second

	// maxShadowCheckBytes limits the JSON body of POST /api/shadowcheck.
	maxShadowCheckBytes = 2048

	// shadowCheckCacheTTL keeps identical audits out of the Twitch API for a
	// minute. The per-IP limiter protects the shared app token quota.
	shadowCheckCacheTTL   = 60 * time.Second
	shadowCheckRateLimit  = 15
	shadowCheckRateWindow = time.Minute
)

// User facing messages. The same texts are returned by the HTTP API and the
// CLI so both interfaces behave identically.
const (
	errChannelInvalid      = "Please enter a valid Twitch username."
	errNotConfigured       = "ShadowCheck is not configured yet. Please set TWITCH_CLIENT_ID and TWITCH_CLIENT_SECRET."
	errUpstream            = "Twitch API is currently unavailable. Please try again."
	errChannelNotFound     = "Twitch channel not found."
	errRateLimited         = "Too many audits. Please wait a moment and try again."
	errCredentialsRejected = "Twitch rejected the API credentials. Please check TWITCH_CLIENT_ID and TWITCH_CLIENT_SECRET."
	errUpstreamRateLimit   = "Twitch API rate limit reached. Please try again shortly."
)

// twitchLoginPattern matches valid Twitch logins after normalization.
var twitchLoginPattern = regexp.MustCompile(`^[a-z0-9_]{2,25}$`)

// twitchCredentialPattern sanity-checks client id/secret. Twitch issues 30
// character alphanumeric values, so placeholders never pass and the audit
// endpoint reports "not configured" instead of calling Twitch with garbage.
var twitchCredentialPattern = regexp.MustCompile(`^[A-Za-z0-9]{20,64}$`)

type shadowCheckRequest struct {
	Channel string `json:"channel"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type healthResponse struct {
	Status           string `json:"status"`
	Version          string `json:"version"`
	TwitchConfigured bool   `json:"twitch_configured"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	channel := flag.String("channel", "", "Twitch channel login to audit, then exit (no web server)")
	asJSON := flag.Bool("json", false, "with --channel: print the full report as JSON")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("shadowcheck " + version)
		return
	}

	if *channel != "" {
		if err := runChannelCheck(os.Stdout, *channel, *asJSON); err != nil {
			log.Fatalf("shadowcheck: %v", err)
		}
		return
	}
	if *asJSON {
		log.Fatal("shadowcheck: --json requires --channel")
	}

	runServer()
}

// runChannelCheck performs exactly one audit for the CLI and writes either a
// human readable summary or the raw JSON report to out.
func runChannelCheck(out io.Writer, raw string, asJSON bool) error {
	svc := newShadowCheckService(
		strings.TrimSpace(os.Getenv("TWITCH_CLIENT_ID")),
		strings.TrimSpace(os.Getenv("TWITCH_CLIENT_SECRET")),
	)
	if !svc.configured() {
		return errors.New(errNotConfigured)
	}

	login, ok := normalizeTwitchLogin(raw)
	if !ok {
		return errors.New(errChannelInvalid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shadowCheckTimeout)
	defer cancel()

	report, err := svc.audit(ctx, login)
	if err != nil {
		return cliAuditError(err)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	writeReportText(out, report)
	return nil
}

// cliAuditError maps upstream failures to short, actionable CLI messages.
func cliAuditError(err error) error {
	if errors.Is(err, errTwitchChannelNotFound) {
		return errors.New(errChannelNotFound)
	}

	var apiErr *twitchAPIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusNotFound:
			return errors.New(errChannelNotFound)
		case http.StatusUnauthorized, http.StatusForbidden:
			return errors.New(errCredentialsRejected)
		case http.StatusTooManyRequests:
			return errors.New(errUpstreamRateLimit)
		}
	}

	return fmt.Errorf("%s (%v)", errUpstream, err)
}

// writeReportText renders the report for terminals without colour or tables.
func writeReportText(out io.Writer, report shadowCheckReport) {
	channel := report.Channel

	live := "offline"
	if channel.IsLive {
		live = fmt.Sprintf("live for %s, %d viewers",
			formatDuration(time.Duration(channel.UptimeMinutes)*time.Minute), channel.ViewerCount)
	}

	fmt.Fprintf(out, "ShadowCheck v%s — %s\n", version, channel.DisplayName)
	fmt.Fprintf(out, "  %s\n", channel.URL)
	fmt.Fprintf(out, "  score %d/100 (%s), %s\n", report.Score, report.Status, live)
	fmt.Fprintf(out, "  account age %d days, %s\n", channel.AccountAgeDays, broadcasterLabel(channel.BroadcasterType))
	fmt.Fprintf(out, "  category %s, language %s, %d tags\n",
		orDash(channel.GameName), orDash(strings.ToUpper(channel.Language)), len(channel.Tags))
	fmt.Fprintf(out, "  search index %s, %d archived VOD(s)\n\n", indexedLabel(channel), channel.VODCount)

	for _, check := range report.Checks {
		fmt.Fprintf(out, "[%s] %s\n", strings.ToUpper(check.Status), check.Title)
		fmt.Fprintf(out, "       %s\n", check.Detail)
		if check.Status != "pass" && check.Recommendation != "" {
			fmt.Fprintf(out, "    -> %s\n", check.Recommendation)
		}
	}
}

func broadcasterLabel(broadcasterType string) string {
	switch broadcasterType {
	case "partner":
		return "Partner"
	case "affiliate":
		return "Affiliate"
	default:
		return "no Partner/Affiliate status"
	}
}

func indexedLabel(channel shadowCheckChannel) string {
	if channel.SearchIndexed {
		return "yes"
	}
	return "no"
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// runServer starts the HTTP mode. Only PORT is configurable; the default is
// 8080. Everything else is baked into the binary.
func runServer() {
	addr := ":" + listenPort()

	svc := newShadowCheckService(
		strings.TrimSpace(os.Getenv("TWITCH_CLIENT_ID")),
		strings.TrimSpace(os.Getenv("TWITCH_CLIENT_SECRET")),
	)
	if svc.configured() {
		log.Print("shadowcheck: Twitch credentials loaded")
	} else {
		log.Print("shadowcheck: TWITCH_CLIENT_ID/TWITCH_CLIENT_SECRET missing or invalid, audit endpoint answers 503")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(svc),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("shadowcheck: received %s, shutting down", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shadowcheck: graceful shutdown: %v", err)
		}
		close(shutdownDone)
	}()

	log.Printf("shadowcheck v%s: listening on http://localhost%s", version, addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("shadowcheck: server: %v", err)
	}

	<-shutdownDone
	log.Print("shadowcheck: stopped")
}

// listenPort returns PORT or 8080. An empty or whitespace-only value falls
// back to the default so containers without configuration still work.
func listenPort() string {
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		return port
	}
	return defaultPort
}

// ---------------------------------------------------------------------------
// HTTP API
// ---------------------------------------------------------------------------

// shadowCheckCheck is one structured finding of the audit. Status is always
// one of "pass", "warn", "fail" or "info". An "info" finding is neutral: it is
// reported to the user but excluded from the score because the metric cannot
// be measured in the channel's current state (for example live metrics while
// the channel is offline).
type shadowCheckCheck struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Detail         string `json:"detail"`
	Recommendation string `json:"recommendation"`
}

// shadowCheckChannel is the flat channel summary the frontend renders.
type shadowCheckChannel struct {
	Login           string   `json:"login"`
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name"`
	BroadcasterType string   `json:"broadcaster_type"`
	CreatedAt       string   `json:"created_at"`
	AccountAgeDays  int      `json:"account_age_days"`
	ProfileImageURL string   `json:"profile_image_url"`
	URL             string   `json:"url"`
	IsLive          bool     `json:"is_live"`
	IsMature        bool     `json:"is_mature"`
	ViewerCount     int      `json:"viewer_count"`
	GameName        string   `json:"game_name"`
	Title           string   `json:"title"`
	Language        string   `json:"language"`
	Tags            []string `json:"tags"`
	StartedAt       string   `json:"started_at,omitempty"`
	UptimeMinutes   int      `json:"uptime_minutes,omitempty"`
	SearchIndexed   bool     `json:"search_indexed"`
	VODsEnabled     bool     `json:"vods_enabled"`
	VODCount        int      `json:"vod_count"`
	LastVODAt       string   `json:"last_vod_at,omitempty"`
}

type shadowCheckReport struct {
	Channel   shadowCheckChannel `json:"channel"`
	Score     int                `json:"score"`
	Status    string             `json:"status"`
	CheckedAt string             `json:"checked_at"`
	Cached    bool               `json:"cached"`
	Checks    []shadowCheckCheck `json:"checks"`
}

// newHandler wires the three routes and the embedded UI. The UI is served at
// "/" only; there is no static directory to configure or deploy.
func newHandler(svc *shadowCheckService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", handleHealth(svc))
	mux.HandleFunc("GET /api/shadowcheck", svc.handle)
	mux.HandleFunc("POST /api/shadowcheck", svc.handle)
	mux.HandleFunc("GET /", serveIndex)
	return mux
}

func handleHealth(svc *shadowCheckService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Status:           "ok",
			Version:          version,
			TwitchConfigured: svc.configured(),
		})
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexHTML)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("shadowcheck: encode response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Twitch Helix client
// ---------------------------------------------------------------------------

// helixResponse mirrors the { "data": [...] } envelope of the Twitch API.
type helixResponse[T any] struct {
	Data []T `json:"data"`
}

type helixUser struct {
	ID              string    `json:"id"`
	Login           string    `json:"login"`
	DisplayName     string    `json:"display_name"`
	Type            string    `json:"type"`
	BroadcasterType string    `json:"broadcaster_type"`
	Description     string    `json:"description"`
	ProfileImageURL string    `json:"profile_image_url"`
	OfflineImageURL string    `json:"offline_image_url"`
	ViewCount       int       `json:"view_count"`
	CreatedAt       time.Time `json:"created_at"`
}

type helixStream struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	UserLogin   string    `json:"user_login"`
	UserName    string    `json:"user_name"`
	GameID      string    `json:"game_id"`
	GameName    string    `json:"game_name"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	ViewerCount int       `json:"viewer_count"`
	StartedAt   time.Time `json:"started_at"`
	Language    string    `json:"language"`
	TagIDs      []string  `json:"tag_ids"`
	Tags        []string  `json:"tags"`
	IsMature    bool      `json:"is_mature"`
}

type helixChannelInfo struct {
	BroadcasterID    string   `json:"broadcaster_id"`
	BroadcasterLogin string   `json:"broadcaster_login"`
	BroadcasterName  string   `json:"broadcaster_name"`
	GameName         string   `json:"game_name"`
	GameID           string   `json:"game_id"`
	Title            string   `json:"title"`
	Language         string   `json:"language"`
	Tags             []string `json:"tags"`
	// ContentClassificationLabels is the list of enabled label IDs, e.g.
	// ["MatureGame"] — Helix returns plain strings here, not objects.
	ContentClassificationLabels []string `json:"content_classification_labels"`
}

type helixSearchChannel struct {
	ID               string `json:"id"`
	BroadcasterLogin string `json:"broadcaster_login"`
	DisplayName      string `json:"display_name"`
	IsLive           bool   `json:"is_live"`
}

type helixVideo struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	UserLogin   string    `json:"user_login"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
	Duration    string    `json:"duration"`
	Type        string    `json:"type"`
	URL         string    `json:"url"`
}

// twitchTokenManager caches the OAuth app access token in memory. The mutex is
// held while fetching so a burst of audits triggers a single token request
// instead of hammering the Twitch token endpoint.
type twitchTokenManager struct {
	clientID     string
	clientSecret string
	client       *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newTwitchTokenManager(clientID, clientSecret string, client *http.Client) *twitchTokenManager {
	return &twitchTokenManager{clientID: clientID, clientSecret: clientSecret, client: client}
}

func (m *twitchTokenManager) configured() bool {
	return m != nil &&
		twitchCredentialPattern.MatchString(m.clientID) &&
		twitchCredentialPattern.MatchString(m.clientSecret)
}

// Token returns a valid app access token, fetching a fresh one when the cache
// is empty or the token is about to expire.
func (m *twitchTokenManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.token != "" && time.Now().Add(twitchTokenRefreshMargin).Before(m.expiresAt) {
		return m.token, nil
	}

	form := url.Values{}
	form.Set("client_id", m.clientID)
	form.Set("client_secret", m.clientSecret)
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, twitchTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("twitch token endpoint returned %s: %s", resp.Status, truncateText(string(body), 200))
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("twitch token response: %w", err)
	}
	if payload.AccessToken == "" {
		return "", errors.New("twitch token response without access_token")
	}

	ttl := time.Duration(payload.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	m.token = payload.AccessToken
	m.expiresAt = time.Now().Add(ttl)
	return m.token, nil
}

// invalidate drops the cached token, but only if it still is the one the
// upstream rejected (a concurrent refresh may already have replaced it).
func (m *twitchTokenManager) invalidate(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == token {
		m.token = ""
		m.expiresAt = time.Time{}
	}
}

// twitchAPIError reports a non-200 Helix response.
type twitchAPIError struct {
	Status int
	Body   string
}

func (e *twitchAPIError) Error() string {
	return fmt.Sprintf("twitch api returned %d: %s", e.Status, e.Body)
}

// twitchHelix is a minimal Helix client. A 401 invalidates the cached token
// and retries exactly once, which covers tokens revoked out of band.
type twitchHelix struct {
	tokens *twitchTokenManager
	client *http.Client
}

func (c *twitchHelix) get(ctx context.Context, path string, query url.Values, out any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return err
		}

		endpoint := twitchHelixBaseURL + path
		if len(query) > 0 {
			endpoint += "?" + query.Encode()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Client-Id", c.tokens.clientID)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.tokens.invalidate(token)
			lastErr = &twitchAPIError{Status: resp.StatusCode, Body: truncateText(string(body), 200)}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return &twitchAPIError{Status: resp.StatusCode, Body: truncateText(string(body), 200)}
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		return nil
	}
	return lastErr
}

// shadowCheckCache keeps finished audits for a short TTL so repeated lookups
// of the same channel do not consume Twitch API quota.
type shadowCheckCache struct {
	mu      sync.Mutex
	entries map[string]shadowCheckCacheEntry
}

type shadowCheckCacheEntry struct {
	report    shadowCheckReport
	expiresAt time.Time
}

func newShadowCheckCache() *shadowCheckCache {
	return &shadowCheckCache{entries: make(map[string]shadowCheckCacheEntry)}
}

func (c *shadowCheckCache) get(login string) (shadowCheckReport, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[login]
	if !ok {
		return shadowCheckReport{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, login)
		return shadowCheckReport{}, false
	}
	return entry.report, true
}

func (c *shadowCheckCache) set(login string, report shadowCheckReport) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	c.entries[login] = shadowCheckCacheEntry{
		report:    report,
		expiresAt: now.Add(shadowCheckCacheTTL),
	}
}

// shadowRateLimiter is a small sliding-window limiter keyed by client IP.
type shadowRateLimiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	hits      map[string][]time.Time
	lastSweep time.Time
}

func newShadowRateLimiter(limit int, window time.Duration) *shadowRateLimiter {
	return &shadowRateLimiter{
		limit:  limit,
		window: window,
		hits:   make(map[string][]time.Time),
	}
}

// allow records a hit for key and reports whether it stays within the window.
// When rejected it returns the time until the oldest hit leaves the window.
func (l *shadowRateLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	window := l.window
	cutoff := now.Add(-window)

	// Drop stale buckets now and then so the map cannot grow forever.
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) > window {
		for k, stamps := range l.hits {
			if kept := trimHits(stamps, cutoff); len(kept) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = kept
			}
		}
		l.lastSweep = now
	}

	kept := trimHits(l.hits[key], cutoff)
	if len(kept) >= l.limit {
		l.hits[key] = kept
		retry := window - now.Sub(kept[0])
		if retry < time.Second {
			retry = time.Second
		}
		return false, retry
	}
	l.hits[key] = append(kept, now)
	return true, 0
}

// trimHits returns the timestamps that are still inside the window. It reuses
// the backing array, so the caller must replace the stored slice.
func trimHits(stamps []time.Time, cutoff time.Time) []time.Time {
	kept := stamps[:0]
	for _, ts := range stamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	return kept
}

// ---------------------------------------------------------------------------
// Audit endpoint
// ---------------------------------------------------------------------------

// shadowCheckService bundles the shared state of the audit endpoint.
type shadowCheckService struct {
	helix   *twitchHelix
	tokens  *twitchTokenManager
	cache   *shadowCheckCache
	limiter *shadowRateLimiter
}

func newShadowCheckService(clientID, clientSecret string) *shadowCheckService {
	client := &http.Client{Timeout: twitchHTTPTimeout}
	tokens := newTwitchTokenManager(clientID, clientSecret, client)
	return &shadowCheckService{
		helix:   &twitchHelix{tokens: tokens, client: client},
		tokens:  tokens,
		cache:   newShadowCheckCache(),
		limiter: newShadowRateLimiter(shadowCheckRateLimit, shadowCheckRateWindow),
	}
}

func (s *shadowCheckService) configured() bool {
	return s.tokens.configured()
}

// handle serves both POST /api/shadowcheck (JSON body) and GET
// /api/shadowcheck?channel=<login>, which makes share links work without
// JavaScript on the client side.
func (s *shadowCheckService) handle(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: errNotConfigured})
		return
	}

	if ok, retry := s.limiter.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: errRateLimited})
		return
	}

	raw := r.URL.Query().Get("channel")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxShadowCheckBytes)
		defer r.Body.Close()

		var req shadowCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "Request body is too large."})
				return
			}
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: errChannelInvalid})
			return
		}
		raw = req.Channel
	}

	login, ok := normalizeTwitchLogin(raw)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: errChannelInvalid})
		return
	}

	if cached, found := s.cache.get(login); found {
		cached.Cached = true
		writeJSON(w, http.StatusOK, cached)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), shadowCheckTimeout)
	defer cancel()

	report, err := s.audit(ctx, login)
	if err != nil {
		s.writeAuditError(w, login, err)
		return
	}

	s.cache.set(login, report)
	writeJSON(w, http.StatusOK, report)
}

func (s *shadowCheckService) writeAuditError(w http.ResponseWriter, login string, err error) {
	if errors.Is(err, errTwitchChannelNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: errChannelNotFound})
		return
	}

	var apiErr *twitchAPIError
	if errors.As(err, &apiErr) {
		log.Printf("shadowcheck: audit %q: %v", login, err)
		switch apiErr.Status {
		case http.StatusNotFound:
			writeJSON(w, http.StatusNotFound, errorResponse{Error: errChannelNotFound})
		case http.StatusUnauthorized, http.StatusForbidden:
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: errCredentialsRejected})
		case http.StatusTooManyRequests:
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: errUpstreamRateLimit})
		default:
			writeJSON(w, http.StatusBadGateway, errorResponse{Error: errUpstream})
		}
		return
	}

	log.Printf("shadowcheck: audit %q: %v", login, err)
	writeJSON(w, http.StatusBadGateway, errorResponse{Error: errUpstream})
}

var errTwitchChannelNotFound = errors.New("twitch channel not found")

// clientIP prefers X-Real-IP, which a reverse proxy sets from $remote_addr and
// therefore cannot be spoofed by clients. X-Forwarded-For is only used as a
// fallback and read from the right, because proxies append to it.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		parts := strings.Split(v, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := strings.TrimSpace(parts[i]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// normalizeTwitchLogin trims, lowercases and validates a user supplied channel
// name. Common URL prefixes are stripped so pasted channel links work too.
func normalizeTwitchLogin(raw string) (string, bool) {
	login := strings.ToLower(strings.TrimSpace(raw))
	login = strings.TrimPrefix(login, "@")
	for _, prefix := range []string{
		"https://www.twitch.tv/", "https://twitch.tv/",
		"http://www.twitch.tv/", "http://twitch.tv/",
		"www.twitch.tv/", "twitch.tv/",
	} {
		login = strings.TrimPrefix(login, prefix)
	}
	login = strings.TrimSuffix(login, "/")
	if !twitchLoginPattern.MatchString(login) {
		return "", false
	}
	return login, true
}

// truncateText shortens untrusted upstream text for log lines.
func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// formatDuration renders a coarse, human readable uptime such as "2h 14m".
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	hours := int(d / time.Hour)
	minutes := int(d/time.Minute) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// mergeTags combines the legacy tags array with tag_ids, deduplicating case
// insensitively while keeping the first spelling.
func mergeTags(groups ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, group := range groups {
		for _, tag := range group {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			key := strings.ToLower(tag)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, tag)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Audit engine
// ---------------------------------------------------------------------------

// twitchSnapshot bundles the raw Helix data a single audit is built from.
type twitchSnapshot struct {
	user        helixUser
	stream      *helixStream
	channel     *helixChannelInfo
	searchHit   bool
	searchIndex int
	videos      []helixVideo
}

// audit queries all required Helix endpoints and turns the result into a
// report. The channel lookup runs first because the broadcaster id is needed
// for the remaining calls; those four calls then run in parallel.
func (s *shadowCheckService) audit(ctx context.Context, login string) (shadowCheckReport, error) {
	var users helixResponse[helixUser]
	if err := s.helix.get(ctx, "/users", url.Values{"login": {login}}, &users); err != nil {
		return shadowCheckReport{}, err
	}
	if len(users.Data) == 0 {
		return shadowCheckReport{}, errTwitchChannelNotFound
	}
	user := users.Data[0]

	var (
		streams  helixResponse[helixStream]
		channels helixResponse[helixChannelInfo]
		searches helixResponse[helixSearchChannel]
		videos   helixResponse[helixVideo]
	)

	lookups := []func() error{
		func() error {
			return s.helix.get(ctx, "/streams", url.Values{"user_id": {user.ID}}, &streams)
		},
		func() error {
			return s.helix.get(ctx, "/channels", url.Values{"broadcaster_id": {user.ID}}, &channels)
		},
		func() error {
			return s.helix.get(ctx, "/search/channels", url.Values{"query": {login}, "first": {"10"}}, &searches)
		},
		func() error {
			return s.helix.get(ctx, "/videos", url.Values{
				"user_id": {user.ID},
				"type":    {"archive"},
				"first":   {"5"},
			}, &videos)
		},
	}

	errCh := make(chan error, len(lookups))
	var wg sync.WaitGroup
	for _, lookup := range lookups {
		wg.Add(1)
		go func(fn func() error) {
			defer wg.Done()
			if err := fn(); err != nil {
				errCh <- err
			}
		}(lookup)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return shadowCheckReport{}, err
	}

	snap := twitchSnapshot{user: user}
	if len(streams.Data) > 0 {
		snap.stream = &streams.Data[0]
	}
	if len(channels.Data) > 0 {
		snap.channel = &channels.Data[0]
	}
	for i, hit := range searches.Data {
		if strings.EqualFold(hit.BroadcasterLogin, login) {
			snap.searchHit = true
			snap.searchIndex = i + 1
			break
		}
	}
	snap.videos = videos.Data
	// Defensive: the API returns newest first, but the report relies on it.
	sort.Slice(snap.videos, func(i, j int) bool {
		return snap.videos[i].CreatedAt.After(snap.videos[j].CreatedAt)
	})

	return buildShadowCheckReport(snap, time.Now().UTC()), nil
}

// buildShadowCheckReport derives the flat channel summary and the five checks
// from the raw snapshot.
func buildShadowCheckReport(snap twitchSnapshot, now time.Time) shadowCheckReport {
	user := snap.user
	ageDays := int(now.Sub(user.CreatedAt).Hours() / 24)
	if ageDays < 0 {
		ageDays = 0
	}

	category := ""
	title := ""
	language := ""
	var tags []string
	isMature := false

	if snap.stream != nil {
		category = snap.stream.GameName
		title = snap.stream.Title
		language = snap.stream.Language
		tags = mergeTags(snap.stream.Tags, snap.stream.TagIDs)
		isMature = snap.stream.IsMature
	}
	if snap.channel != nil {
		if category == "" {
			category = snap.channel.GameName
		}
		if title == "" {
			title = snap.channel.Title
		}
		if language == "" {
			language = snap.channel.Language
		}
		if len(tags) == 0 {
			tags = mergeTags(snap.channel.Tags)
		}
		if snap.stream == nil {
			isMature = hasMatureLabel(snap.channel.ContentClassificationLabels)
		}
	}
	if tags == nil {
		tags = []string{}
	}

	channel := shadowCheckChannel{
		Login:           user.Login,
		ID:              user.ID,
		DisplayName:     user.DisplayName,
		BroadcasterType: user.BroadcasterType,
		CreatedAt:       user.CreatedAt.UTC().Format(time.RFC3339),
		AccountAgeDays:  ageDays,
		ProfileImageURL: user.ProfileImageURL,
		URL:             "https://twitch.tv/" + user.Login,
		IsLive:          snap.stream != nil,
		IsMature:        isMature,
		GameName:        category,
		Title:           title,
		Language:        language,
		Tags:            tags,
		SearchIndexed:   snap.searchHit,
		VODsEnabled:     len(snap.videos) > 0,
		VODCount:        len(snap.videos),
	}
	if snap.stream != nil {
		channel.ViewerCount = snap.stream.ViewerCount
		channel.StartedAt = snap.stream.StartedAt.UTC().Format(time.RFC3339)
		channel.UptimeMinutes = int(now.Sub(snap.stream.StartedAt) / time.Minute)
		if channel.UptimeMinutes < 0 {
			channel.UptimeMinutes = 0
		}
	}
	if len(snap.videos) > 0 {
		channel.LastVODAt = snap.videos[0].CreatedAt.UTC().Format(time.RFC3339)
	}

	checks := []shadowCheckCheck{
		checkAccountStatus(user, ageDays),
		checkSearchDiscovery(snap.searchHit, snap.searchIndex),
		checkMetadata(category, language, tags),
		checkVODArchive(snap.videos, now),
		checkLiveHealth(snap.stream, snap.channel, now),
	}

	score, status := scoreChecks(checks)
	return shadowCheckReport{
		Channel:   channel,
		Score:     score,
		Status:    status,
		CheckedAt: now.Format(time.RFC3339),
		Checks:    checks,
	}
}

// hasMatureLabel reports whether the channel has an enabled mature content
// classification label stored in its channel metadata.
func hasMatureLabel(labels []string) bool {
	for _, label := range labels {
		if strings.Contains(strings.ToLower(label), "mature") {
			return true
		}
	}
	return false
}

// checkWeights define how much each finding contributes to the total score.
var checkWeights = map[string]int{
	"account_status":   20,
	"search_discovery": 25,
	"metadata":         25,
	"vod_archive":      15,
	"live_stream":      15,
}

// scoreChecks aggregates the individual checks into a 0-100 score and a
// status bucket. A pass earns the full weight, a warning roughly half. Neutral
// "info" findings are excluded entirely and the remaining weights are
// normalized to 100 %, so an unmeasurable metric (e.g. live health while the
// channel is offline) can never lower the score.
func scoreChecks(checks []shadowCheckCheck) (int, string) {
	total := 0
	earned := 0
	for _, check := range checks {
		weight := checkWeights[check.ID]
		if weight == 0 {
			continue
		}
		if check.Status == "info" {
			continue
		}
		total += weight * 100
		switch check.Status {
		case "pass":
			earned += weight * 100
		case "warn":
			earned += weight * 55
		}
	}

	score := 0
	if total > 0 {
		score = (earned*100 + total/2) / total
	}

	switch {
	case score >= 85:
		return score, "healthy"
	case score >= 50:
		return score, "warning"
	default:
		return score, "critical"
	}
}

func checkAccountStatus(user helixUser, ageDays int) shadowCheckCheck {
	check := shadowCheckCheck{ID: "account_status", Title: "Account & Status"}

	statusLabel := "no Partner/Affiliate status"
	switch user.BroadcasterType {
	case "partner":
		statusLabel = "Partner"
	case "affiliate":
		statusLabel = "Affiliate"
	}

	switch {
	case ageDays < 7:
		check.Status = "fail"
		check.Detail = fmt.Sprintf("Account is only %d day(s) old (%s). New accounts start with limited discovery and monetization.", ageDays, statusLabel)
		check.Recommendation = "Stream consistently for a few weeks before judging reach — Twitch weighs account history."
	case ageDays < 30:
		check.Status = "warn"
		check.Detail = fmt.Sprintf("Account is %d days old (%s) — the channel history is still short.", ageDays, statusLabel)
		check.Recommendation = "Keep a regular stream schedule so viewers and Twitch can build trust in the channel."
	case user.BroadcasterType == "partner" || user.BroadcasterType == "affiliate":
		check.Status = "pass"
		check.Detail = fmt.Sprintf("%s channel, account age %d days.", statusLabel, ageDays)
		check.Recommendation = "No action needed."
	default:
		check.Status = "warn"
		check.Detail = fmt.Sprintf("No Partner or Affiliate status after %d days.", ageDays)
		check.Recommendation = "Apply for the Affiliate program once you meet the follower and streaming requirements."
	}
	return check
}

func checkSearchDiscovery(indexed bool, position int) shadowCheckCheck {
	check := shadowCheckCheck{ID: "search_discovery", Title: "Search & Discovery Indexing"}

	if !indexed {
		check.Status = "fail"
		check.Detail = "The channel is not among the top 10 Twitch search results for its own name."
		check.Recommendation = "Set a category and tags, stream regularly and keep a distinctive channel name so Twitch indexes the channel."
		return check
	}
	if position > 3 {
		check.Status = "warn"
		check.Detail = fmt.Sprintf("Channel is indexed, but only at position %d for its own name.", position)
		check.Recommendation = "Strengthen name and brand consistency (avatar, panels, stream titles) so the channel ranks higher."
		return check
	}
	check.Status = "pass"
	check.Detail = fmt.Sprintf("Channel is indexed and appears at position %d in Twitch search.", position)
	check.Recommendation = "No action needed."
	return check
}

func checkMetadata(category, language string, tags []string) shadowCheckCheck {
	check := shadowCheckCheck{ID: "metadata", Title: "Metadata & Tag Completeness"}

	missing := make([]string, 0, 3)
	if category == "" {
		missing = append(missing, "category")
	}
	if language == "" {
		missing = append(missing, "language")
	}
	if len(tags) < 3 {
		missing = append(missing, fmt.Sprintf("tags (%d set)", len(tags)))
	}

	switch {
	case len(missing) == 0:
		check.Status = "pass"
		check.Detail = fmt.Sprintf("Category %q, language %s and %d tags are configured.", category, strings.ToUpper(language), len(tags))
		check.Recommendation = "No action needed."
	case category == "":
		check.Status = "fail"
		check.Detail = "No category is configured — the channel is missing its most important discovery signal."
		check.Recommendation = "Set a category before going live and add at least three relevant tags."
	default:
		check.Status = "warn"
		check.Detail = "Incomplete metadata: " + strings.Join(missing, ", ") + "."
		check.Recommendation = "Fill in category, language and at least three tags that describe your content."
	}
	return check
}

func checkVODArchive(videos []helixVideo, now time.Time) shadowCheckCheck {
	check := shadowCheckCheck{ID: "vod_archive", Title: "VOD Archiving"}

	if len(videos) == 0 {
		check.Status = "fail"
		check.Detail = "No archived broadcasts (VODs) were found."
		check.Recommendation = "Enable \"Store past broadcasts\" in your Twitch channel settings so streams can be rewatched, clipped and audited."
		return check
	}

	last := videos[0].CreatedAt
	age := now.Sub(last)
	when := last.UTC().Format("2006-01-02")

	if age <= 30*24*time.Hour {
		check.Status = "pass"
		check.Detail = fmt.Sprintf("%d archived VOD(s) found, the latest from %s.", len(videos), when)
		check.Recommendation = "No action needed."
		return check
	}

	days := int(age.Hours() / 24)
	check.Status = "warn"
	check.Detail = fmt.Sprintf("%d archived VOD(s) found, but the latest is from %s (%d days ago).", len(videos), when, days)
	check.Recommendation = "Verify that VODs are still being stored — archives help with clips, off-stream discovery and technical review."
	return check
}

// offlineLiveDetail is the neutral hint shown for the live check when the
// channel is offline and live metrics cannot be measured.
const offlineLiveDetail = "Channel is currently offline — live stream metrics will be analyzed automatically once you go live."

func checkLiveHealth(stream *helixStream, saved *helixChannelInfo, now time.Time) shadowCheckCheck {
	check := shadowCheckCheck{ID: "live_stream", Title: "Live Stream Health"}

	if stream == nil {
		check.Status = "info"
		check.Detail = offlineLiveDetail
		check.Recommendation = "Run ShadowCheck again while you are live to verify uptime, category and the mature flag."
		return check
	}

	uptime := now.Sub(stream.StartedAt)
	if uptime < 0 {
		uptime = 0
	}

	details := []string{
		fmt.Sprintf("live for %s", formatDuration(uptime)),
		fmt.Sprintf("%d viewers", stream.ViewerCount),
	}
	if stream.GameName != "" {
		details = append(details, "category "+stream.GameName)
	}
	if stream.IsMature {
		details = append(details, "mature flag on")
	}
	detail := strings.Join(details, ", ") + "."

	savedCategory := ""
	if saved != nil {
		savedCategory = saved.GameName
	}

	switch {
	case stream.Title == "":
		check.Status = "warn"
		check.Detail = "Stream is live without a title. " + detail
		check.Recommendation = "Use a descriptive title with the game name and a hook — titles are a discovery signal."
	case stream.GameName == "":
		check.Status = "fail"
		check.Detail = "Stream is live without a category. " + detail
		check.Recommendation = "Always pick a category before going live, otherwise the stream is almost invisible."
	case savedCategory != "" && !strings.EqualFold(savedCategory, stream.GameName):
		check.Status = "warn"
		check.Detail = fmt.Sprintf("Live category %q differs from the saved channel category %q. %s", stream.GameName, savedCategory, detail)
		check.Recommendation = "Keep the live category and the saved channel metadata in sync."
	case uptime < 5*time.Minute:
		check.Status = "warn"
		check.Detail = "Stream just started — too early for stable health metrics. " + detail
		check.Recommendation = "Re-run the audit once the stream has been live for more than five minutes."
	default:
		check.Status = "pass"
		check.Detail = detail
		check.Recommendation = "No action needed."
	}
	return check
}
