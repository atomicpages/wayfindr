package config

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Key is a TOML key path inside one connector's table: {"url"},
// {"headers", "X-Team"}, {"auth", "authorize_params", "prompt"}, or
// {"auth"} for the whole auth table.
type Key []string

// String writes the key as TOML does, quoting parts that aren't bare keys:
// auth.authorize_params.prompt.
func (k Key) String() string { return toml.Key(k).String() }

// Set is one key an [Edit] sets, and its value.
type Set struct {
	Key Key
	// Value is anything that encodes as a TOML value other than a table: a
	// string, a Template, a []string, a Duration, a bool, an int.
	Value any
}

// Edit is a change to one connector's table: keys to set and keys to
// unset. No key may be given twice, or inside another key the edit names.
type Edit struct {
	// Set sets each key, adding it when it is missing. A key inside a
	// table the connector lacks creates the table: Key{"auth", "type"} on
	// a connector without auth adds an auth table.
	Set []Set
	// Unset removes each key: a value, or a whole table such as
	// Key{"auth"}.
	Unset []Key
}

// Edited is what [EditConnector] made of the file.
type Edited struct {
	// File is the edited file. It is the src EditConnector was given when
	// Changed is false.
	File []byte
	// Before and After are the config before and after the edit. After is
	// Before when Changed is false.
	Before, After *Config
	// Changed is false when every key set already had its value, so the
	// file is left alone.
	Changed bool
}

// EditConnector returns src with the edit applied to the connector's
// table, and the configs before and after. Only the lines of the keys
// changed differ; everything else, the table's comments and position
// included, is kept byte for byte. A key is set on its own line, a new key
// goes after the last key of its table, and a missing subtable is added
// after the connector's last table. An inline table such as
// auth = { type = "oauth" } is rewritten whole on its line and stays
// inline, keeping the order of its keys and adding new ones last.
//
// The whole result must parse: a result the config's rules refuse fails
// with Parse's error unchanged (a *ValidationError, say, for unsetting
// client_id while client_secret is set). Otherwise it fails with an
// [*Error]: CaseEditNotFound if src has no such connector, CaseKeyNotSet
// for unsetting a key that isn't there, KindInvalid for an edit that
// changes nothing or names a key twice, or a KindUnsafe Case if src
// doesn't parse, the connector is not written as tables, a key the edit
// changes is written with dotted keys or spans lines, or the result would
// differ from src in anything but the keys the edit names.
func EditConnector(src []byte, name string, e Edit, opts Options) (Edited, error) {
	ops, err := editOps(name, e)

	if err != nil {
		return Edited{}, err
	}

	before, err := Parse(src, opts)

	if err != nil {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: name, Err: err}
	}

	if _, ok := before.Connectors[name]; !ok {
		return Edited{}, &Error{Kind: KindConflict, Case: CaseEditNotFound, Connector: name}
	}

	rawBefore, err := decodeRaw(src)

	if err != nil {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: name, Err: err}
	}

	had := rawConnector(rawBefore, name)
	want, _ := cloneRaw(had).(map[string]any)

	for _, op := range ops {
		if err := op.applyRaw(want); err != nil {
			return Edited{}, err
		}
	}

	if reflect.DeepEqual(want, had) {
		return Edited{File: src, Before: before, After: before}, nil
	}

	lines := bytes.SplitAfter(src, []byte("\n"))

	for _, op := range ops {
		if lines, err = op.applyText(lines); err != nil {
			return Edited{}, err
		}
	}

	out := bytes.Join(lines, nil)

	if err := checkRaw(rawBefore, out, name, want); err != nil {
		return Edited{}, err
	}

	// The raw check proved that only the keys the edit names changed, so
	// a refusal now is about their new values.
	after, err := Parse(out, opts)

	if err != nil {
		return Edited{}, err
	}

	if _, ok, err := onlyChanged(before, out, name, opts); err != nil || !ok {
		return Edited{}, &Error{Kind: KindUnsafe, Case: CaseChangesOthers, Connector: name, Err: err}
	}

	return Edited{File: out, Before: before, After: after, Changed: true}, nil
}

// editOp is one key of an Edit: set to text (the TOML value, val decoded),
// or unset.
type editOp struct {
	connector string
	key       Key
	set       bool
	text      string
	val       any
}

