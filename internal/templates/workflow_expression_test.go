package templates_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
)

// evalIf evaluates a job's if: condition the way GitHub Actions does, for
// the subset of its expression syntax the reusable deploy workflow uses:
// string literals, property paths (with the .*. filter), ==, !=, !, &&,
// ||, parentheses, contains() and startsWith(). A missing property is
// null, which is not equal to any string. It is enough to check which jobs
// an event runs without GitHub, and fails the test on anything else.
func evalIf(t *testing.T, expr string, ctx map[string]any) bool {
	t.Helper()
	expr = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(expr), "${{"), "}}"))
	p := &exprParser{t: t, tokens: tokenize(t, expr), ctx: ctx}
	v := p.or()
	if p.pos != len(p.tokens) {
		t.Fatalf("if: %q: unexpected %q", expr, p.tokens[p.pos])
	}
	return truthy(v)
}

func tokenize(t *testing.T, s string) []string {
	t.Helper()
	var tokens []string
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				t.Fatalf("unterminated string in %q", s)
			}
			tokens = append(tokens, s[i:i+j+2])
			i += j + 2
		case strings.HasPrefix(s[i:], "==") || strings.HasPrefix(s[i:], "!=") || strings.HasPrefix(s[i:], "&&") || strings.HasPrefix(s[i:], "||"):
			tokens = append(tokens, s[i:i+2])
			i += 2
		case strings.ContainsRune("()!,", c):
			tokens = append(tokens, string(c))
			i++
		default:
			j := i
			for j < len(s) && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) || strings.ContainsRune("._-*", rune(s[j]))) {
				j++
			}
			if j == i {
				t.Fatalf("unexpected %q in %q", s[i:], s)
			}
			tokens = append(tokens, s[i:j])
			i = j
		}
	}
	return tokens
}

type exprParser struct {
	t      *testing.T
	tokens []string
	pos    int
	ctx    map[string]any
}

func (p *exprParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *exprParser) next() string {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *exprParser) expect(tok string) {
	if got := p.next(); got != tok {
		p.t.Fatalf("expected %q, got %q", tok, got)
	}
}

func (p *exprParser) or() any {
	v := p.and()
	for p.peek() == "||" {
		p.next()
		r := p.and()
		v = truthy(v) || truthy(r)
	}
	return v
}

func (p *exprParser) and() any {
	v := p.comparison()
	for p.peek() == "&&" {
		p.next()
		r := p.comparison()
		v = truthy(v) && truthy(r)
	}
	return v
}

func (p *exprParser) comparison() any {
	v := p.unary()
	switch p.peek() {
	case "==":
		p.next()
		return equal(v, p.unary())
	case "!=":
		p.next()
		return !equal(v, p.unary())
	}
	return v
}

func (p *exprParser) unary() any {
	if p.peek() == "!" {
		p.next()
		return !truthy(p.unary())
	}
	return p.primary()
}

func (p *exprParser) primary() any {
	tok := p.next()
	switch {
	case tok == "(":
		v := p.or()
		p.expect(")")
		return v
	case strings.HasPrefix(tok, "'"):
		return strings.Trim(tok, "'")
	case tok == "true", tok == "false":
		return tok == "true"
	case p.peek() == "(":
		p.next()
		var args []any
		for p.peek() != ")" {
			args = append(args, p.or())
			if p.peek() == "," {
				p.next()
			}
		}
		p.expect(")")
		return call(p.t, tok, args)
	default:
		return lookupPath(p.ctx, strings.Split(tok, "."))
	}
}

func call(t *testing.T, name string, args []any) any {
	switch name {
	case "contains":
		if list, ok := args[0].([]any); ok {
			for _, item := range list {
				if equal(item, args[1]) {
					return true
				}
			}
			return false
		}
		s, _ := args[0].(string)
		sub, _ := args[1].(string)
		return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
	case "startsWith":
		s, _ := args[0].(string)
		prefix, _ := args[1].(string)
		return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
	}
	t.Fatalf("unsupported function %s", name)
	return nil
}

func lookupPath(v any, path []string) any {
	for i, key := range path {
		if key == "*" {
			list, _ := v.([]any)
			var out []any
			for _, item := range list {
				out = append(out, lookupPath(item, path[i+1:]))
			}
			return out
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		// GitHub compares strings ignoring case.
		return strings.EqualFold(as, bs)
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	default:
		return true
	}
}
