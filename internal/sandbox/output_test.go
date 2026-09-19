package sandbox

import (
	"strings"
	"testing"
)

func TestOutputBufferConsumesOverflowAndKeepsPrefix(t *testing.T) {
	for _, limit := range []int{0, 5, 9, 10} {
		output := OutputBuffer{Limit: limit}
		for _, chunk := range []string{"abc", "defghi"} {
			n, err := output.Write([]byte(chunk))
			if err != nil || n != len(chunk) {
				t.Fatalf("limit %d: output was not consumed: n=%d err=%v", limit, n, err)
			}
		}
		want := "abcdefghi"
		truncated := limit > 0 && limit < len(want)
		if truncated {
			want = want[:limit]
		}
		if output.String() != want || output.Truncated != truncated {
			t.Fatalf("limit %d: got %q, truncated=%t", limit, output.String(), output.Truncated)
		}
	}

	output := OutputBuffer{Limit: MaxCommandOutputBytes}
	chunk := []byte(strings.Repeat("x", 4096))
	for range 4096 {
		if n, err := output.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("overflow stopped draining: n=%d err=%v", n, err)
		}
	}
	if len(output.data) != MaxCommandOutputBytes || cap(output.data) > 2*MaxCommandOutputBytes || !output.Truncated {
		t.Fatalf("capture grew with output: length=%d capacity=%d truncated=%t", len(output.data), cap(output.data), output.Truncated)
	}
}
