package corpus

import (
	"strings"
	"testing"
)

func TestApplyReplacesOnlyTheFunction(t *testing.T) {
	mod := "import math\n\ndef f(x):\n    return x - 1\n\ndef g():\n    return 2\n"
	got := Apply(mod, "f", "def f(x):\n    return x + 1\n")
	want := "import math\n\ndef f(x):\n    return x + 1\n\ndef g():\n    return 2\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractFunctionFromFencedAnswer(t *testing.T) {
	answer := "Here is the fix:\n```python\ndef add(a, b):\n    return a + b\n```\nDone."
	if got := ExtractFunction(answer); got != "def add(a, b):\n    return a + b\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEveryIssueHasATestAndAFix(t *testing.T) {
	for _, i := range All() {
		if !strings.Contains(i.Module, "def "+i.Function+"(") || !strings.Contains(i.Fix, "def "+i.Function+"(") || i.Tests == "" {
			t.Fatalf("issue %s is incomplete", i.ID)
		}
	}
}
