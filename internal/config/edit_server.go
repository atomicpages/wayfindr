package config

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
)

// byteOrderMark is the UTF-8 byte order mark some editors put at the start of
// a file.
const byteOrderMark = "\xEF\xBB\xBF"

// SetPublicURL returns src with server.public_url set to publicURL, or
// removed when publicURL is empty, and the configs before and after (ADR
// 0021). Only the line of the key differs: the comments, the order, and
// the rest of the [server] table are kept byte for byte. An existing key is
// rewritten on its line, keeping a comment after it; a missing key goes
// after the last key of the [server] table; a file with no [server] table
// gets one at its end, set off by a blank line. Clearing a key that isn't
// there changes nothing.
//
// publicURL is checked by the rule Parse applies, and a refusal is that
// *ValidationError. Clearing it while server.oauth.enabled is true is a
// *ValidationError at server.public_url too (ADR 0022), found before the
// file is edited. Otherwise it fails with an [*Error] (it names no
// Connector): a KindUnsafe Case if src doesn't parse, server is written as
// a dotted key or an inline table, the value of the key spans lines, or the
// result would differ from src in anything but public_url.
//
// A UTF-8 byte order mark at the start of src is kept: Parse ignores it, so
// the editor does too. Left in place it would hide a [server] header on the
// first line, and the key would go into a second [server] table.
func SetPublicURL(src []byte, publicURL string, opts Options) (Edited, error) {
	if rest, ok := bytes.CutPrefix(src, []byte(byteOrderMark)); ok {
		e, err := SetPublicURL(rest, publicURL, opts)

		if err == nil {
			e.File = slices.Concat([]byte(byteOrderMark), e.File)
		}

		return e, err
	}

	before, err := Parse(src, opts)

	if err != nil {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Err: err}
	}

	var v validator
	v.publicURL(publicURL)

	if err := v.err(); err != nil {
		return Edited{}, err
	}

	if publicURL == "" && before.Server.OAuth.Enabled {
		v.add("server.public_url", "is needed while server.oauth.enabled is true; turn it off first")

		return Edited{}, v.err()
	}

	if before.Server.PublicURL == publicURL {
		return Edited{File: src, Before: before, After: before}, nil
	}

	var text string

	if publicURL != "" {
		// A validated URL has no character a basic string must escape, but the
		// encoder is the one place that decides how a string is written.
		if text, _, err = renderValue(publicURL); err != nil {
			return Edited{}, err
		}
	}

	out, err := setTableKey(src, []string{"server"}, CaseServerNotTable, "public_url", text)

	if err != nil {
		return Edited{}, err
	}

	after, err := Parse(out, opts)

	if err != nil {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Err: err}
	}

	want := *before
	want.Server.PublicURL = publicURL

	if !reflect.DeepEqual(&want, after) {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseChangesOthers}
	}

	return Edited{File: out, Before: before, After: after, Changed: !bytes.Equal(out, src)}, nil
}

// SetOAuthEnabled returns src with server.oauth.enabled set to enabled,
// and the configs before and after (ADR 0022). Only that line differs:
// an existing key is rewritten on its line, keeping a comment after it;
// a missing key goes after the last key of the [server.oauth] table; a
// file with no [server.oauth] table gets one at its end, set off by a
// blank line, holding only enabled = true. Setting false where the key
// is absent changes nothing; setting false where it is present writes
// enabled = false and keeps the table and its clients.
//
// Turning it on with no server.public_url is a *ValidationError at
// server.oauth.enabled, and turning it on while the file lists no clients is
// one at server.oauth.clients, both found before the file is edited.
// Otherwise it fails with an [*Error]: KindUnsafe CaseUnparsable if src
// doesn't parse; CaseOAuthNotTable if server or server.oauth is written as a
// dotted key or an inline table (oauth.enabled = ... inside [server],
// oauth = {...}, server.oauth.enabled = ... at the top), with Key naming
// which, server or server.oauth; CaseMultiline if the value spans lines;
// CaseChangesOthers if the result differs from src in anything but
// server.oauth.enabled. A UTF-8 byte order mark is kept, as SetPublicURL
// keeps it.
func SetOAuthEnabled(src []byte, enabled bool, opts Options) (Edited, error) {
	if rest, ok := bytes.CutPrefix(src, []byte(byteOrderMark)); ok {
		e, err := SetOAuthEnabled(rest, enabled, opts)

		if err == nil {
			e.File = slices.Concat([]byte(byteOrderMark), e.File)
		}

		return e, err
	}

	before, err := Parse(src, opts)

	if err != nil {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Err: err}
	}

	if before.Server.OAuth.Enabled == enabled {
		return Edited{File: src, Before: before, After: before}, nil
	}

	if enabled {
		// The rules Parse would apply once it is on, found before the file is
		// edited so the refusal is a validation error, not an unparsable file.
		var v validator

		v.oauth(Server{PublicURL: before.Server.PublicURL, OAuth: OAuth{Enabled: true, Clients: before.Server.OAuth.Clients}})

		if err := v.err(); err != nil {
			return Edited{}, err
		}
	}

	text := "false"

	if enabled {
		text = "true"
	}

	out, err := setTableKey(src, []string{"server", "oauth"}, CaseOAuthNotTable, "enabled", text)

	if err != nil {
		return Edited{}, err
	}

	after, err := Parse(out, opts)

	if err != nil {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Err: err}
	}

	want := *before
	want.Server.OAuth.Enabled = enabled

	if !reflect.DeepEqual(&want, after) {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseChangesOthers}
	}

	return Edited{File: out, Before: before, After: after, Changed: !bytes.Equal(out, src)}, nil
}

