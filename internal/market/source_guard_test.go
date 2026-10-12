package market

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A-518 (b): a static guard over the non-test Go source. docs/acceptance.md claims that user content renders through
// html/template without casts to trusted HTML, JavaScript or URLs, that database queries bind values as parameters, and
// that forwarded IP headers are not trusted. This test makes those claims fail a build instead of a review:
//
//   - template.HTML, template.JS, template.URL (and the other trusted html/template types) may not be used as a
//     conversion anywhere;
//   - the text of a query may not depend on anything but constants: the query argument of a database method, and any
//     concatenation with an SQL-looking literal, must be "fixed" (see fixedness below), and fmt.Sprintf may not build
//     SQL at all. The sites in sqlBuildAllowlist are the exceptions, each with the reason it is safe;
//   - nothing reads or writes a X-Forwarded-*, Forwarded, X-Real-IP, True-Client-IP or CF-Connecting-IP header, or
//     RemoteAddr. The sign-in and write limiters key on accounts, handles and the one shared anonymous class, never on
//     an address the client or a proxy could choose.
//
// Fixedness is a small syntactic analysis, not type checking: an expression is fixed when it is a string literal, a
// constant, a package variable initialised from fixed text, a local variable every assignment of which is fixed
// (including a loop variable over a literal list of fixed text), a call of a function in the package whose every
// return is fixed, a parameter of an unexported function every call of which passes fixed text, or strconv.Itoa and
// friends (a number cannot carry SQL). A value from a request, a database row, a struct field or another package is
// never fixed. The analysis matches calls by name, so it errs towards "not fixed"; it cannot follow a query handed to
// a method it does not know. A new site is therefore a review question the allowlist forces into the open, not a
// proof of safety.

// sqlBuildAllowlist names the functions allowed to hand a non-fixed query to the database, as "path:function", with
// why each is safe. A new entry needs the same: the text comes from code or embedded files, never from a request, and
// every value is bound as a parameter.
var sqlBuildAllowlist = map[string]string{
	"internal/market/migrate.go:applyMigrations": "executes the numbered migration files embedded in the binary (go:embed); the text is the project's own SQL, selected by version, never request input",
}

type sourceFinding struct{ where, what string }

type sourceFile struct {
	rel string
	f   *ast.File
}

var sqlish = regexp.MustCompile(`\b(SELECT\b|INSERT\s+INTO\b|UPDATE\s+\S+\s+SET\b|DELETE\s+FROM\b|WHERE\b|ORDER\s+BY\b|GROUP\s+BY\b|RETURNING\b|ON\s+CONFLICT\b)`)

var untrustedHeader = regexp.MustCompile(`(?i)^(x-forwarded-[a-z-]+|forwarded|x-real-ip|true-client-ip|cf-connecting-ip)$`)

var trustedTemplateTypes = map[string]bool{"HTML": true, "JS": true, "URL": true, "CSS": true, "HTMLAttr": true, "JSStr": true, "Srcset": true}

// dbSinks are the database/sql and pgx methods that take query text, with the index of the query argument.
var dbSinks = map[string]int{"Exec": 0, "Query": 0, "QueryRow": 0, "Prepare": 0, "ExecContext": 1, "QueryContext": 1, "QueryRowContext": 1, "PrepareContext": 1}

// numericText are functions whose result is digits only.
var numericText = map[string]bool{"strconv.Itoa": true, "strconv.FormatInt": true, "strconv.FormatUint": true}

// parseSourceTree parses the non-test Go files of root's cmd and internal trees, grouped by package directory.
func parseSourceTree(t *testing.T, root string) (*token.FileSet, map[string][]sourceFile) {
	t.Helper()
	fset := token.NewFileSet()
	dirs := map[string][]sourceFile{}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			dirs[filepath.Dir(path)] = append(dirs[filepath.Dir(path)], sourceFile{filepath.ToSlash(rel), f})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return fset, dirs
}

type callSite struct {
	call *ast.CallExpr
	in   *ast.FuncDecl // nil at package level
}

