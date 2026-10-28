package moderation

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// gbnfNode is one node of the GBNF subset the shipped grammars use: literals,
// character classes, rule references, groups, alternation and the ?, *, + and
// {m,n} repetitions. The matcher below is a Go test oracle for properties of a
// grammar's language; it is not llama.cpp's parser (the native parser evidence
// is in docs/evidence/moderation-agent-validation.md).
type gbnfNode struct {
	kind     byte // 'l' literal, 'c' class, 'r' rule reference, 's' sequence, 'a' alternation, 'p' repetition
	text     string
	ranges   [][2]rune
	negate   bool
	children []*gbnfNode
	min, max int // repetition bounds; max < 0 is unbounded
}

type gbnfGrammar map[string]*gbnfNode

func parseGBNF(t *testing.T, src string) gbnfGrammar {
	t.Helper()
	g := gbnfGrammar{}
	for _, line := range strings.Split(src, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		name, body, ok := strings.Cut(line, " ::= ")
		if !ok || g[name] != nil {
			t.Fatalf("unparsable or repeated rule: %q", line)
		}
		p := &gbnfParser{t: t, src: body}
		g[name] = p.alternation()
		if p.skipSpace(); p.pos != len(p.src) {
			t.Fatalf("rule %s: trailing text %q", name, p.src[p.pos:])
		}
	}
	var check func(*gbnfNode)
	check = func(n *gbnfNode) {
		if n.kind == 'r' && g[n.text] == nil {
			t.Fatalf("undefined rule %s", n.text)
		}
		for _, c := range n.children {
			check(c)
		}
	}
	for _, node := range g {
		check(node)
	}
	if g["root"] == nil {
		t.Fatal("grammar has no root")
	}
	return g
}

type gbnfParser struct {
	t   *testing.T
	src string
	pos int
}

func (p *gbnfParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *gbnfParser) alternation() *gbnfNode {
	alt := &gbnfNode{kind: 'a', children: []*gbnfNode{p.sequence()}}
	for p.skipSpace(); p.pos < len(p.src) && p.src[p.pos] == '|'; p.skipSpace() {
		p.pos++
		alt.children = append(alt.children, p.sequence())
	}
	return alt
}

func (p *gbnfParser) sequence() *gbnfNode {
	seq := &gbnfNode{kind: 's'}
	for p.skipSpace(); p.pos < len(p.src) && p.src[p.pos] != '|' && p.src[p.pos] != ')'; p.skipSpace() {
		seq.children = append(seq.children, p.postfix(p.atom()))
	}
	return seq
}

func (p *gbnfParser) atom() *gbnfNode {
	switch c := p.src[p.pos]; {
	case c == '"':
		end := p.pos + 1
		for ; end < len(p.src) && p.src[end] != '"'; end++ {
			if p.src[end] == '\\' {
				end++
			}
		}
		if end >= len(p.src) {
			p.t.Fatalf("unterminated literal in %q", p.src)
		}
		text, err := strconv.Unquote(p.src[p.pos : end+1])
		if err != nil {
			p.t.Fatalf("bad literal at %q: %v", p.src[p.pos:], err)
		}
		p.pos = end + 1
		return &gbnfNode{kind: 'l', text: text}
	case c == '[':
		p.pos++
		n := &gbnfNode{kind: 'c'}
		if p.src[p.pos] == '^' {
			n.negate = true
			p.pos++
		}
		char := func() rune {
			r := rune(p.src[p.pos])
			p.pos++
			if r == '\\' {
				r = map[byte]rune{'t': '\t', 'n': '\n', 'r': '\r', '\\': '\\', ']': ']', '[': '[', '-': '-', '^': '^'}[p.src[p.pos]]
				if r == 0 {
					p.t.Fatalf("unsupported class escape in %q", p.src)
				}
				p.pos++
			}
			return r
		}
		for p.src[p.pos] != ']' {
			lo := char()
			hi := lo
			if p.src[p.pos] == '-' && p.src[p.pos+1] != ']' {
				p.pos++
				hi = char()
			}
			n.ranges = append(n.ranges, [2]rune{lo, hi})
		}
		p.pos++
		return n
	case c == '(':
		p.pos++
		n := p.alternation()
		if p.pos >= len(p.src) || p.src[p.pos] != ')' {
			p.t.Fatalf("unclosed group in %q", p.src)
		}
		p.pos++
		return n
	default:
		start := p.pos
		for p.pos < len(p.src) && (p.src[p.pos] == '-' || p.src[p.pos] >= 'a' && p.src[p.pos] <= 'z' || p.src[p.pos] >= 'A' && p.src[p.pos] <= 'Z' || p.src[p.pos] >= '0' && p.src[p.pos] <= '9') {
			p.pos++
		}
		if start == p.pos {
			p.t.Fatalf("unexpected %q in %q", p.src[p.pos:], p.src)
		}
		return &gbnfNode{kind: 'r', text: p.src[start:p.pos]}
	}
}

