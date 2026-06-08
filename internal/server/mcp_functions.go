package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/sandbox"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/templates"
	"github.com/jtarchie/topbanana/internal/textedit"
)

// This file completes the MCP editing surface for the platform's dynamic
// templates (contact-form, guestbook, tiny-shop): the server-side function
// handlers an external agent can't otherwise author, plus the one settings
// tool it needs to turn features on. Every handler reuses the same machinery
// the web UI does — sandbox.Invoke via invokeWithCAS, the meta sidecar via
// ReadMeta/WriteMeta — so MCP-driven and human-driven edits behave identically.

const (
	mcpFunctionsDir = "functions/"
	mcpJSExt        = ".js"
	mcpJSCType      = "application/javascript; charset=utf-8"

	// mcpFunctionNudge points at the loop that gives function edits feedback
	// and then publishes them.
	mcpFunctionNudge = "use test_function to exercise it, then lint_site to publish"
)

func mcpFunctionPath(name string) string { return mcpFunctionsDir + name + mcpJSExt }

// mcpRequireFunctions gates the mutating function tools on the site actually
// having server-side functions enabled (template default OR the per-site
// override). Authoring a handler on a site that can't run it would silently do
// nothing, so we steer the agent to configure_site first.
func (s *Server) mcpRequireFunctions(ctx context.Context, slug string) error {
	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	if tmpl == nil || !tmpl.EnablesFunctions {
		return errors.New("server-side functions are not enabled for this site — enable them first with configure_site (enable_functions=true)")
	}
	return nil
}

// --- function authoring -----------------------------------------------------

type writeFunctionInput struct {
	Slug   string `json:"slug"   jsonschema:"The site slug"`
	Name   string `json:"name"   jsonschema:"Handler name (matches [a-z0-9-_], max 40 chars), served at /api/<name>."`
	Source string `json:"source" jsonschema:"CommonJS module source: module.exports = function(request) { ... return response.* }. See topbanana://guide/functions."`
}

func (s *Server) registerWriteFunction(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_function",
		Description: "Create or overwrite a server-side handler (functions/<name>.js, served at /api/<name>) in a site the caller owns. The site must have functions enabled (configure_site). Source is a CommonJS module — read topbanana://guide/functions for the runtime contract.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in writeFunctionInput) (*mcp.CallToolResult, any, error) {
		err := s.mcpAuthorizeFunction(ctx, in.Slug, in.Name, true)
		if err != nil {
			return nil, nil, err
		}
		if len(in.Source) > mcpMaxFileBytes {
			return nil, nil, fmt.Errorf("source too large: %d bytes (max %d)", len(in.Source), mcpMaxFileBytes)
		}
		err = s.store.Write(ctx, in.Slug, mcpFunctionPath(in.Name), in.Source, mcpJSCType, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("write function %q: %w", in.Name, err)
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "name": in.Name, "path": mcpFunctionPath(in.Name),
			"endpoint": "/api/" + in.Name, "next": mcpFunctionNudge,
		})
	})
}

type readFunctionInput struct {
	Slug string `json:"slug" jsonschema:"The site slug"`
	Name string `json:"name" jsonschema:"Handler name to read"`
}

func (s *Server) registerReadFunction(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_function",
		Description: "Read the source of a server-side handler (functions/<name>.js) in a site the caller owns.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in readFunctionInput) (*mcp.CallToolResult, any, error) {
		err := s.mcpAuthorizeFunction(ctx, in.Slug, in.Name, false)
		if err != nil {
			return nil, nil, err
		}
		src, err := s.loadFunctionSource(ctx, in.Slug, in.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("read function %q: %w", in.Name, err)
		}
		return mcpJSON(map[string]any{"slug": in.Slug, "name": in.Name, "source": src})
	})
}

type editFunctionInput struct {
	Slug       string `json:"slug"                  jsonschema:"The site slug"`
	Name       string `json:"name"                  jsonschema:"Handler name to edit"`
	OldText    string `json:"old_text"              jsonschema:"Exact text to find (whitespace-tolerant fallback applies)."`
	NewText    string `json:"new_text"              jsonschema:"Replacement text."`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence."`
}

func (s *Server) registerEditFunction(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "edit_function",
		Description: "Surgically edit a server-side handler (functions/<name>.js) in a site the caller owns — same find/replace semantics as edit_file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in editFunctionInput) (*mcp.CallToolResult, any, error) {
		err := s.mcpAuthorizeFunction(ctx, in.Slug, in.Name, true)
		if err != nil {
			return nil, nil, err
		}
		src, err := s.loadFunctionSource(ctx, in.Slug, in.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("read function %q: %w", in.Name, err)
		}
		edit, err := textedit.ApplyEdit(src, in.OldText, in.NewText, in.ReplaceAll)
		if err != nil {
			return nil, nil, err
		}
		if len(edit.Content) > mcpMaxFileBytes {
			return nil, nil, fmt.Errorf("source too large after edit: %d bytes (max %d)", len(edit.Content), mcpMaxFileBytes)
		}
		err = s.store.Write(ctx, in.Slug, mcpFunctionPath(in.Name), edit.Content, mcpJSCType, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("write function %q: %w", in.Name, err)
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "name": in.Name, "replacements": edit.Count, "note": edit.Note,
			"next": mcpFunctionNudge,
		})
	})
}

