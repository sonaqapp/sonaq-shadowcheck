package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTwitchLogin(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"RedAlertDJ", "redalertdj", true},
		{"  @Red_Alert  ", "red_alert", true},
		{"https://www.twitch.tv/RedAlertDJ/", "redalertdj", true},
		{"twitch.tv/redalertdj", "redalertdj", true},
		{"a", "", false},
		{"bad-name", "", false},
		{"bad name", "", false},
		{strings.Repeat("a", 26), "", false},
		{"", "", false},
	}

	for _, tc := range tests {
		got, ok := normalizeTwitchLogin(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("normalizeTwitchLogin(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTwitchCredentialsConfigured(t *testing.T) {
	if newTwitchTokenManager("DEINE_CLIENT_ID", "DEIN_CLIENT_SECRET", nil).configured() {
		t.Error("env placeholders must not count as configured")
	}
	if newTwitchTokenManager("", "", nil).configured() {
		t.Error("empty credentials must not count as configured")
	}

	id := strings.Repeat("a", 30)
	secret := strings.Repeat("b", 30)
	if !newTwitchTokenManager(id, secret, nil).configured() {
		t.Error("valid 30 character credentials should be configured")
	}
}

func TestListenPort(t *testing.T) {
	t.Setenv("PORT", "9090")
	if got := listenPort(); got != "9090" {
		t.Errorf("listenPort() = %q, want 9090", got)
	}

	t.Setenv("PORT", "   ")
	if got := listenPort(); got != defaultPort {
		t.Errorf("listenPort() with blank PORT = %q, want %q", got, defaultPort)
	}
}

func TestShadowRateLimiter(t *testing.T) {
	limiter := newShadowRateLimiter(2, time.Minute)

	if ok, _ := limiter.allow("1.2.3.4"); !ok {
		t.Fatal("first request should pass")
	}
	if ok, _ := limiter.allow("1.2.3.4"); !ok {
		t.Fatal("second request should pass")
	}

	ok, retry := limiter.allow("1.2.3.4")
	if ok {
		t.Fatal("third request should be rate limited")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("unexpected retry duration: %v", retry)
	}

	if ok, _ := limiter.allow("5.6.7.8"); !ok {
		t.Fatal("a different IP must not be limited")
	}
}

func TestShadowCheckCacheTTL(t *testing.T) {
	cache := newShadowCheckCache()
	cache.set("redalertdj", shadowCheckReport{Score: 42})

	if got, ok := cache.get("redalertdj"); !ok || got.Score != 42 {
		t.Fatalf("cache get = (%+v, %v), want score 42", got, ok)
	}

	// Force the entry to expire instead of waiting for the 60s TTL.
	cache.mu.Lock()
	entry := cache.entries["redalertdj"]
	entry.expiresAt = time.Now().Add(-time.Second)
	cache.entries["redalertdj"] = entry
	cache.mu.Unlock()

	if _, ok := cache.get("redalertdj"); ok {
		t.Fatal("expired cache entry must not be returned")
	}
}

func TestScoreChecks(t *testing.T) {
	build := func(status string) []shadowCheckCheck {
		return []shadowCheckCheck{
			{ID: "account_status", Status: status},
			{ID: "search_discovery", Status: status},
			{ID: "metadata", Status: status},
			{ID: "vod_archive", Status: status},
			{ID: "live_stream", Status: status},
		}
	}

	if score, status := scoreChecks(build("pass")); score != 100 || status != "healthy" {
		t.Errorf("all pass = (%d, %q), want (100, healthy)", score, status)
	}
	if score, status := scoreChecks(build("warn")); score != 55 || status != "warning" {
		t.Errorf("all warn = (%d, %q), want (55, warning)", score, status)
	}
	if score, status := scoreChecks(build("fail")); score != 0 || status != "critical" {
		t.Errorf("all fail = (%d, %q), want (0, critical)", score, status)
	}

	// The neutral live check must be excluded and the rest normalized to 100.
	offline := build("pass")
	offline[4].Status = "info"
	if score, status := scoreChecks(offline); score != 100 || status != "healthy" {
		t.Errorf("offline pass = (%d, %q), want (100, healthy)", score, status)
	}
}

func TestMergeTagsAndMatureLabel(t *testing.T) {
	tags := mergeTags([]string{"Deutsch", "", "Tech"}, []string{"tech", "Coding"})
	if len(tags) != 3 {
		t.Fatalf("mergeTags = %#v, want 3 unique tags", tags)
	}
	if tags[0] != "Deutsch" || tags[1] != "Tech" || tags[2] != "Coding" {
		t.Fatalf("mergeTags = %#v, want [Deutsch Tech Coding]", tags)
	}

	// content_classification_labels is a plain string array in Helix, not an
	// array of objects; decoding must keep that shape.
	var response helixResponse[helixChannelInfo]
	raw := `{"data":[{"broadcaster_id":"1","content_classification_labels":["MatureGame","Gambling"]}]}`
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("decode labels: %v", err)
	}
	labels := response.Data[0].ContentClassificationLabels
	if len(labels) != 2 || labels[0] != "MatureGame" || labels[1] != "Gambling" {
		t.Fatalf("labels = %#v, want [MatureGame Gambling]", labels)
	}
	if !hasMatureLabel(labels) {
		t.Error("hasMatureLabel must detect MatureGame")
	}
	if hasMatureLabel([]string{"Gambling"}) {
		t.Error("hasMatureLabel must ignore non-mature labels")
	}
	if hasMatureLabel(nil) {
		t.Error("hasMatureLabel must handle missing labels")
	}
}

func TestBuildShadowCheckReportOffline(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	snap := twitchSnapshot{
		user: helixUser{
			ID:              "123456",
			Login:           "redalertdj",
			DisplayName:     "RedAlertDJ",
			BroadcasterType: "affiliate",
			CreatedAt:       now.AddDate(0, 0, -400),
			ProfileImageURL: "https://example.com/avatar.png",
		},
		channel: &helixChannelInfo{
			GameName:                    "Science & Technology",
			Language:                    "de",
			Tags:                        []string{"Deutsch", "Tech", "Coding"},
			ContentClassificationLabels: []string{"MatureGame"},
		},
		searchHit:   true,
		searchIndex: 1,
		videos:      []helixVideo{{ID: "v1", CreatedAt: now.AddDate(0, 0, -3)}},
	}

	report := buildShadowCheckReport(snap, now)

	if report.Channel.Login != "redalertdj" || report.Channel.ID != "123456" {
		t.Errorf("unexpected channel identity: %+v", report.Channel)
	}
	if report.Channel.IsLive {
		t.Error("channel must be reported offline without a stream")
	}
	if report.Channel.AccountAgeDays != 400 {
		t.Errorf("account age = %d, want 400", report.Channel.AccountAgeDays)
	}
	if !report.Channel.SearchIndexed || !report.Channel.VODsEnabled || report.Channel.VODCount != 1 {
		t.Errorf("unexpected discovery data: %+v", report.Channel)
	}
	if report.Channel.GameName != "Science & Technology" || report.Channel.Language != "de" || len(report.Channel.Tags) != 3 {
		t.Errorf("channel metadata was not taken from /helix/channels: %+v", report.Channel)
	}
	if !report.Channel.IsMature {
		t.Error("mature flag must be derived from content_classification_labels")
	}

	wantIDs := []string{"account_status", "search_discovery", "metadata", "vod_archive", "live_stream"}
	if len(report.Checks) != len(wantIDs) {
		t.Fatalf("got %d checks, want %d", len(report.Checks), len(wantIDs))
	}
	for i, id := range wantIDs {
		if report.Checks[i].ID != id {
			t.Errorf("check %d id = %q, want %q", i, report.Checks[i].ID, id)
		}
		if report.Checks[i].Detail == "" || report.Checks[i].Recommendation == "" {
			t.Errorf("check %q is missing detail or recommendation", report.Checks[i].ID)
		}
	}
	if report.Checks[4].Status != "info" {
		t.Errorf("offline live check status = %q, want info", report.Checks[4].Status)
	}
	if report.Checks[4].Detail != offlineLiveDetail {
		t.Errorf("offline live check detail = %q, want %q", report.Checks[4].Detail, offlineLiveDetail)
	}

	if report.Score != 100 || report.Status != "healthy" {
		t.Errorf("score/status = (%d, %q), want (100, healthy)", report.Score, report.Status)
	}
	if report.Cached {
		t.Error("fresh report must not be marked as cached")
	}
}

func TestBuildShadowCheckReportLive(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	snap := twitchSnapshot{
		user: helixUser{
			ID:              "123456",
			Login:           "redalertdj",
			DisplayName:     "RedAlertDJ",
			BroadcasterType: "partner",
			CreatedAt:       now.AddDate(-2, 0, 0),
		},
		stream: &helixStream{
			GameName:    "Science & Technology",
			Title:       "Building ShadowCheck",
			Language:    "de",
			ViewerCount: 42,
			StartedAt:   now.Add(-90 * time.Minute),
			IsMature:    true,
			Tags:        []string{"Deutsch", "Tech", "Coding"},
		},
		channel: &helixChannelInfo{
			GameName: "Science & Technology",
			Language: "de",
			Tags:     []string{"Deutsch", "Tech", "Coding"},
		},
		searchHit:   true,
		searchIndex: 2,
		videos:      []helixVideo{{ID: "v1", CreatedAt: now.AddDate(0, 0, -1)}},
	}

	report := buildShadowCheckReport(snap, now)

	if !report.Channel.IsLive || !report.Channel.IsMature {
		t.Errorf("live/mature flags missing: %+v", report.Channel)
	}
	if report.Channel.ViewerCount != 42 || report.Channel.UptimeMinutes != 90 {
		t.Errorf("unexpected live metrics: %+v", report.Channel)
	}
	if report.Checks[4].Status != "pass" {
		t.Errorf("live check status = %q, want pass (%s)", report.Checks[4].Status, report.Checks[4].Detail)
	}
	if report.Score != 100 || report.Status != "healthy" {
		t.Errorf("score/status = (%d, %q), want (100, healthy)", report.Score, report.Status)
	}
}

func TestClientIP(t *testing.T) {
	r, err := http.NewRequest(http.MethodPost, "/api/shadowcheck", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = "127.0.0.1:5555"
	if got := clientIP(r); got != "127.0.0.1" {
		t.Errorf("remote addr fallback = %q, want 127.0.0.1", got)
	}

	r.Header.Set("X-Forwarded-For", "9.9.9.9, 8.8.8.8")
	if got := clientIP(r); got != "8.8.8.8" {
		t.Errorf("x-forwarded-for = %q, want right-most 8.8.8.8", got)
	}

	r.Header.Set("X-Real-IP", "7.7.7.7")
	if got := clientIP(r); got != "7.7.7.7" {
		t.Errorf("x-real-ip = %q, want 7.7.7.7", got)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{90 * time.Minute, "1h 30m"},
		{14 * time.Minute, "14m"},
		{-time.Minute, "0m"},
	}
	for _, tc := range tests {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteReportText(t *testing.T) {
	report := shadowCheckReport{
		Channel: shadowCheckChannel{
			Login:           "redalertdj",
			DisplayName:     "RedAlertDJ",
			URL:             "https://twitch.tv/redalertdj",
			AccountAgeDays:  400,
			BroadcasterType: "affiliate",
			GameName:        "Science & Technology",
			Language:        "de",
			Tags:            []string{"Deutsch", "Tech", "Coding"},
			SearchIndexed:   true,
			VODCount:        3,
		},
		Score:  55,
		Status: "warning",
		Checks: []shadowCheckCheck{
			{ID: "account_status", Title: "Account & Status", Status: "pass", Detail: "Affiliate channel.", Recommendation: "No action needed."},
			{ID: "metadata", Title: "Metadata & Tag Completeness", Status: "warn", Detail: "Incomplete metadata.", Recommendation: "Add tags."},
		},
	}

	var buf bytes.Buffer
	writeReportText(&buf, report)
	out := buf.String()

	for _, want := range []string{
		"ShadowCheck v0.3.0",
		"RedAlertDJ",
		"score 55/100 (warning)",
		"Affiliate",
		"[PASS] Account & Status",
		"[WARN] Metadata & Tag Completeness",
		"-> Add tags.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report output is missing %q:\n%s", want, out)
		}
	}
}

func TestRunChannelCheckValidation(t *testing.T) {
	t.Setenv("TWITCH_CLIENT_ID", "")
	t.Setenv("TWITCH_CLIENT_SECRET", "")

	if err := runChannelCheck(io.Discard, "redalertdj", false); err == nil || err.Error() != errNotConfigured {
		t.Errorf("missing credentials error = %v, want %q", err, errNotConfigured)
	}

	t.Setenv("TWITCH_CLIENT_ID", strings.Repeat("a", 30))
	t.Setenv("TWITCH_CLIENT_SECRET", strings.Repeat("b", 30))

	if err := runChannelCheck(io.Discard, "bad-name", false); err == nil || err.Error() != errChannelInvalid {
		t.Errorf("invalid channel error = %v, want %q", err, errChannelInvalid)
	}
}

func TestCLIAuditError(t *testing.T) {
	if got := cliAuditError(errTwitchChannelNotFound).Error(); got != errChannelNotFound {
		t.Errorf("not found error = %q, want %q", got, errChannelNotFound)
	}
	if got := cliAuditError(&twitchAPIError{Status: http.StatusUnauthorized}).Error(); got != errCredentialsRejected {
		t.Errorf("unauthorized error = %q, want %q", got, errCredentialsRejected)
	}
	if got := cliAuditError(&twitchAPIError{Status: http.StatusTooManyRequests}).Error(); got != errUpstreamRateLimit {
		t.Errorf("rate limit error = %q, want %q", got, errUpstreamRateLimit)
	}
	if got := cliAuditError(&twitchAPIError{Status: http.StatusBadGateway}).Error(); !strings.Contains(got, errUpstream) {
		t.Errorf("upstream error = %q, want it to contain %q", got, errUpstream)
	}
}

// newTestShadowCheckService uses fake but well-formed credentials. The tests
// below never reach Twitch because they either fail validation, hit the rate
// limiter or are served from the cache.
func newTestShadowCheckService() *shadowCheckService {
	return newShadowCheckService(strings.Repeat("a", 30), strings.Repeat("b", 30))
}

func TestHandlerHealthAndIndex(t *testing.T) {
	handler := newHandler(newShadowCheckService("", ""))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want %d", rec.Code, http.StatusOK)
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Errorf("index content type = %q, want text/html", contentType)
	}
	body := rec.Body.String()
	for _, want := range []string{"ShadowCheck", "auditForm", "api/shadowcheck", "searchParams"} {
		if !strings.Contains(body, want) {
			t.Errorf("embedded UI is missing %q", want)
		}
	}
	for _, unwanted := range []string{"early access", "Impressum"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(unwanted)) {
			t.Errorf("embedded UI must not contain %q", unwanted)
		}
	}
	if forms := strings.Count(body, "<form"); forms != 1 {
		t.Errorf("embedded UI must contain exactly one form, got %d", forms)
	}

	req = httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", rec.Code, http.StatusOK)
	}
	var health healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.Status != "ok" || health.Version != version {
		t.Errorf("health = %+v, want status ok and version %q", health, version)
	}
	if health.TwitchConfigured {
		t.Error("empty credentials must be reported as not configured")
	}
}

