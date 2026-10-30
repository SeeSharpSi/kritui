package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newPWATestHandler(t *testing.T) http.Handler {
	t.Helper()

	database := openTestDatabase(t)
	return newMux(
		database,
		newTestToolRegistry(t),
		newTestCommandRegistry(t, database),
		newToolCallStore(),
		nil,
		newAuthService(database, false),
	)
}

func TestPWAPublicAssetsBypassAuthentication(t *testing.T) {
	handler := newPWATestHandler(t)

	tests := []struct {
		path         string
		cacheControl string
		contentType  string
	}{
		{path: "/static/manifest.json?v=32", cacheControl: staticAssetCacheControl},
		{path: "/static/icon-192.png?v=2", cacheControl: staticAssetCacheControl},
		{path: "/static/icon-512.png?v=2", cacheControl: staticAssetCacheControl},
		{path: "/static/icon-512-maskable.png?v=2", cacheControl: staticAssetCacheControl},
		{path: "/static/styles.css?v=32", cacheControl: staticAssetCacheControl},
		{path: "/static/app.js?v=32", cacheControl: staticAssetCacheControl},
		{path: "/static/htmx.min.js", cacheControl: staticAssetCacheControl},
		{path: "/static/hx-sse.js", cacheControl: staticAssetCacheControl},
		{path: "/static/fonts/inter-latin.woff2", cacheControl: staticAssetCacheControl},
		{path: "/static/fonts/jetbrains-mono-latin.woff2", cacheControl: staticAssetCacheControl},
		{path: "/favicon.ico", cacheControl: "public, max-age=86400", contentType: "image/x-icon"},
		{path: "/logo.png", cacheControl: "public, max-age=86400", contentType: "image/png"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if location := response.Header().Get("Location"); location != "" {
				t.Fatalf("public asset redirected to %q", location)
			}
			if cacheControl := response.Header().Get("Cache-Control"); cacheControl != test.cacheControl {
				t.Errorf("Cache-Control = %q, want %q", cacheControl, test.cacheControl)
			}
			if test.contentType != "" {
				if contentType := response.Header().Get("Content-Type"); contentType != test.contentType {
					t.Errorf("Content-Type = %q, want %q", contentType, test.contentType)
				}
			}
			if response.Body.Len() == 0 {
				t.Error("body is empty")
			}
		})
	}

	t.Run("/static/app.js registers the service worker", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/static/app.js?v=32", nil))

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
		requireContains(t, response.Body.String(), "navigator.serviceWorker.register('/sw.js')")
	})

	t.Run("/sw.js", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sw.js", nil))

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
		if location := response.Header().Get("Location"); location != "" {
			t.Fatalf("service worker redirected to %q", location)
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "text/javascript; charset=utf-8" {
			t.Errorf("Content-Type = %q, want JavaScript", contentType)
		}
		if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", cacheControl)
		}
		if allowed := response.Header().Get("Service-Worker-Allowed"); allowed != "/" {
			t.Errorf("Service-Worker-Allowed = %q, want /", allowed)
		}
		requireContains(t, response.Body.String(), "addEventListener('fetch'")
	})

	t.Run("/healthz", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
		if location := response.Header().Get("Location"); location != "" {
			t.Fatalf("health check redirected to %q", location)
		}
	})
}