func (p *gbnfParser) postfix(n *gbnfNode) *gbnfNode {
	for p.pos < len(p.src) {
		lo, hi := 0, 0
		switch p.src[p.pos] {
		case '?':
			lo, hi = 0, 1
			p.pos++
		case '*':
			lo, hi = 0, -1
			p.pos++
		case '+':
			lo, hi = 1, -1
			p.pos++
		case '{':
			end := strings.IndexByte(p.src[p.pos:], '}')
			if end < 0 {
				p.t.Fatalf("unclosed repetition in %q", p.src)
			}
			first, second, comma := strings.Cut(p.src[p.pos+1:p.pos+end], ",")
			var err error
			if lo, err = strconv.Atoi(first); err != nil {
				p.t.Fatalf("bad repetition in %q", p.src)
			}
			hi = lo
			if comma {
				hi = -1
				if second != "" {
					if hi, err = strconv.Atoi(second); err != nil {
						p.t.Fatalf("bad repetition in %q", p.src)
					}
				}
			}
			p.pos += end + 1
		default:
			return n
		}
		n = &gbnfNode{kind: 'p', children: []*gbnfNode{n}, min: lo, max: hi}
	}
	return n
}

// ends returns every position at which n can finish matching s from pos.
func (g gbnfGrammar) ends(n *gbnfNode, s string, pos int) map[int]bool {
	out := map[int]bool{}
	switch n.kind {
	case 'l':
		if strings.HasPrefix(s[pos:], n.text) {
			out[pos+len(n.text)] = true
		}
	case 'c':
		if pos < len(s) {
			r, size := utf8.DecodeRuneInString(s[pos:])
			in := false
			for _, rg := range n.ranges {
				in = in || r >= rg[0] && r <= rg[1]
			}
			if in != n.negate {
				out[pos+size] = true
			}
		}
	case 'r':
		return g.ends(g[n.text], s, pos)
	case 's':
		out[pos] = true
		for _, c := range n.children {
			next := map[int]bool{}
			for p := range out {
				maps.Copy(next, g.ends(c, s, p))
			}
			out = next
		}
	case 'a':
		for _, c := range n.children {
			maps.Copy(out, g.ends(c, s, pos))
		}
	case 'p':
		current := map[int]bool{pos: true}
		if n.min == 0 {
			out[pos] = true
		}
		for i := 1; (n.max < 0 || i <= n.max) && len(current) > 0 && i <= len(s)+1; i++ {
			next := map[int]bool{}
			for p := range current {
				maps.Copy(next, g.ends(n.children[0], s, p))
			}
			current = next
			if i >= n.min {
				maps.Copy(out, current)
			}
		}
	}
	return out
}

func (g gbnfGrammar) accepts(s string) bool { return g.ends(g["root"], s, 0)[len(s)] }

// unbounded names a rule containing a *, + or {m,} repetition, if any.
func (g gbnfGrammar) unbounded() string {
	var walk func(*gbnfNode) bool
	walk = func(n *gbnfNode) bool {
		return n.kind == 'p' && n.max < 0 || slices.ContainsFunc(n.children, walk)
	}
	for _, name := range slices.Sorted(maps.Keys(g)) {
		if walk(g[name]) {
			return name
		}
	}
	return ""
}

func ruleLine(t *testing.T, grammar, name string) string {
	t.Helper()
	for _, line := range strings.Split(grammar, "\n") {
		if strings.HasPrefix(line, name+" ::= ") {
			return line
		}
	}
	t.Fatalf("grammar lacks rule %s", name)
	return ""
}

// The oracle itself must see the unbounded whitespace that LLM94-1 measured.
func TestGrammarOracleMatchesTheSubset(t *testing.T) {
	g := parseGBNF(t, "root ::= \"{\" ws item (ws \",\" ws item){0,2} ws \"}\" ws\nitem ::= [a-c] | \"\\\"x\\\"\"\nws ::= [ \\t\\n\\r]*")
	for _, ok := range []string{"{a}", "{ a , b ,c }", `{"x",a}`, "{a}\n\n\n\n\n\n"} {
		if !g.accepts(ok) {
			t.Errorf("oracle refuses %q", ok)
		}
	}
	for _, bad := range []string{"{}", "{a,b,c,a}", "{d}", "{a}x", "{a"} {
		if g.accepts(bad) {
			t.Errorf("oracle accepts %q", bad)
		}
	}
	if g.unbounded() != "ws" {
		t.Errorf("oracle missed the unbounded whitespace rule: %q", g.unbounded())
	}
}
