package main

import "testing"

func TestPathEdit(t *testing.T) {
	dir := `E:\Ferry`
	cases := []struct{ in, add, remove string }{
		{"", `E:\Ferry`, ""},
		{`C:\a;C:\b`, `C:\a;C:\b;E:\Ferry`, `C:\a;C:\b`},
		{`C:\a;`, `C:\a;E:\Ferry`, `C:\a;`},
		{`C:\a;e:\ferry\;C:\b`, `C:\a;e:\ferry\;C:\b`, `C:\a;C:\b`},
		{`"E:\Ferry";%USERPROFILE%\bin`, `"E:\Ferry";%USERPROFILE%\bin`, `%USERPROFILE%\bin`},
		{`E:\Ferry2`, `E:\Ferry2;E:\Ferry`, `E:\Ferry2`},
	}
	for _, c := range cases {
		if got := pathAdd(c.in, dir); got != c.add {
			t.Errorf("pathAdd(%q) = %q, want %q", c.in, got, c.add)
		}
		if got := pathRemove(c.in, dir); got != c.remove {
			t.Errorf("pathRemove(%q) = %q, want %q", c.in, got, c.remove)
		}
	}
}
