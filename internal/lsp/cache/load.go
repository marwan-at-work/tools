// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"go/types"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/lsp/debug/tag"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/memoize"
	"golang.org/x/tools/internal/packagesinternal"
	"golang.org/x/tools/internal/span"
	errors "golang.org/x/xerrors"
)

// metadata holds package metadata extracted from a call to packages.Load.
type metadata struct {
	id              packageID
	pkgPath         packagePath
	name            packageName
	goFiles         []span.URI
	compiledGoFiles []span.URI
	forTest         packagePath
	typesSizes      types.Sizes
	errors          []packages.Error
	deps            []packageID
	missingDeps     map[packagePath]struct{}
	module          *packages.Module

	// config is the *packages.Config associated with the loaded package.
	config *packages.Config
}

// load calls packages.Load for the given scopes, updating package metadata,
// import graph, and mapped files with the result.
func (s *snapshot) load(ctx context.Context, allowNetwork bool, scopes ...interface{}) error {
	var query []string
	var containsDir bool // for logging
	for _, scope := range scopes {
		switch scope := scope.(type) {
		case packagePath:
			if scope == "command-line-arguments" {
				panic("attempted to load command-line-arguments")
			}
			// The only time we pass package paths is when we're doing a
			// partial workspace load. In those cases, the paths came back from
			// go list and should already be GOPATH-vendorized when appropriate.
			query = append(query, string(scope))
		case fileURI:
			uri := span.URI(scope)
			// Don't try to load a file that doesn't exist.
			fh := s.FindFile(uri)
			if fh == nil || fh.Kind() != source.Go {
				continue
			}
			query = append(query, fmt.Sprintf("file=%s", uri.Filename()))
		case moduleLoadScope:
			query = append(query, fmt.Sprintf("%s/...", scope))
		case viewLoadScope:
			// If we are outside of GOPATH, a module, or some other known
			// build system, don't load subdirectories.
			if !s.ValidBuildConfiguration() {
				query = append(query, "./")
			} else {
				query = append(query, "./...")
			}
		default:
			panic(fmt.Sprintf("unknown scope type %T", scope))
		}
		switch scope.(type) {
		case viewLoadScope, moduleLoadScope:
			containsDir = true
		}
	}
	if len(query) == 0 {
		return nil
	}
	sort.Strings(query) // for determinism

	ctx, done := event.Start(ctx, "cache.view.load", tag.Query.Of(query))
	defer done()

	flags := source.LoadWorkspace
	if allowNetwork {
		flags |= source.AllowNetwork
	}
	_, inv, cleanup, err := s.goCommandInvocation(ctx, flags, &gocommand.Invocation{
		WorkingDir: s.view.rootURI.Filename(),
	})
	if err != nil {
		return err
	}

	// Set a last resort deadline on packages.Load since it calls the go
	// command, which may hang indefinitely if it has a bug. golang/go#42132
	// and golang/go#42255 have more context.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cfg := s.config(ctx, inv)
	pkgs, err := packages.Load(cfg, query...)
	cleanup()

	// If the context was canceled, return early. Otherwise, we might be
	// type-checking an incomplete result. Check the context directly,
	// because go/packages adds extra information to the error.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		event.Error(ctx, "go/packages.Load", err, tag.Snapshot.Of(s.ID()), tag.Directory.Of(cfg.Dir), tag.Query.Of(query), tag.PackageCount.Of(len(pkgs)))
	} else {
		event.Log(ctx, "go/packages.Load", tag.Snapshot.Of(s.ID()), tag.Directory.Of(cfg.Dir), tag.Query.Of(query), tag.PackageCount.Of(len(pkgs)))
	}
	if len(pkgs) == 0 {
		if err != nil {
			// Try to extract the load error into a structured error with
			// diagnostics.
			if criticalErr := s.parseLoadError(ctx, err); criticalErr != nil {
				return criticalErr
			}
		} else {
			err = fmt.Errorf("no packages returned")
		}
		return errors.Errorf("%v: %w", err, source.PackagesLoadError)
	}
	for _, pkg := range pkgs {
		if !containsDir || s.view.Options().VerboseOutput {
			event.Log(ctx, "go/packages.Load", tag.Snapshot.Of(s.ID()), tag.PackagePath.Of(pkg.PkgPath), tag.Files.Of(pkg.CompiledGoFiles))
		}
		// Ignore packages with no sources, since we will never be able to
		// correctly invalidate that metadata.
		if len(pkg.GoFiles) == 0 && len(pkg.CompiledGoFiles) == 0 {
			continue
		}
		// Special case for the builtin package, as it has no dependencies.
		if pkg.PkgPath == "builtin" {
			if err := s.buildBuiltinPackage(ctx, pkg.GoFiles); err != nil {
				return err
			}
			continue
		}
		// Skip test main packages.
		if isTestMain(pkg, s.view.gocache) {
			continue
		}
		// Skip filtered packages. They may be added anyway if they're
		// dependencies of non-filtered packages.
		if s.view.allFilesExcluded(pkg) {
			continue
		}
		// Set the metadata for this package.
		m, err := s.setMetadata(ctx, packagePath(pkg.PkgPath), pkg, cfg, map[packageID]struct{}{})
		if err != nil {
			return err
		}
		if _, err := s.buildPackageHandle(ctx, m.id, s.workspaceParseMode(m.id)); err != nil {
			return err
		}
	}
	// Rebuild the import graph when the metadata is updated.
	s.clearAndRebuildImportGraph()

	return nil
}

