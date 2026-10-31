// Package corpus holds the software issues the agents work on: each is a
// small Python module with one bug, the tests that fail because of it, and,
// for the stand-in model only, a correct fix and a plausible wrong one.
package corpus

import "fmt"

// Issue is one task: fix Function in Module so Tests pass.
type Issue struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Function string `json:"function"`
	Module   string `json:"module"` // the buggy module's full source
	Tests    string `json:"tests"`  // runs against module.py; exits non-zero on failure
	// Fix and WrongFix are full replacements of Function, used only by the
	// stand-in model. WrongFix is what it proposes first for issues that
	// exercise the retry round; a real model sees neither.
	Fix      string `json:"-"`
	WrongFix string `json:"-"`
}

// NeedsSecondRound reports whether the stand-in model proposes the wrong fix
// first for this issue, so the verification-and-retry round runs.
func (i Issue) NeedsSecondRound() bool { return i.WrongFix != "" }

var issues = []Issue{
	{
		ID: "add-sign", Title: "add() subtracts instead of adding", Function: "add",
		Module:   "def add(a, b):\n    return a - b\n",
		Tests:    "from module import add\nassert add(2, 3) == 5\nassert add(-1, 1) == 0\nprint('ok')\n",
		Fix:      "def add(a, b):\n    return a + b\n",
		WrongFix: "def add(a, b):\n    return abs(a - b)\n",
	},
	{
		ID: "is-even", Title: "is_even() is true for odd numbers", Function: "is_even",
		Module: "def is_even(n):\n    return n % 2 == 1\n",
		Tests:  "from module import is_even\nassert is_even(4)\nassert not is_even(7)\nassert is_even(0)\nprint('ok')\n",
		Fix:    "def is_even(n):\n    return n % 2 == 0\n",
	},
	{
		ID: "max-of", Title: "max_of() returns the smallest item", Function: "max_of",
		Module: "def max_of(items):\n    best = items[0]\n    for x in items:\n        if x < best:\n            best = x\n    return best\n",
		Tests:  "from module import max_of\nassert max_of([3, 9, 2]) == 9\nassert max_of([-5]) == -5\nprint('ok')\n",
		Fix:    "def max_of(items):\n    best = items[0]\n    for x in items:\n        if x > best:\n            best = x\n    return best\n",
	},
	{
		ID: "reverse-words", Title: "reverse_words() doesn't reverse", Function: "reverse_words",
		Module:   "def reverse_words(s):\n    return ' '.join(s.split())\n",
		Tests:    "from module import reverse_words\nassert reverse_words('a b c') == 'c b a'\nassert reverse_words('one') == 'one'\nprint('ok')\n",
		Fix:      "def reverse_words(s):\n    return ' '.join(reversed(s.split()))\n",
		WrongFix: "def reverse_words(s):\n    return s[::-1]\n",
	},
	{
		ID: "clamp", Title: "clamp() mixes up its bounds", Function: "clamp",
		Module: "def clamp(x, lo, hi):\n    return max(hi, min(lo, x))\n",
		Tests:  "from module import clamp\nassert clamp(5, 0, 10) == 5\nassert clamp(-3, 0, 10) == 0\nassert clamp(42, 0, 10) == 10\nprint('ok')\n",
		Fix:    "def clamp(x, lo, hi):\n    return max(lo, min(hi, x))\n",
	},
	{
		ID: "count-vowels", Title: "count_vowels() misses 'u'", Function: "count_vowels",
		Module:   "def count_vowels(s):\n    return sum(1 for c in s.lower() if c in 'aeio')\n",
		Tests:    "from module import count_vowels\nassert count_vowels('queue') == 4\nassert count_vowels('xyz') == 0\nassert count_vowels('AEIOU') == 5\nprint('ok')\n",
		Fix:      "def count_vowels(s):\n    return sum(1 for c in s.lower() if c in 'aeiou')\n",
		WrongFix: "def count_vowels(s):\n    return sum(1 for c in s if c in 'aeiou')\n",
	},
	{
		ID: "fizzbuzz", Title: "fizzbuzz() checks 5 before 15", Function: "fizzbuzz",
		Module: "def fizzbuzz(n):\n    if n % 5 == 0:\n        return 'Buzz'\n    if n % 3 == 0:\n        return 'Fizz'\n    if n % 15 == 0:\n        return 'FizzBuzz'\n    return str(n)\n",
		Tests:  "from module import fizzbuzz\nassert fizzbuzz(15) == 'FizzBuzz'\nassert fizzbuzz(9) == 'Fizz'\nassert fizzbuzz(10) == 'Buzz'\nassert fizzbuzz(7) == '7'\nprint('ok')\n",
		Fix:    "def fizzbuzz(n):\n    if n % 15 == 0:\n        return 'FizzBuzz'\n    if n % 5 == 0:\n        return 'Buzz'\n    if n % 3 == 0:\n        return 'Fizz'\n    return str(n)\n",
	},
	{
		ID: "mean", Title: "mean() divides by the wrong count", Function: "mean",
		Module: "def mean(xs):\n    return sum(xs) / (len(xs) + 1)\n",
		Tests:  "from module import mean\nassert mean([2, 4, 6]) == 4\nassert mean([5]) == 5\nprint('ok')\n",
		Fix:    "def mean(xs):\n    return sum(xs) / len(xs)\n",
	},
}

// All returns every issue.
func All() []Issue { return append([]Issue(nil), issues...) }

// Get returns the issue with id.
func Get(id string) (Issue, error) {
	for _, i := range issues {
		if i.ID == id {
			return i, nil
		}
	}
	return Issue{}, fmt.Errorf("unknown issue %q", id)
}