// pkgFacts is what the fixedness analysis knows about one package.
type pkgFacts struct {
	consts   map[string]bool
	vars     map[string][]ast.Expr
	funcs    map[string][]*ast.FuncDecl
	calls    map[string][]callSite
	imported map[string]bool // import names of every file: a selector on one is another package's
	memo     map[ast.Node]bool
	visiting map[ast.Node]bool
}

func newPkgFacts(files []sourceFile) *pkgFacts {
	p := &pkgFacts{consts: map[string]bool{}, vars: map[string][]ast.Expr{}, funcs: map[string][]*ast.FuncDecl{},
		calls: map[string][]callSite{}, imported: map[string]bool{}, memo: map[ast.Node]bool{}, visiting: map[ast.Node]bool{}}
	for _, sf := range files {
		for _, im := range sf.f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			p.imported[name] = true
		}
		for _, decl := range sf.f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				p.funcs[d.Name.Name] = append(p.funcs[d.Name.Name], d)
			case *ast.GenDecl:
				for _, s := range d.Specs {
					vs, ok := s.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, id := range vs.Names {
						switch {
						case d.Tok == token.CONST:
							p.consts[id.Name] = true
						case d.Tok == token.VAR && len(vs.Values) == len(vs.Names):
							p.vars[id.Name] = append(p.vars[id.Name], vs.Values[i])
						case d.Tok == token.VAR && len(vs.Values) == 0:
							p.vars[id.Name] = append(p.vars[id.Name], &ast.BasicLit{Kind: token.STRING, Value: `""`})
						default:
							p.vars[id.Name] = append(p.vars[id.Name], nil) // a multi-value initialiser: unknown
						}
					}
				}
			}
		}
	}
	for _, sf := range files {
		for _, decl := range sf.f.Decls {
			fd, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					switch fun := c.Fun.(type) {
					case *ast.Ident:
						p.calls[fun.Name] = append(p.calls[fun.Name], callSite{c, fd})
					case *ast.SelectorExpr:
						p.calls[fun.Sel.Name] = append(p.calls[fun.Sel.Name], callSite{c, fd})
					}
				}
				return true
			})
		}
	}
	return p
}

// fixed reports whether e's text is decided by code alone, in fd (the function it appears in; nil at package level).
func (p *pkgFacts) fixed(e ast.Expr, fd *ast.FuncDecl) bool {
	if e == nil {
		return false
	}
	if v, ok := p.memo[e]; ok {
		return v
	}
	if p.visiting[e] {
		return false
	}
	p.visiting[e] = true
	v := p.fixedUncached(e, fd)
	delete(p.visiting, e)
	p.memo[e] = v
	return v
}

func (p *pkgFacts) fixedUncached(e ast.Expr, fd *ast.FuncDecl) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return true // string, number or character
	case *ast.ParenExpr:
		return p.fixed(v.X, fd)
	case *ast.BinaryExpr:
		return p.fixed(v.X, fd) && p.fixed(v.Y, fd)
	case *ast.CompositeLit:
		for _, el := range v.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			if !p.fixed(el, fd) {
				return false
			}
		}
		return true
	case *ast.Ident:
		return p.fixedIdent(v, fd)
	case *ast.CallExpr:
		return p.fixedCall(v, fd)
	}
	return false
}

func (p *pkgFacts) fixedIdent(id *ast.Ident, fd *ast.FuncDecl) bool {
	name := id.Name
	if name == "nil" || name == "true" || name == "false" {
		return true
	}
	// The parser resolved a name declared in this file to its declaration: one declared inside fd is a local, judged
	// by its own assignments, not by those of another variable of the same name in the function.
	if fd != nil && id.Obj != nil && id.Obj.Decl != nil {
		if d, ok := id.Obj.Decl.(ast.Node); ok && fd.Pos() <= d.Pos() && d.Pos() < fd.End() {
			return p.localFixed(id.Obj, fd)
		}
	}
	if p.consts[name] {
		return true
	}
	if inits, ok := p.vars[name]; ok {
		for _, init := range inits {
			if !p.fixed(init, nil) {
				return false
			}
		}
		return true
	}
	return false
}