func (s *snapshot) parseLoadError(ctx context.Context, loadErr error) *source.CriticalError {
	if strings.Contains(loadErr.Error(), "cannot find main module") {
		return s.WorkspaceLayoutError(ctx)
	}
	criticalErr := &source.CriticalError{
		MainError: loadErr,
	}
	// Attempt to place diagnostics in the relevant go.mod files, if any.
	for _, uri := range s.ModFiles() {
		fh, err := s.GetFile(ctx, uri)
		if err != nil {
			continue
		}
		srcErr := s.extractGoCommandError(ctx, s, fh, loadErr.Error())
		if srcErr == nil {
			continue
		}
		criticalErr.ErrorList = append(criticalErr.ErrorList, srcErr)
	}
	return criticalErr
}

// workspaceLayoutErrors returns a diagnostic for every open file, as well as
// an error message if there are no open files.
func (s *snapshot) WorkspaceLayoutError(ctx context.Context) *source.CriticalError {
	// Assume the workspace is misconfigured only if we've detected an invalid
	// build configuration. Currently, a valid build configuration is either a
	// module at the root of the view or a GOPATH workspace.
	if s.ValidBuildConfiguration() {
		return nil
	}
	if len(s.workspace.getKnownModFiles()) == 0 {
		return nil
	}
	if s.view.userGo111Module == off {
		return nil
	}
	if s.workspace.moduleSource != legacyWorkspace {
		return nil
	}
	// The user's workspace contains go.mod files and they have
	// GO111MODULE=on, so we should guide them to create a
	// workspace folder for each module.

	// Add a diagnostic to every open file, or return a general error if
	// there aren't any.
	var open []source.VersionedFileHandle
	s.mu.Lock()
	for _, fh := range s.files {
		if s.isOpenLocked(fh.URI()) {
			open = append(open, fh)
		}
	}
	s.mu.Unlock()

	msg := `gopls requires one module per workspace folder.
You can work with multiple modules by opening each one as a workspace folder.
Improvements to this workflow will be coming soon (https://github.com/golang/go/issues/32394),
and you can learn more here: https://github.com/golang/go/issues/36899.`

	criticalError := &source.CriticalError{
		MainError: errors.New(msg),
	}
	if len(open) == 0 {
		return criticalError
	}
	for _, fh := range open {
		// Place the diagnostics on the package or module declarations.
		var rng protocol.Range
		switch fh.Kind() {
		case source.Go:
			if pgf, err := s.ParseGo(ctx, fh, source.ParseHeader); err == nil {
				pkgDecl := span.NewRange(s.FileSet(), pgf.File.Package, pgf.File.Name.End())
				if spn, err := pkgDecl.Span(); err == nil {
					rng, _ = pgf.Mapper.Range(spn)
				}
			}
		case source.Mod:
			if pmf, err := s.ParseMod(ctx, fh); err == nil {
				if pmf.File.Module != nil && pmf.File.Module.Syntax != nil {
					rng, _ = rangeFromPositions(pmf.Mapper, pmf.File.Module.Syntax.Start, pmf.File.Module.Syntax.End)
				}
			}
		}
		criticalError.ErrorList = append(criticalError.ErrorList, &source.Error{
			URI:     fh.URI(),
			Range:   rng,
			Kind:    source.ListError,
			Message: msg,
		})
	}
	return criticalError
}

type workspaceDirKey string

type workspaceDirData struct {
	dir string
	err error
}

// getWorkspaceDir gets the URI for the workspace directory associated with
// this snapshot. The workspace directory is a temp directory containing the
// go.mod file computed from all active modules.
func (s *snapshot) getWorkspaceDir(ctx context.Context) (span.URI, error) {
	s.mu.Lock()
	h := s.workspaceDirHandle
	s.mu.Unlock()
	if h != nil {
		return getWorkspaceDir(ctx, h, s.generation)
	}
	file, err := s.workspace.modFile(ctx, s)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	modContent, err := file.Format()
	if err != nil {
		return "", err
	}
	sumContent, err := s.workspace.sumFile(ctx, s)
	if err != nil {
		return "", err
	}
	hash.Write(modContent)
	hash.Write(sumContent)
	key := workspaceDirKey(hash.Sum(nil))
	s.mu.Lock()
	h = s.generation.Bind(key, func(context.Context, memoize.Arg) interface{} {
		tmpdir, err := ioutil.TempDir("", "gopls-workspace-mod")
		if err != nil {
			return &workspaceDirData{err: err}
		}

		for name, content := range map[string][]byte{
			"go.mod": modContent,
			"go.sum": sumContent,
		} {
			filename := filepath.Join(tmpdir, name)
			if err := ioutil.WriteFile(filename, content, 0644); err != nil {
				os.RemoveAll(tmpdir)
				return &workspaceDirData{err: err}
			}
		}

		return &workspaceDirData{dir: tmpdir}
	}, func(v interface{}) {
		d := v.(*workspaceDirData)
		if d.dir != "" {
			if err := os.RemoveAll(d.dir); err != nil {
				event.Error(context.Background(), "cleaning workspace dir", err)
			}
		}
	})
	s.workspaceDirHandle = h
	s.mu.Unlock()
	return getWorkspaceDir(ctx, h, s.generation)
}

