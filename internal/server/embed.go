package server

import _ "embed"

//go:embed edit_toolbar.html
var editToolbarTemplate string

//go:embed theme_preview_listener.html
var themePreviewListenerTemplate string

//go:embed selection_listener.html
var selectionListenerTemplate string

//go:embed favicon.svg
var faviconSVG string

//go:embed templates/layout.html
var layoutTemplate string

//go:embed templates/image_drawer.html
var imageDrawerTemplate string

//go:embed templates/landing.html
var landingTemplate string

//go:embed templates/apps.html
var appsTemplate string

//go:embed templates/workspace.html
var workspaceTemplate string

//go:embed templates/manage.html
var manageTemplate string

//go:embed templates/system.html
var systemTemplate string

//go:embed templates/visual_edit.html
var visualEditTemplate string

//go:embed templates/function_edit.html
var functionEditTemplate string

//go:embed templates/files.html
var filesTemplate string

//go:embed templates/debug.html
var debugTemplate string

//go:embed templates/debug_edit.html
var debugEditTemplate string

//go:embed templates/login.html
var loginTemplate string

//go:embed templates/register.html
var registerTemplate string

//go:embed templates/account.html
var accountTemplate string

//go:embed templates/admin_users.html
var adminUsersTemplate string

//go:embed templates/error.html
var errorTemplate string

//go:embed templates/privacy.html
var privacyTemplate string

//go:embed templates/terms.html
var termsTemplate string
