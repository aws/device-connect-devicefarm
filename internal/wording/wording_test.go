// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package wording

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parsePackageSources parses every non-test Go file of this package.
func parsePackageSources(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fileSet := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		require.NoError(t, err)
		files = append(files, file)
	}
	require.NotEmpty(t, files)
	return fileSet, files
}

// go vet checks a format only where a printf call takes it: it misses errors.New, a string
// join and a struct field. So the Format suffix must tell the truth.
func TestGivenEveryConstantWhenItsNameIsReadThenTheFormatSuffixMatchesItsVerbs(t *testing.T) {
	fileSet, files := parsePackageSources(t)
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	_, err := (&types.Config{}).Check("wording", fileSet, files, info)
	require.NoError(t, err)

	for _, object := range info.Defs {
		declared, isConstant := object.(*types.Const)
		if !isConstant {
			continue
		}
		require.Equal(t, constant.String, declared.Val().Kind(), declared.Name())
		text := constant.StringVal(declared.Val())
		if strings.HasSuffix(declared.Name(), "Format") {
			// A percent sign left after every literal "%%" is gone starts a verb.
			assert.Contains(t, strings.ReplaceAll(text, "%%", ""), "%", declared.Name())
		} else {
			assert.NotContains(t, text, "%", declared.Name())
		}
	}
}

func TestGivenThePackageSourcesWhenParsedThenTheyHoldConstantsOnly(t *testing.T) {
	fileSet, files := parsePackageSources(t)
	for _, file := range files {
		assert.Empty(t, file.Imports, "%s imports a package", fileSet.Position(file.Pos()).Filename)
		for _, declaration := range file.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			assert.True(t, isGeneral && general.Tok == token.CONST, "%s is not a const declaration", fileSet.Position(declaration.Pos()))
		}
	}
}