// editOps checks e and orders its keys: unsets first, then sets, shorter
// keys first, so a table is created before the keys under it.
func editOps(name string, e Edit) ([]editOp, error) {
	if len(e.Set) == 0 && len(e.Unset) == 0 {
		return nil, &Error{Kind: KindInvalid, Case: CaseNothingToChange, Connector: name}
	}

	var ops []editOp

	for _, k := range e.Unset {
		ops = append(ops, editOp{connector: name, key: k})
	}

	sets := slices.Clone(e.Set)
	slices.SortStableFunc(sets, func(a, b Set) int { return len(a.Key) - len(b.Key) })

	for _, s := range sets {
		text, val, err := renderValue(s.Value)

		if err != nil {
			return nil, fmt.Errorf("edit connector %s: %s: %w", name, s.Key, err)
		}

		ops = append(ops, editOp{connector: name, key: s.Key, set: true, text: text, val: val})
	}

	for i, a := range ops {
		if len(a.key) == 0 {
			return nil, fmt.Errorf("edit connector %s: empty key", name)
		}

		for _, b := range ops[i+1:] {
			if hasPrefix(a.key, b.key) || hasPrefix(b.key, a.key) {
				return nil, &Error{Kind: KindInvalid, Case: CaseKeyTwice, Connector: name, Key: a.key.String()}
			}
		}
	}

	return ops, nil
}

// renderValue writes v as a one-line TOML value, and returns it decoded
// back as the decoder reads it.
func renderValue(v any) (string, any, error) {
	if v == nil {
		return "", nil, fmt.Errorf("no value")
	}

	var b bytes.Buffer

	if err := toml.NewEncoder(&b).Encode(map[string]any{"v": v}); err != nil {
		return "", nil, fmt.Errorf("encode value: %w", err)
	}

	text, ok := strings.CutPrefix(strings.TrimSuffix(b.String(), "\n"), "v = ")

	if !ok || strings.ContainsAny(text, "\r\n") {
		return "", nil, fmt.Errorf("%T is not a one-line TOML value", v)
	}

	var back map[string]any

	if _, err := toml.Decode("v = "+text, &back); err != nil {
		return "", nil, fmt.Errorf("decode value back: %w", err)
	}

	return text, back["v"], nil
}

func hasPrefix(k, prefix Key) bool {
	return len(k) >= len(prefix) && slices.Equal(k[:len(prefix)], prefix)
}

// applyRaw applies the op to the connector's table as the decoder reads
// it, giving what the edited file must read as.
func (op editOp) applyRaw(t map[string]any) error {
	for _, seg := range op.key[:len(op.key)-1] {
		next, ok := t[seg]

		if !ok {
			if !op.set {
				return op.notSet()
			}

			next = map[string]any{}
			t[seg] = next
		}

		sub, ok := next.(map[string]any)

		if !ok {
			return op.unparsable()
		}

		t = sub
	}

	leaf := op.key[len(op.key)-1]

	if !op.set {
		if _, ok := t[leaf]; !ok {
			return op.notSet()
		}

		delete(t, leaf)

		return nil
	}

	if _, ok := t[leaf].(map[string]any); ok {
		return op.unparsable()
	}

	t[leaf] = op.val

	return nil
}

func (op editOp) notSet() error {
	return &Error{Kind: KindConflict, Case: CaseKeyNotSet, Connector: op.connector, Key: op.key.String()}
}

// unparsable refuses an op that puts a value where a table is, or the
// other way round: the file would not parse after it.
func (op editOp) unparsable() error {
	return &Error{
		Kind: KindUnsafe, Case: CaseUnparsable, Connector: op.connector,
		Err: fmt.Errorf("%s: a table and a value in one place", op.key),
	}
}

func (op editOp) refuse(c Case, key Key) error {
	return &Error{Kind: KindUnsafe, Case: c, Connector: op.connector, Key: key.String()}
}

