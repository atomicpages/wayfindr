package oauthserver

import (
	"encoding/json"
	"net/http"
)

// resourceMetadata is the protected resource metadata (RFC 9728).
type resourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// serverMetadata is the authorization server metadata (RFC 8414). It names
// no scopes and no registration endpoint (ADR 0022).
type serverMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// ResourceMetadata serves the protected resource metadata, on both
// ResourceMetadataPath and ResourceMetadataPath + "/mcp". 404 while off.
func (s *Server) ResourceMetadata(w http.ResponseWriter, r *http.Request) {
	cfg, on := s.running()
	if !on {
		http.NotFound(w, r)

		return
	}

	issuer := cfg.Issuer()

	writeMetadata(w, resourceMetadata{
		Resource:               Resource(issuer),
		AuthorizationServers:   []string{issuer},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Wayfindr",
	})
}

// ServerMetadata serves the authorization server metadata. 404 while off.
func (s *Server) ServerMetadata(w http.ResponseWriter, r *http.Request) {
	cfg, on := s.running()
	if !on {
		http.NotFound(w, r)

		return
	}

	issuer := cfg.Issuer()

	writeMetadata(w, serverMetadata{
		Issuer:                                     issuer,
		AuthorizationEndpoint:                      issuer + AuthorizePath,
		TokenEndpoint:                              issuer + TokenPath,
		ResponseTypesSupported:                     []string{"code"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:              []string{"S256"},
		TokenEndpointAuthMethodsSupported:          []string{"none"},
		ClientIDMetadataDocumentSupported:          true,
		AuthorizationResponseIssParameterSupported: true,
	})
}

func writeMetadata(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// The documents are fixed structs of strings and bools.
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}
