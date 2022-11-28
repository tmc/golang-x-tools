// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package mod provides core features related to go.mod file
// handling for use by Go editors and tools.
package mod

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
	"golang.org/x/tools/gopls/internal/govulncheck"
	"golang.org/x/tools/gopls/internal/lsp/command"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/gopls/internal/lsp/source"
	"golang.org/x/tools/gopls/internal/span"
	"golang.org/x/tools/internal/event"
	"golang.org/x/vuln/osv"
)

// Diagnostics returns diagnostics for the modules in the workspace.
//
// It waits for completion of type-checking of all active packages.
func Diagnostics(ctx context.Context, snapshot source.Snapshot) (map[source.VersionedFileIdentity][]*source.Diagnostic, error) {
	ctx, done := event.Start(ctx, "mod.Diagnostics", source.SnapshotLabels(snapshot)...)
	defer done()

	return collectDiagnostics(ctx, snapshot, ModDiagnostics)
}

// UpgradeDiagnostics returns upgrade diagnostics for the modules in the
// workspace with known upgrades.
func UpgradeDiagnostics(ctx context.Context, snapshot source.Snapshot) (map[source.VersionedFileIdentity][]*source.Diagnostic, error) {
	ctx, done := event.Start(ctx, "mod.UpgradeDiagnostics", source.SnapshotLabels(snapshot)...)
	defer done()

	return collectDiagnostics(ctx, snapshot, ModUpgradeDiagnostics)
}

// VulnerabilityDiagnostics returns vulnerability diagnostics for the active modules in the
// workspace with known vulnerabilites.
func VulnerabilityDiagnostics(ctx context.Context, snapshot source.Snapshot) (map[source.VersionedFileIdentity][]*source.Diagnostic, error) {
	ctx, done := event.Start(ctx, "mod.VulnerabilityDiagnostics", source.SnapshotLabels(snapshot)...)
	defer done()

	return collectDiagnostics(ctx, snapshot, ModVulnerabilityDiagnostics)
}

func collectDiagnostics(ctx context.Context, snapshot source.Snapshot, diagFn func(context.Context, source.Snapshot, source.FileHandle) ([]*source.Diagnostic, error)) (map[source.VersionedFileIdentity][]*source.Diagnostic, error) {
	reports := make(map[source.VersionedFileIdentity][]*source.Diagnostic)
	for _, uri := range snapshot.ModFiles() {
		fh, err := snapshot.GetVersionedFile(ctx, uri)
		if err != nil {
			return nil, err
		}
		reports[fh.VersionedFileIdentity()] = []*source.Diagnostic{}
		diagnostics, err := diagFn(ctx, snapshot, fh)
		if err != nil {
			return nil, err
		}
		for _, d := range diagnostics {
			fh, err := snapshot.GetVersionedFile(ctx, d.URI)
			if err != nil {
				return nil, err
			}
			reports[fh.VersionedFileIdentity()] = append(reports[fh.VersionedFileIdentity()], d)
		}
	}
	return reports, nil
}

// ModDiagnostics waits for completion of type-checking of all active
// packages, then returns diagnostics from diagnosing the packages in
// the workspace and from tidying the go.mod file.
func ModDiagnostics(ctx context.Context, snapshot source.Snapshot, fh source.FileHandle) (diagnostics []*source.Diagnostic, err error) {
	pm, err := snapshot.ParseMod(ctx, fh)
	if err != nil {
		if pm == nil || len(pm.ParseErrors) == 0 {
			return nil, err
		}
		return pm.ParseErrors, nil
	}

	// Packages in the workspace can contribute diagnostics to go.mod files.
	// TODO(rfindley): Try to avoid calling DiagnosePackage on all packages in the workspace here,
	// for every go.mod file. If gc_details is enabled, it looks like this could lead to extra
	// go command invocations (as gc details is not memoized).
	wspkgs, err := snapshot.ActivePackages(ctx)
	if err != nil && !source.IsNonFatalGoModError(err) {
		event.Error(ctx, fmt.Sprintf("workspace packages: diagnosing %s", pm.URI), err)
	}
	if err == nil {
		for _, pkg := range wspkgs {
			pkgDiagnostics, err := snapshot.DiagnosePackage(ctx, pkg)
			if err != nil {
				return nil, err
			}
			diagnostics = append(diagnostics, pkgDiagnostics[fh.URI()]...)
		}
	}

	tidied, err := snapshot.ModTidy(ctx, pm)
	if err != nil && !source.IsNonFatalGoModError(err) {
		event.Error(ctx, fmt.Sprintf("tidy: diagnosing %s", pm.URI), err)
	}
	if err == nil {
		for _, d := range tidied.Diagnostics {
			if d.URI != fh.URI() {
				continue
			}
			diagnostics = append(diagnostics, d)
		}
	}
	return diagnostics, nil
}

