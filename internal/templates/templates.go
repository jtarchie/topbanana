package templates

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// SiteTemplate is one of the presets a user can pick when starting a new build.
//
// A template expresses itself across the three surfaces the agent already sees:
// the system prompt (PromptAddendum), the filesystem (Skeleton, pre-written
// before the agent runs), and the lint/retry loop (Checks). When
// EnablesFunctions is true, the build additionally exposes the
// write_function/read_function/list_functions tools and the /api/* router
// for this slug — opt-in so existing brochure templates are byte-for-byte
// unchanged.
type SiteTemplate struct {
	ID               string
	Label            string
	Description      string
	PromptAddendum   string
	Skeleton         map[string]string
	Examples         map[string]string
	Checks           []Check
	EnablesFunctions bool
}

// Check is a declarative invariant for a generated file. The lint loop runs
// these alongside the structural HTML checks so any failure becomes a concrete
// fix-prompt for the agent to address on retry.
type Check struct {
	File        string   `json:"file"`
	MustContain []string `json:"must_contain"`
	Message     string   `json:"message"`
}

const (
	defaultID = "blank"
	root      = "sites"
)

//go:embed sites
var templatesFS embed.FS

type templateMeta struct {
	Label            string  `json:"label"`
	Description      string  `json:"description"`
	Checks           []Check `json:"checks,omitempty"`
	EnablesFunctions bool    `json:"enables_functions,omitempty"`
}

var (
	allTemplates []*SiteTemplate
	byID         map[string]*SiteTemplate
)

func init() {
	loaded, err := loadAll()
	if err != nil {
		panic(fmt.Errorf("load site templates: %w", err))
	}
	allTemplates = loaded
	byID = make(map[string]*SiteTemplate, len(loaded))
	for _, t := range loaded {
		byID[t.ID] = t
	}
	if byID[defaultID] == nil {
		panic(fmt.Errorf("default site template %q is missing", defaultID))
	}
}

// All returns the registry in stable order: the default first, then the rest
// alphabetically.
func All() []*SiteTemplate {
	return allTemplates
}

// Get looks up a template by id. Unknown ids fall back to the default
// ("blank") so a stale form value never breaks a build.
func Get(id string) *SiteTemplate {
	if t, ok := byID[id]; ok {
		return t
	}
	return byID[defaultID]
}

func loadAll() ([]*SiteTemplate, error) {
	entries, err := fs.ReadDir(templatesFS, root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	tmpls := make([]*SiteTemplate, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := loadOne(e.Name())
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", e.Name(), err)
		}
		tmpls = append(tmpls, t)
	}

	sort.SliceStable(tmpls, func(i, j int) bool {
		switch {
		case tmpls[i].ID == defaultID:
			return true
		case tmpls[j].ID == defaultID:
			return false
		default:
			return tmpls[i].ID < tmpls[j].ID
		}
	})

	return tmpls, nil
}

func loadOne(id string) (*SiteTemplate, error) {
	promptPath := path.Join(root, id, "prompt.md")
	raw, err := fs.ReadFile(templatesFS, promptPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", promptPath, err)
	}

	meta, body, err := parseFrontmatter(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", promptPath, err)
	}

	skeleton, err := loadSkeleton(id)
	if err != nil {
		return nil, fmt.Errorf("load skeleton: %w", err)
	}

	examples, err := loadExamples(id)
	if err != nil {
		return nil, fmt.Errorf("load examples: %w", err)
	}

	return &SiteTemplate{
		ID:               id,
		Label:            meta.Label,
		Description:      meta.Description,
		PromptAddendum:   strings.TrimSpace(body),
		Skeleton:         skeleton,
		Examples:         examples,
		Checks:           meta.Checks,
		EnablesFunctions: meta.EnablesFunctions,
	}, nil
}

func loadSkeleton(id string) (map[string]string, error) {
	return loadDir(id, "skeleton")
}

// loadExamples reads aspirational reference HTML pages from
// sites/{id}/examples. Unlike skeletons (which are seeded onto the
// filesystem before the agent runs), examples are surfaced to the model
// through synthetic read_example tool calls so they act as few-shot
// "what good looks like" references without being written to the site.
func loadExamples(id string) (map[string]string, error) {
	return loadDir(id, "examples")
}

// loadDir reads every file under sites/{id}/{sub} into a map keyed by the
// path relative to that subdirectory. Missing directory returns an empty
// map (templates aren't required to ship skeletons or examples).
func loadDir(id, sub string) (map[string]string, error) {
	base := path.Join(root, id, sub)
	out := make(map[string]string)

	_, err := fs.Stat(templatesFS, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("stat %s: %w", base, err)
	}

	err = fs.WalkDir(templatesFS, base, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", p, walkErr)
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, base+"/")
		b, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", base, err)
	}
	return out, nil
}

// parseFrontmatter reads a `---\n{json}\n---\n` header followed by a markdown
// body. JSON (rather than YAML) keeps the parser dependency-free.
func parseFrontmatter(raw string) (templateMeta, string, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	const open = "---\n"
	if !strings.HasPrefix(raw, open) {
		return templateMeta{}, raw, nil
	}
	rest := raw[len(open):]

	const close1 = "\n---\n"
	const close2 = "\n---"

	var fmText, body string
	if i := strings.Index(rest, close1); i >= 0 {
		fmText = rest[:i]
		body = rest[i+len(close1):]
	} else if strings.HasSuffix(rest, close2) {
		fmText = rest[:len(rest)-len(close2)]
		body = ""
	} else {
		return templateMeta{}, "", errors.New("frontmatter not closed")
	}

	var meta templateMeta
	if strings.TrimSpace(fmText) != "" {
		err := json.Unmarshal([]byte(fmText), &meta)
		if err != nil {
			return templateMeta{}, "", fmt.Errorf("frontmatter JSON: %w", err)
		}
	}
	return meta, body, nil
}
