package model

import "testing"

func TestQuoteNamePreservesJoinedEmojiAndEscapesLiteralText(t *testing.T) {
	name := "👩‍💻 " + `\u200d` + "\n\x1b\""
	want := `"👩‍💻 \\u200d\n\x1b\""`
	if got := QuoteName(name); got != want {
		t.Fatalf("QuoteName = %q, want %q", got, want)
	}
}
