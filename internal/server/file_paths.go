package server

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// fileKind tags a user-supplied path with which "area" of the site it belongs
// to. The delete/rename handlers reject cross-kind renames so a user can't
// turn an HTML page into a JS handler by typing `functions/foo.js` into the
// rename field — that needs to round-trip through the LLM agent which
// understands the contract.
type fileKind int

const (
	kindHTML fileKind = iota + 1
	kindAsset
	kindFunction
)

func (k fileKind) String() string {
	switch k {
	case kindHTML:
		return "html"
	case kindAsset:
		return "asset"
	case kindFunction:
		return "function"
	}
	return "unknown"
}

const maxUserPathLen = 200

var (
	functionNameRe  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
	allowedAssetExt = map[string]bool{
		".jpg": true, ".jpeg": true, ".png": true,
		".gif": true, ".webp": true, ".svg": true,
	}
)

// classifyUserPath validates a path supplied by a site owner via the
// delete/rename UI and returns which area of the site it lives in. Stricter
// than the agent's validateHTMLPath because it gates every user-facing area,
// not just HTML writes.
func classifyUserPath(p string) (fileKind, error) {
	err := checkUserPathShape(p)
	if err != nil {
		return 0, err
	}
	switch {
	case strings.HasPrefix(p, "functions/"):
		return classifyFunctionPath(p)
	case strings.HasPrefix(p, "assets/"):
		return classifyAssetPath(p)
	default:
		return classifyHTMLPath(p)
	}
}

func checkUserPathShape(p string) error {
	for _, check := range userPathShapeChecks {
		err := check(p)
		if err != nil {
			return err
		}
	}
	return nil
}

var userPathShapeChecks = []func(string) error{
	checkUserPathBasics,
	checkUserPathSegments,
	checkUserPathControlChars,
	checkUserPathReserved,
}

func checkUserPathBasics(p string) error {
	switch {
	case p == "":
		return errors.New("path is required")
	case len(p) > maxUserPathLen:
		return fmt.Errorf("path too long (max %d chars)", maxUserPathLen)
	case strings.HasPrefix(p, "/"):
		return errors.New("path must be relative (no leading /)")
	case strings.Contains(p, `\`):
		return errors.New("path must use forward slashes")
	}
	return nil
}

func checkUserPathSegments(p string) error {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q contains an empty or relative segment", p)
		}
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q is not canonical", p)
	}
	return nil
}

func checkUserPathControlChars(p string) error {
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path %q contains a control character", p)
		}
	}
	return nil
}

func checkUserPathReserved(p string) error {
	if strings.HasPrefix(p, "_") || strings.Contains(p, "/_") {
		return fmt.Errorf("path %q is under a reserved prefix", p)
	}
	if strings.HasPrefix(p, ".") || strings.Contains(p, "/.") {
		return fmt.Errorf("path %q is under a reserved prefix", p)
	}
	return nil
}

func classifyFunctionPath(p string) (fileKind, error) {
	name := strings.TrimSuffix(strings.TrimPrefix(p, "functions/"), ".js")
	if !strings.HasSuffix(p, ".js") || strings.Contains(name, "/") || !functionNameRe.MatchString(name) {
		return 0, fmt.Errorf("function path %q must be functions/<name>.js", p)
	}
	return kindFunction, nil
}

func classifyAssetPath(p string) (fileKind, error) {
	rest := strings.TrimPrefix(p, "assets/")
	if rest == "" {
		return 0, fmt.Errorf("asset path %q is missing a filename", p)
	}
	ext := strings.ToLower(path.Ext(rest))
	if !allowedAssetExt[ext] {
		return 0, fmt.Errorf("asset path %q has unsupported extension %q", p, ext)
	}
	return kindAsset, nil
}

func classifyHTMLPath(p string) (fileKind, error) {
	if !strings.HasSuffix(p, ".html") {
		return 0, fmt.Errorf("path %q must end in .html (or live under assets/ or functions/)", p)
	}
	return kindHTML, nil
}