type deleteFunctionInput struct {
	Slug string `json:"slug" jsonschema:"The site slug"`
	Name string `json:"name" jsonschema:"Handler name to delete"`
}

func (s *Server) registerDeleteFunction(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_function",
		Description: "Delete a server-side handler from a site the caller owns. Its /api/<name> endpoint returns 404 afterward.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteFunctionInput) (*mcp.CallToolResult, any, error) {
		err := s.mcpAuthorizeFunction(ctx, in.Slug, in.Name, true)
		if err != nil {
			return nil, nil, err
		}
		err = s.store.Delete(ctx, in.Slug, mcpFunctionPath(in.Name))
		if err != nil {
			return nil, nil, fmt.Errorf("delete function %q: %w", in.Name, err)
		}
		return mcpJSON(map[string]any{"ok": true, "slug": in.Slug, "name": in.Name})
	})
}

type listFunctionsInput struct {
	Slug string `json:"slug" jsonschema:"The site slug"`
}

func (s *Server) registerListFunctions(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_functions",
		Description: "List the server-side handler names under functions/ in a site the caller owns. Each maps to /api/<name>.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listFunctionsInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		files, err := s.store.List(ctx, in.Slug)
		if err != nil {
			return nil, nil, fmt.Errorf("list files: %w", err)
		}
		names := make([]string, 0)
		for _, f := range files {
			if !strings.HasPrefix(f, mcpFunctionsDir) || !strings.HasSuffix(f, mcpJSExt) {
				continue
			}
			bare := strings.TrimSuffix(strings.TrimPrefix(f, mcpFunctionsDir), mcpJSExt)
			if bare != "" {
				names = append(names, bare)
			}
		}
		sort.Strings(names)
		return mcpJSON(map[string]any{"slug": in.Slug, "functions": names})
	})
}

// mcpAuthorizeFunction is the shared gate for the function tools: ownership,
// a valid handler name, and (for mutations) that the site has functions
// enabled. read/list pass mutate=false so inspection works regardless.
func (s *Server) mcpAuthorizeFunction(ctx context.Context, slug, name string, mutate bool) error {
	_, err := s.mcpUserAndAuthorize(ctx, slug)
	if err != nil {
		return err
	}
	err = textedit.ValidateFunctionName(name)
	if err != nil {
		return err
	}
	if mutate {
		return s.mcpRequireFunctions(ctx, slug)
	}
	return nil
}

// --- function testing -------------------------------------------------------

type testFunctionInput struct {
	Slug        string `json:"slug"                   jsonschema:"The site slug"`
	Name        string `json:"name"                   jsonschema:"Handler name to invoke"`
	Method      string `json:"method,omitempty"       jsonschema:"HTTP method (default GET)."`
	Body        string `json:"body,omitempty"         jsonschema:"Request body."`
	ContentType string `json:"content_type,omitempty" jsonschema:"Body content type (e.g. application/json or application/x-www-form-urlencoded), so the handler sees request.json / request.form."`
}

func (s *Server) registerTestFunction(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "test_function",
		Description: "Invoke a server-side handler in the sandbox and return its status, headers, body, and console logs — the feedback loop for dynamic code. Runs against the site's real key-value state.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in testFunctionInput) (*mcp.CallToolResult, any, error) {
		err := s.mcpAuthorizeFunction(ctx, in.Slug, in.Name, true)
		if err != nil {
			return nil, nil, err
		}
		src, err := s.loadFunctionSource(ctx, in.Slug, in.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("load function %q: %w", in.Name, err)
		}
		method := in.Method
		if method == "" {
			method = "GET"
		}
		req := sandbox.Request{
			Method:  method,
			Path:    "/api/" + in.Name,
			Query:   map[string]string{},
			Headers: map[string]string{"content-type": in.ContentType},
			Body:    in.Body,
		}
		parseTestRequestBody(&req, in.ContentType, in.Body)

		var logs []string
		logFn := func(level, line string) { logs = append(logs, level+": "+line) }

		resp, err := s.invokeWithCAS(ctx, in.Slug, in.Name, src, req, logFn)
		if err != nil {
			return nil, nil, fmt.Errorf("invoke function %q: %w", in.Name, err)
		}
		return mcpJSON(map[string]any{
			"slug": in.Slug, "name": in.Name,
			"status": resp.Status, "content_type": resp.ContentType,
			"headers": resp.Headers, "body": string(resp.Body), "logs": logs,
		})
	})
}

// --- site settings ----------------------------------------------------------

