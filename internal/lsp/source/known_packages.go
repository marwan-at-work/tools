package source

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// KnownPackages returns a list of all known packages
// in this workspace that are not imported by the
// given file.
func KnownPackages(ctx context.Context, snapshot Snapshot, fh VersionedFileHandle) ([]string, error) {
	pkg, pgf, err := GetParsedFile(ctx, snapshot, fh, NarrowestPackage)
	if err != nil {
		return nil, fmt.Errorf("GetParsedFile: %w", err)
	}
	alreadyImported := map[string]struct{}{}
	for _, imp := range pgf.File.Imports {
		alreadyImported[imp.Path.Value] = struct{}{}
	}
	pkgs, err := snapshot.KnownPackages(ctx)
	if err != nil {
		return nil, err
	}
	visited := map[string]struct{}{}
	var resp []string
	for _, knownPkg := range pkgs {
		path := knownPkg.PkgPath()
		gofiles := knownPkg.CompiledGoFiles()
		if len(gofiles) == 0 || gofiles[0].File.Name == nil {
			continue
		}
		pkgName := gofiles[0].File.Name.Name
		// package main cannot be imported
		if pkgName == "main" {
			continue
		}
		// no need to import what the file already imports
		if _, ok := alreadyImported[path]; ok {
			continue
		}
		// snapshot.KnownPackages could have multiple versions of a pkg
		if _, ok := visited[path]; ok {
			continue
		}
		visited[path] = struct{}{}
		// make sure internal packages are importable by the file
		if !isValidImport(pkg.PkgPath(), path) {
			continue
		}
		// naive check on cyclical imports
		if isCyclicalImport(pkg, knownPkg) {
			continue
		}
		resp = append(resp, path)
	}
	sort.Slice(resp, func(i, j int) bool {
		importI, importJ := resp[i], resp[j]
		iHasDot := strings.Contains(importI, ".")
		jHasDot := strings.Contains(importJ, ".")
		if iHasDot && !jHasDot {
			return false
		}
		if jHasDot && !iHasDot {
			return true
		}
		return importI < importJ
	})
	return resp, nil
}

func isCyclicalImport(pkg, imported Package) bool {
	for _, imp := range imported.Imports() {
		if imp.PkgPath() == pkg.PkgPath() {
			return true
		}
	}
	return false
}

func isValidImport(pkgPath, importPkgPath string) bool {
	if strings.HasPrefix(importPkgPath, "internal/") {
		return false
	}
	if i := strings.LastIndex(importPkgPath, "/internal/"); i > -1 {
		return strings.HasPrefix(pkgPath, importPkgPath[:i])
	}
	if i := strings.LastIndex(importPkgPath, "/internal"); i > -1 {
		return strings.HasPrefix(pkgPath, importPkgPath[:i])
	}
	return true
}
