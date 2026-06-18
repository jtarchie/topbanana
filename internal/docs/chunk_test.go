package docs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func headingsOf(cs []Chunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Heading
	}
	return out
}

func TestChunkSource_FoldsClassNamesAndSplitsComponents(t *testing.T) {
	src := sourceDef{Name: "daisyUI", ID: "daisyui", Body: strings.Join([]string{
		"# daisyUI 5",
		"intro line",
		"## components",
		"### button",
		"Buttons do things",
		"#### Class names",
		"- component: `btn`",
		"- color: `btn-primary`",
		"### badge",
		"Badges show status",
		"#### Class names",
		"- component: `badge`",
	}, "\n")}

	chunks := chunkSource(src)
	byHeading := make(map[string]Chunk, len(chunks))
	for _, c := range chunks {
		byHeading[c.Heading] = c
	}

	btn, ok := byHeading["button"]
	if !ok {
		t.Fatalf("no button chunk; got headings %v", headingsOf(chunks))
	}
	// The #### Class names subsection folds into the button body.
	if !strings.Contains(btn.Body, "btn-primary") || !strings.Contains(btn.Body, "Class names") {
		t.Errorf("button body missing folded class names: %q", btn.Body)
	}
	if btn.Breadcrumb != "daisyUI > daisyUI 5 > components > button" {
		t.Errorf("button breadcrumb = %q", btn.Breadcrumb)
	}
	if btn.componentClass != "btn" {
		t.Errorf("button componentClass = %q, want btn", btn.componentClass)
	}
	// badge is a separate chunk, not merged into button.
	if strings.Contains(btn.Body, "Badges show status") {
		t.Errorf("button chunk leaked badge content: %q", btn.Body)
	}
	if _, ok := byHeading["badge"]; !ok {
		t.Errorf("no badge chunk; got %v", headingsOf(chunks))
	}
}

func TestChunkSource_CodeFenceHeadingsAreInert(t *testing.T) {
	src := sourceDef{Name: "daisyUI", ID: "daisyui", Body: strings.Join([]string{
		"### card",
		"A card",
		"```html",
		"### not a heading",
		`<div class="card"></div>`,
		"```",
		"after fence",
	}, "\n")}

	chunks := chunkSource(src)
	if len(chunks) != 1 {
		t.Fatalf("fence heading split the chunk: got %d chunks %v", len(chunks), headingsOf(chunks))
	}
	if !strings.Contains(chunks[0].Body, "### not a heading") {
		t.Errorf("fenced text dropped: %q", chunks[0].Body)
	}
}

func TestChunkSource_StripsFrontmatter(t *testing.T) {
	src := sourceDef{Name: "daisyUI", ID: "daisyui", Body: strings.Join([]string{
		"---",
		"name: daisyui",
		"secret: should-not-appear",
		"---",
		"### alert",
		"Alerts inform",
	}, "\n")}

	chunks := chunkSource(src)
	for _, c := range chunks {
		if strings.Contains(c.Body, "should-not-appear") {
			t.Errorf("frontmatter leaked into chunk body: %q", c.Body)
		}
	}
	if len(chunks) != 1 || chunks[0].Heading != "alert" {
		t.Fatalf("want one alert chunk, got %v", headingsOf(chunks))
	}
}

func TestChunkSource_TruncatesRunawaySection(t *testing.T) {
	huge := strings.Repeat("padding word ", maxChunkSourceBytes) // >> cap
	src := sourceDef{Name: "daisyUI", ID: "daisyui", Body: "### big\n" + huge}
	chunks := chunkSource(src)
	if len(chunks) != 1 {
		t.Fatalf("want one chunk, got %d", len(chunks))
	}
	if len(chunks[0].Body) > maxChunkSourceBytes {
		t.Errorf("section body not capped: %d > %d", len(chunks[0].Body), maxChunkSourceBytes)
	}
}

func TestCapBytes(t *testing.T) {
	got, truncated := capBytes(strings.Repeat("a", 100), 40)
	if !truncated || len(got) > 40 {
		t.Errorf("capBytes(100, 40) = len %d trunc %v", len(got), truncated)
	}
	if got, truncated := capBytes("short", 40); truncated || got != "short" {
		t.Errorf("capBytes should not cut short input: %q %v", got, truncated)
	}
	// Multibyte safety: a cut must never split a rune.
	got, _ = capBytes(strings.Repeat("é", 50), 35) // "é" is 2 bytes
	if !utf8.ValidString(got) {
		t.Errorf("capBytes split a rune: %q", got)
	}
}