// configureSiteInput uses pointers so the agent updates only the fields it
// sends; a nil field is left untouched.
type configureSiteInput struct {
	Slug            string  `json:"slug"                        jsonschema:"The site slug"`
	Title           *string `json:"title,omitempty"             jsonschema:"Human-readable site title."`
	Description     *string `json:"description,omitempty"       jsonschema:"Short site description."`
	Private         *bool   `json:"private,omitempty"           jsonschema:"Hide the site from the public web (owner/super-admin only)."`
	EnableFunctions *bool   `json:"enable_functions,omitempty"  jsonschema:"Enable server-side functions (/api/* handlers). Ignored when the template already enables them."`
	EnablePublicAPI *bool   `json:"enable_public_api,omitempty" jsonschema:"Let /api/* bypass the same-origin check (genuine public APIs / webhooks)."`
}

func (s *Server) registerConfigureSite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "configure_site",
		Description: "Update settings on a site the caller owns: title, description, private, enable_functions, enable_public_api. Only the fields you pass are changed. (Custom domains, ownership transfer, and deletion stay in the web UI.)",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in configureSiteInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		meta := s.build.ReadMeta(ctx, in.Slug)
		if in.Title != nil {
			meta.Title = strings.TrimSpace(*in.Title)
		}
		if in.Description != nil {
			meta.Description = strings.TrimSpace(*in.Description)
		}
		if in.Private != nil {
			meta.Private = *in.Private
		}
		if in.EnablePublicAPI != nil {
			meta.EnablesPublicAPI = *in.EnablePublicAPI
		}
		if in.EnableFunctions != nil {
			// Only honour the override when the template doesn't already enable
			// functions, mirroring settingsSubmitHandler so the surfaces agree.
			if base := templates.Get(meta.Template); base == nil || !base.EnablesFunctions {
				meta.EnablesFunctions = *in.EnableFunctions
			}
		}

		s.snapshotBefore(ctx, in.Slug, snapshot.ReasonSettings)
		err = s.build.WriteMeta(ctx, in.Slug, meta)
		if err != nil {
			return nil, nil, fmt.Errorf("save settings: %w", err)
		}
		// Refresh the in-memory indexes so a private flip takes effect for
		// routing immediately, like the web settings handler does.
		s.rebuildDomainIndexLogging(ctx)

		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug,
			"title": meta.Title, "description": meta.Description,
			"private": meta.Private, "enables_functions": meta.EnablesFunctions,
			"enables_public_api": meta.EnablesPublicAPI,
		})
	})
}

// --- functions guide + prompt -----------------------------------------------

// registerFunctionsGuide exposes the runtime contract as a resource, and
// add_function as a guided prompt. Registered with the function tools so they
// only appear when those tools do.
func (s *Server) registerFunctionsGuide(srv *mcp.Server) {
	srv.AddResource(
		&mcp.Resource{
			URI:         "topbanana://guide/functions",
			Name:        "Functions runtime guide",
			Description: "The server-side functions contract: the CommonJS handler shape, the available globals (request, response, kv, escape, validate), and the forbidden APIs.",
			MIMEType:    "text/markdown",
		},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return mcpTextResource(req.Params.URI, "text/markdown", agent.FunctionsGuide()), nil
		},
	)

	srv.AddPrompt(
		&mcp.Prompt{
			Name:        "add_function",
			Title:       "Add a server-side function",
			Description: "Scaffold a new /api handler for a site you own: loads the runtime contract and an example, then asks you to implement a specific behaviour.",
			Arguments: []*mcp.PromptArgument{
				{Name: "slug", Description: "The site slug", Required: true},
				{Name: "purpose", Description: "What the handler should do", Required: true},
			},
		},
		s.addFunctionPromptHandler,
	)
}

func (s *Server) addFunctionPromptHandler(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	slug := strings.TrimSpace(req.Params.Arguments["slug"])
	if slug == "" {
		return nil, errInvalidPromptArg("slug is required")
	}
	_, err := s.mcpUserAndAuthorize(ctx, slug)
	if err != nil {
		return nil, err
	}
	purpose := strings.TrimSpace(req.Params.Arguments["purpose"])

	var b strings.Builder
	fmt.Fprintf(&b, "Add a server-side function to the Top Banana site %q.\n\n", slug)
	if purpose != "" {
		fmt.Fprintf(&b, "Purpose: %s\n\n", purpose)
	}
	if s.mcpRequireFunctions(ctx, slug) != nil {
		b.WriteString("This site does not have functions enabled yet — call configure_site with enable_functions=true first.\n\n")
	}
	b.WriteString("Read topbanana://guide/functions for the runtime contract (globals, handler shape, forbidden APIs). ")
	b.WriteString("Write the handler with write_function, exercise it with test_function, then run lint_site.\n\n")
	b.WriteString("Skeleton:\n\n```js\nmodule.exports = function (request) {\n  // request.method, request.json, request.form, kv.*\n  return response.json({ ok: true });\n};\n```\n")

	return &mcp.GetPromptResult{
		Description: "Add a function to " + slug,
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: b.String()}},
		},
	}, nil
}
