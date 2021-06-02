// Copyright (c) 2021 Uber Technologies, Inc.
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

package engine

import (
	"errors"
	"go/ast"
	"go/token"
	"reflect"

	"github.com/uber-go/gopatch/internal/data"
	"github.com/uber-go/gopatch/internal/goast"
	"github.com/uber-go/gopatch/internal/pgo"
)

// stmtSliceContainerMatcher matches AST nodes that contain statement slices
// ([]Stmt) anywhere in a Go AST.
//
// Specifically, it matches BlockStmt, CaseClause, and CommClause nodes.
type stmtSliceContainerMatcher struct {
	Stmts Matcher // matcher for a []Stmt
}

// Compiles a Matcher from a pgo.StmtList. When a list of statements is
// provided at the top level in the minus section of the patch, we should
// match anywhere in the AST where a []ast.Stmt can be present. We'll use
// stmtSliceContainerMatcher for this.
func (c *matcherCompiler) compilePGoStmtList(slist *pgo.StmtList) Matcher {
	var list []ast.Stmt
	if len(slist.List) > 0 {
		list = append(list, dotsStmt(c.patchStart))
		list = append(list, slist.List...)
		list = append(list, dotsStmt(c.patchEnd))
	}
	return stmtSliceContainerMatcher{
		Stmts: c.compile(reflect.ValueOf(list)),
	}
}

func (m stmtSliceContainerMatcher) Match(v reflect.Value, d data.Data, r Region) (data.Data, bool) {
	t := v.Type()
	if t.Kind() != reflect.Ptr {
		return d, false
	}

	// Instead of copying individual fields of BlockStmt, CaseClause, and
	// CommClause, we will match against the statements (present under
	// .List in BlockStmt and .Body under CaseClause and CommClause) and
	// make a shallow copy of all other attributes of the object, to be
	// replicated in the Replacer.

	v, t = v.Elem(), t.Elem()
	var (
		stmtField string

		// Position of the end of the text right before statements
		// start. For block statements, this will be the position of
		// "{", for case clauses, it will be the position of ":".
		stmtPreludeEnd token.Pos
	)
	switch t {
	case goast.BlockStmtType:
		stmtField = "List"
		stmtPreludeEnd = r.Pos
	case goast.CaseClauseType, goast.CommClauseType:
		stmtField = "Body"
		stmtPreludeEnd = token.Pos(v.FieldByName("Colon").Int())
	default:
		return d, false
	}

	var (
		// Fields besides the one containing []Stmt.
		fields []stmtListField

		// Information about the field containing []Stmt.
		stmtsField stmtListField
	)
	for i := 0; i < t.NumField(); i++ {
		f := stmtListField{FieldIdx: i, Value: v.Field(i)}
		if t.Field(i).Name == stmtField {
			stmtsField = f
		} else {
			fields = append(fields, f)
		}
	}

	r.Pos = stmtPreludeEnd + 1
	return m.Stmts.Match(stmtsField.Value, data.WithValue(d, stmtListKey, stmtListData{
		Type:         t,
		StmtFieldIdx: stmtsField.FieldIdx,
		OtherFields:  fields,
		UnchangedRegion: Region{
			Pos: r.Pos,
			End: stmtPreludeEnd,
		},
	}), r)
}

// stmtSliceContainerReplacer reproduces an AST node for which a statement
// list was previously matched.
//
// For example, if we previously matched a CaseClause, this will reproduce the
// original CaseClause but with its body replaced with the output of the Stmts
// replacer.
type stmtSliceContainerReplacer struct {
	Stmts Replacer // replacer for []Stmt
}

// Compiles a Replacer from a pgo.StmtList. When a list of statements is
// provided at the top level in the plus section of teh patch, we should be
// able to reproduce the original container for these statements (BlockStmt,
// CaseClause, CommClause) as-is with only the statement list modified.
func (c *replacerCompiler) compilePGoStmtList(slist *pgo.StmtList) Replacer {
	var list []ast.Stmt
	if len(slist.List) > 0 {
		list = append(list, dotsStmt(c.patchStart))
		list = append(list, slist.List...)
		list = append(list, dotsStmt(c.patchEnd))
	}
	return stmtSliceContainerReplacer{
		Stmts: c.compile(reflect.ValueOf(list)),
	}
}

func (r stmtSliceContainerReplacer) Replace(d data.Data, cl Changelog, pos token.Pos) (reflect.Value, error) {
	var sd stmtListData
	if !data.Lookup(d, stmtListKey, &sd) {
		return reflect.Value{}, errors.New("no statement matches found")
	}

	// Reproduce the original struct without setting Stmts.
	node := reflect.New(sd.Type).Elem()
	for _, f := range sd.OtherFields {
		node.Field(f.FieldIdx).Set(f.Value)
	}

	stmts, err := r.Stmts.Replace(d, cl, sd.UnchangedRegion.End)
	if err != nil {
		return reflect.Value{}, err
	}
	node.Field(sd.StmtFieldIdx).Set(stmts)

	cl.Unchanged(sd.UnchangedRegion.Pos, sd.UnchangedRegion.End)
	return node.Addr(), nil
}

type _stmtListKey struct{}

var stmtListKey _stmtListKey

type stmtListData struct {
	// Type of statement that we matched.
	//
	// This is one of BlockStmt, CaseClause, or CommClause.
	Type reflect.Type

	// Index in Type at which the []ast.Stmt can be found.
	//
	// That is, Type.Field(StmtFieldIdx) should be []ast.Stmt.
	StmtFieldIdx int

	// Indexes and values of the other fields in the type.
	OtherFields []stmtListField

	// Region of the original statement (block, case, or comm) that was
	// unmodified.
	//
	// For block, it's up to the "{", for case and comm, it's up to the
	// ":" after the label.
	UnchangedRegion Region
}

type stmtListField struct {
	// Index of the field in Type.
	FieldIdx int

	// Captured value of the field.
	Value reflect.Value
}

func dotsStmt(pos token.Pos) ast.Stmt {
	return &ast.ExprStmt{X: &pgo.Dots{Dots: pos}}
}