// applyText applies the op to the file's lines.
func (op editOp) applyText(lines [][]byte) ([][]byte, error) {
	l, err := scanConnector(lines, op.connector)

	if err != nil {
		return nil, err
	}

	k := op.key
	cur := l.span(nil)

	for d, seg := range k {
		leaf := d == len(k)-1
		prefix := k[:d+1]

		if cur != nil {
			st, dotted := cur.find(seg)

			if dotted {
				return nil, op.refuse(CaseDottedKeys, prefix)
			}

			if st != nil {
				if st.first != st.last {
					return nil, op.refuse(CaseMultiline, prefix)
				}

				switch {
				case leaf && op.set:
					return l.replaceValue(st, op.text), nil
				case leaf:
					return slices.Delete(lines, st.first, st.first+1), nil
				case !st.inline:
					return nil, op.unparsable()
				}

				return l.editInline(st, op, k[d+1:])
			}
		}

		under := l.hasUnder(prefix)

		if leaf {
			switch {
			case op.set && under:
				return nil, op.unparsable()
			case op.set && cur != nil:
				return l.insertKey(cur, seg, op.text), nil
			case op.set:
				return l.newTable(k[:d], seg, op.text), nil
			case !under:
				return nil, op.notSet()
			}

			return l.removeTables(prefix), nil
		}

		cur = l.span(prefix)

		if cur == nil && !under {
			if !op.set {
				return nil, op.notSet()
			}

			return l.newTable(k[:len(k)-1], k[len(k)-1], op.text), nil
		}
		// A nil cur with tables under it is a table only its subtables
		// define, such as auth when only [connectors.x.auth.authorize_params]
		// is written: it has no lines of its own.
	}

	return nil, op.unparsable()
}

// connLayout is where a connector's tables and keys are in the file's
// lines.
type connLayout struct {
	lines [][]byte
	name  string
	spans []connSpan
}

// connSpan is one of the connector's table spans.
type connSpan struct {
	tableSpan
	// path is the table's key below the connector: nil for the connector's
	// own table, {"auth"} for its auth table.
	path   Key
	header int
	stmts  []stmt
}

// stmt is one key = value statement in a table's body.
type stmt struct {
	key Key
	// first and last are the lines it spans.
	first, last int
	// valStart and valEnd are the value's byte offsets in its line, when
	// first == last.
	valStart, valEnd int
	inline           bool
}

// scanConnector finds the connector's tables in lines and the statements
// in their bodies. It refuses a connector without its own
// [connectors.<name>] table.
func scanConnector(lines [][]byte, name string) (connLayout, error) {
	l := connLayout{lines: lines, name: name}

	for _, s := range tableSpans(lines) {
		if !isConnectorTable(s.key, name) {
			continue
		}

		h := s.start

		for h < s.end && isComment(lines[h]) {
			h++
		}

		l.spans = append(l.spans, connSpan{tableSpan: s, path: s.key[2:], header: h, stmts: scanBody(lines, h+1, s.end)})
	}

	if l.span(nil) == nil {
		return connLayout{}, &Error{Kind: KindUnsafe, Case: CaseNotTables, Connector: name}
	}

	return l, nil
}

// span returns the table at path, or nil if no header defines it.
func (l connLayout) span(path Key) *connSpan {
	for i := range l.spans {
		if slices.Equal(l.spans[i].path, path) {
			return &l.spans[i]
		}
	}

	return nil
}

// hasUnder reports whether a header defines the table at path or one
// below it.
func (l connLayout) hasUnder(path Key) bool {
	return slices.ContainsFunc(l.spans, func(s connSpan) bool { return hasPrefix(s.path, path) })
}

// find returns the statement that assigns key in the table. dotted is true
// when a dotted key (key.x = …) writes into key instead.
func (s *connSpan) find(key string) (st *stmt, dotted bool) {
	for i := range s.stmts {
		if s.stmts[i].key[0] != key {
			continue
		}

		if len(s.stmts[i].key) > 1 {
			return nil, true
		}

		st = &s.stmts[i]
	}

	return st, false
}

func (l connLayout) replaceValue(st *stmt, text string) [][]byte {
	line := l.lines[st.first]
	l.lines[st.first] = slices.Concat(line[:st.valStart], []byte(text), line[st.valEnd:])

	return l.lines
}