// setTableKey sets key to the one-line TOML value text in the table at the
// key path table (["server"], ["server", "oauth"]), or removes it when text
// is empty. It refuses with notTable when the table can't be given a header of
// its own: the table, or any prefix of it, is written as a dotted key or an
// inline table. The error's Key is the prefix that is not a table. The caller checks that the result parses and reads as
// intended.
func setTableKey(src []byte, table []string, notTable Case, key, text string) ([]byte, error) {
	lines := slices.Collect(bytes.Lines(src))
	spans := tableSpans(lines)

	// Before the first header, a statement for the table's first segment is a
	// dotted key or an inline table, which a header can't be put beside.
	top := len(lines)

	if len(spans) > 0 {
		top = spans[0].start
	}

	for _, st := range scanBody(lines, 0, top) {
		if st.key[0] == table[0] {
			return nil, &Error{Kind: KindUnsafe, Case: notTable, Key: table[0]}
		}
	}

	// Inside the header of each proper prefix, a statement for the next
	// segment is the same.
	for i := 1; i < len(table); i++ {
		j := slices.IndexFunc(spans, func(s tableSpan) bool { return slices.Equal(s.key, table[:i]) })

		if j < 0 {
			continue
		}

		s := spans[j]
		header := s.start

		for header < s.end && isComment(lines[header]) {
			header++
		}

		for _, st := range scanBody(lines, header+1, s.end) {
			if st.key[0] == table[i] {
				return nil, &Error{Kind: KindUnsafe, Case: notTable, Key: strings.Join(table[:i+1], ".")}
			}
		}
	}

	found := slices.IndexFunc(spans, func(s tableSpan) bool { return slices.Equal(s.key, table) })

	if found < 0 {
		if text == "" {
			return src, nil
		}

		return appendTable(src, table, key, text), nil
	}

	s := spans[found]
	header := s.start

	for header < s.end && isComment(lines[header]) {
		header++
	}

	stmts := scanBody(lines, header+1, s.end)
	l := connLayout{lines: lines}

	i := slices.IndexFunc(stmts, func(st stmt) bool { return slices.Equal(st.key, Key{key}) })

	switch {
	case i >= 0 && stmts[i].first != stmts[i].last:
		return nil, &Error{Kind: KindUnsafe, Case: CaseMultiline, Key: strings.Join(table, ".") + "." + key}
	case i >= 0 && text == "":
		st := stmts[i]

		return bytes.Join(slices.Delete(lines, st.first, st.last+1), nil), nil
	case i >= 0:
		return bytes.Join(l.replaceValue(&stmts[i], text), nil), nil
	case text == "":
		return src, nil
	}

	// A new key goes after the table's last statement, indented as it is, or
	// right after the header.
	insert, indent := header+1, ""

	if n := len(stmts); n > 0 {
		insert = stmts[n-1].last + 1
		indent = leadingSpace(lines[stmts[n-1].first])
	}

	eol := l.endLine(insert - 1)

	return bytes.Join(slices.Insert(lines, insert, []byte(indent+key+" = "+text+eol)), nil), nil
}

// appendTable adds a table at the key path table holding key = text at the
// end of src, after one blank line, in the line break the file uses.
func appendTable(src []byte, table []string, key, text string) []byte {
	eol := "\n"

	if bytes.Contains(src, []byte("\r\n")) {
		eol = "\r\n"
	}

	out := bytes.Clone(src)

	if len(out) > 0 {
		if !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, eol...)
		}

		out = append(out, eol...)
	}

	return append(out, "["+strings.Join(table, ".")+"]"+eol+key+" = "+text+eol...)
}
