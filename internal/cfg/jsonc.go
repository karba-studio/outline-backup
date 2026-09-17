package cfg

// stripComments removes // line comments and /* block */ comments from JSON,
// so a configuration file can explain itself to the next person who opens it.
//
// The scanner tracks string literals, because the single most common value in
// this file is a URL: naively cutting at the first "//" would turn
// "https://s3.example.com" into "https:. Escapes are tracked for the same
// reason, so a Windows path like "C:\\server\\outline" survives intact.
//
// Comment bytes are replaced with spaces rather than removed, which keeps every
// remaining byte at its original offset. That matters because encoding/json
// reports errors by offset, and an error pointing at the wrong line is worse
// than no comment support at all.
func stripComments(in []byte) []byte {
	out := make([]byte, len(in))
	copy(out, in)

	const (
		normal = iota
		inString
		inLine
		inBlock
	)
	state := normal
	escaped := false

	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case normal:
			switch {
			case c == '"':
				state = inString
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				out[i], out[i+1] = ' ', ' '
				i++
				state = inLine
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				out[i], out[i+1] = ' ', ' '
				i++
				state = inBlock
			}

		case inString:
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				state = normal
			}

		case inLine:
			// Newlines are kept so line numbers in error messages stay true.
			if c == '\n' {
				state = normal
				continue
			}
			out[i] = ' '

		case inBlock:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = normal
				continue
			}
			if c != '\n' {
				out[i] = ' '
			}
		}
	}
	return out
}
