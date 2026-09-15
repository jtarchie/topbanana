package lint

import (
	"context"
	"path"
	"strings"

	"github.com/jtarchie/topbanana/internal/store"
)

// checkStylesheetURLs resolves url() against the stylesheet's own dir, not the linking page's; app.css is compiled output.
func checkStylesheetURLs(ctx context.Context, s *store.Store, slug string, files []string, lc linkCheckContext) []Error {
	var errs []Error
	for _, file := range files {
		if file == localStylesheetName || !strings.HasSuffix(file, ".css") {
			continue
		}
		obj, err := s.Read(ctx, slug, file)
		if err != nil {
			continue
		}
		for _, v := range cssURLs(obj.Content) {
			errs = append(errs, checkLink(file, path.Dir(file), v, lc)...)
		}
	}
	return errs
}
