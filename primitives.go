package main

import (
	"context"
	"fmt"
	"reflect"

	"github.com/DmitryNaumov/mcp-filter/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// primitiveState keeps the original upstream catalogs so metadata can be
// restored when filtering is toggled without reconnecting the MCP client.
type primitiveState struct {
	server    *mcp.Server
	session   *mcp.ClientSession
	prompts   []*mcp.Prompt
	resources []*mcp.Resource
	templates []*mcp.ResourceTemplate
	current   primitiveCatalog
}

type primitiveCatalog struct {
	prompts   map[string]*mcp.Prompt
	resources map[string]*mcp.Resource
	templates map[string]*mcp.ResourceTemplate
}

func newPrimitiveState(ctx context.Context, server *mcp.Server, session *mcp.ClientSession) (*primitiveState, error) {
	s := &primitiveState{server: server, session: session}
	initialized := session.InitializeResult()
	if initialized == nil || initialized.Capabilities == nil {
		return s, nil
	}
	if initialized.Capabilities.Prompts != nil {
		result, err := session.ListPrompts(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("list upstream prompts: %w", err)
		}
		s.prompts = result.Prompts
	}
	if initialized.Capabilities.Resources != nil {
		resources, err := session.ListResources(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("list upstream resources: %w", err)
		}
		s.resources = resources.Resources
		templates, err := session.ListResourceTemplates(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("list upstream resource templates: %w", err)
		}
		s.templates = templates.ResourceTemplates
	}
	return s, nil
}

func (s *primitiveState) prepare(entry config.Entry) (primitiveCatalog, error) {
	next := primitiveCatalog{
		prompts:   make(map[string]*mcp.Prompt, len(s.prompts)),
		resources: make(map[string]*mcp.Resource, len(s.resources)),
		templates: make(map[string]*mcp.ResourceTemplate, len(s.templates)),
	}
	for _, original := range s.prompts {
		item, err := overlayMetadata("prompts/list", original, entry.Metadata.Patches)
		if err != nil {
			return primitiveCatalog{}, fmt.Errorf("apply metadata for prompt %q: %w", original.Name, err)
		}
		if item.Name != original.Name {
			return primitiveCatalog{}, fmt.Errorf("metadata patch cannot change prompt name %q", original.Name)
		}
		next.prompts[original.Name] = item
	}
	for _, original := range s.resources {
		item, err := overlayMetadata("resources/list", original, entry.Metadata.Patches)
		if err != nil {
			return primitiveCatalog{}, fmt.Errorf("apply metadata for resource %q: %w", original.URI, err)
		}
		if item.URI != original.URI {
			return primitiveCatalog{}, fmt.Errorf("metadata patch cannot change resource URI %q", original.URI)
		}
		next.resources[original.URI] = item
	}
	for _, original := range s.templates {
		item, err := overlayMetadata("resources/templates/list", original, entry.Metadata.Patches)
		if err != nil {
			return primitiveCatalog{}, fmt.Errorf("apply metadata for resource template %q: %w", original.URITemplate, err)
		}
		if item.URITemplate != original.URITemplate {
			return primitiveCatalog{}, fmt.Errorf("metadata patch cannot change resource template URI %q", original.URITemplate)
		}
		next.templates[original.URITemplate] = item
	}
	return next, nil
}

func (s *primitiveState) apply(next primitiveCatalog) {
	for name, item := range next.prompts {
		if reflect.DeepEqual(s.current.prompts[name], item) {
			continue
		}
		upstreamName := name
		s.server.AddPrompt(item, func(ctx context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			params := *request.Params
			params.Name = upstreamName
			return s.session.GetPrompt(ctx, &params)
		})
	}
	for uri, item := range next.resources {
		if reflect.DeepEqual(s.current.resources[uri], item) {
			continue
		}
		s.server.AddResource(item, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			params := *request.Params
			return s.session.ReadResource(ctx, &params)
		})
	}
	for uri, item := range next.templates {
		if reflect.DeepEqual(s.current.templates[uri], item) {
			continue
		}
		s.server.AddResourceTemplate(item, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			params := *request.Params
			return s.session.ReadResource(ctx, &params)
		})
	}
	s.current = next
}