func getWorkspaceDir(ctx context.Context, h *memoize.Handle, g *memoize.Generation) (span.URI, error) {
	v, err := h.Get(ctx, g, nil)
	if err != nil {
		return "", err
	}
	return span.URIFromPath(v.(*workspaceDirData).dir), nil
}

// setMetadata extracts metadata from pkg and records it in s. It
// recurses through pkg.Imports to ensure that metadata exists for all
// dependencies.
func (s *snapshot) setMetadata(ctx context.Context, pkgPath packagePath, pkg *packages.Package, cfg *packages.Config, seen map[packageID]struct{}) (*metadata, error) {
	id := packageID(pkg.ID)
	if _, ok := seen[id]; ok {
		return nil, errors.Errorf("import cycle detected: %q", id)
	}
	// Recreate the metadata rather than reusing it to avoid locking.
	m := &metadata{
		id:         id,
		pkgPath:    pkgPath,
		name:       packageName(pkg.Name),
		forTest:    packagePath(packagesinternal.GetForTest(pkg)),
		typesSizes: pkg.TypesSizes,
		errors:     pkg.Errors,
		config:     cfg,
		module:     pkg.Module,
	}

	for _, filename := range pkg.CompiledGoFiles {
		uri := span.URIFromPath(filename)
		m.compiledGoFiles = append(m.compiledGoFiles, uri)
		s.addID(uri, m.id)
	}
	for _, filename := range pkg.GoFiles {
		uri := span.URIFromPath(filename)
		m.goFiles = append(m.goFiles, uri)
		s.addID(uri, m.id)
	}

	// TODO(rstambler): is this still necessary?
	copied := map[packageID]struct{}{
		id: {},
	}
	for k, v := range seen {
		copied[k] = v
	}
	for importPath, importPkg := range pkg.Imports {
		importPkgPath := packagePath(importPath)
		importID := packageID(importPkg.ID)

		m.deps = append(m.deps, importID)

		// Don't remember any imports with significant errors.
		if importPkgPath != "unsafe" && len(importPkg.CompiledGoFiles) == 0 {
			if m.missingDeps == nil {
				m.missingDeps = make(map[packagePath]struct{})
			}
			m.missingDeps[importPkgPath] = struct{}{}
			continue
		}
		if s.getMetadata(importID) == nil {
			if _, err := s.setMetadata(ctx, importPkgPath, importPkg, cfg, copied); err != nil {
				event.Error(ctx, "error in dependency", err)
			}
		}
	}

	// Add the metadata to the cache.
	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO: We should make sure not to set duplicate metadata,
	// and instead panic here. This can be done by making sure not to
	// reset metadata information for packages we've already seen.
	if original, ok := s.metadata[m.id]; ok {
		m = original
	} else {
		s.metadata[m.id] = m
	}

	// Set the workspace packages. If any of the package's files belong to the
	// view, then the package may be a workspace package.
	for _, uri := range append(m.compiledGoFiles, m.goFiles...) {
		if !s.view.contains(uri) {
			continue
		}

		// The package's files are in this view. It may be a workspace package.
		if strings.Contains(string(uri), "/vendor/") {
			// Vendored packages are not likely to be interesting to the user.
			continue
		}

		switch {
		case m.forTest == "":
			// A normal package.
			s.workspacePackages[m.id] = pkgPath
		case m.forTest == m.pkgPath, m.forTest+"_test" == m.pkgPath:
			// The test variant of some workspace package or its x_test.
			// To load it, we need to load the non-test variant with -test.
			s.workspacePackages[m.id] = m.forTest
		default:
			// A test variant of some intermediate package. We don't care about it.
		}
	}
	return m, nil
}

func isTestMain(pkg *packages.Package, gocache string) bool {
	// Test mains must have an import path that ends with ".test".
	if !strings.HasSuffix(pkg.PkgPath, ".test") {
		return false
	}
	// Test main packages are always named "main".
	if pkg.Name != "main" {
		return false
	}
	// Test mains always have exactly one GoFile that is in the build cache.
	if len(pkg.GoFiles) > 1 {
		return false
	}
	if !source.InDir(gocache, pkg.GoFiles[0]) {
		return false
	}
	return true
}