// insertKey adds key = text after the table's last statement, indented as
// it is, or after its header.
func (l connLayout) insertKey(s *connSpan, key, text string) [][]byte {
	at, indent := s.header+1, ""

	if n := len(s.stmts); n > 0 {
		last := s.stmts[n-1]
		at = last.last + 1
		indent = leadingSpace(l.lines[last.first])
	}

	eol := l.endLine(at - 1)

	return slices.Insert(l.lines, at, []byte(indent+Key{key}.String()+" = "+text+eol))
}

// newTable adds a [connectors.<name>.<path>] table holding key = text after
// the connector's last table, set off by a blank line.
func (l connLayout) newTable(path Key, key, text string) [][]byte {
	last := l.spans[len(l.spans)-1]
	at := last.header + 1

	if n := len(last.stmts); n > 0 {
		at = last.stmts[n-1].last + 1
	}

	eol := l.endLine(at - 1)
	header := "[" + toml.Key(slices.Concat([]string{"connectors", l.name}, path)).String() + "]"

	return slices.Insert(l.lines, at, []byte(eol), []byte(header+eol), []byte(Key{key}.String()+" = "+text+eol))
}

// endLine makes sure line i ends in a line break, and returns the break
// the file uses there.
func (l connLayout) endLine(i int) string {
	line := l.lines[i]

	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		return "\r\n"
	case !bytes.HasSuffix(line, []byte("\n")):
		l.lines[i] = slices.Concat(line, []byte("\n"))
	}

	return "\n"
}

// removeTables drops the tables at path and below it, as RemoveConnector
// drops a connector's.
func (l connLayout) removeTables(path Key) [][]byte {
	drop := make([]bool, len(l.lines))
	last := 0

	for _, s := range l.spans {
		if !hasPrefix(s.path, path) {
			continue
		}

		last = max(last, s.end)

		for i := s.start; i < s.end; i++ {
			drop[i] = true
		}
	}

	if last == len(l.lines) {
		i := len(l.lines) - 1

		for i > 0 && drop[i-1] {
			i--
		}

		if i > 0 && isBlank(l.lines[i-1]) {
			drop[i-1] = true
		}
	}

	var out [][]byte

	for i, line := range l.lines {
		if !drop[i] {
			out = append(out, line)
		}
	}

	return out
}

// editInline applies op to the inline table st, at path inside it, and
// writes the table back on its line.
func (l connLayout) editInline(st *stmt, op editOp, path Key) ([][]byte, error) {
	line := l.lines[st.first]
	t, err := parseInline(string(line[st.valStart:st.valEnd]))

	if err != nil {
		return nil, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: op.connector, Err: err}
	}

	cur := t

	for _, seg := range path[:len(path)-1] {
		next, ok := cur.vals[seg]

		if !ok {
			if !op.set {
				return nil, op.notSet()
			}

			next = &inlineTable{vals: map[string]any{}}
			cur.put(seg, next)
		}

		sub, ok := next.(*inlineTable)

		if !ok {
			return nil, op.unparsable()
		}

		cur = sub
	}

	leaf := path[len(path)-1]

	switch _, ok := cur.vals[leaf]; {
	case op.set:
		cur.put(leaf, op.val)
	case !ok:
		return nil, op.notSet()
	default:
		cur.del(leaf)
	}

	text, err := t.render()

	if err != nil {
		return nil, &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: op.connector, Err: err}
	}

	return l.replaceValue(st, text), nil
}

// inlineTable is an inline table's keys in the order they are written.
// A value is an *inlineTable for a table inside it.
type inlineTable struct {
	keys []string
	vals map[string]any
}

func (t *inlineTable) put(k string, v any) {
	if _, ok := t.vals[k]; !ok {
		t.keys = append(t.keys, k)
	}

	t.vals[k] = v
}

func (t *inlineTable) del(k string) {
	delete(t.vals, k)
	t.keys = slices.DeleteFunc(t.keys, func(s string) bool { return s == k })
}

// parseInline decodes an inline table's text, keeping its key order.
func parseInline(text string) (*inlineTable, error) {
	var m map[string]any
	md, err := toml.Decode("v = "+text, &m)

	if err != nil {
		return nil, fmt.Errorf("decode inline table: %w", err)
	}

	v, ok := m["v"].(map[string]any)

	if !ok {
		return nil, fmt.Errorf("not an inline table: %s", text)
	}

	return orderInline(v, md.Keys(), toml.Key{"v"}), nil
}

