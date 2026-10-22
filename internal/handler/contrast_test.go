package handler

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The palette lives in :root as light-dark(light, dark) pairs. This reads them
// out of the embedded stylesheet, so an edit to a colour is checked rather than
// trusted, and fails if a literal colour sneaks in outside that block.
func TestThemeContrast(t *testing.T) {
	b, err := staticFS.ReadFile("static/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(b), "")

	start := strings.Index(css, ":root {")
	if start < 0 {
		t.Fatal("no :root block")
	}
	end := start + strings.Index(css[start:], "\n}")
	root, rest := css[start:end], css[:start]+css[end:]

	type pair struct{ light, dark string }
	tokens := map[string]pair{}
	for _, m := range regexp.MustCompile(`--([a-z-]+):\s*light-dark\(\s*(#[0-9a-fA-F]{6})\s*,\s*(#[0-9a-fA-F]{6})\s*\)`).FindAllStringSubmatch(root, -1) {
		tokens[m[1]] = pair{m[2], m[3]}
	}

	checked := 0
	for _, p := range [][2]string{
		{"ink", "paper"}, {"ink", "paper-sunk"},
		{"ink-soft", "paper"}, {"ink-soft", "paper-sunk"},
		{"ink-faint", "paper"}, {"ink-faint", "paper-sunk"},
		{"accent-ink", "accent"}, {"accent-ink", "accent-hover"},
		{"warn", "paper"}, {"warn", "paper-sunk"},
		{"pending", "paper"}, {"pending", "paper-sunk"},
		{"highlight-ink", "highlight"},
	} {
		fg, ok1 := tokens[p[0]]
		bg, ok2 := tokens[p[1]]
		if !ok1 || !ok2 {
			t.Errorf("token missing for %s on %s", p[0], p[1])
			continue
		}
		for theme, c := range map[string][2]string{"light": {fg.light, bg.light}, "dark": {fg.dark, bg.dark}} {
			checked++
			if r := contrast(t, c[0], c[1]); r < 4.5 {
				t.Errorf("%s: %s (%s) on %s (%s) is %.2f:1, want at least 4.5", theme, p[0], c[0], p[1], c[1], r)
			}
		}
	}
	if checked < 26 {
		t.Fatalf("checked only %d colour pairs", checked)
	}

	if m := literalColour(rest); m != "" {
		t.Errorf("literal colour %q outside :root; use a token", m)
	}
}

// literalColour returns the first colour written as a value outside :root: a
// hex, a colour function, a named colour or a system colour, in any case.
// Tokens (var(--x)), transparent, currentcolor and the CSS-wide keywords are fine.
func literalColour(css string) string {
	hex := regexp.MustCompile(`(?i)#[0-9a-f]{3,8}\b`)
	fn := regexp.MustCompile(`(?i)\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(`)
	skip := regexp.MustCompile(`(?i)var\([^)]*\)|url\([^)]*\)`)
	// Whole hyphenated words, so background-color is one word and not "background"
	// and "color". Quoted text (content, grid-template-areas) is not a colour.
	word := regexp.MustCompile(`[A-Za-z-]+`)
	quoted := regexp.MustCompile(`"[^"]*"|'[^']*'`)
	// Properties whose values are names that may look like a colour: the font
	// shorthand's "menu", an animation or area called "mark", and so on.
	notColour := regexp.MustCompile(`^(?:font|font-family|content|quotes|will-change|transition(?:-.*)?|animation(?:-.*)?|grid(?:-.*)?|counter-.*|list-style(?:-type)?)$`)
	for _, d := range regexp.MustCompile(`(?s)(?:^|[;{}\s])([a-zA-Z-]+)\s*:\s*([^;{}]+)[;}]`).FindAllStringSubmatch(quoted.ReplaceAllString(css, `""`), -1) {
		if strings.HasPrefix(d[1], "--") || notColour.MatchString(strings.ToLower(d[1])) {
			continue
		}
		v := skip.ReplaceAllString(d[2], "")
		if m := hex.FindString(v); m != "" {
			return m
		}
		if m := fn.FindString(v); m != "" {
			return m
		}
		for _, w := range word.FindAllString(v, -1) {
			if namedColours[strings.ToLower(w)] {
				return w
			}
		}
	}
	return ""
}

// CSS named colours and system colours. transparent and currentcolor are
// deliberately absent: neither is a literal colour. The deprecated system colours
// that are also ordinary words (Background, Menu, Window, Scrollbar) are left out,
// as they would flag legitimate values.
var namedColours = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`aliceblue antiquewhite aqua aquamarine azure beige bisque black
	blanchedalmond blue blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue
	cornsilk crimson cyan darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki
	darkmagenta darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen darkslateblue
	darkslategray darkslategrey darkturquoise darkviolet deeppink deepskyblue dimgray dimgrey dodgerblue
	firebrick floralwhite forestgreen fuchsia gainsboro ghostwhite gold goldenrod gray green greenyellow
	grey honeydew hotpink indianred indigo ivory khaki lavender lavenderblush lawngreen lemonchiffon
	lightblue lightcoral lightcyan lightgoldenrodyellow lightgray lightgreen lightgrey lightpink
	lightsalmon lightseagreen lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime
	limegreen linen magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple mediumseagreen
	mediumslateblue mediumspringgreen mediumturquoise mediumvioletred midnightblue mintcream mistyrose
	moccasin navajowhite navy oldlace olive olivedrab orange orangered orchid palegoldenrod palegreen
	paleturquoise palevioletred papayawhip peachpuff peru pink plum powderblue purple rebeccapurple red
	rosybrown royalblue saddlebrown salmon sandybrown seagreen seashell sienna silver skyblue slateblue
	slategray slategrey snow springgreen steelblue tan teal thistle tomato turquoise violet wheat white
	whitesmoke yellow yellowgreen
	accentcolor accentcolortext activetext buttonborder buttonface buttontext canvas canvastext field
	fieldtext graytext highlight highlighttext linktext mark marktext selecteditem selecteditemtext
	visitedtext activeborder activecaption appworkspace buttonhighlight buttonshadow
	captiontext inactiveborder inactivecaption inactivecaptiontext infotext
	menutext threeddarkshadow threedface threedhighlight threedlightshadow threedshadow
	windowframe windowtext`) {
		m[w] = true
	}
	return m
}()