func TestPWAManifestIsPublicAndInstallable(t *testing.T) {
	handler := newPWATestHandler(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/static/manifest.json?v=32", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Fatalf("manifest redirected to %q", location)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != staticAssetCacheControl {
		t.Errorf("Cache-Control = %q, want %q", cacheControl, staticAssetCacheControl)
	}

	var manifest struct {
		ID       string `json:"id"`
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.NewDecoder(response.Body).Decode(&manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.ID != "/" || manifest.StartURL != "/" || manifest.Scope != "/" {
		t.Errorf("manifest identity = id %q, start_url %q, scope %q; want all /", manifest.ID, manifest.StartURL, manifest.Scope)
	}
	if manifest.Display != "standalone" {
		t.Errorf("manifest display = %q, want standalone", manifest.Display)
	}

	found := map[string]bool{}
	for _, icon := range manifest.Icons {
		found[icon.Sizes+" "+icon.Purpose] = true

		if icon.Type != "image/png" {
			t.Errorf("manifest icon %q type = %q, want image/png", icon.Src, icon.Type)
		}

		parsed, err := url.Parse(icon.Src)
		if err != nil {
			t.Errorf("parse icon source %q: %v", icon.Src, err)
			continue
		}
		if parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/static/") {
			t.Errorf("manifest icon source %q is not a public same-origin static path", icon.Src)
			continue
		}

		iconResponse := httptest.NewRecorder()
		handler.ServeHTTP(iconResponse, httptest.NewRequest(http.MethodGet, icon.Src, nil))
		if iconResponse.Code != http.StatusOK {
			t.Errorf("manifest icon %q status = %d, want %d", icon.Src, iconResponse.Code, http.StatusOK)
		}
		if location := iconResponse.Header().Get("Location"); location != "" {
			t.Errorf("manifest icon %q redirected to %q without a session", icon.Src, location)
		}
		if iconResponse.Body.Len() == 0 {
			t.Errorf("manifest icon %q body is empty", icon.Src)
		}
	}

	for _, expected := range []string{"192x192 ", "512x512 ", "512x512 maskable"} {
		if !found[expected] {
			t.Errorf("manifest is missing icon %q; got %v", expected, found)
		}
	}
}

func TestPWAAuthPagesIncludeMobileInstallMetadata(t *testing.T) {
	handler := newPWATestHandler(t)

	tests := []struct {
		path   string
		action string
		label  string
	}{
		{path: "/login", action: "/login", label: "sign in"},
		{path: "/signup", action: "/signup", label: "create account"},
	}

	for _, test := range tests {
		t.Run(test.label, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if location := response.Header().Get("Location"); location != "" {
				t.Fatalf("public auth page redirected to %q", location)
			}

			body := response.Body.String()
			requireContains(t, body,
				`name="viewport"`,
				"width=device-width",
				"viewport-fit=cover",
				`name="theme-color"`,
				`rel="manifest"`,
				`href="/static/manifest.json?v=58"`,
				`rel="apple-touch-icon"`,
				`href="/static/icon-192.png?v=58"`,
				`src="/static/app.js?v=58"`,
				"defer",
				`name="username"`,
				`name="password"`,
				`type="password"`,
				`action="`+test.action+`"`,
			)
			requireNotContains(t, body,
				`type="email"`,
				`name="email"`,
				`autocomplete="email"`,
			)
		})
	}
}

func TestPWAAuthMiddlewareProtectsAppAndPreservesLoginEntry(t *testing.T) {
	database := openTestDatabase(t)
	handler := newMux(
		database,
		newTestToolRegistry(t),
		newTestCommandRegistry(t, database),
		newToolCallStore(),
		nil,
		newAuthService(database, false),
	)

	chatCount := func() int {
		t.Helper()
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&count); err != nil {
			t.Fatalf("count chats: %v", err)
		}
		return count
	}
	before := chatCount()

	t.Run("full page redirect preserves chat query", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?chat=8", nil))

		if response.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusSeeOther)
		}
		location := response.Header().Get("Location")
		parsed, err := url.Parse(location)
		if err != nil {
			t.Fatalf("parse Location %q: %v", location, err)
		}
		if parsed.Scheme != "" || parsed.Host != "" || parsed.Path != "/login" {
			t.Fatalf("Location = %q, want same-origin /login", location)
		}
		if next := parsed.Query().Get("next"); next != "/?chat=8" {
			t.Errorf("login next = %q, want /?chat=8", next)
		}
		if after := chatCount(); after != before {
			t.Errorf("unauthenticated home allocated chats: %d -> %d", before, after)
		}
	})

	t.Run("htmx navigation redirects without login fragment", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "http://example.com/history?chat=8", nil)
		request.Header.Set("HX-Request", "true")
		request.Header.Set("HX-Current-URL", "http://example.com/?chat=8")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
		}
		redirect := response.Header().Get("HX-Redirect")
		parsed, err := url.Parse(redirect)
		if err != nil {
			t.Fatalf("parse HX-Redirect %q: %v", redirect, err)
		}
		if parsed.Scheme != "" || parsed.Host != "" || parsed.Path != "/login" {
			t.Fatalf("HX-Redirect = %q, want same-origin /login", redirect)
		}
		if next := parsed.Query().Get("next"); next != "/?chat=8" {
			t.Errorf("HX-Redirect next = %q, want /?chat=8", next)
		}
		if response.Body.Len() != 0 {
			t.Fatalf("htmx redirect body = %q, want empty", response.Body.String())
		}
		requireNotContains(t, response.Body.String(), "<!DOCTYPE", "Sign in")
	})

	t.Run("event stream requires authentication", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/messages/tools", nil)
		request.Header.Set("Accept", "text/event-stream")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
		if location := response.Header().Get("Location"); location != "" {
			t.Errorf("event stream rejection redirected to %q", location)
		}
		requireNotContains(t, response.Body.String(), "<!DOCTYPE", "Sign in")
	})

	if after := chatCount(); after != before {
		t.Errorf("unauthenticated requests allocated chats: %d -> %d", before, after)
	}
}