func TestShadowCheckHandleNotConfigured(t *testing.T) {
	svc := newShadowCheckService("DEINE_CLIENT_ID", "DEIN_CLIENT_SECRET")
	req := httptest.NewRequest(http.MethodPost, "/api/shadowcheck", strings.NewReader(`{"channel":"redalertdj"}`))
	rec := httptest.NewRecorder()

	svc.handle(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var payload errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if payload.Error != errNotConfigured {
		t.Errorf("error = %q, want %q", payload.Error, errNotConfigured)
	}
}

func TestShadowCheckHandleValidation(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   string
		want   int
	}{
		{"invalid channel", http.MethodPost, "/api/shadowcheck", `{"channel":"bad-name"}`, http.StatusBadRequest},
		{"missing channel", http.MethodPost, "/api/shadowcheck", `{}`, http.StatusBadRequest},
		{"invalid json", http.MethodPost, "/api/shadowcheck", `nope`, http.StatusBadRequest},
		{"body too large", http.MethodPost, "/api/shadowcheck", `{"channel":"` + strings.Repeat("a", maxShadowCheckBytes) + `"}`, http.StatusRequestEntityTooLarge},
		{"get without channel", http.MethodGet, "/api/shadowcheck", "", http.StatusBadRequest},
		{"get invalid channel", http.MethodGet, "/api/shadowcheck?channel=bad-name", "", http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestShadowCheckService()
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			svc.handle(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			var payload errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if payload.Error == "" {
				t.Error("error response must contain an error message")
			}
		})
	}
}

