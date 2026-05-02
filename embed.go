package main

import _ "embed"

//go:embed static/landing.html
var landingPage string

//go:embed static/agent_prompt.md
var systemPrompt string
