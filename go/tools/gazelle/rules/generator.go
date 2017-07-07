/* Copyright 2016 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rules

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	bf "github.com/bazelbuild/buildtools/build"
	"github.com/bazelbuild/rules_go/go/tools/gazelle/config"
	"github.com/bazelbuild/rules_go/go/tools/gazelle/packages"
	"path"
	"sort"
)

const (
	// goRulesBzl is the label of the Skylark file which provides Go rules
	goRulesBzl = "@io_bazel_rules_go//go:def.bzl"
	// defaultLibName is the name of the default go_library rule in a Go
	// package directory. It must be consistent to DEFAULT_LIB in go/private/common.bf.
	defaultLibName = "go_default_library"
	// defaultTestName is a name of an internal test corresponding to
	// defaultLibName. It does not need to be consistent to something but it
	// just needs to be unique in the Bazel package
	defaultTestName = "go_default_test"
	// defaultXTestName is a name of an external test corresponding to
	// defaultLibName.
	defaultXTestName = "go_default_xtest"
	// defaultProtosName is the name of a filegroup created
	// whenever the library contains .pb.go files
	defaultProtosName = "go_default_library_protos"
	// defaultCgoLibName is the name of the default cgo_library rule in a Go package directory.
	defaultCgoLibName = "cgo_default_library"
)

// Generator generates Bazel build rules for Go build targets
type Generator interface {
	// Generate generates a syntax tree of a BUILD file for "pkg". The file
	// contains rules for each non-empty target in "pkg". It also contains
	// "load" statements necessary for the rule constructors. If this is the
	// top-level package in the repository, the file will contain a
	// "go_prefix" rule.
	Generate(pkg *packages.Package) *bf.File

	// GenerateVendor generates a single BUILD file for all vendored packages
	// under ${bfPath}/vendor/.
	GenerateVendor(bfPath string, vendored []*packages.Package) *bf.File
}

func NewGenerator(c *config.Config) Generator {
	var (
		// TODO(yugui) Support another resolver to cover the pattern 2 in
		// https://github.com/bazelbuild/rules_go/issues/16#issuecomment-216010843
		r = structuredResolver{goPrefix: c.GoPrefix}
	)

	var e labelResolver
	switch c.DepMode {
	case config.ExternalMode:
		e = newExternalResolver()
	case config.VendorMode:
		e = vendoredResolver{}
	case config.UnoMode:
		dirs := sort.StringSlice{}
		for _, dir := range c.Dirs {
			dirs = append(dirs, strings.TrimPrefix(dir, path.Clean(c.RepoRoot) + "/"))
		}
		// Sort all subprojects so that we can pick the most specific match when
		// looking for the subproject that a package belongs to.
		sort.Sort(dirs)
		sort.Reverse(dirs)
		e = unoResolver{
			projRoots: []string(dirs),
		}
	default:
		return nil
	}

	return &generator{
		c: c,
		r: resolverFunc(func(importpath, dir string) (label, error) {
			if importpath != c.GoPrefix && !strings.HasPrefix(importpath, c.GoPrefix+"/") && !isRelative(importpath) {
				return e.resolve(importpath, dir)
			}
			return r.resolve(importpath, dir)
		}),
	}
}

type generator struct {
	c *config.Config
	r labelResolver
}

func (g *generator) Generate(pkg *packages.Package) *bf.File {
	f := &bf.File{
		Path: filepath.Join(pkg.Dir, g.c.DefaultBuildFileName()),
	}
	rs := g.generateRules(pkg.Dir, pkg)
	if load := g.generateLoad(rs); load != nil {
		f.Stmt = append(f.Stmt, load)
	}
	for _, r := range rs {
		f.Stmt = append(f.Stmt, r.Call)
	}
	return f
}

func (g *generator) GenerateVendor(bfPath string, vendored []*packages.Package) *bf.File {
	f := &bf.File{
		Path: filepath.Join(bfPath, g.c.DefaultBuildFileName()),
	}

	// Generate the rules that apply to each package.
	rules := make([]*bf.Rule, 0)
	for _, vendoredPkg := range vendored {
		rules = append(rules, g.generateVendorRules(bfPath, vendoredPkg)...)
	}

	if load := g.generateLoad(rules); load != nil {
		f.Stmt = append(f.Stmt, load)
	}
	for _, r := range rules {
		f.Stmt = append(f.Stmt, r.Call)
	}
	return f
}

func (g *generator) generateRules(bfPath string, pkg *packages.Package) []*bf.Rule {
	var rules []*bf.Rule
	if pkg.Rel == "" {
		rules = append(rules, newRule("go_prefix", []interface{}{g.c.GoPrefix}, nil))
	}

	cgoLibrary, r := g.generateCgoLib(bfPath, pkg)
	if r != nil {
		rules = append(rules, r)
	}

	library, r := g.generateLib(bfPath, pkg, cgoLibrary)
	if r != nil {
		rules = append(rules, r)
	}

	if r := g.generateBin(bfPath, pkg, library); r != nil {
		rules = append(rules, r)
	}

	if r := g.filegroup(bfPath, pkg); r != nil {
		rules = append(rules, r)
	}

	if r := g.generateTest(bfPath, pkg, library); r != nil {
		rules = append(rules, r)
	}

	if r := g.generateXTest(bfPath, pkg, library); r != nil {
		rules = append(rules, r)
	}

	return rules
}

func (g *generator) generateVendorRules(bfPath string, pkg *packages.Package) []*bf.Rule {
	var rules []*bf.Rule
	cgoLibrary, r := g.generateCgoLib(bfPath, pkg)
	if r != nil {
		rules = append(rules, r)
	}

	_, r = g.generateLib(bfPath, pkg, cgoLibrary)
	if r != nil {
		rules = append(rules, r)
	}

	if r := g.filegroup(bfPath, pkg); r != nil {
		rules = append(rules, r)
	}

	return rules
}

func (g *generator) generateBin(bfPath string, pkg *packages.Package, library string) *bf.Rule {
	if !pkg.IsCommand() || pkg.Binary.Sources.IsEmpty() && library == "" {
		return nil
	}
	name := filepath.Base(pkg.Dir)
	visibility := checkInternalVisibility(pkg.Rel, "//visibility:public")
	return g.generateRule(bfPath, pkg.Rel, "go_binary", name, visibility, library, false, pkg.Binary)
}

func (g *generator) generateLib(bfPath string, pkg *packages.Package, cgoName string) (string, *bf.Rule) {
	if !pkg.Library.HasGo() && cgoName == "" {
		return "", nil
	}

	name := defaultLibName
	var visibility string
	if pkg.IsCommand() {
		// Libraries made for a go_binary should not be exposed to the public.
		visibility = "//visibility:private"
	} else {
		visibility = checkInternalVisibility(pkg.Rel, "//visibility:public")
	}

	rule := g.generateRule(bfPath, pkg.Rel, "go_library", name, visibility, cgoName, false, pkg.Library)
	return rule.Name(), rule
}

func (g *generator) generateCgoLib(bfPath string, pkg *packages.Package) (string, *bf.Rule) {
	if !pkg.CgoLibrary.HasGo() {
		return "", nil
	}

	name := defaultCgoLibName
	visibility := "//visibility:private"
	rule := g.generateRule(bfPath, pkg.Rel, "cgo_library", name, visibility, "", false, pkg.CgoLibrary)
	return rule.Name(), rule
}

// checkInternalVisibility overrides the given visibility if the package is
// internal.
func checkInternalVisibility(rel, visibility string) string {
	if i := strings.LastIndex(rel, "/internal/"); i >= 0 {
		visibility = fmt.Sprintf("//%s:__subpackages__", rel[:i])
	} else if strings.HasPrefix(rel, "internal/") {
		visibility = "//:__subpackages__"
	}
	return visibility
}

// filegroup is a small hack for directories with pre-generated .pb.go files
// and also source .proto files.  This creates a filegroup for the .proto in
// addition to the usual go_library for the .pb.go files.
func (g *generator) filegroup(bfPath string, pkg *packages.Package) *bf.Rule {
	bfRel := relPath(bfPath, pkg.Dir)

	if !pkg.HasPbGo || len(pkg.Protos) == 0 {
		return nil
	}

	name := defaultProtosName
	protos := pkg.Protos
	if bfRel != "" {
		protos = make([]string, 0, len(pkg.Protos))
		for _, proto := range protos {
			protos = append(protos, bfRel + "/" + proto)
		}

		name = bfRel + "_" + defaultProtosName
	}

	return newRule("filegroup", nil, []keyvalue{
		{key: "name", value: name},
		{key: "srcs", value: protos},
		{key: "visibility", value: []string{"//visibility:public"}},
	})
}

func (g *generator) generateTest(bfPath string, pkg *packages.Package, library string) *bf.Rule {
	if !pkg.Test.HasGo() {
		return nil
	}

	var name string
	if library == "" || library == defaultLibName {
		name = defaultTestName
	} else {
		name = library + "_test"
	}

	return g.generateRule(bfPath, pkg.Rel, "go_test", name, "", library, pkg.HasTestdata, pkg.Test)
}

func (g *generator) generateXTest(bfPath string, pkg *packages.Package, library string) *bf.Rule {
	if !pkg.XTest.HasGo() {
		return nil
	}

	var name string
	if library == "" || library == defaultLibName {
		name = defaultXTestName
	} else {
		name = library + "_xtest"
	}

	return g.generateRule(bfPath, pkg.Rel, "go_test", name, "", "", pkg.HasTestdata, pkg.XTest)
}

func (g *generator) generateRule(bfPath, rel, kind, name, visibility, library string, hasTestdata bool, target packages.Target) *bf.Rule {
	bfRel := relPath(bfPath, path.Join(g.c.RepoRoot, rel))
	prependBfRel := func(s string) (string, error) {
		return bfRel + "/" + s, nil
	}

	if bfRel != "" {
		//name = bfRel + "_" + name
		name = bfRel
	}

	// Construct attrs in the same order that bf.Rewrite uses. See
	// namePriority in github.com/bazelbuild/buildtools/build/rewrite.go.
	attrs := []keyvalue{
		{"name", name},
	}
	if !target.Sources.IsEmpty() {
		// Ignore errs here since its not possible for prependBfRel to return an error.
		srcs := target.Sources
		if bfRel != "" {
			srcs, _ = target.Sources.Map(prependBfRel)
		}
		attrs = append(attrs, keyvalue{"srcs", srcs})
	}
	if !target.CLinkOpts.IsEmpty() {
		attrs = append(attrs, keyvalue{"clinkopts", target.CLinkOpts})
	}
	if !target.COpts.IsEmpty() {
		attrs = append(attrs, keyvalue{"copts", target.COpts})
	}
	if hasTestdata {
		var glob globvalue
		if bfRel != "" {
			glob = globvalue{patterns: []string{bfRel + "/testdata/**"}}
		} else {
			glob = globvalue{patterns: []string{"testdata/**"}}
		}
		attrs = append(attrs, keyvalue{"data", glob})
	}
	if library != "" {
		attrs = append(attrs, keyvalue{"library", ":" + library})
	}
	if visibility != "" {
		attrs = append(attrs, keyvalue{"visibility", []string{visibility}})
	}
	if !target.Imports.IsEmpty() {
		deps := g.dependencies(target.Imports, rel)
		attrs = append(attrs, keyvalue{"deps", deps})
	}
	return newRule(kind, nil, attrs)
}

func (g *generator) generateLoad(rs []*bf.Rule) bf.Expr {
	loadableKinds := []string{
		// keep sorted
		"cgo_library",
		"go_binary",
		"go_library",
		"go_prefix",
		"go_test",
	}

	kinds := make(map[string]bool)
	for _, r := range rs {
		kinds[r.Kind()] = true
	}
	args := make([]bf.Expr, 0, len(kinds)+1)
	args = append(args, &bf.StringExpr{Value: goRulesBzl})
	for _, k := range loadableKinds {
		if kinds[k] {
			args = append(args, &bf.StringExpr{Value: k})
		}
	}
	if len(args) == 1 {
		return nil
	}
	return &bf.CallExpr{
		X:            &bf.LiteralExpr{Token: "load"},
		List:         args,
		ForceCompact: true,
	}
}

func (g *generator) dependencies(imports packages.PlatformStrings, dir string) packages.PlatformStrings {
	resolve := func(imp string) (string, error) {
		if l, err := g.r.resolve(imp, dir); err != nil {
			return "", fmt.Errorf("in dir %q, could not resolve import path %q: %v", dir, imp, err)
		} else {
			return l.String(), nil
		}
	}

	deps, errors := imports.Map(resolve)
	for _, err := range errors {
		log.Print(err)
	}
	deps.Clean()
	return deps
}

// isRelative determines if an importpath is relative.
func isRelative(importpath string) bool {
	return strings.HasPrefix(importpath, "./") || strings.HasPrefix(importpath, "..")
}

// relPath returns the relative path between the provided paths or returns "" if
// the two paths aren't relative.
func relPath(base string, p string) string {
	cleanP := path.Clean(p)
	cleanBase := path.Clean(base)

	if cleanP != cleanBase && strings.HasPrefix(cleanP, cleanBase) {
		return strings.TrimPrefix(cleanP, cleanBase)[1:]
	} else {
		return ""
	}
}
