package sandbox

// OutputBuffer captures a prefix while consuming all writes, so a full buffer
// never blocks process output. Limit <= 0 preserves unrestricted internal exec.
// Each output stream must have its own buffer and a single writer.
type OutputBuffer struct {
	Limit     int
	Truncated bool
	data      []byte
}

func (b *OutputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Limit > 0 {
		p = p[:min(len(p), b.Limit-len(b.data))]
	}
	b.data = append(b.data, p...)
	b.Truncated = b.Truncated || len(p) < n
	return n, nil
}

func (b *OutputBuffer) String() string { return string(b.data) }
