package observability

import (
	"fmt"
	"regexp"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// redactingEncoder wraps a zapcore.Encoder and redacts substrings matching any
// of the given regular expressions from every encoded log line. Patterns are
// compiled once at construction; matches are replaced with `[REDACTED]`.
type redactingEncoder struct {
	zapcore.Encoder
	patterns []*regexp.Regexp
}

// NewRedactingEncoder wraps inner with regex-based PII redaction. Patterns are
// Go regexp syntax (RE2). Returns error if any pattern fails to compile.
func NewRedactingEncoder(inner zapcore.Encoder, patterns []string) (zapcore.Encoder, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("compile %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return &redactingEncoder{Encoder: inner, patterns: compiled}, nil
}

// Clone preserves redaction patterns when zap clones encoders for goroutine safety.
func (r *redactingEncoder) Clone() zapcore.Encoder {
	return &redactingEncoder{
		Encoder:  r.Encoder.Clone(),
		patterns: r.patterns,
	}
}

// EncodeEntry runs the inner encoder, then applies regex redaction on the
// emitted bytes before returning the buffer.
func (r *redactingEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	buf, err := r.Encoder.EncodeEntry(ent, fields)
	if err != nil {
		return nil, err
	}
	if len(r.patterns) == 0 {
		return buf, nil
	}
	out := buf.Bytes()
	for _, re := range r.patterns {
		out = re.ReplaceAll(out, []byte("[REDACTED]"))
	}
	// zap reuses the buffer; we cannot just swap, so reset and rewrite.
	buf.Reset()
	_, _ = buf.Write(out)
	return buf, nil
}