// ModUpgradeDiagnostics adds upgrade quick fixes for individual modules if the upgrades
// are recorded in the view.
func ModUpgradeDiagnostics(ctx context.Context, snapshot source.Snapshot, fh source.FileHandle) (upgradeDiagnostics []*source.Diagnostic, err error) {
	pm, err := snapshot.ParseMod(ctx, fh)
	if err != nil {
		// Don't return an error if there are parse error diagnostics to be shown, but also do not
		// continue since we won't be able to show the upgrade diagnostics.
		if pm != nil && len(pm.ParseErrors) != 0 {
			return nil, nil
		}
		return nil, err
	}

	upgrades := snapshot.View().ModuleUpgrades(fh.URI())
	for _, req := range pm.File.Require {
		ver, ok := upgrades[req.Mod.Path]
		if !ok || req.Mod.Version == ver {
			continue
		}
		rng, err := pm.Mapper.OffsetRange(req.Syntax.Start.Byte, req.Syntax.End.Byte)
		if err != nil {
			return nil, err
		}
		// Upgrade to the exact version we offer the user, not the most recent.
		title := fmt.Sprintf("%s%v", upgradeCodeActionPrefix, ver)
		cmd, err := command.NewUpgradeDependencyCommand(title, command.DependencyArgs{
			URI:        protocol.URIFromSpanURI(fh.URI()),
			AddRequire: false,
			GoCmdArgs:  []string{req.Mod.Path + "@" + ver},
		})
		if err != nil {
			return nil, err
		}
		upgradeDiagnostics = append(upgradeDiagnostics, &source.Diagnostic{
			URI:            fh.URI(),
			Range:          rng,
			Severity:       protocol.SeverityInformation,
			Source:         source.UpgradeNotification,
			Message:        fmt.Sprintf("%v can be upgraded", req.Mod.Path),
			SuggestedFixes: []source.SuggestedFix{source.SuggestedFixFromCommand(cmd, protocol.QuickFix)},
		})
	}

	return upgradeDiagnostics, nil
}

const upgradeCodeActionPrefix = "Upgrade to "

