// Copyright (c) 2025 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package gen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/thriftrw/compile"
	"go.uber.org/thriftrw/internal/plugin"
	"go.uber.org/thriftrw/plugin/api"

	"go.uber.org/multierr"
)

// CodeGenerator lists possible code generators for a plugin.
type CodeGenerator struct {
	ServiceGenerator api.ServiceGenerator
}

// Options controls how code gets generated.
type Options struct {
	// OutputDir is the directory into which all generated code is written.
	//
	// This must be an absolute path.
	OutputDir string

	// PackagePrefix controls the import path prefix for all generated
	// packages.
	PackagePrefix string

	// ThriftRoot is the directory within whose tree all Thrift files consumed
	// are contained. The locations of the Thrift files relative to the
	// ThriftFile determines the module structure in OutputDir.
	//
	// This must be an absolute path.
	ThriftRoot string

	// NoRecurse determines whether code should be generated for included Thrift
	// files as well. If true, code gets generated only for the first module.
	NoRecurse bool

	// If true, we will not generate versioncheck.go files.
	NoVersionCheck bool

	// Code generation plugin
	Plugin CodeGenerator

	// Do not generate types.go
	NoTypes bool

	// Do not generate constants.go
	NoConstants bool

	// Do not generate service helpers
	NoServiceHelpers bool

	// Do not embed IDLs in generated code
	NoEmbedIDL bool

	// Do not generate Zap logging code
	NoZap bool

	// Name of the file to be generated by ThriftRW.
	OutputFile string

	// Generates an error on MarshalText and MarshalJSON if the enum value is
	// unrecognized.
	EnumTextMarshalStrict bool
}

// Generate generates code based on the given options.
func Generate(m *compile.Module, o *Options) error {
	if !filepath.IsAbs(o.ThriftRoot) {
		return fmt.Errorf(
			"ThriftRoot must be an absolute path: %q is not absolute",
			o.ThriftRoot)
	}

	if !filepath.IsAbs(o.OutputDir) {
		return fmt.Errorf(
			"OutputDir must be an absolute path: %q is not absolute",
			o.OutputDir)
	}

	importer := thriftPackageImporter{
		ImportPrefix: o.PackagePrefix,
		ThriftRoot:   o.ThriftRoot,
	}

	// Mapping of filenames relative to OutputDir to their contents.
	files := make(map[string][]byte)
	genBuilder := newGenerateServiceBuilder(importer)

	generate := func(m *compile.Module) error {
		path, contents, err := generateModule(m, importer, genBuilder, o)
		if err != nil {
			return generateError{Name: m.ThriftPath, Reason: err}
		}

		if err := addFile(files, path, contents); err != nil {
			return generateError{Name: m.ThriftPath, Reason: err}
		}

		return nil
	}

	// Root Modules correspond to the Thrift files that ThriftRW is
	// called with. Currently, ThriftRW can only be called with one
	// Thrift file at a time.
	if _, err := genBuilder.AddRootModule(m.ThriftPath); err != nil {
		return err
	}

	// Note that we call generate directly on only those modules that we need
	// to generate code for. If the user used --no-recurse, we're not going to
	// generate code for included modules.
	// Specifying an OutputFile file also means that code for included modules
	// should not be generated, since code for multiple modules cannot
	// be compiled into a single file.
	if o.NoRecurse || len(o.OutputFile) > 0 {
		if err := generate(m); err != nil {
			return err
		}
	} else {
		if err := m.Walk(generate); err != nil {
			return err
		}
	}

	plug := o.Plugin.ServiceGenerator
	if plug == nil {
		plug = plugin.EmptyServiceGenerator
	}

	res, err := plug.Generate(genBuilder.Build())
	if err != nil {
		return err
	}

	if err := mergeFiles(files, res.Files); err != nil {
		return err
	}

	for relPath, contents := range files {
		fullPath := filepath.Join(o.OutputDir, relPath)
		directory := filepath.Dir(fullPath)

		if err := os.MkdirAll(directory, 0755); err != nil {
			return fmt.Errorf("could not create directory %q: %v", directory, err)
		}

		if err := os.WriteFile(fullPath, contents, 0644); err != nil {
			return fmt.Errorf("failed to write %q: %v", fullPath, err)
		}
	}

	return nil
}

// normalizePackageName replaces hyphens in the file name with underscores.
func normalizePackageName(p string) string {
	return strings.Replace(filepath.Base(p), "-", "_", -1)
}

// ThriftPackageImporter determines import paths from a Thrift root.
type ThriftPackageImporter interface {
	// RelativePackage returns the import path for the top-level package of the
	// given Thrift file relative to the ImportPrefix.
	RelativePackage(file string) (importPath string, err error)

	// RelativePackage returns the relative path of the given Thrift file
	// relative to the ThriftRoot.
	RelativeThriftFilePath(file string) (relPath string, err error)

	// Package returns the import path for the top-level package of the given Thrift
	// file.
	Package(file string) (importPath string, err error)
}