func orderInline(m map[string]any, keys []toml.Key, at toml.Key) *inlineTable {
	t := &inlineTable{vals: map[string]any{}}

	for _, k := range keys {
		if len(k) != len(at)+1 || !slices.Equal(k[:len(at)], at) {
			continue
		}

		name := k[len(at)]
		v := m[name]

		if sub, ok := v.(map[string]any); ok {
			v = orderInline(sub, keys, k)
		}

		t.put(name, v)
	}

	return t
}

// render writes the table inline: { a = 1, b = { c = 2 } }.
func (t *inlineTable) render() (string, error) {
	if len(t.keys) == 0 {
		return "{}", nil
	}

	parts := make([]string, len(t.keys))

	for i, k := range t.keys {
		v, err := renderInlineValue(t.vals[k])

		if err != nil {
			return "", err
		}

		parts[i] = Key{k}.String() + " = " + v
	}

	return "{ " + strings.Join(parts, ", ") + " }", nil
}

func renderInlineValue(v any) (string, error) {
	switch v := v.(type) {
	case *inlineTable:
		return v.render()
	case map[string]any:
		return (&inlineTable{keys: slices.Sorted(maps.Keys(v)), vals: v}).render()
	case []map[string]any:
		return renderInlineArray(v)
	case []any:
		return renderInlineArray(v)
	}

	text, _, err := renderValue(v)

	return text, err
}

func renderInlineArray[T any](vs []T) (string, error) {
	parts := make([]string, len(vs))

	for i, v := range vs {
		s, err := renderInlineValue(v)

		if err != nil {
			return "", err
		}

		parts[i] = s
	}

	return "[" + strings.Join(parts, ", ") + "]", nil
}

// checkRaw requires out to read as before in everything but the
// connector, and the connector to read as want.
func checkRaw(before map[string]any, out []byte, name string, want map[string]any) error {
	after, err := decodeRaw(out)

	if err != nil {
		return &Error{Kind: KindUnsafe, Case: CaseUnparsable, Connector: name, Err: err}
	}

	got := rawConnector(after, name)

	if !reflect.DeepEqual(withoutConnector(before, name), withoutConnector(after, name)) {
		return &Error{Kind: KindUnsafe, Case: CaseChangesOthers, Connector: name}
	}

	if !reflect.DeepEqual(got, want) {
		return &Error{Kind: KindUnsafe, Case: CaseReadsDifferently, Connector: name}
	}

	return nil
}

func decodeRaw(src []byte) (map[string]any, error) {
	var m map[string]any

	if _, err := toml.NewDecoder(bytes.NewReader(src)).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	return m, nil
}

func rawConnector(m map[string]any, name string) map[string]any {
	cs, _ := m["connectors"].(map[string]any)
	c, _ := cs[name].(map[string]any)

	return c
}

func withoutConnector(m map[string]any, name string) map[string]any {
	m, _ = cloneRaw(m).(map[string]any)

	if cs, ok := m["connectors"].(map[string]any); ok {
		delete(cs, name)
	}

	return m
}

// cloneRaw deep-copies a decoded TOML value's tables and arrays.
func cloneRaw(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))

		for k, e := range v {
			out[k] = cloneRaw(e)
		}

		return out
	case []any:
		out := make([]any, len(v))

		for i, e := range v {
			out[i] = cloneRaw(e)
		}

		return out
	case []map[string]any:
		out := make([]map[string]any, len(v))

		for i, e := range v {
			out[i], _ = cloneRaw(e).(map[string]any)
		}

		return out
	}

	return v
}

func leadingSpace(line []byte) string {
	return string(line[:len(line)-len(bytes.TrimLeft(line, " \t"))])
}