// localFixed reports whether every binding of the local variable obj in fd is fixed: its parameter position, its
// assignments, its range clause, its declaration. A variable with no binding the analysis understands (a receiver,
// a closure parameter, a multi-value call) is not fixed.
func (p *pkgFacts) localFixed(obj *ast.Object, fd *ast.FuncDecl) bool {
	bound, fixed := false, true
	note := func(f bool) { bound = true; fixed = fixed && f }
	is := func(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && id.Obj == obj }
	idx := 0
	for _, field := range fd.Type.Params.List {
		for _, n := range field.Names {
			if n.Obj == obj {
				_, variadic := field.Type.(*ast.Ellipsis)
				note(!variadic && p.paramFixed(fd, idx))
			}
			idx++
		}
		if len(field.Names) == 0 {
			idx++
		}
	}
	if fd.Body != nil {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range s.Lhs {
					if is(lhs) {
						if len(s.Lhs) == len(s.Rhs) {
							note(p.fixed(s.Rhs[i], fd))
						} else {
							note(false) // a multi-value call
						}
					}
				}
			case *ast.RangeStmt:
				for _, x := range []ast.Expr{s.Key, s.Value} {
					if x != nil && is(x) {
						note(p.fixed(s.X, fd))
					}
				}
			case *ast.ValueSpec:
				for i, id := range s.Names {
					if id.Obj == obj {
						switch {
						case len(s.Values) == len(s.Names):
							note(p.fixed(s.Values[i], fd))
						case len(s.Values) == 0:
							note(true) // the zero value
						default:
							note(false)
						}
					}
				}
			}
			return true
		})
	}
	return bound && fixed
}

// paramFixed reports whether the idx'th parameter of fd gets fixed text at every call of fd. An exported function may be
// called from other packages, so its parameters are never fixed.
func (p *pkgFacts) paramFixed(fd *ast.FuncDecl, idx int) bool {
	if fd.Name.IsExported() {
		return false
	}
	sites := p.calls[fd.Name.Name]
	if len(sites) == 0 {
		return false
	}
	for _, cs := range sites {
		if sel, ok := cs.call.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && p.imported[id.Name] {
				continue // another package's function of the same name
			}
		}
		if idx >= len(cs.call.Args) || cs.call.Ellipsis.IsValid() && idx >= len(cs.call.Args)-1 || !p.fixed(cs.call.Args[idx], cs.in) {
			return false
		}
	}
	return true
}

func (p *pkgFacts) fixedCall(c *ast.CallExpr, fd *ast.FuncDecl) bool {
	allFixed := func() bool {
		for _, a := range c.Args {
			if !p.fixed(a, fd) {
				return false
			}
		}
		return true
	}
	switch fun := c.Fun.(type) {
	case *ast.Ident:
		switch fun.Name {
		case "string", "append":
			return allFixed()
		}
		return p.fixedLocalFunc(fun.Name, fd)
	case *ast.SelectorExpr:
		if id, ok := fun.X.(*ast.Ident); ok && p.imported[id.Name] {
			full := id.Name + "." + fun.Sel.Name
			switch {
			case numericText[full]:
				return true
			case full == "strings.Join" || full == "strings.Repeat" || full == "strings.TrimSuffix" || full == "strings.TrimPrefix" || full == "strings.ReplaceAll" || full == "fmt.Sprint":
				return allFixed()
			}
			return false
		}
		return p.fixedLocalFunc(fun.Sel.Name, fd)
	case *ast.ArrayType:
		return allFixed() // a conversion such as []string(x)
	}
	return false
}

