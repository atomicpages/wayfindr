package auth

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/atomicpages/wayfindr/internal/config"
)

// TestChooseSender tests chooseSender directly: it is the decision table of
// design §7.3's sender selector (CODING_STANDARDS: Tests). Its effect at
// the broker's seam is covered in dpop_test.go.
func TestChooseSender(t *testing.T) {
	required := &oauthex.ProtectedResourceMetadata{DPOPBoundAccessTokensRequired: true}
	es256 := MetadataExtensions{DPoPSigningAlgValuesSupported: []string{"RS256", "ES256"}}
	rs256 := MetadataExtensions{DPoPSigningAlgValuesSupported: []string{"RS256"}}
	none := MetadataExtensions{DPoPSigningAlgValuesSupported: []string{}}
	dpopChallenge := []string{`DPoP algs="ES256", resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`}
	bearerChallenge := []string{`Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`}

	tests := []struct {
		name      string
		mode      config.DPoPMode
		disc      Discovery
		challenge []string
		bound     bool   // the grant already has a DPoP key
		want      string // "bearer", "dpop", or the error Case
	}{
		{"never, asked by the metadata", config.DPoPNever, Discovery{PRM: required, Extensions: es256}, nil, false, "bearer"},
		{"never, asked by the challenge", config.DPoPNever, Discovery{Extensions: es256}, dpopChallenge, false, "bearer"},
		{"always, algs not listed", config.DPoPAlways, Discovery{}, nil, false, "dpop"},
		{"always, ES256 listed", config.DPoPAlways, Discovery{Extensions: es256}, nil, false, "dpop"},
		{"always, ES256 not listed", config.DPoPAlways, Discovery{Extensions: rs256}, nil, false, string(CaseDPoPAlgUnsupported)},
		{"always, none listed", config.DPoPAlways, Discovery{Extensions: none}, nil, false, string(CaseDPoPAlgUnsupported)},
		{"auto, asked by the metadata", config.DPoPAuto, Discovery{PRM: required, Extensions: es256}, nil, false, "dpop"},
		{"auto, asked by the challenge", config.DPoPAuto, Discovery{Extensions: es256}, dpopChallenge, false, "dpop"},
		{"auto, a Bearer challenge", config.DPoPAuto, Discovery{Extensions: es256}, bearerChallenge, false, "bearer"},
		{"auto, not asked", config.DPoPAuto, Discovery{Extensions: es256}, nil, false, "bearer"},
		{"auto, asked, algs not listed", config.DPoPAuto, Discovery{PRM: required}, dpopChallenge, false, "bearer"},
		{"auto, asked, ES256 not listed", config.DPoPAuto, Discovery{PRM: required, Extensions: rs256}, nil, false, "bearer"},
		{"unset is auto", "", Discovery{PRM: required, Extensions: es256}, nil, false, "dpop"},
		{"auto, bound, no longer asked", config.DPoPAuto, Discovery{Extensions: es256}, nil, true, "dpop"},
		{"never, bound", config.DPoPNever, Discovery{Extensions: es256}, nil, true, "bearer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := chooseSender(config.Auth{DPoP: tt.mode}, tt.disc, tt.challenge, tt.bound)
			got := ""

			switch s.(type) {
			case bearer:
				got = "bearer"
			case dpop:
				got = "dpop"
			}

			if e, ok := AsError(err); ok {
				got = string(e.Case)
			}

			if got != tt.want {
				t.Errorf("chooseSender = %T, %v; want %s", s, err, tt.want)
			}
		})
	}
}