func TestShadowCheckHandleRateLimit(t *testing.T) {
	svc := newTestShadowCheckService()

	var rec *httptest.ResponseRecorder
	for i := 0; i <= shadowCheckRateLimit; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/shadowcheck", strings.NewReader(`{"channel":"bad-name"}`))
		rec = httptest.NewRecorder()
		svc.handle(rec, req)
	}

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status after %d requests = %d, want %d", shadowCheckRateLimit+1, rec.Code, http.StatusTooManyRequests)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("rate limited response must set Retry-After")
	}
}

func TestShadowCheckHandleCacheHit(t *testing.T) {
	svc := newTestShadowCheckService()
	svc.cache.set("redalertdj", shadowCheckReport{
		Channel:   shadowCheckChannel{Login: "redalertdj", DisplayName: "RedAlertDJ"},
		Score:     77,
		Status:    "warning",
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	})

	// The @ prefix is normalized, so this must hit the same cache entry.
	req := httptest.NewRequest(http.MethodPost, "/api/shadowcheck", strings.NewReader(`{"channel":"@RedAlertDJ"}`))
	rec := httptest.NewRecorder()

	svc.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var report shadowCheckReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !report.Cached {
		t.Error("cache hit must be flagged as cached")
	}
	if report.Score != 77 || report.Status != "warning" {
		t.Errorf("score/status = (%d, %q), want (77, warning)", report.Score, report.Status)
	}

	// GET share links use the same cache.
	req = httptest.NewRequest(http.MethodGet, "/api/shadowcheck?channel=redalertdj", nil)
	rec = httptest.NewRecorder()
	svc.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode GET report: %v", err)
	}
	if !report.Cached || report.Score != 77 {
		t.Errorf("GET report = %+v, want cached score 77", report)
	}
}
