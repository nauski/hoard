package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
)

// requireAdmin guards the dashboard and its API with the dashboard password
// (HTTP Basic auth; the user name is ignored). With no password configured it
// lets everything through, as before.
//
// Three routes stay outside it because clients call them without the
// dashboard password: /healthz, enrollment redemption (guarded by its
// single-use token) and progress reports (guarded per client in handleReport).
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/api/enroll/redeem", "/api/report":
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.Load().DashboardPassword == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.isAdmin(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="hoard", charset="UTF-8"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "dashboard password required"})
			return
		}
		// The browser attaches Basic credentials to requests other sites make
		// to this address too, so refuse state-changing requests that did not
		// come from the dashboard's own pages.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && crossSite(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAdmin reports whether the request carries the dashboard password.
func (s *Server) isAdmin(r *http.Request) bool {
	want := s.cfg.Load().DashboardPassword
	_, got, ok := r.BasicAuth()
	return ok && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// isClient reports whether the request carries host's own login.
func (s *Server) isClient(r *http.Request, host string) bool {
	c, exists := s.cfg.Load().Clients[host]
	user, got, ok := r.BasicAuth()
	return ok && exists && user == host &&
		subtle.ConstantTimeCompare([]byte(got), []byte(c.HTTPPassword)) == 1
}

// crossSite reports whether a browser says the request came from another site.
func crossSite(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site != "same-origin" && site != "none"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err != nil || u.Host != r.Host
	}
	return false
}

// randomSecret returns 32 random bytes as hex.
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