func contrast(t *testing.T, a, b string) float64 {
	la, lb := luminance(t, a), luminance(t, b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func luminance(t *testing.T, hex string) float64 {
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatalf("bad colour %q", hex)
		}
		x := float64(v) / 255
		if x <= 0.03928 {
			c[i] = x / 12.92
		} else {
			c[i] = math.Pow((x+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func TestLiteralColourCatchesWhatItShould(t *testing.T) {
	for _, bad := range []string{
		"a { color: red; }", "a { color: White; }", "a { color: CanvasText }", "a { color: #f00; }",
		"a { background: rgb(1 2 3); }", "a { background: color-mix(in oklab, var(--x) 8%, white); }",
		"a { border: 1px solid Tomato; }", "a { outline: 2px solid HIGHLIGHT; }",
		"a { color: GrayText; }", "a { color: #AbC; }", "a { color: RGB(1 2 3); }",
		"a { background: color-mix(in oklab, var(--x), Red); }",
		"@media (min-width: 1px) { a { color: navy } }", "a { background: black }",
	} {
		if literalColour(bad) == "" {
			t.Errorf("not caught: %s", bad)
		}
	}
	for _, ok := range []string{
		"a { color: var(--highlight); }", "a { background: transparent; border-color: currentcolor; }",
		"a { background: color-mix(in oklab, var(--warn) 8%, var(--paper-sunk)); }",
		"a { white-space: nowrap; display: inline-flex; }", "a { border: 0; color: inherit; }",
		"a { transition: background-color 200ms; }", "a { font: menu; }",
		`a { grid-template-areas: "menu main"; }`, `a::after { content: "Mark"; }`,
		"a { transition-property: color, background-color, border-color; }",
	} {
		if m := literalColour(ok); m != "" {
			t.Errorf("false positive %q in: %s", m, ok)
		}
	}
}