// ModVulnerabilityDiagnostics adds diagnostics for vulnerabilities in individual modules
// if the vulnerability is recorded in the view.
func ModVulnerabilityDiagnostics(ctx context.Context, snapshot source.Snapshot, fh source.FileHandle) (vulnDiagnostics []*source.Diagnostic, err error) {
	pm, err := snapshot.ParseMod(ctx, fh)
	if err != nil {
		// Don't return an error if there are parse error diagnostics to be shown, but also do not
		// continue since we won't be able to show the vulnerability diagnostics.
		if pm != nil && len(pm.ParseErrors) != 0 {
			return nil, nil
		}
		return nil, err
	}

	fromGovulncheck := true
	vs := snapshot.View().Vulnerabilities(fh.URI())[fh.URI()]
	if vs == nil && snapshot.View().Options().Vulncheck == source.ModeVulncheckImports {
		vs, err = snapshot.ModVuln(ctx, fh.URI())
		if err != nil {
			return nil, err
		}
		fromGovulncheck = false
	}
	if vs == nil || len(vs.Vulns) == 0 {
		return nil, nil
	}

	vulncheck, err := command.NewRunGovulncheckCommand("Run govulncheck", command.VulncheckArgs{
		URI:     protocol.DocumentURI(fh.URI()),
		Pattern: "./...",
	})
	if err != nil {
		// must not happen
		return nil, err // TODO: bug report
	}
	suggestVulncheck := source.SuggestedFixFromCommand(vulncheck, protocol.QuickFix)

	type modVuln struct {
		mod  *govulncheck.Module
		vuln *govulncheck.Vuln
	}
	vulnsByModule := make(map[string][]modVuln)
	for _, vuln := range vs.Vulns {
		for _, mod := range vuln.Modules {
			vulnsByModule[mod.Path] = append(vulnsByModule[mod.Path], modVuln{mod, vuln})
		}
	}

	for _, req := range pm.File.Require {
		vulns := vulnsByModule[req.Mod.Path]
		if len(vulns) == 0 {
			continue
		}
		// note: req.Syntax is the line corresponding to 'require', which means
		// req.Syntax.Start can point to the beginning of the "require" keyword
		// for a single line require (e.g. "require golang.org/x/mod v0.0.0").
		start := req.Syntax.Start.Byte
		if len(req.Syntax.Token) == 3 {
			start += len("require ")
		}
		rng, err := pm.Mapper.OffsetRange(start, req.Syntax.End.Byte)
		if err != nil {
			return nil, err
		}
		// Map affecting vulns to 'warning' level diagnostics,
		// others to 'info' level diagnostics.
		// Fixes will include only the upgrades for warning level diagnostics.
		var warningFixes, infoFixes []source.SuggestedFix
		var warning, info []string
		var relatedInfo []source.RelatedInformation
		for _, mv := range vulns {
			mod, vuln := mv.mod, mv.vuln
			// It is possible that the source code was changed since the last
			// govulncheck run and information in the `vulns` info is stale.
			// For example, imagine that a user is in the middle of updating
			// problematic modules detected by the govulncheck run by applying
			// quick fixes. Stale diagnostics can be confusing and prevent the
			// user from quickly locating the next module to fix.
			// Ideally we should rerun the analysis with the updated module
			// dependencies or any other code changes, but we are not yet
			// in the position of automatically triggerring the analysis
			// (govulncheck can take a while). We also don't know exactly what
			// part of source code was changed since `vulns` was computed.
			// As a heuristic, we assume that a user upgrades the affecting
			// module to the version with the fix or the latest one, and if the
			// version in the require statement is equal to or higher than the
			// fixed version, skip generating a diagnostic about the vulnerability.
			// Eventually, the user has to rerun govulncheck.
			if mod.FixedVersion != "" && semver.IsValid(req.Mod.Version) && semver.Compare(mod.FixedVersion, req.Mod.Version) <= 0 {
				continue
			}
			if !vuln.IsCalled() {
				info = append(info, vuln.OSV.ID)
			} else {
				warning = append(warning, vuln.OSV.ID)
				relatedInfo = append(relatedInfo, listRelatedInfo(ctx, snapshot, vuln)...)
			}
			// Upgrade to the exact version we offer the user, not the most recent.
			if fixedVersion := mod.FixedVersion; semver.IsValid(fixedVersion) && semver.Compare(req.Mod.Version, fixedVersion) < 0 {
				cmd, err := getUpgradeCodeAction(fh, req, fixedVersion)
				if err != nil {
					return nil, err // TODO: bug report
				}
				sf := source.SuggestedFixFromCommand(cmd, protocol.QuickFix)
				if !vuln.IsCalled() {
					infoFixes = append(infoFixes, sf)
				} else {
					warningFixes = append(warningFixes, sf)
				}
			}
		}

		if len(warning) == 0 && len(info) == 0 {
			continue
		}
		// Add an upgrade for module@latest.
		// TODO(suzmue): verify if latest is the same as fixedVersion.
		latest, err := getUpgradeCodeAction(fh, req, "latest")
		if err != nil {
			return nil, err // TODO: bug report
		}
		sf := source.SuggestedFixFromCommand(latest, protocol.QuickFix)
		if len(warningFixes) > 0 {
			warningFixes = append(warningFixes, sf)
		}
		if len(infoFixes) > 0 {
			infoFixes = append(infoFixes, sf)
		}
		if !fromGovulncheck {
			infoFixes = append(infoFixes, suggestVulncheck)
		}

		sort.Strings(warning)
		sort.Strings(info)

		if len(warning) > 0 {
			vulnDiagnostics = append(vulnDiagnostics, &source.Diagnostic{
				URI:            fh.URI(),
				Range:          rng,
				Severity:       protocol.SeverityWarning,
				Source:         source.Vulncheck,
				Message:        getVulnMessage(req.Mod.Path, warning, true, fromGovulncheck),
				SuggestedFixes: warningFixes,
				Related:        relatedInfo,
			})
		}
		if len(info) > 0 {
			vulnDiagnostics = append(vulnDiagnostics, &source.Diagnostic{
				URI:            fh.URI(),
				Range:          rng,
				Severity:       protocol.SeverityInformation,
				Source:         source.Vulncheck,
				Message:        getVulnMessage(req.Mod.Path, info, false, fromGovulncheck),
				SuggestedFixes: infoFixes,
				Related:        relatedInfo,
			})
		}
	}

	// Add standard library vulnerabilities.
	stdlibVulns := vulnsByModule["stdlib"]
	if len(stdlibVulns) == 0 {
		return vulnDiagnostics, nil
	}

	// Put the standard library diagnostic on the module declaration.
	rng, err := pm.Mapper.OffsetRange(pm.File.Module.Syntax.Start.Byte, pm.File.Module.Syntax.End.Byte)
	if err != nil {
		return vulnDiagnostics, nil // TODO: bug report
	}

	stdlib := stdlibVulns[0].mod.FoundVersion
	var warning, info []string
	var relatedInfo []source.RelatedInformation
	for _, mv := range stdlibVulns {
		vuln := mv.vuln
		stdlib = mv.mod.FoundVersion
		if !vuln.IsCalled() {
			info = append(info, vuln.OSV.ID)
		} else {
			warning = append(warning, vuln.OSV.ID)
			relatedInfo = append(relatedInfo, listRelatedInfo(ctx, snapshot, vuln)...)
		}
	}
	if len(warning) > 0 {
		vulnDiagnostics = append(vulnDiagnostics, &source.Diagnostic{
			URI:      fh.URI(),
			Range:    rng,
			Severity: protocol.SeverityWarning,
			Source:   source.Vulncheck,
			Message:  getVulnMessage(stdlib, warning, true, fromGovulncheck),
			Related:  relatedInfo,
		})
	}
	if len(info) > 0 {
		vulnDiagnostics = append(vulnDiagnostics, &source.Diagnostic{
			URI:      fh.URI(),
			Range:    rng,
			Severity: protocol.SeverityInformation,
			Source:   source.Vulncheck,
			Message:  getVulnMessage(stdlib, info, false, fromGovulncheck),
			Related:  relatedInfo,
		})
	}
	return vulnDiagnostics, nil
}

