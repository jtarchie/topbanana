package server

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v5"
)

// checkLinksHandler probes inline, bounded by linkcheck's budget, so the redirect lands on fresh answers.
func (s *sitesController) checkLinksHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	if s.linkChecker == nil {
		return echo.NewHTTPError(http.StatusNotFound, "link checking is not enabled")
	}
	_, err = s.linkChecker.Check(c.Request().Context(), slug)
	if err != nil {
		return fmt.Errorf("check links: %w", err)
	}
	return c.Redirect(http.StatusSeeOther, "/manage/"+slug+"?flash="+url.QueryEscape("Links checked.")+"#links-heading") //nolint:wrapcheck
}