// scanBody reads the key = value statements in lines[from:to], a table's
// body. A value may span lines (a multi-line string or array); its lines
// are not read as statements. It is lenient: the caller's parse check
// refuses a result it read wrongly.
func scanBody(lines [][]byte, from, to int) []stmt {
	if from >= to {
		return nil
	}

	buf := bytes.Join(lines[from:to], nil)
	starts := make([]int, 0, to-from)
	off := 0

	for _, line := range lines[from:to] {
		starts = append(starts, off)
		off += len(line)
	}

	lineOf := func(p int) int {
		i, found := slices.BinarySearch(starts, p)

		if !found {
			i--
		}

		return i
	}

	var stmts []stmt

	for p := 0; p < len(buf); {
		p = skipSpace(buf, p)

		if p >= len(buf) {
			break
		}

		if buf[p] == '\n' || buf[p] == '\r' || buf[p] == '#' {
			p = lineEnd(buf, p)

			continue
		}

		start := p
		key, p2, ok := lexKey(buf, p)

		if !ok {
			p = lineEnd(buf, p)

			continue
		}

		vs := skipSpace(buf, p2)
		ve := lexValue(buf, vs)
		first, last := lineOf(start), lineOf(max(ve-1, vs))
		stmts = append(stmts, stmt{
			key:      key,
			first:    from + first,
			last:     from + last,
			valStart: vs - starts[first],
			valEnd:   ve - starts[first],
			inline:   vs < len(buf) && buf[vs] == '{',
		})
		p = lineEnd(buf, ve)
	}

	return stmts
}

func skipSpace(b []byte, p int) int {
	for p < len(b) && (b[p] == ' ' || b[p] == '\t') {
		p++
	}

	return p
}

// lineEnd returns the offset after the line break at or after p.
func lineEnd(b []byte, p int) int {
	if i := bytes.IndexByte(b[p:], '\n'); i >= 0 {
		return p + i + 1
	}

	return len(b)
}

// lexKey reads a possibly dotted key and the = after it, returning the
// offset after the =.
func lexKey(b []byte, p int) (Key, int, bool) {
	var key Key

	for {
		p = skipSpace(b, p)

		if p >= len(b) {
			return nil, p, false
		}

		switch c := b[p]; {
		case c == '"' || c == '\'':
			end := lexString(b, p)
			part := string(b[p:end])

			if c == '"' {
				s, err := strconv.Unquote(part)

				if err != nil {
					return nil, p, false
				}

				part = s
			} else {
				part = strings.Trim(part, "'")
			}

			key = append(key, part)
			p = end
		case isBareKeyChar(c):
			end := p

			for end < len(b) && isBareKeyChar(b[end]) {
				end++
			}

			key = append(key, string(b[p:end]))
			p = end
		default:
			return nil, p, false
		}

		p = skipSpace(b, p)

		if p >= len(b) {
			return nil, p, false
		}

		switch b[p] {
		case '.':
			p++
		case '=':
			return key, p + 1, true
		default:
			return nil, p, false
		}
	}
}

func isBareKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// lexValue returns the offset just after the value that starts at p.
func lexValue(b []byte, p int) int {
	if p >= len(b) {
		return p
	}

	switch b[p] {
	case '"', '\'':
		return lexString(b, p)
	case '[', '{':
		depth := 0

		for i := p; i < len(b); {
			switch b[i] {
			case '"', '\'':
				i = lexString(b, i)

				continue
			case '#':
				i = lineEnd(b, i)

				continue
			case '[', '{':
				depth++
			case ']', '}':
				depth--

				if depth == 0 {
					return i + 1
				}
			}

			i++
		}

		return len(b)
	}

	end := p

	for end < len(b) && b[end] != '\n' && b[end] != '#' {
		end++
	}

	for end > p && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}

	return end
}

// lexString returns the offset just after the string that starts at p:
// basic or literal, one-line or multi-line.
func lexString(b []byte, p int) int {
	q := b[p]
	triple := bytes.HasPrefix(b[p:], []byte{q, q, q})
	i := p + 1

	if triple {
		i = p + 3
	}

	for i < len(b) {
		switch {
		case q == '"' && b[i] == '\\':
			i += 2
		case triple && bytes.HasPrefix(b[i:], []byte{q, q, q}):
			i += 3

			// Up to two more quotes end the string's content.
			for n := 0; n < 2 && i < len(b) && b[i] == q; n++ {
				i++
			}

			return i
		case !triple && b[i] == q:
			return i + 1
		case !triple && b[i] == '\n':
			return i
		default:
			i++
		}
	}

	return len(b)
}
