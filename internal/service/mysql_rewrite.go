// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// Native dumps can retain schema-qualified triggers/views. Rewrite identifiers,
// including SQL in versioned comments, while preserving string literals and data.
// Dynamic SQL inside string literals is deliberately not rewritten.
func rewriteSQLSchema(input io.Reader, output io.Writer, source, target string) error {
	r := bufio.NewReader(input)
	w := bufio.NewWriter(output)
	put := func(b byte) error { return w.WriteByte(b) }
	qualified := func() (bool, error) {
		for i := 1; i <= 4096; i++ {
			b, e := r.Peek(i)
			if e != nil {
				if e == io.EOF {
					return false, nil
				}
				return false, e
			}
			last := b[i-1]
			if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
				continue
			}
			return last == '.', nil
		}
		return false, errors.New("SQL identifier lookahead exceeds safe bound")
	}
	ident := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '$'
	}
	for {
		b, e := r.ReadByte()
		if e == io.EOF {
			return w.Flush()
		}
		if e != nil {
			return e
		}
		if b == '"' {
			peek, _ := r.Peek(len(source) + 2)
			if len(peek) >= len(source)+1 && string(peek[:len(source)+1]) == source+"\"" && (len(peek) == len(source)+1 || peek[len(source)+1] != '"') {
				for range len(source) + 1 {
					_, _ = r.ReadByte()
				}
				q, err := qualified()
				if err != nil {
					return err
				}
				text := "\"" + source + "\""
				if q {
					text = "`" + target + "`"
				}
				if _, e = w.WriteString(text); e != nil {
					return e
				}
				continue
			}
		}
		if b == '\'' || b == '"' || b == '`' {
			quote := b
			var token strings.Builder
			quotedIdent := quote == '`'
			if !quotedIdent {
				if e = put(b); e != nil {
					return e
				}
			}
			for {
				c, err := r.ReadByte()
				if err != nil {
					return errors.New("unterminated SQL quoted value")
				}
				if quotedIdent {
					token.WriteByte(c)
					if token.Len() > 4096 {
						return errors.New("SQL quoted identifier too long")
					}
				} else {
					if e = put(c); e != nil {
						return e
					}
				}
				if c == '\\' {
					next, err := r.ReadByte()
					if err != nil {
						return err
					}
					if quotedIdent {
						token.WriteByte(next)
					} else {
						if e = put(next); e != nil {
							return e
						}
					}
					continue
				}
				if c == quote {
					next, _ := r.Peek(1)
					if len(next) > 0 && next[0] == quote {
						r.ReadByte()
						if quotedIdent {
							token.WriteByte(quote)
						} else {
							if e = put(quote); e != nil {
								return e
							}
						}
						continue
					}
					break
				}
			}
			if quotedIdent {
				raw := token.String()
				value := strings.ReplaceAll(strings.TrimSuffix(raw, "`"), "``", "`")
				q, err := qualified()
				if err != nil {
					return err
				}
				if value == source && q {
					raw = target + "`"
				}
				if _, e = w.WriteString("`" + raw); e != nil {
					return e
				}
			}
			continue
		}
		if ident(b) {
			var token strings.Builder
			token.WriteByte(b)
			for {
				next, _ := r.Peek(1)
				if len(next) == 0 || !ident(next[0]) {
					break
				}
				r.ReadByte()
				token.WriteByte(next[0])
				if token.Len() > 4096 {
					return errors.New("SQL identifier too long")
				}
			}
			text := token.String()
			if text == source {
				q, err := qualified()
				if err != nil {
					return err
				}
				if q {
					text = "`" + target + "`"
				}
			}
			if _, e = w.WriteString(text); e != nil {
				return e
			}
			continue
		}
		if b == '#' {
			if e = put(b); e != nil {
				return e
			}
			for {
				c, err := r.ReadByte()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				if e = put(c); e != nil {
					return e
				}
				if c == '\n' {
					break
				}
			}
			continue
		}
		if b == '-' {
			peek, _ := r.Peek(2)
			if len(peek) == 2 && peek[0] == '-' && (peek[1] == ' ' || peek[1] == '\t' || peek[1] == '\n') {
				if e = put(b); e != nil {
					return e
				}
				for {
					c, err := r.ReadByte()
					if err == io.EOF {
						break
					}
					if err != nil {
						return err
					}
					if e = put(c); e != nil {
						return e
					}
					if c == '\n' {
						break
					}
				}
				continue
			}
		}
		if b == '/' {
			peek, _ := r.Peek(3)
			if len(peek) >= 2 && peek[0] == '*' && peek[1] != '!' && !(len(peek) == 3 && peek[1] == 'M' && peek[2] == '!') {
				if e = put(b); e != nil {
					return e
				}
				previous := byte(0)
				for {
					c, err := r.ReadByte()
					if err != nil {
						return errors.New("unterminated SQL comment")
					}
					if e = put(c); e != nil {
						return e
					}
					if previous == '*' && c == '/' {
						break
					}
					previous = c
				}
				continue
			}
		}
		if e = put(b); e != nil {
			return e
		}
	}
}