// fixedLocalFunc reports whether every return of every function of this name in the package is fixed text.
func (p *pkgFacts) fixedLocalFunc(name string, _ *ast.FuncDecl) bool {
	decls := p.funcs[name]
	if len(decls) == 0 {
		return false
	}
	for _, d := range decls {
		if d.Body == nil || d.Type.Results == nil {
			return false
		}
		returns := 0
		ok := true
		ast.Inspect(d.Body, func(n ast.Node) bool {
			switch r := n.(type) {
			case *ast.FuncLit:
				return false // its returns are its own
			case *ast.ReturnStmt:
				returns++
				for _, res := range r.Results {
					if !p.fixed(res, d) {
						ok = false
					}
				}
				if len(r.Results) == 0 {
					ok = false
				}
			}
			return true
		})
		if !ok || returns == 0 {
			return false
		}
	}
	return true
}

// sourceFindings applies the guard to the files of one package.
func sourceFindings(fset *token.FileSet, files []sourceFile) ([]sourceFinding, map[string]bool) {
	facts := newPkgFacts(files)
	// literals are the string literals of a concatenation, in order.
	var literals func(e ast.Expr) []string
	literals = func(e ast.Expr) []string {
		switch v := e.(type) {
		case *ast.BasicLit:
			if s, err := strconv.Unquote(v.Value); err == nil && v.Kind == token.STRING {
				return []string{s}
			}
		case *ast.ParenExpr:
			return literals(v.X)
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				return append(literals(v.X), literals(v.Y)...)
			}
		}
		return nil
	}
	looksLikeSQL := func(parts []string) bool {
		for _, s := range parts {
			if sqlish.MatchString(s) {
				return true
			}
		}
		return false
	}
	var out []sourceFinding
	allowed := map[string]bool{} // allowlist entries that suppressed a finding
	for _, sf := range files {
		// htmlTemplate is the local name of html/template in this file ("" when it is not imported).
		htmlTemplate := ""
		for _, im := range sf.f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == "html/template" {
				htmlTemplate = "template"
				if im.Name != nil {
					htmlTemplate = im.Name.Name
				}
			}
		}
		for _, decl := range sf.f.Decls {
			fd, _ := decl.(*ast.FuncDecl)
			fn := "(package level)"
			if fd != nil {
				fn = fd.Name.Name
			}
			site := sf.rel + ":" + fn
			add := func(pos token.Pos, sql bool, what string) {
				if sql && sqlBuildAllowlist[site] != "" {
					allowed[site] = true
					return
				}
				out = append(out, sourceFinding{fset.Position(pos).String() + " (" + site + ")", what})
			}
			concatSeen := map[ast.Node]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CallExpr:
					sel, ok := v.Fun.(*ast.SelectorExpr)
					if !ok {
						break
					}
					if id, ok := sel.X.(*ast.Ident); ok && htmlTemplate != "" && id.Name == htmlTemplate && trustedTemplateTypes[sel.Sel.Name] {
						add(v.Pos(), false, "template."+sel.Sel.Name+" conversion marks text as trusted and skips html/template escaping")
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "fmt" && sel.Sel.Name == "Sprintf" && len(v.Args) > 0 && looksLikeSQL(literals(v.Args[0])) {
						add(v.Pos(), true, "SQL text built with fmt.Sprintf")
					}
					if i, ok := dbSinks[sel.Sel.Name]; ok && len(v.Args) > i && !facts.fixed(v.Args[i], fd) {
						add(v.Args[i].Pos(), true, "the query passed to "+sel.Sel.Name+" is not fixed text")
					}
				case *ast.BinaryExpr:
					if v.Op != token.ADD || concatSeen[v] {
						break
					}
					// The whole chain is judged once, from its outermost node.
					ast.Inspect(v, func(m ast.Node) bool {
						if b, ok := m.(*ast.BinaryExpr); ok && b.Op == token.ADD {
							concatSeen[b] = true
						}
						return true
					})
					if looksLikeSQL(literals(v)) && !facts.fixed(v, fd) {
						add(v.Pos(), true, "SQL text concatenated with an operand that is not fixed text")
					}
				case *ast.SelectorExpr:
					if v.Sel.Name == "RemoteAddr" {
						add(v.Pos(), false, "RemoteAddr is read: limits and logs must not depend on a client or proxy address")
					}
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if s, err := strconv.Unquote(v.Value); err == nil && untrustedHeader.MatchString(s) {
							add(v.Pos(), false, "header "+s+" is read or written: forwarded address and scheme headers are not trusted")
						}
					}
				}
				return true
			})
		}
	}
	return out, allowed
}

