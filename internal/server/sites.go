package server

import (
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/jtarchie/topbanana/internal/portable"
)

// sitesController is the heart of the admin UI: everything you do to a site you
// own. It spans creation (build), the app list, the workspace + manage views,
// the text/visual/theme editors, settings, history, data, files, export/import,
// remix, transfer, and clarify. Handlers live in their topic files
// (workspace.go, manage.go, visual_edit.go, …) but hang off this controller;
// shared helpers (render, startBuild, snapshotBefore, siteURL, the registry,
// …) stay on the embedded *Server.
type sitesController struct{ *Server }

// register mounts the site-management routes. g is the requireUser admin group;
// owns is the per-slug ownership gate applied to every route keyed by :slug.
// build and apps are account-wide (no :slug) so they skip owns.
func (s *sitesController) register(g *echo.Group, owns echo.MiddlewareFunc) {
	// Body caps: the larger envelope covers prompt POSTs that also carry
	// multipart markdown attachments; the smaller one bounds plain prompt
	// bodies. Import has its own archive-sized cap.
	promptBodyCap := middleware.BodyLimit(maxPromptBodyBytes)
	promptWithAttachmentsBodyCap := middleware.BodyLimit(maxPromptBodyWithAttachmentsBytes)

	g.POST("/build", s.buildHandler, promptWithAttachmentsBodyCap)
	g.GET("/apps", s.appsHandler)

	g.GET("/workspace/:slug", s.workspaceHandler, owns)
	g.GET("/manage/:slug", s.manageHandler, owns)
	g.GET("/edit/:slug", s.redirectToWorkspace, owns)
	g.POST("/edit/:slug", s.editSubmitHandler, owns, promptWithAttachmentsBodyCap)
	g.POST("/relint/:slug", s.relintHandler, owns)
	g.GET("/edit/:slug/visual", s.visualEditHandler, owns)
	g.POST("/edit/:slug/visual", s.visualEditSaveHandler, owns, promptBodyCap)
	g.GET("/edit/:slug/theme", s.redirectToWorkspace, owns)
	g.POST("/edit/:slug/theme", s.themeStudioApplyHandler, owns)
	g.GET("/export/:slug", s.exportHandler, owns)
	g.POST("/import", s.importHandler, middleware.BodyLimit(portable.MaxArchiveBytes+(64*1024)))
	g.DELETE("/files/:slug", s.deleteFileHandler, owns)
	g.PATCH("/files/:slug", s.renameFileHandler, owns)
	g.GET("/settings/:slug", s.redirectToManage, owns)
	g.POST("/settings/:slug", s.settingsSubmitHandler, owns)
	g.POST("/settings/:slug/delete", s.settingsDeleteHandler, owns)
	g.POST("/manage/:slug/remix", s.remixHandler, owns)
	g.POST("/apps/:slug/transfer", s.transferAppHandler, owns)
	g.GET("/history/:slug", s.redirectToWorkspace, owns)
	g.POST("/history/:slug/restore", s.historyRestoreHandler, owns)
	g.POST("/history/:slug/delete", s.historyDeleteHandler, owns)
	g.GET("/data/:slug", s.dataHandler, owns)
	g.GET("/files/:slug", s.filesHandler, owns)
	g.POST("/clarify/:slug", s.clarifyHandler, owns, promptBodyCap)
}
