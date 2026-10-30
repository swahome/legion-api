// Package legacy imports the LegionCore MySQL data into the Legion schema.
package legacy

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Row is one legacy row keyed by column name. A nil value is SQL NULL.
type Row map[string]*string

func (r Row) Str(col string) string {
	if v := r[col]; v != nil {
		return *v
	}
	return ""
}

// Ptr returns the value, or nil when it is NULL or blank.
func (r Row) Ptr(col string) *string {
	v := r[col]
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	s := strings.TrimSpace(*v)
	return &s
}

// Int parses an integer column. NULL, blank and junk read as 0.
func (r Row) Int(col string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(r.Str(col)), 10, 64)
	return n
}

// Flag reports whether the column holds 1 (legacy booleans are int or varchar).
func (r Row) Flag(col string) bool { return r.Int(col) == 1 }

// Tables holds the parsed rows of each requested table, in dump order.
type Tables map[string][]Row

// ReadDump parses the INSERT statements of a mysqldump / phpMyAdmin export
// and returns the rows of the named tables. Other tables are skipped.
func ReadDump(path string, want ...string) (Tables, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, t := range want {
		keep[t] = true
	}
	p := &parser{b: data}
	out := Tables{}
	for {
		table, cols, ok, err := p.nextInsert()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if !ok {
			return out, nil
		}
		rows, err := p.values(len(cols))
		if err != nil {
			return nil, fmt.Errorf("%s: table %s: %w", path, table, err)
		}
		if !keep[table] {
			continue
		}
		for _, vals := range rows {
			row := make(Row, len(cols))
			for i, c := range cols {
				row[c] = vals[i]
			}
			out[table] = append(out[table], row)
		}
	}
}

type parser struct {
	b   []byte
	pos int
}

var insertPrefix = []byte("INSERT INTO `")

// nextInsert advances to the next "INSERT INTO `t` (`a`, `b`) VALUES".
func (p *parser) nextInsert() (table string, cols []string, ok bool, err error) {
	for {
		i := bytes.Index(p.b[p.pos:], insertPrefix)
		if i < 0 {
			return "", nil, false, nil
		}
		start := p.pos + i
		// Only match at the start of a line, not inside a string value.
		if start > 0 && p.b[start-1] != '\n' {
			p.pos = start + len(insertPrefix)
			continue
		}
		p.pos = start + len(insertPrefix)
		end := bytes.IndexByte(p.b[p.pos:], '`')
		if end < 0 {
			return "", nil, false, fmt.Errorf("unterminated table name at byte %d", start)
		}
		table = string(p.b[p.pos : p.pos+end])
		p.pos += end + 1
		open := bytes.IndexByte(p.b[p.pos:], '(')
		closeIdx := bytes.Index(p.b[p.pos:], []byte(") VALUES"))
		if open < 0 || closeIdx < 0 || open > closeIdx {
			return "", nil, false, fmt.Errorf("table %s: INSERT without a column list", table)
		}
		for _, c := range strings.Split(string(p.b[p.pos+open+1:p.pos+closeIdx]), ",") {
			cols = append(cols, strings.Trim(strings.TrimSpace(c), "`"))
		}
		p.pos += closeIdx + len(") VALUES")
		return table, cols, true, nil
	}
}

// values parses "(…),(…);" tuples.
func (p *parser) values(ncols int) ([][]*string, error) {
	var rows [][]*string
	for {
		p.skipSpace()
		if p.pos >= len(p.b) {
			return nil, fmt.Errorf("unexpected end of file")
		}
		switch p.b[p.pos] {
		case ';':
			p.pos++
			return rows, nil
		case ',':
			p.pos++
			continue
		case '(':
			p.pos++
			row, err := p.tuple()
			if err != nil {
				return nil, err
			}
			if len(row) != ncols {
				return nil, fmt.Errorf("row has %d values, want %d", len(row), ncols)
			}
			rows = append(rows, row)
		default:
			return nil, fmt.Errorf("unexpected %q at byte %d", p.b[p.pos], p.pos)
		}
	}
}

func (p *parser) tuple() ([]*string, error) {
	var row []*string
	for {
		p.skipSpace()
		if p.pos >= len(p.b) {
			return nil, fmt.Errorf("unterminated row")
		}
		c := p.b[p.pos]
		switch {
		case c == ')':
			p.pos++
			return row, nil
		case c == ',':
			p.pos++
		case c == '\'':
			s, err := p.quoted()
			if err != nil {
				return nil, err
			}
			row = append(row, &s)
		default:
			start := p.pos
			for p.pos < len(p.b) && p.b[p.pos] != ',' && p.b[p.pos] != ')' {
				p.pos++
			}
			tok := strings.TrimSpace(string(p.b[start:p.pos]))
			if strings.EqualFold(tok, "NULL") {
				row = append(row, nil)
			} else {
				row = append(row, &tok)
			}
		}
	}
}

func (p *parser) quoted() (string, error) {
	p.pos++ // opening quote
	var sb strings.Builder
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		switch c {
		case '\\':
			if p.pos+1 >= len(p.b) {
				return "", fmt.Errorf("dangling escape")
			}
			n := p.b[p.pos+1]
			switch n {
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case '0':
				sb.WriteByte(0)
			case 'Z':
				sb.WriteByte(26)
			default:
				sb.WriteByte(n)
			}
			p.pos += 2
		case '\'':
			if p.pos+1 < len(p.b) && p.b[p.pos+1] == '\'' {
				sb.WriteByte('\'')
				p.pos += 2
				continue
			}
			p.pos++
			return sb.String(), nil
		default:
			sb.WriteByte(c)
			p.pos++
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func (p *parser) skipSpace() {
	for p.pos < len(p.b) {
		switch p.b[p.pos] {
		case ' ', '\n', '\r', '\t':
			p.pos++
		default:
			return
		}
	}
}