type thriftPackageImporter struct {
	ImportPrefix string
	ThriftRoot   string
}

func (i thriftPackageImporter) RelativePackage(file string) (string, error) {
	return filepath.Rel(i.ThriftRoot, strings.TrimSuffix(file, ".thrift"))
}

func (i thriftPackageImporter) RelativeThriftFilePath(file string) (string, error) {
	return filepath.Rel(i.ThriftRoot, file)
}

func (i thriftPackageImporter) Package(file string) (string, error) {
	pkg, err := i.RelativePackage(file)
	if err != nil {
		return "", err
	}
	return filepath.Join(i.ImportPrefix, pkg), nil
}

func mergeFiles(dest, src map[string][]byte) error {
	var err error
	for path, contents := range src {
		err = multierr.Append(err, addFile(dest, path, contents))
	}
	return err
}

func addFile(dest map[string][]byte, path string, contents []byte) error {
	if _, ok := dest[path]; ok {
		return fmt.Errorf("file generation conflict: "+
			"multiple sources are trying to write to %q", path)
	}
	dest[path] = contents
	return nil
}

// generateModule generates the code for the given Thrift file and returns the
// path to the output file relative to OutputDir and the contents of the file.
func generateModule(
	m *compile.Module,
	i thriftPackageImporter,
	builder *generateServiceBuilder,
	o *Options,
) (outputFilepath string, contents []byte, err error) {
	// packageRelPath is the path relative to outputDir into which we'll be
	// writing the package for this Thrift file. For $thriftRoot/foo/bar.thrift,
	// packageRelPath is foo/bar, and packageDir is $outputDir/foo/bar. All
	// files for bar.thrift will be written to the $outputDir/foo/bar/ tree. The
	// package will be importable via $importPrefix/foo/bar.
	packageRelPath, err := i.RelativePackage(m.ThriftPath)
	if err != nil {
		return "", nil, err
	}
	// TODO(abg): Prefer top-level package name from `namespace go` directive.
	outputFilename := filepath.Base(packageRelPath)

	// Output file name defaults to the package name.
	outputFilename = outputFilename + ".go"
	if len(o.OutputFile) > 0 {
		outputFilename = o.OutputFile
	}
	outputFilepath = filepath.Join(packageRelPath, outputFilename)

	// importPath is the full import path for the top-level package generated
	// for this Thrift file.
	importPath, err := i.Package(m.ThriftPath)
	if err != nil {
		return "", nil, err
	}

	// converts package name from ab-def to ab_def for golang code generation
	normalizedPackageName := normalizePackageName(filepath.Base(packageRelPath))
	g := NewGenerator(&GeneratorOptions{
		Importer:              i,
		ImportPath:            importPath,
		PackageName:           normalizedPackageName,
		NoZap:                 o.NoZap,
		EnumTextMarshalStrict: o.EnumTextMarshalStrict,
	})

	if len(m.Constants) > 0 {
		for _, constantName := range sortStringKeys(m.Constants) {
			if err := Constant(g, m.Constants[constantName]); err != nil {
				return "", nil, err
			}
		}
	}

	if len(m.Types) > 0 {
		for _, typeName := range sortStringKeys(m.Types) {
			if err := TypeDefinition(g, m.Types[typeName]); err != nil {
				return "", nil, err
			}
		}
	}

	if !o.NoEmbedIDL {
		if err := embedIDL(g, i, m); err != nil {
			return "", nil, err
		}
	}

	addModules := func(m *compile.Module) error {
		_, err := builder.AddModule(m.ThriftPath)
		return err
	}

	if err := m.Walk(addModules); err != nil {
		return "", nil, err
	}

	// Services must be generated last because names of user-defined types take
	// precedence over the names we pick for the service types.
	if len(m.Services) > 0 {
		for _, serviceName := range sortStringKeys(m.Services) {
			service := m.Services[serviceName]

			// generateModule gets called only for those modules for which we
			// need to generate code. With --no-recurse, generateModule is
			// called only on the root file specified by the user and not its
			// included modules. Only services defined in these files are
			// considered root services; plugins will generate code only for
			// root services, even though they have information about the
			// whole service tree.
			if _, err := builder.AddRootService(service); err != nil {
				return "", nil, err
			}
		}

		if err = Services(g, m.Services); err != nil {
			return "", nil, fmt.Errorf("could not generate code for services %v", err)
		}
	}

	buff := new(bytes.Buffer)
	if err := g.Write(buff, nil); err != nil {
		return "", nil, fmt.Errorf("could not write output for file %q: %v", outputFilename, err)
	}

	return outputFilepath, buff.Bytes(), nil
}
