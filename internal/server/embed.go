package server

import _ "embed"

//go:embed edit_toolbar.html
var editToolbarTemplate string

//go:embed templates/layout.html
var layoutTemplate string

//go:embed templates/landing.html
var landingTemplate string

//go:embed templates/apps.html
var appsTemplate string

//go:embed templates/progress.html
var progressTemplate string

//go:embed templates/edit.html
var editTemplate string

//go:embed templates/settings.html
var settingsTemplate string

//go:embed templates/visual_edit.html
var visualEditTemplate string