func getVulnMessage(mod string, vulns []string, used, fromGovulncheck bool) string {
	var b strings.Builder
	if used {
		switch len(vulns) {
		case 1:
			fmt.Fprintf(&b, "%v has a vulnerability used in the code: %v.", mod, vulns[0])
		default:
			fmt.Fprintf(&b, "%v has vulnerabilities used in the code: %v.", mod, strings.Join(vulns, ", "))
		}
	} else {
		if fromGovulncheck {
			switch len(vulns) {
			case 1:
				fmt.Fprintf(&b, "%v has a vulnerability %v that is not used in the code.", mod, vulns[0])
			default:
				fmt.Fprintf(&b, "%v has known vulnerabilities %v that are not used in the code.", mod, strings.Join(vulns, ", "))
			}
		} else {
			switch len(vulns) {
			case 1:
				fmt.Fprintf(&b, "%v has a vulnerability %v.", mod, vulns[0])
			default:
				fmt.Fprintf(&b, "%v has known vulnerabilities %v.", mod, strings.Join(vulns, ", "))
			}
		}
	}
	return b.String()
}

func listRelatedInfo(ctx context.Context, snapshot source.Snapshot, vuln *govulncheck.Vuln) []source.RelatedInformation {
	var ri []source.RelatedInformation
	for _, m := range vuln.Modules {
		for _, p := range m.Packages {
			for _, c := range p.CallStacks {
				if len(c.Frames) == 0 {
					continue
				}
				entry := c.Frames[0]
				pos := entry.Position
				if pos.Filename == "" {
					continue // token.Position Filename is an optional field.
				}
				uri := span.URIFromPath(pos.Filename)
				startPos := protocol.Position{
					Line: uint32(pos.Line) - 1,
					// We need to read the file contents to precisesly map
					// token.Position (pos) to the UTF16-based column offset
					// protocol.Position requires. That can be expensive.
					// We need this related info to just help users to open
					// the entry points of the callstack and once the file is
					// open, we will compute the precise location based on the
					// open file contents. So, use the beginning of the line
					// as the position here instead of precise UTF16-based
					// position computation.
					Character: 0,
				}
				ri = append(ri, source.RelatedInformation{
					URI: uri,
					Range: protocol.Range{
						Start: startPos,
						End:   startPos,
					},
					Message: fmt.Sprintf("[%v] %v -> %v.%v", vuln.OSV.ID, entry.Name(), p.Path, c.Symbol),
				})
			}
		}
	}
	return ri
}

