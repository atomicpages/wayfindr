package cli

import (
	"slices"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
)

// scopeReport is the --json output of `scope` without a connector.
type scopeReport struct {
	// Dir is the working directory the scope file was looked up from.
	Dir string `json:"dir"`
	// Profile is --profile; "" for none.
	Profile string `json:"profile,omitempty"`
	// ScopeFile is the scope file's path; "" for none.
	ScopeFile string `json:"scope_file,omitempty"`
	// Connectors are the configured connectors, in the daemon's order.
	// Always an array; empty when nothing narrows, as the daemon isn't asked.
	Connectors []scopeConnector `json:"connectors"`
	// Unconfigured are the connectors the scope file names that are not
	// configured. Always an array.
	Unconfigured []string `json:"unconfigured"`
}

// scopeConnector is one row of a scopeReport. Each scope's fields are
// present only when that scope applies.
type scopeConnector struct {
	Name string `json:"name"`
	// InScope is whether the profile and the scope file both allow it.
	InScope bool `json:"in_scope"`
	// InProfile and ProfileTools are what the profile says: whether it
	// allows the connector, and its tools list there, absent when it has
	// none or leaves the connector out.
	InProfile    *bool    `json:"in_profile,omitempty"`
	ProfileTools []string `json:"profile_tools,omitempty"`
	// InScopeFile and ScopeTools are the same for the scope file.
	InScopeFile *bool    `json:"in_scope_file,omitempty"`
	ScopeTools  []string `json:"scope_tools,omitempty"`
}

// scopeToolsReport is the --json output of `scope <connector>`.
type scopeToolsReport struct {
	Dir       string `json:"dir"`
	Profile   string `json:"profile,omitempty"`
	ScopeFile string `json:"scope_file,omitempty"`
	Connector string `json:"connector"`
	// Tools are as in `connector tools --json`, with Exposed only if the
	// config, the profile, and the scope file all expose the tool. Always an
	// array.
	Tools []admin.ToolEntry `json:"tools"`
}

// emptyScopeReport is the --json output when nothing narrows: the daemon
// isn't asked, so there are no connectors or tools to list.
func emptyScopeReport(dir string, args []string) any {
	if len(args) == 1 {
		return scopeToolsReport{Dir: dir, Connector: args[0], Tools: []admin.ToolEntry{}}
	}

	return scopeReport{Dir: dir, Connectors: []scopeConnector{}, Unconfigured: []string{}}
}

// newScopeReport is writeScope's table as a scopeReport. Tools lists keep
// their entries as written; the table makes them printable.
func newScopeReport(dir string, v scopeView, connectors []admin.Connector) scopeReport {
	r := scopeReport{
		Dir: dir, Profile: v.n.ProfileName, ScopeFile: v.file,
		Connectors: []scopeConnector{}, Unconfigured: []string{},
	}

	for _, c := range connectors {
		row := scopeConnector{Name: c.Name, InScope: v.n.Allows(c.Name)}

		if v.n.ProfileName != "" {
			row.InProfile, row.ProfileTools = scopeSays(v.n.Profile, c.Name)
		}

		if v.file != "" {
			row.InScopeFile, row.ScopeTools = scopeSays(v.n.File, c.Name)
		}

		r.Connectors = append(r.Connectors, row)
	}

	if v.n.File != nil {
		for _, name := range v.n.File.Connectors {
			if !slices.ContainsFunc(connectors, func(c admin.Connector) bool { return c.Name == name }) {
				r.Unconfigured = append(r.Unconfigured, name)
			}
		}
	}

	return r
}

// scopeSays is whether s allows connector, and its tools list there: nil
// when s leaves the connector out or has no list for it.
func scopeSays(s *config.Scope, connector string) (*bool, []string) {
	allows := s.Allows(connector)

	if !allows || s == nil {
		return &allows, nil
	}

	return &allows, s.Tools[connector]
}
