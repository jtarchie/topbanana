package main

import _ "embed"

//go:embed static/landing.html
var landingPage string

//go:embed static/agent_prompt.md
var systemPrompt string

//go:embed static/edit_toolbar.html
var editToolbarTemplate string

//go:embed templates/apps.html
var appsTemplate string

//go:embed templates/progress.html
var progressTemplate string

//go:embed templates/edit.html
var editTemplate string
