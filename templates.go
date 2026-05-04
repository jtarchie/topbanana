package main

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
// before the agent runs), and the lint/retry loop (Checks).
type SiteTemplate struct {
	ID             string
	Label          string
	Description    string
	PromptAddendum string
	Skeleton       map[string]string
	Checks         []TemplateCheck
}

// TemplateCheck is a declarative invariant for a generated file. The lint loop
// runs these alongside the structural HTML checks so any failure becomes a
// concrete fix-prompt for the agent to address on retry.
type TemplateCheck struct {
	File        string   `json:"file"`
	MustContain []string `json:"must_contain"`
	Message     string   `json:"message"`
}

const (
	defaultTemplateID = "blank"
	templatesRoot     = "templates/sites"
)

//go:embed templates/sites
var templatesFS embed.FS

type templateMeta struct {
	Label       string          `json:"label"`
	Description string          `json:"description"`
	Checks      []TemplateCheck `json:"checks,omitempty"`
}

var (
	siteTemplatesAll []*SiteTemplate
	siteTemplateByID map[string]*SiteTemplate
)

func init() {
	all, err := loadSiteTemplates()
	if err != nil {
		panic(fmt.Errorf("load site templates: %w", err))
	}
	siteTemplatesAll = all
	siteTemplateByID = make(map[string]*SiteTemplate, len(all))
	for _, t := range all {
		siteTemplateByID[t.ID] = t
	}
	if siteTemplateByID[defaultTemplateID] == nil {
		panic(fmt.Errorf("default site template %q is missing", defaultTemplateID))
	}
}

// AllSiteTemplates returns the registry in stable order: the default first,
// then the rest alphabetically.
func AllSiteTemplates() []*SiteTemplate {
	return siteTemplatesAll
}

// GetSiteTemplate looks up a template by id. Unknown ids fall back to the
// default ("blank") so a stale form value never breaks a build.
func GetSiteTemplate(id string) *SiteTemplate {
	if t, ok := siteTemplateByID[id]; ok {
		return t
	}
	return siteTemplateByID[defaultTemplateID]
}

func loadSiteTemplates() ([]*SiteTemplate, error) {
	entries, err := fs.ReadDir(templatesFS, templatesRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", templatesRoot, err)
	}

	tmpls := make([]*SiteTemplate, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := loadSiteTemplate(e.Name())
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", e.Name(), err)
		}
		tmpls = append(tmpls, t)
	}

	sort.SliceStable(tmpls, func(i, j int) bool {
		switch {
		case tmpls[i].ID == defaultTemplateID:
			return true
		case tmpls[j].ID == defaultTemplateID:
			return false
		default:
			return tmpls[i].ID < tmpls[j].ID
		}
	})

	return tmpls, nil
}

func loadSiteTemplate(id string) (*SiteTemplate, error) {
	promptPath := path.Join(templatesRoot, id, "prompt.md")
	raw, err := fs.ReadFile(templatesFS, promptPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", promptPath, err)
	}

	meta, body, err := parseFrontmatter(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", promptPath, err)
	}

	skeleton, err := loadTemplateSkeleton(id)
	if err != nil {
		return nil, fmt.Errorf("load skeleton: %w", err)
	}

	return &SiteTemplate{
		ID:             id,
		Label:          meta.Label,
		Description:    meta.Description,
		PromptAddendum: strings.TrimSpace(body),
		Skeleton:       skeleton,
		Checks:         meta.Checks,
	}, nil
}

func loadTemplateSkeleton(id string) (map[string]string, error) {
	base := path.Join(templatesRoot, id, "skeleton")
	skeleton := make(map[string]string)

	_, err := fs.Stat(templatesFS, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return skeleton, nil
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
		skeleton[rel] = string(b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", base, err)
	}
	return skeleton, nil
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
