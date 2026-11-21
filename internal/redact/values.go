package redact

import (
	"net/url"
	"slices"
	"strings"
)

// minValueLen is the shortest value Values masks. A shorter one is no
// secret worth the name, and replacing every occurrence of, say, "1" would
// garble the text it is applied to.
const minValueLen = 4

// Values masks known values in free text: the resolved values of a
// connector's ${…} references, which can sit anywhere in a URL, its path
// included, where URL and URLQueries don't look (wayfindr-wt2v). Each value
// is replaced by the text given for it, such as the reference itself, so a
// masked error still says which secret was in play. The zero Values masks
// nothing.
type Values struct {
	olds []string
	r    *strings.Replacer
}

// NewValues returns the Values that replaces each value with its
// replacement, given as pairs: value, replacement, value, replacement. A
// value shorter than four bytes is skipped. Each value's path- and
// query-escaped forms are masked too, as URLs quote them.
func NewValues(pairs ...string) Values {
	var v Values
	var args []string

	for i := 0; i+1 < len(pairs); i += 2 {
		old, repl := pairs[i], pairs[i+1]

		if len(old) < minValueLen {
			continue
		}

		for _, form := range []string{old, url.PathEscape(old), url.QueryEscape(old)} {
			if !slices.Contains(v.olds, form) {
				v.olds = append(v.olds, form)
				args = append(args, form, repl)
			}
		}
	}

	if len(args) > 0 {
		v.r = strings.NewReplacer(args...)
	}

	return v
}

// Apply returns s with every value replaced.
func (v Values) Apply(s string) string {
	if v.r == nil {
		return s
	}

	return v.r.Replace(s)
}

// Contains reports whether s holds any of the values, in any form Apply
// would replace.
func (v Values) Contains(s string) bool {
	for _, old := range v.olds {
		if strings.Contains(s, old) {
			return true
		}
	}

	return false
}

// Err returns err with its text masked by Apply; errors.Is and errors.As
// still reach err. Nil stays nil, and so does an err when v masks nothing.
func (v Values) Err(err error) error {
	if err == nil || v.r == nil {
		return err
	}

	return maskedError{err: err, v: v}
}

// maskedError is an error whose text has v's values masked.
type maskedError struct {
	err error
	v   Values
}

func (e maskedError) Error() string { return e.v.Apply(e.err.Error()) }

func (e maskedError) Unwrap() error { return e.err }
