package embed

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
)

// ModelID returns the model identity (design §5): the hex sha256 of the
// normalised base URL, the model, and the dimensions. Stored vectors whose
// identity differs from the current one are pending and get re-embedded.
//
// Encoding: each field is written as a netstring, "<byte length>:<bytes>,",
// in the order base_url, model, dimensions (decimal), and the three are
// concatenated with nothing between them. Length prefixes make the encoding
// unambiguous for any bytes in any field. For example
//
//	25:https://api.openai.com/v1,22:text-embedding-3-small,4:1536,
//
// Normalisation of base_url: trailing slashes are removed and the scheme and
// host are lowercased, so "https://API.openai.com/v1/" and
// "https://api.openai.com/v1" share an identity. Nothing else is rewritten
// (no default-port removal, no path cleaning); a URL that does not parse is
// used as written, minus trailing slashes.
func ModelID(baseURL, model string, dimensions int) string {
	var b strings.Builder

	for _, f := range []string{normaliseBaseURL(baseURL), model, strconv.Itoa(dimensions)} {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
		b.WriteByte(',')
	}

	sum := sha256.Sum256([]byte(b.String()))

	return hex.EncodeToString(sum[:])
}

func normaliseBaseURL(raw string) string {
	s := strings.TrimRight(raw, "/")
	u, err := url.Parse(s)

	if err != nil || u.Scheme == "" || u.Host == "" {
		return s
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	return u.String()
}