func formatMessage(v *govulncheck.Vuln) string {
	details := []byte(v.OSV.Details)
	// Remove any new lines that are not preceded or followed by a new line.
	for i, r := range details {
		if r == '\n' && i > 0 && details[i-1] != '\n' && i+1 < len(details) && details[i+1] != '\n' {
			details[i] = ' '
		}
	}
	return strings.TrimSpace(strings.Replace(string(details), "\n\n", "\n\n  ", -1))
}

// href returns a URL embedded in the entry if any.
// If no suitable URL is found, it returns a default entry in
// pkg.go.dev/vuln.
func href(vuln *osv.Entry) string {
	for _, affected := range vuln.Affected {
		if url := affected.DatabaseSpecific.URL; url != "" {
			return url
		}
	}
	for _, r := range vuln.References {
		if r.Type == "WEB" {
			return r.URL
		}
	}
	return fmt.Sprintf("https://pkg.go.dev/vuln/%s", vuln.ID)
}

func getUpgradeCodeAction(fh source.FileHandle, req *modfile.Require, version string) (protocol.Command, error) {
	cmd, err := command.NewUpgradeDependencyCommand(upgradeTitle(version), command.DependencyArgs{
		URI:        protocol.URIFromSpanURI(fh.URI()),
		AddRequire: false,
		GoCmdArgs:  []string{req.Mod.Path + "@" + version},
	})
	if err != nil {
		return protocol.Command{}, err
	}
	return cmd, nil
}

func upgradeTitle(fixedVersion string) string {
	title := fmt.Sprintf("%s%v", upgradeCodeActionPrefix, fixedVersion)
	return title
}

// SelectUpgradeCodeActions takes a list of code actions for a required module
// and returns a more selective list of upgrade code actions,
// where the code actions have been deduped. Code actions unrelated to upgrade
// remain untouched.
func SelectUpgradeCodeActions(actions []protocol.CodeAction) []protocol.CodeAction {
	if len(actions) <= 1 {
		return actions // return early if no sorting necessary
	}
	var others []protocol.CodeAction

	set := make(map[string]protocol.CodeAction)
	for _, action := range actions {
		if strings.HasPrefix(action.Title, upgradeCodeActionPrefix) {
			set[action.Command.Title] = action
		} else {
			others = append(others, action)
		}
	}
	var upgrades []protocol.CodeAction
	for _, action := range set {
		upgrades = append(upgrades, action)
	}
	// Sort results by version number, latest first.
	// There should be no duplicates at this point.
	sort.Slice(upgrades, func(i, j int) bool {
		vi, vj := getUpgradeVersion(upgrades[i]), getUpgradeVersion(upgrades[j])
		return vi == "latest" || (vj != "latest" && semver.Compare(vi, vj) > 0)
	})
	// Choose at most one specific version and the latest.
	if getUpgradeVersion(upgrades[0]) == "latest" {
		return append(upgrades[:2], others...)
	}
	return append(upgrades[:1], others...)
}

func getUpgradeVersion(p protocol.CodeAction) string {
	return strings.TrimPrefix(p.Title, upgradeCodeActionPrefix)
}
