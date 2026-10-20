package oauth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// ResourceMetadata serves RFC 9728 protected resource metadata: which
// authorization server issues tokens for /mcp. The SDK's handler adds the CORS
// headers a browser-based client needs to read it.
func (s *Server) ResourceMetadata() http.Handler {
	return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.Resource(),
		AuthorizationServers:   []string{s.cfg.BaseURL},
		ScopesSupported:        []string{Scope},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Leave",
	})
}

// AuthorizationServerMetadata serves RFC 8414 metadata. Everything it names is
// what the handlers actually do: S256 only, public clients only.
func (s *Server) AuthorizationServerMetadata() http.Handler {
	meta := &oauthex.AuthServerMeta{
		Issuer:                                 s.cfg.BaseURL,
		AuthorizationEndpoint:                  s.cfg.BaseURL + "/oauth/authorize",
		TokenEndpoint:                          s.cfg.BaseURL + "/oauth/token",
		RegistrationEndpoint:                   s.cfg.BaseURL + "/oauth/register",
		RevocationEndpoint:                     s.cfg.BaseURL + "/oauth/revoke",
		ScopesSupported:                        []string{Scope},
		ResponseTypesSupported:                 []string{"code"},
		ResponseModesSupported:                 []string{"query"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:          []string{"S256"},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(meta)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// Register is dynamic client registration (RFC 7591, POST /oauth/register).
// Anyone may register: the client names itself, and the consent screen says
// so. What a registration buys is only the right to ask a signed-in person.
func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	if s.limited(w, r, s.registerLimit) {
		return
	}
	bad := func(code, desc string) {
		writeErr(w, badRequest(code, desc))
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var m oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		bad("invalid_client_metadata", "The body is not valid client metadata JSON.")
		return
	}
	// Only what this server does can be registered for. Other fields (the
	// auth method, a logo) are accepted and ignored, as RFC 7591 allows.
	if !subset(m.GrantTypes, "authorization_code", "refresh_token") ||
		(len(m.GrantTypes) > 0 && !contains(m.GrantTypes, "authorization_code")) {
		bad("invalid_client_metadata", "grant_types must be authorization_code, optionally with refresh_token.")
		return
	}
	if !subset(m.ResponseTypes, "code") {
		bad("invalid_client_metadata", "response_types must be code.")
		return
	}
	if !subset(splitScope(m.Scope), Scope) {
		bad("invalid_client_metadata", "The only scope is "+Scope+".")
		return
	}
	c, err := s.RegisterClient(r.Context(), m.ClientName, m.RedirectURIs)
	if err != nil {
		var ie InvalidError
		if errors.As(err, &ie) {
			bad("invalid_redirect_uri", ie.Error())
			return
		}
		slog.Error("oauth register", "err", err)
		writeErr(w, &oauthError{status: http.StatusInternalServerError, Code: "server_error"})
		return
	}
	resp := &oauthex.ClientRegistrationResponse{
		ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{
			RedirectURIs:            c.RedirectURIs,
			TokenEndpointAuthMethod: "none",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			ClientName:              c.Name,
			Scope:                   Scope,
		},
		ClientID:         c.ID,
		ClientIDIssuedAt: time.Now(),
	}
	writeJSON(w, http.StatusCreated, resp)
}

func contains(xs []string, x string) bool { return slices.Contains(xs, x) }

// subset reports whether every element of xs is one of allowed. An empty list
// passes: the field was not given.
func subset(xs []string, allowed ...string) bool {
	for _, x := range xs {
		if !slices.Contains(allowed, x) {
			return false
		}
	}
	return true
}

func splitScope(s string) []string { return strings.Fields(s) }
