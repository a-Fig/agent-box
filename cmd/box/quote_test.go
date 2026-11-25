package main

import "testing"

func TestQuoteArgs(t *testing.T) {
	got := quoteArgs([]string{"mcp", "add", "box", "--", `C:\Users\Jane Doe\bin\box.exe`, "mcp"})
	want := `mcp add box -- "C:\Users\Jane Doe\bin\box.exe" mcp`
	if got != want {
		t.Fatalf("quoteArgs = %s, want %s", got, want)
	}
}