func TestSourceGuardNoTrustedCastsDynamicSQLOrForwardedHeaders(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	fset, dirs := parseSourceTree(t, root)
	keys := make([]string, 0, len(dirs))
	for d := range dirs {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	scanned := 0
	used := map[string]bool{}
	for _, d := range keys {
		scanned += len(dirs[d])
		found, allowed := sourceFindings(fset, dirs[d])
		for _, f := range found {
			t.Errorf("%s: %s", f.where, f.what)
		}
		for site := range allowed {
			used[site] = true
		}
	}
	for site := range sqlBuildAllowlist {
		if !used[site] {
			t.Errorf("sqlBuildAllowlist entry %s suppresses nothing: remove it", site)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d source files under %s: the guard is not looking at the tree", scanned, root)
	}
}

// TestSourceGuardRecognisesWhatItRefuses runs the guard over small sources, so that the scan above cannot pass
// merely because the matcher stopped matching.
func TestSourceGuardRecognisesWhatItRefuses(t *testing.T) {
	const src = `package p

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
)

const fixedQ = "SELECT 1 FROM t WHERE id=$1"

var table = "t"

func fragment(alias string) string { return "(" + alias + ".a=$1)" }

func callsFragment(db *DB, id string) {
	db.QueryRow(fixedQ, id)
	db.QueryRowContext(ctx, "SELECT a FROM t WHERE b=$1"+" AND c=$2", id, id)
	db.Exec("UPDATE "+table+" SET a=$1", id)
	db.Exec("DELETE FROM t WHERE "+fragment("o")+" AND x=$1", id)
	db.Exec("DELETE FROM t WHERE x<"+strconv.Itoa(3)+" AND y=$1", id)
	for _, col := range []string{"a", "b"} {
		db.Exec("UPDATE t SET "+col+"=NULL WHERE id=$1", id)
	}
	q := "SELECT 1"
	if id != "" {
		q += " WHERE id=$1"
	}
	db.Query(q, id)
	_ = fmt.Sprintf("hello %s", id)
	_ = "Request from " + id
	helper(db, "SELECT 1 WHERE x=$1")
}

func helper(db *DB, query string) { db.Query(query, 1) }

func casts(s string) {
	_ = template.HTML(s)
	_ = template.JS(s)
	_ = template.URL(s)
}

func sprintf(db *DB, col string) {
	q := fmt.Sprintf("SELECT %s FROM t", col)
	db.Query(q)
}

func concat(db *DB, id string) {
	db.Exec("DELETE FROM t WHERE id='" + id + "'")
}

func variable(db *DB, r *http.Request) {
	db.QueryContext(ctx, r.URL.Path)
}

func exported(db *DB, query string) { db.Query(query) }

func Exported(db *DB, query string) { db.Query(query) }

func calledWithRequestText(db *DB, r *http.Request) {
	helper2(db, r.FormValue("q"))
}

func helper2(db *DB, query string) { db.Query(query) }

func headers(r *http.Request) {
	_ = r.Header.Get("X-Forwarded-For")
	_ = r.Header.Get("x-real-ip")
	_ = r.RemoteAddr
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	found, _ := sourceFindings(fset, []sourceFile{{"p.go", f}})
	for _, fd := range found {
		site := fd.where[strings.Index(fd.where, "(")+1 : len(fd.where)-1]
		got[site]++
	}
	want := map[string]int{"p.go:casts": 3, "p.go:sprintf": 2, "p.go:concat": 2, "p.go:variable": 1, "p.go:headers": 3,
		"p.go:exported": 1, "p.go:Exported": 1, "p.go:helper2": 1}
	if len(got) != len(want) {
		t.Errorf("findings by function = %v, want %v", got, want)
	}
	for site, n := range want {
		if got[site] != n {
			t.Errorf("%s: %d findings, want %d (all: %v)", site, got[site], n, got)
		}
	}
}
