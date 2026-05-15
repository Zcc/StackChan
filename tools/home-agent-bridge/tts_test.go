package main

import "testing"

func TestResample24to16_Empty(t *testing.T) {
	out := resample24to16(nil)
	if out != nil {
		t.Fatalf("expected nil, got %v", out)
	}
}

func TestResample24to16_Ratio(t *testing.T) {
	// 3 input samples → 2 output samples
	in := make([]int16, 300)
	for i := range in {
		in[i] = int16(i)
	}
	out := resample24to16(in)
	expected := 200
	if len(out) != expected {
		t.Fatalf("expected %d samples, got %d", expected, len(out))
	}
}

func TestResample24to16_Values(t *testing.T) {
	// 6 samples at 24kHz → 4 samples at 16kHz
	// indices: 0, 1.5, 3, 4.5
	// values at those positions (linear interp):
	// idx 0 → 100, idx 1.5 → avg(200,300)=250, idx 3 → 400, idx 4.5 → avg(500,600)=550
	in := []int16{100, 200, 300, 400, 500, 600}
	out := resample24to16(in)
	if len(out) != 4 {
		t.Fatalf("expected 4 samples, got %d", len(out))
	}
	want := []int16{100, 250, 400, 550}
	for i, v := range want {
		if out[i] != v {
			t.Errorf("out[%d] = %d, want %d", i, out[i], v)
		}
	}
}

func TestEscapeXML(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"a & b", "a &amp; b"},
		{"<tag>", "&lt;tag&gt;"},
		{`"quote"`, "&quot;quote&quot;"},
		{"it's", "it&apos;s"},
	}
	for _, tc := range cases {
		got := escapeXML(tc.in)
		if got != tc.want {
			t.Errorf("escapeXML(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGenerateUUID(t *testing.T) {
	id := generateUUID()
	if len(id) != 32 {
		t.Errorf("expected 32-char UUID, got %d chars: %s", len(id), id)
	}
	// Should be unique
	id2 := generateUUID()
	if id == id2 {
		t.Error("two UUIDs should not be equal")
	}
}
