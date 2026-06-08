package server

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

type clarifyRequest struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

// clarifyHandler receives the user's answer to an ask_user question and
// delivers it to the waiting agent goroutine via the events tracker.
func (s *sitesController) clarifyHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	var in clarifyRequest
	err = c.Bind(&in)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request: "+err.Error())
	}
	if in.QuestionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "question_id is required")
	}
	if len(in.Answer) > maxAPIFieldBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "answer exceeds maximum length")
	}

	if !s.events.Resolve(slug, in.QuestionID, in.Answer) {
		return notFound()
	}
	return c.JSON(http.StatusOK, map[string]bool{"ok": true}) //nolint:wrapcheck
}
